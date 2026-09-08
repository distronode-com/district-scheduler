package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/server"
)

// The tenant index is a LIST, which makes it the one public surface where a missing
// workspace binding shows up as somebody else's data on the page rather than as a 404.
// Every other host-scoped page names its subject in the URL — /book/{slug}, /manage/{token}
// — so an unbound read there returns the wrong ONE row and is at least visible. This one
// returns every row there is.
//
// ⛔ Asserted against a real NOBYPASSRLS application role, which is why it is here and not
// in tenant_index_test.go: no handler in the tree carries a workspace predicate, so on the
// suite's own superuser DSN — or on SQLite, where ForWorkspace is the identity function —
// this would pass whether the isolation worked or not. dbtest.RequireTenantPair skips
// LOUDLY rather than quietly falling back.
func TestTenancy_theIndexListsOnlyItsOwnWorkspace(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)

	cfg := &config.Config{
		MultiTenant:   true,
		BaseURL:       "https://app.calnode.example",
		PublicBaseURL: "https://app.calnode.example",
		DatabaseURL:   "postgres://app", // not dialled; New takes the handle
		// The console OFF — this fixture is specifically the configuration in which the
		// root serves the index. newTenancyFixture models an ordinary deployment and
		// leaves it on, which is why this builds its own mux.
		AdminSPA: false,
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	mux, drain := server.New(workerCtx, cfg, app, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopWorker(); drain() })

	a := seedTenant(t, app, platform, "acme", hostA)
	b := seedTenant(t, app, platform, "globex", hostB)

	get := func(host string) (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newRequest(t, http.MethodGet, host, "/", nil))
		return rec.Code, rec.Body.String()
	}

	code, body := get(hostA)
	if code != http.StatusOK {
		t.Fatalf("GET / on %s = %d; want 200 — %s", hostA, code, body)
	}
	if !strings.Contains(body, "/book/"+a.eventSlug) {
		t.Errorf("acme's index does not list acme's own event type %q:\n%s", a.eventSlug, body)
	}
	if strings.Contains(body, b.eventSlug) {
		t.Errorf("acme's index leaked globex's event type %q — the read is not workspace-bound", b.eventSlug)
	}

	// The mirror, so a fixture that seeded only one tenant's rows could not pass by
	// accident.
	code, body = get(hostB)
	if code != http.StatusOK {
		t.Fatalf("GET / on %s = %d; want 200 — %s", hostB, code, body)
	}
	if !strings.Contains(body, "/book/"+b.eventSlug) {
		t.Errorf("globex's index does not list globex's own event type %q", b.eventSlug)
	}
	if strings.Contains(body, a.eventSlug) {
		t.Errorf("globex's index leaked acme's event type %q", a.eventSlug)
	}
}
