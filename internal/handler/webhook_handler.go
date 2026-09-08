package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/calnode/calnode/internal/netutil"
	"github.com/calnode/calnode/internal/webhook"
)

var validWebhookEvents = []string{
	"booking.created", "booking.cancelled", "booking.rescheduled", "booking.reminder",
	"recording.completed", "transcript.ready", "notes.ready",
}

func (h *Handler) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)

	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Fields []string `json:"fields"` // optional payload field selection; nil = default set
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.URL == "" {
		h.writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	u, err := url.ParseRequestURI(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		h.writeError(w, http.StatusBadRequest, "url must be a valid http or https URL")
		return
	}
	// ⛔ `http://` stays legal on a single-tenant instance and is refused on a
	// multi-tenant one (L1). A booking payload carries the attendee's name, email address
	// and intake answers, so plaintext delivery is a disclosure — but a self-hoster
	// posting to a receiver on their own machine is the intended configuration of a
	// self-hostable product, and the tier of that judgement is the same one the CalDAV
	// guard makes: on a multi-tenant instance the URL is a TENANT's, the traffic leaves
	// the operator's network, and the operator is the one whose customers' data it is.
	//
	// Enforced here rather than only in the platform's own op catalog, which already
	// requires https, because a tenant admin holds a raw `cno_` key their Developer tab
	// minted and can reach this route without going through it.
	if h.multiTenant && u.Scheme != "https" {
		h.writeError(w, http.StatusBadRequest, "url must be https: booking payloads carry attendee details")
		return
	}
	if err := validateWebhookURL(r.Context(), u); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Events) == 0 {
		h.writeError(w, http.StatusBadRequest, "events must not be empty")
		return
	}
	for _, e := range req.Events {
		if !slices.Contains(validWebhookEvents, e) {
			h.writeError(w, http.StatusBadRequest, "unknown event: "+e)
			return
		}
	}

	wh, secret, err := h.webhookSvc.Create(r.Context(), user.ID, req.URL, req.Events)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "create webhook", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Apply the field selection (if any) as a follow-up update so Create keeps its
	// stable signature; unknown keys are filtered out.
	if req.Fields != nil {
		if err := h.webhookSvc.Update(r.Context(), user.ID, wh.ID, nil, &req.Fields); err != nil {
			h.logger.ErrorContext(r.Context(), "create webhook: set fields", "error", err)
		} else {
			wh.Fields = webhook.ValidFields(req.Fields)
		}
	}

	h.writeJSON(w, http.StatusCreated, map[string]any{
		"id":         wh.ID,
		"url":        wh.URL,
		"events":     wh.Events,
		"fields":     wh.Fields,
		"secret":     secret,
		"is_active":  wh.IsActive,
		"created_at": wh.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// PatchWebhook handles PATCH /v1/webhooks/{id} — update events and/or the payload
// field selection of an existing webhook.
func (h *Handler) PatchWebhook(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)

	var req struct {
		Events *[]string `json:"events"`
		Fields *[]string `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Events != nil {
		if len(*req.Events) == 0 {
			h.writeError(w, http.StatusBadRequest, "events must not be empty")
			return
		}
		for _, e := range *req.Events {
			if !slices.Contains(validWebhookEvents, e) {
				h.writeError(w, http.StatusBadRequest, "unknown event: "+e)
				return
			}
		}
	}
	if err := h.webhookSvc.Update(r.Context(), user.ID, id, req.Events, req.Fields); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		if errors.Is(err, webhook.ErrManaged) {
			h.writeError(w, http.StatusForbidden, managedRowMessage)
			return
		}
		h.logger.ErrorContext(r.Context(), "update webhook", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())

	webhooks, err := h.webhookSvc.List(r.Context(), user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list webhooks", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	items := make([]map[string]any, len(webhooks))
	for i, wh := range webhooks {
		items[i] = map[string]any{
			"id":         wh.ID,
			"url":        wh.URL,
			"events":     wh.Events,
			"fields":     wh.Fields,
			"is_active":  wh.IsActive,
			"created_at": wh.CreatedAt.UTC().Format(time.RFC3339),
		}
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// DeleteWebhook handles DELETE /v1/webhooks/{id}.
//
// ⛔ A managed webhook answers 403, not 404 — the row exists, the caller owns the user it
// hangs off, and the actionable answer is that the platform provisioned it. This is the
// subscription an integration receives every booking on; deleting it used to be one click
// on the settings page and broke the integration with nothing on either side to say so.
func (h *Handler) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")

	if err := h.webhookSvc.Delete(r.Context(), user.ID, id); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		if errors.Is(err, webhook.ErrManaged) {
			h.writeError(w, http.StatusForbidden, managedRowMessage)
			return
		}
		h.logger.ErrorContext(r.Context(), "delete webhook", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	webhookID := r.PathValue("id")

	deliveries, err := h.webhookSvc.ListDeliveries(r.Context(), user.ID, webhookID)
	if err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "list deliveries", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	items := make([]map[string]any, len(deliveries))
	for i, d := range deliveries {
		item := map[string]any{
			"id":            d.ID,
			"webhook_id":    d.WebhookID,
			"event":         d.Event,
			"status":        d.Status,
			"attempt_count": d.AttemptCount,
		}
		if d.BookingID != "" {
			item["booking_id"] = d.BookingID
		}
		if d.ResponseStatus != nil {
			item["response_status"] = *d.ResponseStatus
		}
		if d.LastAttemptedAt != nil {
			item["last_attempted_at"] = *d.LastAttemptedAt
		}
		items[i] = item
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// validateWebhookURL resolves the URL host and rejects any address in a
// loopback, link-local, or private range to prevent SSRF.
// validateWebhookURL rejects a webhook URL whose host resolves (now) to a private/
// loopback address — the same SSRF check the worker re-applies at actual delivery
// time (netutil.ResolveSafe), since DNS can change between saving a URL and
// delivering to it.
func validateWebhookURL(ctx context.Context, u *url.URL) error {
	if _, err := netutil.ResolveSafe(ctx, u.Hostname()); err != nil {
		return fmt.Errorf("webhook URL must not resolve to a private or loopback address: %w", err)
	}
	return nil
}
