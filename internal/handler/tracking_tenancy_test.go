package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/handler"
)

// L6: `head_html` is raw HTML injected into the <head> of the workspace's own booking
// pages, and publicCSP RELAXES that page's Content-Security-Policy to fit it.
//
// Self-scoped — session cookies are host-only — but on the operator's domain, and the
// relaxation is the worse half: a tenant that can write the field turns a strict policy
// into `script-src 'self' 'unsafe-inline' https:` on a page that collects names, email
// addresses and card details. The platform's own catalog never offered the field, so
// refusing it costs the console nothing.
//
// Two halves, and both are needed. The write is refused (400 managed_by_platform) and the
// READ ignores whatever a row already holds — from a workspace provisioned before this
// shipped, from an import, or from a single-tenant instance later switched over.

func patchTracking(t *testing.T, h *handler.Handler, body, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchTrackingSettings)(rec, authReq(http.MethodPatch, "/v1/settings/tracking", body, apiKey))
	return rec
}

func getTracking(t *testing.T, h *handler.Handler, apiKey string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetTrackingSettings)(rec, authReq(http.MethodGet, "/v1/settings/tracking", "", apiKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET tracking: %d — %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestPatchTrackingSettings_multiTenantRefusesHeadHTML(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetMultiTenant(true)

	rec := patchTracking(t, h, `{"head_html":"<script src=\"https://attacker.test/a.js\"></script>"}`, key)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 — %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "managed_by_platform" {
		t.Errorf("error = %q; want managed_by_platform", body.Error)
	}
}

// ⛔ Refused, not stripped. A tenant who believes their tag is installed and then sees no
// traffic has a worse problem than one who is told no — and the same reasoning is why the
// platform's catalog uses .strict() rather than dropping unknown keys.
func TestPatchTrackingSettings_multiTenantWritesNothingWhenRefused(t *testing.T) {
	h, database, key, _ := setupWorkspaceWithDB(t)
	h.SetMultiTenant(true)

	if rec := patchTracking(t, h, `{"head_html":"<script></script>","ga4_measurement_id":"G-ABC1234567"}`, key); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}

	// The whole PATCH is refused, so the GA4 id in the same body is not applied either.
	// That is the intended shape: a partially-applied settings save is worse than a
	// refused one, because nothing on either side says which half landed.
	var stored, ga4 string
	if err := database.QueryRow(
		`SELECT COALESCE(head_html,''), COALESCE(ga4_measurement_id,'') FROM server_settings WHERE id = 1`).
		Scan(&stored, &ga4); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "" {
		t.Errorf("head_html = %q; the refusal must not write", stored)
	}
	if ga4 != "" {
		t.Errorf("ga4_measurement_id = %q; a refused PATCH applies nothing", ga4)
	}
}

// The GA4/GTM ids are the tenant's own and keep working: they are validated against an
// exact format, they name the workspace's own analytics property, and they are what the
// platform's console offers on that page.
func TestPatchTrackingSettings_multiTenantKeepsTheTagIDs(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetMultiTenant(true)

	rec := patchTracking(t, h, `{"ga4_measurement_id":"G-ABC1234567","gtm_container_id":"GTM-T2KZ9P4"}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	body := getTracking(t, h, key)
	if body["ga4_measurement_id"] != "G-ABC1234567" {
		t.Errorf("ga4_measurement_id = %v; want the value just written", body["ga4_measurement_id"])
	}
	if body["gtm_container_id"] != "GTM-T2KZ9P4" {
		t.Errorf("gtm_container_id = %v; want the value just written", body["gtm_container_id"])
	}
}

// An empty head_html passes: it is how the console clears the field, and clearing it is
// exactly what this mode wants.
func TestPatchTrackingSettings_multiTenantAllowsClearingHeadHTML(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetMultiTenant(true)

	if rec := patchTracking(t, h, `{"head_html":""}`, key); rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	if rec := patchTracking(t, h, `{"head_html":"   "}`, key); rec.Code != http.StatusOK {
		t.Fatalf("whitespace-only: status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
}

// ⛔ The read half, and the one a write-time check alone would miss: a row that ALREADY
// holds head_html — provisioned before this shipped, restored by an import, or carried
// over from a single-tenant instance — must stop rendering without anyone saving the
// page.
func TestTrackingSettings_multiTenantIgnoresAStoredHeadHTML(t *testing.T) {
	h, database, key, _ := setupWorkspaceWithDB(t)
	seedHeadHTML(t, database, `<script src="https://attacker.test/a.js"></script>`)

	// Single-tenant first, as the control: the operator's own injection still works.
	if got, _ := getTracking(t, h, key)["head_html"].(string); !strings.Contains(got, "attacker.test") {
		t.Fatalf("head_html = %q; the single-tenant control did not seed", got)
	}

	h.SetMultiTenant(true)
	if got, _ := getTracking(t, h, key)["head_html"].(string); got != "" {
		t.Errorf("head_html = %q; a stored value must be ignored in multi-tenant mode", got)
	}
}

func seedHeadHTML(t *testing.T, database *db.DB, html string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE server_settings SET head_html = ? WHERE id = 1`, html); err != nil {
		t.Fatalf("seed head_html: %v", err)
	}
}
