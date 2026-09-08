package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/dbtest"
)

// ADMIN_SPA=off removes the embedded admin console from a multi-tenant instance, whose
// tenants are administered from the platform's own dashboard instead.
//
// These run against the REAL mux rather than against frontend.Handler directly (which is
// what frameancestors_test.go does), because what is under test is the registration: the
// three routes have to keep existing — the classification gate reads them out of
// server.go — while their handlers change. A test on the handler alone could not tell a
// 404 that came through the middleware chain from a mux that had never heard of the path.

// newAdminMux builds the real handler tree for cfg, on this lane's database.
//
// ⚠️ It restores multiTenantLimits, which New sets process-wide from cfg.MultiTenant. A
// multi-tenant case left un-restored would re-key every later rate-limit test in this
// package, which is the shape ratelimit_tenancy_test.go already guards against.
func newAdminMux(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()

	previous := multiTenantLimits
	t.Cleanup(func() { SetMultiTenantLimits(previous) })

	// The worker's context is cancelled before drain, which blocks until the worker
	// finishes its current cycle; the other order hangs with a green test body.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	mux, drain := New(workerCtx, cfg, dbtest.Open(t), slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopWorker(); drain() })
	return mux
}

func getAdminPath(t *testing.T, mux http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The baseline: what the three routes do with ADMIN_SPA unset, which is every deployment
// that existed before it. Pinned so the switch cannot quietly become the default.
func TestAdminSPA_defaultConfigServesTheConsole(t *testing.T) {
	mux := newAdminMux(t, &config.Config{
		BaseURL:       "https://cal.example.test",
		PublicBaseURL: "https://cal.example.test",
		AdminSPA:      true,
	})

	if rec := getAdminPath(t, mux, "/admin"); rec.Code != http.StatusMovedPermanently {
		t.Errorf("GET /admin = %d; want 301", rec.Code)
	} else if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("GET /admin Location = %q; want /admin/", loc)
	}

	rec := getAdminPath(t, mux, "/admin/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ = %d; want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<html") {
		t.Errorf("GET /admin/ did not serve the SPA document; body starts %.80q", body)
	}

	if rec := getAdminPath(t, mux, "/"); rec.Code != http.StatusFound {
		t.Errorf("GET / = %d; want 302", rec.Code)
	} else if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("GET / Location = %q; want /admin/", loc)
	}
}

func TestAdminSPA_offInMultiTenantModeIs404(t *testing.T) {
	mux := newAdminMux(t, &config.Config{
		MultiTenant:   true,
		BaseURL:       "https://app.calnode.example",
		PublicBaseURL: "https://app.calnode.example",
		AdminSPA:      false,
	})

	// The sub-path matters on its own: /admin/ is the SPA fallback, so every
	// client-side route in the console resolves through it. If only the shell 404'd,
	// a deep link would still serve the app.
	for _, path := range []string{"/admin", "/admin/", "/admin/bookings", "/admin/settings/video", "/"} {
		t.Run(path, func(t *testing.T) {
			rec := getAdminPath(t, mux, path)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d; want 404 — %s", path, rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("GET %s Location = %q; want no redirect", path, loc)
			}
		})
	}

	// The switch removes the console, not the instance. The favicon is the same
	// embedded source as the SPA and is registered separately, which is exactly the
	// kind of neighbour a broader change would take with it.
	if rec := getAdminPath(t, mux, "/favicon.ico"); rec.Code != http.StatusOK {
		t.Errorf("GET /favicon.ico = %d; want 200 — the favicon is not part of the console", rec.Code)
	}

	// And the API answers as it always did. A 404 here would be indistinguishable
	// from the route having been removed; 401 is the unauthenticated answer.
	if rec := getAdminPath(t, mux, "/v1/event-types"); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/event-types = %d; want 401 — the /v1 tree is untouched", rec.Code)
	}
}

// ⛔ A single-tenant instance has no other admin UI, so ADMIN_SPA=off is ignored there
// rather than locking the operator out of their own installation. The value is still
// recorded on the Config (boot logs that it did nothing); it is AdminSPAEnabled that
// refuses to act on it.
func TestAdminSPA_offIsIgnoredInSingleTenantMode(t *testing.T) {
	mux := newAdminMux(t, &config.Config{
		BaseURL:       "https://cal.example.test",
		PublicBaseURL: "https://cal.example.test",
		AdminSPA:      false,
	})

	rec := getAdminPath(t, mux, "/admin/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ = %d; want 200 — a self-hoster keeps the console", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<html") {
		t.Errorf("GET /admin/ did not serve the SPA document; body starts %.80q", body)
	}
	if rec := getAdminPath(t, mux, "/"); rec.Code != http.StatusFound {
		t.Errorf("GET / = %d; want 302", rec.Code)
	}
}
