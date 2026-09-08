package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/uid"
)

// The platform member API (F1).
//
// The identity provider — not this fork's credential routes — owns who is in a workspace
// and what they may do there. Each of its people acts in the scheduler as their own user
// with a platform-minted cno_ key, so a booking, an audit line and an API call all name a
// person rather than a shared service account.
//
// Everything here is Platform-wrapped, so h.db is the platform handle: unscoped, policy
// bypassing, binding the empty string. The two rules the rest of platform.go obeys apply
// unchanged and are the reason every statement below names workspace_id — there is no
// policy behind this file to catch a forgotten predicate, and an INSERT that omits the
// column resolves it to '' and fails its foreign key.
//
// ⛔ These deliberately do NOT reuse ssoResolveUser. That resolver never rewrites an
// existing user's role and refuses an archived user, and both are the opposite of what an
// upsert from the identity provider is for: a role change made there has to land here, and
// a person who comes back has to be able to come back.

// platformRoleFlags maps the wire role onto the two columns that store it. Owner implies
// admin, exactly as AuthUser.Role() reads them back.
func platformRoleFlags(role string) (isAdmin, isOwner int, ok bool) {
	switch role {
	case "member":
		return 0, 0, true
	case "admin":
		return 1, 0, true
	case "owner":
		return 1, 1, true
	}
	return 0, 0, false
}

// platformWorkspaceExists reports whether the workspace in the URL exists, writing the 404
// if it does not. Every route in this file calls it before touching a tenant row: on the
// platform handle an unknown workspace is not an error, it is an empty result set, and an
// upsert into one would fail at the foreign key with a 500 instead of the truthful 404.
func (h *Handler) platformWorkspaceExists(w http.ResponseWriter, r *http.Request, id string) bool {
	if _, err := h.readPlatformWorkspace(r.Context(), id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			h.logger.ErrorContext(r.Context(), "platform: read workspace", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return false
		}
		h.writeError(w, http.StatusNotFound, "workspace not found")
		return false
	}
	return true
}

