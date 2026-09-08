package handler

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/calnode/calnode/internal/uid"
)

// keyExecer is the slice of *db.DB and *db.Tx that mintAPIKey needs.
//
// It exists because the two mints it replaced were on different receivers: CreateAPIKey
// writes one row on the handle, and CreateWorkspace writes its key inside the transaction
// that provisions the whole tenant. Taking an interface is what lets there be ONE
// implementation of the key format rather than one per caller.
type keyExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// mintAPIKey writes one api_keys row and returns its id and the plaintext key.
//
// "cno_" + 32 random bytes as hex, hashed with hashAPIKey (SHA-256) before storage,
// plaintext returned to the caller exactly once and never stored. It was written out twice
// — once here, once in CreateWorkspace — and a second copy of a credential format is how
// two drift into keys one code path mints and another cannot verify.
//
// ⚠️ It is not yet the ONLY implementation, and saying so would be the kind of confident
// comment that hides the next bug: Setup (/v1/setup, setup.go) still mints its bootstrap
// key inline. That one is the single-tenant first-user path — it runs before any workspace
// exists, names no workspace_id, and cannot mint a managed key — so folding it in is a
// separate change rather than an oversight here. `grep -rn '"cno_"' internal/` is the
// enumeration; it returns two sites.
//
// workspace_id is named rather than defaulted. On the platform handle the column default
// resolves to the empty string and the row fails its foreign key; on a bound handle the
// default resolves to the same value this names, so single-tenant and credential-scoped
// callers write byte-identical rows to the ones they wrote before.
//
// managed marks a row the PLATFORM owns: hidden from GET /v1/api-keys, refused by
// DELETE /v1/api-keys/{id}, and still accepted by RequireAuth. See migration 00063.
//
// now is passed in rather than read here so a caller writing several rows in one
// transaction stamps them all with one timestamp, and so CreateAPIKey's response carries
// the value that is actually in the row.
func mintAPIKey(ctx context.Context, ex keyExecer, workspaceID, userID, name string, managed bool, now string) (id, plainKey string, err error) {
	plainKey = "cno_" + hex.EncodeToString(mustRandom(32))
	id = uid.New()
	managedFlag := 0
	if managed {
		managedFlag = 1
	}
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO api_keys (id, workspace_id, user_id, name, key_hash, created_at, managed)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, workspaceID, userID, name, hashAPIKey(plainKey), now, managedFlag); err != nil {
		return "", "", fmt.Errorf("mint api key: %w", err)
	}
	return id, plainKey, nil
}

// ListAPIKeys handles GET /v1/api-keys.
//
// ⛔ Managed keys are omitted. A managed key is the platform's credential, minted through
// /v1/platform/workspaces/{id}/users/{uid}/api-keys and spent by an integration the
// workspace's own members do not administer; listing it here offered a delete button for a
// row DeleteAPIKey now refuses, which is worse than not showing it at all.
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT id, name, created_at, last_used_at
		FROM api_keys WHERE user_id = ? AND managed = 0
		ORDER BY created_at DESC`, user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list api keys", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	type keyItem struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		CreatedAt  string  `json:"created_at"`
		LastUsedAt *string `json:"last_used_at"`
	}
	items := []keyItem{}
	for rows.Next() {
		var item keyItem
		if err := rows.Scan(&item.ID, &item.Name, &item.CreatedAt, &item.LastUsedAt); err != nil {
			h.logger.ErrorContext(r.Context(), "scan api key row", "error", err)
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "list api keys: rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// CreateAPIKey handles POST /v1/api-keys.
// The plaintext key is returned once and must be saved by the caller.
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		h.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Name) > 255 {
		h.writeError(w, http.StatusBadRequest, "name must be 255 characters or fewer")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	// managed = false: a key a member mints for themselves is theirs to list and delete.
	keyID, plainKey, err := mintAPIKey(r.Context(), h.db, user.WorkspaceID, user.ID, req.Name, false, now)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "create api key: insert", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusCreated, map[string]any{
		"id":         keyID,
		"name":       req.Name,
		"key":        plainKey,
		"created_at": now,
		"note":       "save this key — it will not be shown again",
	})
}

// DeleteAPIKey handles DELETE /v1/api-keys/{id}.
//
// ⛔ A managed key answers 403, not 404. The row exists and the caller owns the user it
// hangs off — telling them it does not exist would be a lie they can disprove, and the
// honest answer is the actionable one: the platform minted it and only the platform can
// revoke it. The DELETE is still scoped by user_id first, so a key belonging to somebody
// else is 404 exactly as before, managed or not.
func (h *Handler) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")

	var managed int
	err := h.db.QueryRowContext(r.Context(),
		`SELECT managed FROM api_keys WHERE id = ? AND user_id = ?`, id, user.ID).Scan(&managed)
	if err == sql.ErrNoRows {
		h.writeError(w, http.StatusNotFound, "api key not found")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "delete api key: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if managed != 0 {
		h.writeError(w, http.StatusForbidden, managedRowMessage)
		return
	}

	res, err := h.db.ExecContext(r.Context(), `
		DELETE FROM api_keys WHERE id = ? AND user_id = ? AND managed = 0`, id, user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "delete api key", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "api key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// managedRowMessage is the one refusal text for a managed api_keys or webhooks row, so the
// website and the admin UI can match on a single string across both surfaces.
const managedRowMessage = "managed by your platform"
