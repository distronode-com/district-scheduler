package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/secret"
	"github.com/calnode/calnode/internal/uid"
	"github.com/calnode/calnode/internal/webhook"
)

// The platform API (D12). Workspace provisioning for a multi-tenant instance: create a
// tenant, read it, change its host or status, delete it.
//
// Every route here is Platform-wrapped, so h.db is the platform handle — it bypasses the
// row-level security policies and binds no workspace. Two consequences run through this
// whole file:
//
//   - ⛔ EVERY INSERT NAMES workspace_id. The column default is
//     COALESCE(current_setting('app.workspace_id', true), 'default'), and the platform
//     handle sets that parameter to '' before each statement, so an unnamed column
//     resolves to '' and the row fails its foreign key to workspaces(id) with SQLSTATE
//     23503. (An earlier note in PROGRESS.md said such a row lands silently in the
//     `default` workspace; that is what happens on a handle that never sets the
//     parameter at all, not on the paired platform handle. The rule is the same either
//     way, and it is the reason it is stated here rather than assumed.)
//   - Reads are equally unscoped, so every one of them carries its own workspace_id
//     predicate. There is no policy behind this file to catch a forgotten WHERE.
//
// Authentication is a bearer token from CALNODE_PLATFORM_TOKEN, compared in constant
// time. With the token unset — or on a single-tenant instance, which has no workspaces to
// provision — every route 404s rather than 401ing, so a prober cannot tell a
// multi-tenant control plane from an instance that does not implement one.

// SetPlatformToken configures the platform API's bearer token. Empty leaves the API off.
// Set once at boot from config, like SetSSOSecret, so there is no lock here.
func (h *Handler) SetPlatformToken(token string) { h.platformToken = token }

// SetPlatformReturnOrigins configures the origins a platform console may ask the calendar
// OAuth round trip to return to (PLATFORM_RETURN_ORIGINS). Empty leaves the feature off,
// and with it off a `return_to` is REFUSED rather than ignored — see returnToFromRequest.
// Set once at boot from config, like SetPlatformToken, so there is no lock here.
//
// The slice is copied: config's is retained by the *config.Config the caller may keep,
// and an allowlist that another package can append to is not an allowlist.
func (h *Handler) SetPlatformReturnOrigins(origins []string) {
	h.platformReturnOrigins = append([]string(nil), origins...)
}

// platformAuthorized gates every route in this file. It writes the response on failure
// and reports whether the caller may proceed.
func (h *Handler) platformAuthorized(w http.ResponseWriter, r *http.Request) bool {
	// Off unless configured, and off on a single-tenant instance. 404 for both, because
	// "this endpoint does not exist here" is the truth in both cases and neither answer
	// should tell a prober which one applies.
	if h.platformToken == "" || !h.multiTenant {
		http.NotFound(w, r)
		return false
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(h.platformToken)) != 1 {
		h.logger.WarnContext(r.Context(), "platform api: bad token", "path", r.URL.Path)
		h.writeError(w, http.StatusUnauthorized, "invalid platform token")
		return false
	}
	return true
}

// provisionedWebhookEvents is what a provisioned workspace subscribes to: every event this
// codebase emits. Kept as one named list so the set is reviewable and testable rather than
// inline in a marshal call.
var provisionedWebhookEvents = []string{
	"booking.created",
	"booking.cancelled",
	"booking.rescheduled",
	"booking.reminder",
	"recording.completed",
	"transcript.ready",
	"notes.ready",
}