// UpsertWorkspaceUser handles POST /v1/platform/workspaces/{id}/users.
//
// Body: {"email", "name", "role": "owner"|"admin"|"member", "timezone"?}.
// Response 200: {"id", "email", "name", "role", "created"}.
//
// One statement per fact, one transaction for all of them, because the role half can
// rewrite a second row: promoting somebody to owner demotes whoever holds it, and a
// workspace that briefly had two owners or none would be a state TransferOwnership has
// maintained the absence of since it was written.
//
// ⛔ Demoting the CURRENT owner is refused (409 owner_demotion_requires_transfer) rather
// than obeyed. Ownership is a transfer, not an attribute: obeying it would leave the
// workspace with no owner, which nothing in the fork can then repair, since every route
// that grants ownership requires an owner to call it. The caller's move is to upsert the
// NEW owner with role "owner" — which demotes this one in the same transaction — and then
// re-send the demotion if it still wants it.
func (h *Handler) UpsertWorkspaceUser(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	workspaceID := r.PathValue("id")

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Email    string  `json:"email"`
		Name     string  `json:"name"`
		Role     string  `json:"role"`
		Timezone *string `json:"timezone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		h.writeError(w, http.StatusBadRequest, "email must be an email address")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		h.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	isAdmin, isOwner, ok := platformRoleFlags(req.Role)
	if !ok {
		h.writeError(w, http.StatusBadRequest, "role must be owner, admin or member")
		return
	}
	if req.Timezone != nil {
		if _, err := time.LoadLocation(*req.Timezone); err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid timezone: "+*req.Timezone)
			return
		}
	}

	if !h.platformWorkspaceExists(w, r, workspaceID) {
		return
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: upsert user begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// Read before write rather than a single upsert statement, and it is not only for the
	// `created` flag: the two engines cannot spell one upsert between them. users is
	// unique on (workspace_id, email) on Postgres (00060) and still globally unique on
	// email on SQLite, so `ON CONFLICT (workspace_id, email)` names a constraint SQLite
	// does not have and is a hard error there. A select then a write is the same statement
	// on both.
	var existingID string
	var existingIsOwner int
	err = tx.QueryRowContext(r.Context(),
		`SELECT id, is_owner FROM users WHERE workspace_id = ? AND email = ?`, workspaceID, email).
		Scan(&existingID, &existingIsOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		h.logger.ErrorContext(r.Context(), "platform: upsert user read", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	created := errors.Is(err, sql.ErrNoRows)

	// The zero-owner guard. Only the sole current owner being demoted is refused: an owner
	// re-sent as owner is a no-op, and a NEW owner arriving demotes this one below, which
	// is a transfer and leaves the count at one either way.
	if !created && existingIsOwner != 0 && isOwner == 0 {
		var owners int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM users WHERE workspace_id = ? AND is_owner = 1`, workspaceID).
			Scan(&owners); err != nil {
			h.logger.ErrorContext(r.Context(), "platform: count owners", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if owners <= 1 {
			h.writeError(w, http.StatusConflict, "owner_demotion_requires_transfer")
			return
		}
	}

	userID := existingID
	if created {
		userID = uid.New()
		// iana_timezone is omitted when the caller sent none, so the column default
		// ('UTC') applies rather than this route inventing a zone. email_login is omitted
		// for the same reason: whether a person may sign in with a password is a fact
		// about this fork's own login, not about the identity provider's directory.
		if req.Timezone != nil {
			_, err = tx.ExecContext(r.Context(), `
				INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				userID, workspaceID, email, name, *req.Timezone, isAdmin, isOwner)
		} else {
			_, err = tx.ExecContext(r.Context(), `
				INSERT INTO users (id, workspace_id, email, name, is_admin, is_owner)
				VALUES (?, ?, ?, ?, ?, ?)`,
				userID, workspaceID, email, name, isAdmin, isOwner)
		}
		if err != nil {
			if db.IsUniqueViolation(err) {
				// Two concurrent upserts of the same address, or (on SQLite, where the
				// unique is still global) the address already exists in another workspace.
				h.writeError(w, http.StatusConflict, "email already exists")
				return
			}
			h.logger.ErrorContext(r.Context(), "platform: insert user", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	} else {
		// ⛔ archived_at is cleared. An upsert from the directory is the statement "this
		// person is in this workspace", and refusing to un-archive would make a returning
		// employee unreachable through the only API that is supposed to own the answer.
		// archived_by goes with it, or RestoreUser's "you may only restore members you
		// archived" would still be reasoning from a stale actor. Their managed keys were
		// revoked when they were archived (see ArchiveWorkspaceUser), so nothing that was
		// cut comes back with them; the platform mints a new key.
		//
		// iana_timezone is written only when the caller sent one: a person's zone is
		// something they set in this fork's own profile, and an upsert that omits it must
		// not reset it to a default on every sync.
		if req.Timezone != nil {
			_, err = tx.ExecContext(r.Context(), `
				UPDATE users SET name = ?, iana_timezone = ?, is_admin = ?, is_owner = ?,
				                 archived_at = NULL, archived_by = NULL
				WHERE workspace_id = ? AND id = ?`,
				name, *req.Timezone, isAdmin, isOwner, workspaceID, userID)
		} else {
			_, err = tx.ExecContext(r.Context(), `
				UPDATE users SET name = ?, is_admin = ?, is_owner = ?,
				                 archived_at = NULL, archived_by = NULL
				WHERE workspace_id = ? AND id = ?`,
				name, isAdmin, isOwner, workspaceID, userID)
		}
		if err != nil {
			h.logger.ErrorContext(r.Context(), "platform: update user", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// The transfer half. Unconditional for role owner, including on a create: whoever held
	// it is demoted in the same transaction that promotes this one, so the single-owner
	// invariant TransferOwnership maintains holds after every call to this route as well.
	// `id <> ?` is what keeps it from demoting the user it just promoted.
	if isOwner == 1 {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE users SET is_owner = 0 WHERE workspace_id = ? AND id <> ?`, workspaceID, userID); err != nil {
			h.logger.ErrorContext(r.Context(), "platform: demote previous owner", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: upsert user commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.logger.InfoContext(r.Context(), "platform: workspace user upserted",
		"workspace_id", workspaceID, "user_id", userID, "role", req.Role, "created", created)
	h.writeJSON(w, http.StatusOK, map[string]any{
		"id": userID, "email": email, "name": name, "role": req.Role, "created": created,
	})
}
