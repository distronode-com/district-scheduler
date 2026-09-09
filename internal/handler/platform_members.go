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
	}

	// ⛔ The transfer half runs BEFORE the promotion, not after, and the order is load
	// bearing rather than stylistic. idx_users_one_owner_per_workspace refuses a second
	// live owner, and a unique INDEX is checked at the end of each statement: it cannot
	// be deferred to commit, and a PARTIAL unique cannot be written as a deferrable
	// constraint either. So promoting first would transiently make two owners and be
	// refused, inside a transaction where the demote that would have fixed it is still
	// one statement away.
	//
	// Demoting first is also what TransferOwnership has always done, so the two routes
	// that can move ownership now agree. `id <> ?` is what keeps this from demoting the
	// user about to be promoted; on a create that id does not exist yet, so the clause is
	// simply true for every current owner.
	if isOwner == 1 {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE users SET is_owner = 0 WHERE workspace_id = ? AND id <> ?`, workspaceID, userID); err != nil {
			h.logger.ErrorContext(r.Context(), "platform: demote previous owner", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if created {
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

// platformUserInWorkspace resolves {uid} within {id}, writing the 404 when the user is not
// that workspace's or is archived.
//
// ⛔ Archived is a 404 rather than a 409. The platform's view of an archived person is that
// they are not in the workspace, and minting them a credential is exactly what archiving
// was supposed to stop; a 409 would invite a retry loop against a decision that has to be
// undone through the upsert route first.
func (h *Handler) platformUserInWorkspace(w http.ResponseWriter, r *http.Request, workspaceID, userID string) bool {
	var archived sql.NullString
	err := h.db.QueryRowContext(r.Context(),
		`SELECT archived_at FROM users WHERE workspace_id = ? AND id = ?`, workspaceID, userID).
		Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && archived.Valid) {
		h.writeError(w, http.StatusNotFound, "user not found")
		return false
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: read workspace user", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}

// MintWorkspaceUserAPIKey handles POST /v1/platform/workspaces/{id}/users/{uid}/api-keys.
//
// Body: {"name"}. Response 201: {"id", "name", "api_key"} — the plaintext, once.
//
// ⛔ ROTATION, and it is the whole reason this is a transaction. Minting a key with a name
// this user already has DELETES the earlier managed key of that name in the same
// transaction. A caller that lost a key and re-mints therefore never accumulates
// credentials, and the old one stops working at the instant the new one starts rather than
// staying live and unaudited on whatever host still holds it. The trade is deliberate: a
// re-mint is a revocation, so a caller that wants two live keys has to give them two names.
//
// Only MANAGED keys of that name are rotated. A key the person minted for themselves under
// the same name is theirs, and the platform deleting it would be reaching across the line
// this packet exists to draw.
func (h *Handler) MintWorkspaceUserAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	workspaceID, userID := r.PathValue("id"), r.PathValue("uid")

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		h.writeError(w, http.StatusBadRequest, "name must be 1 to 64 characters")
		return
	}
	if !h.platformWorkspaceExists(w, r, workspaceID) {
		return
	}
	if !h.platformUserInWorkspace(w, r, workspaceID, userID) {
		return
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: mint key begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM api_keys WHERE workspace_id = ? AND user_id = ? AND name = ? AND managed = 1`,
		workspaceID, userID, name); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: rotate old key", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	keyID, plainKey, err := mintAPIKey(r.Context(), tx, workspaceID, userID, name, true, now)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: mint key", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: mint key commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.logger.InfoContext(r.Context(), "platform: api key minted",
		"workspace_id", workspaceID, "user_id", userID, "key_id", keyID, "name", name)
	h.writeJSON(w, http.StatusCreated, map[string]any{
		"id": keyID, "name": name, "api_key": plainKey,
	})
}

// DeleteWorkspaceUserAPIKey handles
// DELETE /v1/platform/workspaces/{id}/users/{uid}/api-keys/{keyId} → 204.
//
// Managed or not: a key on that user in that workspace is one the platform may revoke.
// Anything else is 404, so a caller cannot use this route to learn which key ids exist in
// a workspace it did not name.
func (h *Handler) DeleteWorkspaceUserAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	workspaceID, userID, keyID := r.PathValue("id"), r.PathValue("uid"), r.PathValue("keyId")
	if !h.platformWorkspaceExists(w, r, workspaceID) {
		return
	}

	res, err := h.db.ExecContext(r.Context(),
		`DELETE FROM api_keys WHERE workspace_id = ? AND user_id = ? AND id = ?`,
		workspaceID, userID, keyID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: delete api key", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "api key not found")
		return
	}
	h.logger.InfoContext(r.Context(), "platform: api key deleted",
		"workspace_id", workspaceID, "user_id", userID, "key_id", keyID)
	w.WriteHeader(http.StatusNoContent)
}