// platformWorkspaceRequest is the create body (D12), settled against the website client.
type platformWorkspaceRequest struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	PublicHost    string `json:"public_host"`
	Region        string `json:"region"`
	OwnerEmail    string `json:"owner_email"`
	OwnerName     string `json:"owner_name"`
	OwnerTimezone string `json:"owner_timezone"`
	Defaults      struct {
		EmbedAllowedOrigins []string `json:"embed_allowed_origins"`
		Webhook             struct {
			URL    string   `json:"url"`
			Secret string   `json:"secret"`
			Fields []string `json:"fields"`
		} `json:"webhook"`
		EventType struct {
			Slug             string `json:"slug"`
			Name             string `json:"name"`
			DurationMinutes  int    `json:"duration_minutes"`
			MinNoticeMinutes int    `json:"min_notice_minutes"`
			MaxFutureDays    int    `json:"max_future_days"`
			Availability     []struct {
				DayOfWeek int    `json:"day_of_week"`
				StartTime string `json:"start_time"`
				EndTime   string `json:"end_time"`
			} `json:"availability"`
		} `json:"event_type"`
		LiveKitURL       string `json:"livekit_url"`
		LiveKitAPIKey    string `json:"livekit_api_key"`
		LiveKitAPISecret string `json:"livekit_api_secret"`
		STTBaseURL       string `json:"stt_base_url"`
		SMTP             *struct {
			Host     string `json:"host"`
			Port     string `json:"port"`
			User     string `json:"user"`
			Pass     string `json:"pass"`
			TLS      bool   `json:"tls"`
			StartTLS bool   `json:"starttls"`
			From     string `json:"from"`
			FromName string `json:"from_name"`
		} `json:"smtp"`
		LLM *struct {
			Endpoint          string `json:"endpoint"`
			Model             string `json:"model"`
			APIKey            string `json:"api_key"`
			Enabled           bool   `json:"enabled"`
			ExtraInstructions string `json:"extra_instructions"`
		} `json:"llm"`
	} `json:"defaults"`
}

