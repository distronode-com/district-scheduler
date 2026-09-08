package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// H1, second half: PatchGoogleSettings' hot reload is PROCESS-GLOBAL.
//
// `h.SetCalendar`, `h.SetGoogleAuth` and the calendar.Service built beside them live on
// *shared, which every per-request copy of the Handler points at — so one workspace's
// PATCH replaced the Google OAuth client that EVERY other workspace's calendar connect
// and OAuth login used, until the next restart. The route is wrapped in PlatformManaged
// so a tenant credential cannot reach the handler at all in multi-tenant mode; these
// cases assert the second guard, the one nearest the damage, by calling the handler
// DIRECTLY — which is exactly the situation a refactor that dropped the wrapper would
// create.
//
// An internal test because the state it has to read (calBase, googleAuth) is
// deliberately unexported: there is no setter-free way to observe it from outside, and
// adding one to make a test possible would widen the very surface this pins.

// newGoogleTenancyHandler returns a handler over this lane's database with an admin
// caller in context, plus the request that PATCHes new Google credentials.
func newGoogleTenancyHandler(t *testing.T, multiTenant bool) (*Handler, *http.Request) {
	t.Helper()
	database := dbtest.Open(t)
	t.Cleanup(func() { database.Close() })

	h := New(database, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(multiTenant)
	h.SetBaseURL("https://cal.example.test")
	h.SetEncKey(strings.Repeat("ab", 32))
	seedSettingsRow(t, database)

	req := httptest.NewRequest(http.MethodPatch, "/v1/settings/google",
		strings.NewReader(`{"client_id":"tenant.apps.googleusercontent.com","client_secret":"tenant-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, AuthUser{
		ID: "u1", WorkspaceID: "default", Email: "admin@example.test", IsAdmin: true,
	}))
	return h, req
}

// seedSettingsRow makes sure the singleton row every settings handler UPDATEs exists.
func seedSettingsRow(t *testing.T, database *db.DB) {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM server_settings WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("count server_settings: %v", err)
	}
	if n == 0 {
		if _, err := database.Exec(`INSERT INTO server_settings (id) VALUES (1)`); err != nil {
			t.Fatalf("seed server_settings: %v", err)
		}
	}
}

func TestPatchGoogleSettings_multiTenantNeverTouchesProcessGlobalState(t *testing.T) {
	h, req := newGoogleTenancyHandler(t, true)

	// A registry and an OAuth config the instance was booted with. If the handler
	// hot-reloads, both are replaced — which is the bug.
	booted := calendar.NewService(h.db)
	h.SetCalendar(booted)
	h.SetGoogleAuth("instance.apps.googleusercontent.com", "instance-secret",
		"https://cal.example.test/v1/auth/callback", true)

	rec := httptest.NewRecorder()
	h.PatchGoogleSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}

	h.calMu.RLock()
	base := h.calBase
	h.calMu.RUnlock()
	if base != booted {
		t.Error("the calendar registry was replaced; one tenancy's PATCH must not re-point every other tenancy's calendar")
	}
	if got := h.getGoogleAuth(); got == nil || got.ClientID != "instance.apps.googleusercontent.com" {
		t.Errorf("googleAuth client id = %v; want the instance's, untouched", got)
	}

	// The ROW is still written: it is that workspace's own server_settings, and this
	// guard is about process state, not about refusing the write.
	var clientID string
	if err := h.db.QueryRow(`SELECT google_client_id FROM server_settings WHERE id = 1`).Scan(&clientID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if clientID != "tenant.apps.googleusercontent.com" {
		t.Errorf("google_client_id = %q; the row should still hold what was PATCHed", clientID)
	}
}

// The clearing branch is a second, separate call into process state — h.SetCalendar(nil)
// plus googleAuth = nil — and an empty client_id is the cheapest way to reach it. Left
// unguarded it would let one tenancy switch Google calendar OFF for the whole instance.
func TestPatchGoogleSettings_multiTenantClearDoesNotDisableTheInstance(t *testing.T) {
	h, _ := newGoogleTenancyHandler(t, true)
	booted := calendar.NewService(h.db)
	h.SetCalendar(booted)
	h.SetGoogleAuth("instance.apps.googleusercontent.com", "instance-secret",
		"https://cal.example.test/v1/auth/callback", true)

	req := httptest.NewRequest(http.MethodPatch, "/v1/settings/google", strings.NewReader(`{"client_id":""}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, AuthUser{
		ID: "u1", WorkspaceID: "default", IsAdmin: true,
	}))

	rec := httptest.NewRecorder()
	h.PatchGoogleSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}

	h.calMu.RLock()
	base := h.calBase
	h.calMu.RUnlock()
	if base == nil {
		t.Error("the calendar registry was cleared instance-wide by one tenancy's PATCH")
	}
	if h.getGoogleAuth() == nil {
		t.Error("googleAuth was cleared instance-wide by one tenancy's PATCH")
	}
}

// ⛔ Single-tenant keeps the hot reload, and that is the point of gating on the mode
// rather than removing it: there the operator IS the instance, and a settings save that
// needed a restart to take effect is a regression a self-hoster would feel immediately.
func TestPatchGoogleSettings_singleTenantStillHotReloads(t *testing.T) {
	h, req := newGoogleTenancyHandler(t, false)
	h.SetGoogleAuth("instance.apps.googleusercontent.com", "instance-secret",
		"https://cal.example.test/v1/auth/callback", true)

	rec := httptest.NewRecorder()
	h.PatchGoogleSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}

	got := h.getGoogleAuth()
	if got == nil || got.ClientID != "tenant.apps.googleusercontent.com" {
		t.Fatalf("googleAuth client id = %v; a self-hoster's save must take effect without a restart", got)
	}
	h.calMu.RLock()
	base := h.calBase
	h.calMu.RUnlock()
	if base == nil {
		t.Error("calBase is nil; the single-tenant save should have registered gcal")
	}
}
