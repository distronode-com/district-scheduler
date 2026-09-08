package handler

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtime"
	"github.com/calnode/calnode/internal/gcal"
	"github.com/calnode/calnode/internal/secret"
)

// GoogleOAuthConfig holds decrypted Google OAuth settings loaded from the DB.
// Used by server.go to build the initial gcal and auth clients on startup.
type GoogleOAuthConfig struct {
	ClientID     string
	ClientSecret string
}

// LoadGoogleSettingsFromDB reads Google OAuth credentials from server_settings
// and decrypts the client secret. Returns nil (not an error) when client_id is empty.
func LoadGoogleSettingsFromDB(db *db.DB, encKey [32]byte) (*GoogleOAuthConfig, error) {
	var clientID, secretEnc string
	err := db.QueryRow(`
		SELECT google_client_id, google_client_secret_enc
		FROM server_settings WHERE id = 1`).
		Scan(&clientID, &secretEnc)
	if err == sql.ErrNoRows || clientID == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var clientSecret string
	if secretEnc != "" {
		clientSecret, err = secret.Decrypt(encKey, secretEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt google client secret: %w", err)
		}
	}
	return &GoogleOAuthConfig{ClientID: clientID, ClientSecret: clientSecret}, nil
}

// GetGoogleSettings handles GET /v1/settings/google (admin only).
func (h *Handler) GetGoogleSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	var clientID, secretEnc string
	err := h.db.QueryRowContext(r.Context(), `
		SELECT google_client_id, google_client_secret_enc
		FROM server_settings WHERE id = 1`).
		Scan(&clientID, &secretEnc)
	if err != nil && err != sql.ErrNoRows {
		h.logger.ErrorContext(r.Context(), "google settings: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"client_id":         clientID,
		"client_secret_set": secretEnc != "",
		"configured":        clientID != "",
		// base_url is the identity host the server builds OAuth redirect URIs
		// from (see PatchGoogleSettings); the setup UI renders the exact URIs to
		// register in Google Cloud so they always match what we send.
		"base_url": h.baseURL,
	})
}

// PatchGoogleSettings handles PATCH /v1/settings/google (admin only).
// Saves credentials to the DB and hot-reloads the gcal and Google auth clients
// so changes take effect immediately without a server restart.
// If client_secret is omitted or empty the existing stored secret is kept.
//
// ⛔ THE HOT RELOAD IS PROCESS-GLOBAL AND IS SKIPPED ENTIRELY IN MULTI-TENANT MODE
// (H1). `h.SetCalendar`, `h.SetGoogleAuth` and the `calendar.Service` built beside them
// live on *shared, which every per-request copy of the Handler points at — so ONE
// workspace's PATCH replaced the Google OAuth client that every OTHER workspace's
// calendar connect and OAuth login used, until the next restart. The row is still
// written (it is that workspace's own `server_settings`), and it takes effect for that
// workspace the way every other per-tenant setting does; what does not happen is the
// instance-wide swap.
//
// This is belt and braces: the route is wrapped in h.PlatformManaged at registration,
// so a tenant credential never reaches this handler in that mode at all. It is here
// because the wrapper is one line in another package and the blast radius of losing it
// is every tenancy on the process — the guard nearest the damage is the one that has to
// hold if the outer one is ever refactored away.
func (h *Handler) PatchGoogleSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.ClientID == "" {
		// Clearing credentials — wipe both columns so the encrypted secret
		// doesn't linger in the DB after the admin removes Google OAuth.
		if _, err := h.db.ExecContext(r.Context(), `
			UPDATE server_settings SET
			  google_client_id = '', google_client_secret_enc = '',
			  updated_at = ?
			WHERE id = 1`, dbtime.Now()); err != nil {
			h.logger.ErrorContext(r.Context(), "google settings: clear", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !h.multiTenant {
			h.SetCalendar(nil)
			h.authMu.Lock()
			h.googleAuth = nil
			h.authMu.Unlock()
		}
		h.GetGoogleSettings(w, r)
		return
	}

	if req.ClientSecret != "" {
		enc, err := secret.Encrypt(h.encKey, req.ClientSecret)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "google settings: encrypt secret", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if _, err = h.db.ExecContext(r.Context(), `
			UPDATE server_settings SET
			  google_client_id = ?, google_client_secret_enc = ?,
			  updated_at = ?
			WHERE id = 1`, req.ClientID, enc, dbtime.Now()); err != nil {
			h.logger.ErrorContext(r.Context(), "google settings: update", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	} else {
		if _, err := h.db.ExecContext(r.Context(), `
			UPDATE server_settings SET
			  google_client_id = ?,
			  updated_at = ?
			WHERE id = 1`, req.ClientID, dbtime.Now()); err != nil {
			h.logger.ErrorContext(r.Context(), "google settings: update (keep secret)", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// Hot-reload: resolve the active secret and reinitialise gcal + Google auth
	// so changes take effect without a server restart.
	resolvedSecret := req.ClientSecret
	if resolvedSecret == "" {
		var secretEnc string
		if err := h.db.QueryRowContext(r.Context(),
			`SELECT google_client_secret_enc FROM server_settings WHERE id = 1`).
			Scan(&secretEnc); err != nil {
			h.logger.ErrorContext(r.Context(), "google settings: re-read secret", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if secretEnc != "" {
			var decErr error
			resolvedSecret, decErr = secret.Decrypt(h.encKey, secretEnc)
			if decErr != nil {
				h.logger.ErrorContext(r.Context(), "google settings: decrypt existing secret", "error", decErr)
				h.writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}

	if resolvedSecret != "" && !h.multiTenant {
		encKeyHex := hex.EncodeToString(h.encKey[:])
		gc, err := gcal.New(h.db, req.ClientID, resolvedSecret, h.baseURL+"/v1/calendar/callback", encKeyHex)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "google settings: hot-reload gcal failed", "error", err)
			h.writeError(w, http.StatusInternalServerError, "failed to initialize calendar client")
			return
		}
		svc := calendar.NewService(h.db)
		svc.Register(gc)
		h.SetCalendar(svc)
		h.SetGoogleAuth(req.ClientID, resolvedSecret, h.baseURL+"/v1/auth/callback", h.secureCookie)
		h.logger.Info("google settings: credentials updated and gcal hot-reloaded")
	}

	h.GetGoogleSettings(w, r)
}