// CreateWorkspace handles POST /v1/platform/workspaces.
//
// One transaction provisions the whole tenant: the workspaces row, its server_settings
// row seeded from defaults, the owner user, that owner's first cno_ key, the webhook
// subscription, and the default event type with its availability rules. Either a
// workspace exists complete or it does not exist — a half-provisioned tenant would answer
// requests with no owner, or hand out a booking page with no availability.
//
// The api_key and webhook_secret in the 201 are the only time either is legible.
func (h *Handler) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req platformWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if msg := validatePlatformWorkspace(&req); msg != "" {
		h.writeError(w, http.StatusBadRequest, msg)
		return
	}

	ownerID := uid.New()
	etID := uid.New()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// The workspaces row first: every other INSERT below names it as a foreign key, so
	// this is also where a duplicate id or public_host is caught. Both uniques are real
	// constraints rather than a pre-read, so two concurrent provisions of the same tenant
	// cannot both win.
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO workspaces (id, slug, public_host, region, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?)`,
		req.ID, req.Slug, req.PublicHost, req.Region, now, now); err != nil {
		if db.IsUniqueViolation(err) {
			h.writeError(w, http.StatusConflict, "workspace id, slug or public_host already exists")
			return
		}
		h.logger.ErrorContext(r.Context(), "platform: insert workspace", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.seedWorkspaceSettings(r.Context(), tx, &req, now); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: seed settings", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The owner. iana_timezone is the REQUESTED timezone, not UTC: it is what the admin
	// UI renders every time in and what the default event type's availability below is
	// expressed in, so defaulting it would silently move the workspace's working hours.
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner, email_login)
		VALUES (?, ?, ?, ?, ?, 1, 1, 0)`,
		ownerID, req.ID, strings.ToLower(req.OwnerEmail), req.OwnerName, req.OwnerTimezone); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: insert owner", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// managed = 1: this key belongs to the platform, not to the owner it hangs off. It is
	// what an integration on the other side of provisioning spends, and until 00063 the
	// owner could delete it from the admin UI's API-keys page — one click, no warning, and
	// the integration stops. Managed rows are hidden from GET /v1/api-keys and refused
	// (403) by DELETE /v1/api-keys/{id}; RequireAuth still accepts them, and the platform
	// can rotate or delete it through /v1/platform/workspaces/{id}/users/{uid}/api-keys.
	_, plainKey, err := mintAPIKey(r.Context(), tx, req.ID, ownerID, "platform-provisioned", true, now)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: insert api key", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.seedWorkspaceEventType(r.Context(), tx, &req, etID, ownerID); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: seed event type", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The webhook rides the same transaction, and names workspace_id like everything
	// else here. The secret's encoding comes from webhook.NewSecret so the convention
	// (raw bytes encrypted, hex handed out) has exactly one implementation.
	webhookSecret := ""
	if url := req.Defaults.Webhook.URL; url != "" {
		plainSecret, encSecret, err := h.webhookSvc.NewSecret(req.Defaults.Webhook.Secret)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// ⛔ All SEVEN events this codebase emits, not just the booking four. The receiver
		// on the other side of a provisioned tenancy handles all of them, and a
		// subscription written with four means the three media events are silently never
		// delivered — a gap that shows up as "recordings never appear" long after
		// provisioning, with nothing in either system to point at.
		//
		// The three media events fire only when recording or the notetaker is switched on,
		// so a tenancy without them simply never receives them: subscribing is free, and
		// not subscribing is a decision the operator cannot see or undo without an API
		// call nobody knows to make. Enumerated from the tree, not from memory - every
		// Enqueue call site in internal/ emits one of these and nothing else.
		events, _ := json.Marshal(provisionedWebhookEvents)
		// A NULL fields column means "the default payload set" (migration 00027), which
		// is not the same as an empty selection — so an empty list stays NULL rather than
		// becoming [], and the webhook keeps the original booking-metadata shape.
		var fieldsJSON any
		if len(req.Defaults.Webhook.Fields) > 0 {
			fb, _ := json.Marshal(webhook.ValidFields(req.Defaults.Webhook.Fields))
			fieldsJSON = string(fb)
		}
		// managed = 1 for the same reason the key above carries it: this subscription is
		// the platform's, not the owner's. It is how the provisioning caller hears about
		// every booking in the tenancy, and it hung off the owner user where the admin
		// UI's webhooks page listed it with a delete button. Managed webhooks are hidden
		// from GET /v1/webhooks and refused by PATCH/DELETE; the DELIVERY side
		// (webhook.Service.Enqueue) is deliberately untouched and still finds them.
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO webhooks (id, workspace_id, user_id, url, events, fields, secret_enc, managed)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
			uid.New(), req.ID, ownerID, url, string(events), fieldsJSON, encSecret); err != nil {
			h.logger.ErrorContext(r.Context(), "platform: insert webhook", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		webhookSecret = plainSecret
	}

	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.logger.InfoContext(r.Context(), "platform: workspace provisioned",
		"workspace_id", req.ID, "public_host", req.PublicHost, "owner", req.OwnerEmail)
	h.writeJSON(w, http.StatusCreated, map[string]any{
		"api_key":        plainKey,
		"webhook_secret": webhookSecret,
		"note":           "save the api_key and webhook_secret — neither is shown again",
	})
}