// MarkWorkspaceWebhooksManaged handles PATCH /v1/platform/workspaces/{id}/webhooks.
//
// Body: {"url", "managed": true}. Response 200: {"updated": n}.
//
// This is how a tenancy provisioned BEFORE migration 00063 gets its provisioning webhook
// marked. The migration could not do it: unlike the API key, whose name
// ('platform-provisioned') is written by exactly one statement in the tree, a webhook row
// carries nothing that distinguishes the platform's from one the workspace created — url,
// events and fields are all values the caller chose. The platform knows which url it gave;
// nothing in the database does.
//
// Matching is on the exact url, and every webhook in the workspace with that url is
// marked, because the same subscription may exist on more than one user. Idempotent: a
// second call reports the same count.
//
// ⛔ {"managed": false} is 400, not an unmark. Managed is one-way on purpose. The flag's
// value is that a credential caller cannot reach the row, and an un-manage route is a way
// to reach it — the platform's own way, but the platform token is one bearer, and a
// mistaken un-manage restores exactly the delete button this packet removed. Deleting the
// row and re-creating it is the reversal, and it is one an operator has to mean.
func (h *Handler) MarkWorkspaceWebhooksManaged(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	workspaceID := r.PathValue("id")

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		URL     string `json:"url"`
		Managed *bool  `json:"managed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		h.writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	if req.Managed == nil || !*req.Managed {
		h.writeError(w, http.StatusBadRequest, "managed must be true; a managed row cannot be unmanaged")
		return
	}
	if !h.platformWorkspaceExists(w, r, workspaceID) {
		return
	}

	res, err := h.db.ExecContext(r.Context(),
		`UPDATE webhooks SET managed = 1 WHERE workspace_id = ? AND url = ?`, workspaceID, req.URL)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: mark webhooks managed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	updated, _ := res.RowsAffected()

	h.logger.InfoContext(r.Context(), "platform: webhooks marked managed",
		"workspace_id", workspaceID, "url", req.URL, "updated", updated)
	h.writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// ArchiveWorkspaceUser handles POST /v1/platform/workspaces/{id}/users/{uid}/archive → 200
// {"archived": true}.
//
// It mirrors ArchiveUser's rules, minus the ones that are about an ADMIN actor rather than
// about the workspace: the platform is not a member, so "you cannot archive yourself" and
// "only the owner may archive an admin" do not apply to it. What does apply is carried
// over exactly:
//
//   - ⛔ The upcoming-bookings refusal. This fork refuses (409) to archive a member who
//     still hosts a booking that has not happened, because archiving deactivates their
//     event types and hides them from routing while the meeting stays on the calendar with
//     a host nobody can reach. Answered here as 409 {"error":"upcoming_bookings","count":n}
//     and nothing is changed. Reassign or cancel first.
//   - The owner cannot be archived — 409 {"error":"owner_cannot_be_archived"}. Same reason
//     as the demotion refusal above: a workspace with no owner cannot appoint one.
//   - Their event types are deactivated, so public booking pages stop taking bookings.
//
// Beyond ArchiveUser, and the reason this is a transaction: the person's MANAGED API keys
// and their sessions and MCP tokens are deleted with the same commit. Archiving sets
// archived_at, which RequireAuth already checks, so a live key stops working the moment it
// lands — but leaving the row means an un-archive through the upsert route would silently
// bring a credential back to life on whatever host still held it. Their own unmanaged keys
// are left alone: those are the person's, not the platform's, and are inert while archived.
func (h *Handler) ArchiveWorkspaceUser(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	workspaceID, userID := r.PathValue("id"), r.PathValue("uid")
	if !h.platformWorkspaceExists(w, r, workspaceID) {
		return
	}
	if !h.platformUserInWorkspace(w, r, workspaceID, userID) {
		return
	}

	var isOwner int
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT is_owner FROM users WHERE workspace_id = ? AND id = ?`, workspaceID, userID).
		Scan(&isOwner); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: archive read user", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if isOwner != 0 {
		h.writeJSON(w, http.StatusConflict, map[string]any{"error": "owner_cannot_be_archived"})
		return
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var upcoming int
	if err := h.db.QueryRowContext(r.Context(), `
		SELECT COUNT(*) FROM bookings
		WHERE workspace_id = ? AND host_id = ? AND status != 'cancelled' AND end_at > ?`,
		workspaceID, userID, now).Scan(&upcoming); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: archive count bookings", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if upcoming > 0 {
		h.writeJSON(w, http.StatusConflict, map[string]any{"error": "upcoming_bookings", "count": upcoming})
		return
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: archive begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// archived_by is left NULL: the archiver is the platform, which has no users row, and
	// writing an id that names nobody would make RestoreUser's "members you archived
	// yourself" check compare against a phantom. NULL means "not by a member of this
	// workspace", which is the truth and which that check already treats as owner-only.
	for _, stmt := range []struct {
		what string
		sql  string
	}{
		{"archive user", `UPDATE users SET archived_at = ? WHERE workspace_id = ? AND id = ?`},
		{"deactivate event types", `UPDATE event_types SET is_active = 0 WHERE workspace_id = ? AND user_id = ?`},
		{"revoke managed keys", `DELETE FROM api_keys WHERE workspace_id = ? AND user_id = ? AND managed = 1`},
		{"revoke sessions", `DELETE FROM sessions WHERE workspace_id = ? AND user_id = ?`},
		{"revoke oauth tokens", `DELETE FROM oauth_access_tokens WHERE workspace_id = ? AND user_id = ?`},
	} {
		var err error
		if stmt.what == "archive user" {
			_, err = tx.ExecContext(r.Context(), stmt.sql, now, workspaceID, userID)
		} else {
			_, err = tx.ExecContext(r.Context(), stmt.sql, workspaceID, userID)
		}
		if err != nil {
			h.logger.ErrorContext(r.Context(), "platform: archive: "+stmt.what, "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: archive commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.logger.InfoContext(r.Context(), "platform: workspace user archived",
		"workspace_id", workspaceID, "user_id", userID)
	h.writeJSON(w, http.StatusOK, map[string]any{"archived": true})
}