// seedWorkspaceSettings writes the workspace's single server_settings row. id = 1 is kept
// per workspace (D8), so the ~40 `WHERE id = 1` reads elsewhere need no edit.
func (h *Handler) seedWorkspaceSettings(ctx context.Context, tx *db.Tx, req *platformWorkspaceRequest, now string) error {
	var smtpPassEnc, llmKeyEnc, livekitSecretEnc string
	encrypt := func(plain string) (string, error) {
		if plain == "" {
			return "", nil
		}
		return secret.Encrypt(h.encKey, plain)
	}
	var err error
	if req.Defaults.SMTP != nil {
		if smtpPassEnc, err = encrypt(req.Defaults.SMTP.Pass); err != nil {
			return fmt.Errorf("encrypt smtp password: %w", err)
		}
	}
	if req.Defaults.LLM != nil {
		if llmKeyEnc, err = encrypt(req.Defaults.LLM.APIKey); err != nil {
			return fmt.Errorf("encrypt llm api key: %w", err)
		}
	}
	if livekitSecretEnc, err = encrypt(req.Defaults.LiveKitAPISecret); err != nil {
		return fmt.Errorf("encrypt livekit secret: %w", err)
	}

	smtpHost, smtpPort, smtpUser, emailFrom, emailFromName := "", "", "", "", ""
	smtpTLS, smtpStartTLS := 0, 0
	if s := req.Defaults.SMTP; s != nil {
		smtpHost, smtpPort, smtpUser = s.Host, s.Port, s.User
		emailFrom, emailFromName = s.From, s.FromName
		if s.TLS {
			smtpTLS = 1
		}
		if s.StartTLS {
			smtpStartTLS = 1
		}
	}
	llmEndpoint, llmModel, llmExtra := "", "", ""
	llmEnabled := 0
	if l := req.Defaults.LLM; l != nil {
		llmEndpoint, llmModel, llmExtra = l.Endpoint, l.Model, l.ExtraInstructions
		if l.Enabled {
			llmEnabled = 1
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO server_settings
		  (workspace_id, id, smtp_host, smtp_port, smtp_user, smtp_pass_enc, smtp_tls, smtp_starttls,
		   email_from, email_from_name, updated_at,
		   llm_endpoint, llm_model, llm_api_key_enc, llm_enabled, llm_extra_instructions,
		   livekit_url, livekit_api_key, livekit_api_secret_enc,
		   embed_allowed_origins, stt_base_url)
		VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, smtpHost, smtpPort, smtpUser, smtpPassEnc, smtpTLS, smtpStartTLS,
		emailFrom, emailFromName, now,
		llmEndpoint, llmModel, llmKeyEnc, llmEnabled, llmExtra,
		req.Defaults.LiveKitURL, req.Defaults.LiveKitAPIKey, livekitSecretEnc,
		strings.Join(req.Defaults.EmbedAllowedOrigins, ","), req.Defaults.STTBaseURL)
	return err
}

// seedWorkspaceEventType writes the default event type, its owner host row and its
// availability rules.
//
// location_type 'link' and routing_mode 'fixed' are the schema's own defaults (the CHECK
// constraints in migration 00001 admit no 'none' or 'single'), which is what a
// single-host event type created through the admin UI gets. The platform API does not
// take either: a provisioning caller sets up a tenant, and how one event type meets is
// something its owner changes in the UI afterwards.
//
// The rules carry no timezone of their own: availability is stored as local HH:MM and
// interpreted in the OWNER's iana_timezone, which is why owner_timezone is required
// rather than defaulted (a 09:00 rule means nothing until you know whose 09:00).
func (h *Handler) seedWorkspaceEventType(ctx context.Context, tx *db.Tx, req *platformWorkspaceRequest, etID, ownerID string) error {
	et := req.Defaults.EventType
	if et.Slug == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_types
		  (id, workspace_id, user_id, slug, name, duration_minutes,
		   min_notice_minutes, max_future_days, location_type, routing_mode)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'link', 'fixed')`,
		etID, req.ID, ownerID, et.Slug, et.Name, et.DurationMinutes,
		et.MinNoticeMinutes, et.MaxFutureDays); err != nil {
		return fmt.Errorf("insert event type: %w", err)
	}

	// Seed the owner as the single required host, exactly as POST /v1/event-types does.
	//
	// ⛔ LOAD-BEARING, and silent when it is missing. resolveEventTypeHosts reads
	// event_type_hosts and nothing else, so an event type with no row here has no host
	// pool: GET /v1/event-types/{slug}/slots answers 200 with {"hosts":{},"slots":[]} and
	// nothing can ever be booked. Nothing else says so — the booking page renders, the
	// public payload lists the owner by another path, and the workspace reads healthy —
	// so a tenancy provisioned without it looks complete and is not.
	//
	// workspace_id is named for the same reason the two statements around it name it:
	// provisioning runs on the PLATFORM handle, which binds '', so the column's default
	// (COALESCE(current_setting('app.workspace_id', true), 'default')) would not resolve
	// to this tenant.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_type_hosts (id, workspace_id, event_type_id, user_id, role, priority)
		VALUES (?, ?, ?, ?, 'required', 0)`,
		uid.New(), req.ID, etID, ownerID); err != nil {
		return fmt.Errorf("insert event type host: %w", err)
	}

	for _, a := range et.Availability {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO availability_rules
			  (id, workspace_id, user_id, event_type_id, day_of_week, start_time, end_time)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uid.New(), req.ID, ownerID, etID, a.DayOfWeek, a.StartTime, a.EndTime); err != nil {
			return fmt.Errorf("insert availability rule: %w", err)
		}
	}
	return nil
}

// GetWorkspace handles GET /v1/platform/workspaces/{id}.
func (h *Handler) GetWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	ws, err := h.readPlatformWorkspace(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: read workspace", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, ws)
}

// PatchWorkspace handles PATCH /v1/platform/workspaces/{id} — public_host, status, slug.
//
// Nothing else is patchable: the id is referenced by every tenant row, and the region is
// where the data physically is, so neither is a field an operator can change by writing
// to it.
func (h *Handler) PatchWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	id := r.PathValue("id")

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		PublicHost *string `json:"public_host"`
		Status     *string `json:"status"`
		Slug       *string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	set := []string{"updated_at = ?"}
	args := []any{time.Now().UTC().Format(time.RFC3339Nano)}
	if req.PublicHost != nil {
		host := hostOnly(*req.PublicHost)
		if host == "" {
			h.writeError(w, http.StatusBadRequest, "public_host cannot be empty")
			return
		}
		set = append(set, "public_host = ?")
		args = append(args, host)
	}
	if req.Status != nil {
		if *req.Status != "active" && *req.Status != "suspended" {
			h.writeError(w, http.StatusBadRequest, "status must be active or suspended")
			return
		}
		set = append(set, "status = ?")
		args = append(args, *req.Status)
	}
	if req.Slug != nil {
		if strings.TrimSpace(*req.Slug) == "" {
			h.writeError(w, http.StatusBadRequest, "slug cannot be empty")
			return
		}
		set = append(set, "slug = ?")
		args = append(args, *req.Slug)
	}
	if len(set) == 1 {
		h.writeError(w, http.StatusBadRequest, "nothing to update: send public_host, status or slug")
		return
	}
	args = append(args, id)

	res, err := h.db.ExecContext(r.Context(),
		`UPDATE workspaces SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...) // #nosec G202 -- set holds only hardcoded "col = ?" literals; every value is bound
	if err != nil {
		if db.IsUniqueViolation(err) {
			h.writeError(w, http.StatusConflict, "slug or public_host already exists")
			return
		}
		h.logger.ErrorContext(r.Context(), "platform: patch workspace", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "workspace not found")
		return
	}

	ws, err := h.readPlatformWorkspace(r.Context(), id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: read back workspace", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.logger.InfoContext(r.Context(), "platform: workspace patched", "workspace_id", id)
	h.writeJSON(w, http.StatusOK, ws)
}

// DeleteWorkspace handles DELETE /v1/platform/workspaces/{id}.
//
// The row goes and Postgres cascades every tenant table with it (migration 00060's
// REFERENCES workspaces(id) ON DELETE CASCADE). Recordings are the exception that cannot
// cascade: the rows go, but the objects live in S3, so their keys are returned and
// deleting them is the caller's job. Returning them AFTER the delete would mean reading a
// table that no longer has the rows, so they are collected first.
func (h *Handler) DeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	id := r.PathValue("id")

	keys := []string{}
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT object_key FROM recordings WHERE workspace_id = ? AND object_key <> '' ORDER BY object_key`, id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: list recording keys", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			h.logger.ErrorContext(r.Context(), "platform: scan recording key", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		h.logger.ErrorContext(r.Context(), "platform: iterate recording keys", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows.Close()

	res, err := h.db.ExecContext(r.Context(), `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: delete workspace", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "workspace not found")
		return
	}

	h.logger.InfoContext(r.Context(), "platform: workspace deleted",
		"workspace_id", id, "recording_objects", len(keys))
	h.writeJSON(w, http.StatusOK, map[string]any{"recording_object_keys": keys})
}

// readPlatformWorkspace reads one workspace row for the API's responses.
func (h *Handler) readPlatformWorkspace(ctx context.Context, id string) (map[string]any, error) {
	var wsID, slug, publicHost, region, status, createdAt, updatedAt string
	if err := h.db.QueryRowContext(ctx, `
		SELECT id, slug, public_host, region, status, created_at, updated_at
		FROM workspaces WHERE id = ?`, id).
		Scan(&wsID, &slug, &publicHost, &region, &status, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return map[string]any{
		"id": wsID, "slug": slug, "public_host": publicHost, "region": region,
		"status": status, "created_at": createdAt, "updated_at": updatedAt,
	}, nil
}

// validatePlatformWorkspace returns a client-facing message for the first thing wrong
// with a create body, or "" when it is usable.
//
// It is deliberately strict about the id: db.ForWorkspace validates the same shape before
// binding, so an id this API accepted but that could not be bound would produce a
// workspace whose every request failed with ErrInvalidWorkspace.
func validatePlatformWorkspace(req *platformWorkspaceRequest) string {
	if !db.ValidWorkspaceID(req.ID) {
		return "id must match ^[a-z0-9_-]{1,64}$"
	}
	if req.ID == db.DefaultWorkspaceID {
		return "id " + db.DefaultWorkspaceID + " is reserved"
	}
	if strings.TrimSpace(req.Slug) == "" {
		return "slug is required"
	}
	req.PublicHost = hostOnly(req.PublicHost)
	if req.PublicHost == "" {
		return "public_host is required"
	}
	if strings.TrimSpace(req.OwnerEmail) == "" || !strings.Contains(req.OwnerEmail, "@") {
		return "owner_email must be an email address"
	}
	if strings.TrimSpace(req.OwnerName) == "" {
		return "owner_name is required"
	}
	if req.OwnerTimezone == "" {
		return "owner_timezone is required"
	}
	if _, err := time.LoadLocation(req.OwnerTimezone); err != nil {
		return "invalid owner_timezone: " + req.OwnerTimezone
	}
	et := req.Defaults.EventType
	if et.Slug != "" {
		if strings.TrimSpace(et.Name) == "" {
			return "defaults.event_type.name is required"
		}
		if et.DurationMinutes <= 0 {
			return "defaults.event_type.duration_minutes must be positive"
		}
		// validHHMM is override.go's, deliberately reused: availability_rules stores the
		// same HH:MM shape for both surfaces and two validators would drift.
		for _, a := range et.Availability {
			if a.DayOfWeek < 0 || a.DayOfWeek > 6 {
				return "availability day_of_week must be 0 (Sunday) through 6 (Saturday)"
			}
			if !validHHMM(a.StartTime) || !validHHMM(a.EndTime) {
				return "availability start_time and end_time must be HH:MM"
			}
			if a.StartTime >= a.EndTime {
				return "availability start_time must be before end_time"
			}
		}
	}
	return ""
}

// mustRandom returns n cryptographically random bytes. rand.Read from crypto/rand cannot
// fail on any platform this runs on (it panics internally on a broken CSPRNG since Go
// 1.24), so there is no error to thread through the caller.
func mustRandom(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return b
}
