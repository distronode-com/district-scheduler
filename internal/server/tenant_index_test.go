package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/db"
)

// The neutral root (M9). With ADMIN_SPA=off the bare root of a tenant's public host was a
// 404, which is the wrong answer on a host whose whole purpose is to be visited: it says
// "no such path" when what is true is "the console that used to live here is gone". It now
// lists the workspace's public, active event types.
//
// ⛔ No route was added. `GET /{$}` is the same registration it always was; only the
// handler behind it changes, and routes_classified_test.go still reads 184 routes.

const indexHost = "book.tenant.example"

// newIndexMux builds a console-off multi-tenant mux with one workspace on indexHost, and
// seeds the event types the case needs.
//
// ⚠️ The SQLite lane is identity for ForWorkspace (there is one handle and nothing to
// bind), so what this file proves is the PAGE — the visibility predicate, the chrome, the
// headers. That B's rows are unreachable from A's request is a different claim and is
// asserted against a real NOBYPASSRLS role in tenancy_index_test.go.
func newIndexMux(t *testing.T, seed func(*testing.T, *db.DB)) http.Handler {
	t.Helper()
	mux, database := newAdminMuxDB(t, &config.Config{
		MultiTenant:   true,
		BaseURL:       "https://app.calnode.example",
		PublicBaseURL: "https://app.calnode.example",
		AdminSPA:      false,
	})

	ctx := context.Background()
	if _, err := database.Platform().ExecContext(ctx,
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES ('tenant', 'tenant', ?, '', 'active')`,
		indexHost); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO users (id, email, name, is_admin, is_owner) VALUES ('tenant-user', 'owner@tenant.example', 'Tenant Owner', 1, 1)`); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if seed != nil {
		seed(t, database)
	}
	return mux
}

// seedEventType inserts one event type. active/public are the two flags the index filters
// on, so every case here sets them explicitly rather than leaning on the column defaults.
func seedEventType(t *testing.T, database *db.DB, id, slug, name string, active, public bool, locType string) {
	t.Helper()
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, slot_interval_minutes,
		    min_notice_minutes, max_future_days, is_active, is_public, location_type)
		 VALUES (?, 'tenant-user', ?, ?, 30, 30, 0, 60, ?, ?, ?)`,
		id, slug, name, flag(active), flag(public), locType); err != nil {
		t.Fatalf("create event type %s: %v", slug, err)
	}
}

func getIndex(t *testing.T, mux http.Handler, host string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = host
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func TestTenantIndex_listsThePublicActiveEventTypes(t *testing.T) {
	mux := newIndexMux(t, func(t *testing.T, database *db.DB) {
		seedEventType(t, database, "et-1", "intro-call", "Intro call", true, true, "livekit")
		seedEventType(t, database, "et-2", "deep-dive", "Deep dive", true, true, "phone")
		// The two the index must not show. Each is a decision the workspace already made
		// on the event type itself, and the root has no business overriding either.
		seedEventType(t, database, "et-3", "internal-review", "Internal review", true, false, "phone")
		seedEventType(t, database, "et-4", "retired-onboarding", "Retired onboarding", false, true, "phone")
	})

	rec := getIndex(t, mux, indexHost)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{`href="/book/intro-call"`, "Intro call", `href="/book/deep-dive"`, "Deep dive"} {
		if !strings.Contains(body, want) {
			t.Errorf("index is missing %q", want)
		}
	}
	// Asserted by NAME as well as by slug: a template that rendered the row but linked
	// nowhere would pass a slug-only check.
	for _, absent := range []string{"internal-review", "Internal review", "retired-onboarding", "Retired onboarding"} {
		if strings.Contains(body, absent) {
			t.Errorf("index leaked a non-public or inactive event type: %q", absent)
		}
	}

	// Ordered by name, so the page is stable between requests rather than following
	// whatever order the engine felt like returning rows in.
	if i, j := strings.Index(body, "Deep dive"), strings.Index(body, "Intro call"); i > j {
		t.Errorf("event types are not ordered by name: %q came after %q", "Deep dive", "Intro call")
	}

	// The duration and location labels come from the same helpers the booking page uses,
	// so a visitor sees the same words in the same language on both pages.
	if !strings.Contains(body, "30 min") {
		t.Errorf("index does not show the duration label:\n%s", body)
	}
	if !strings.Contains(body, "Video meeting") {
		t.Errorf("index does not show the location label for a livekit event type")
	}
}

// A workspace with nothing public gets the page and one sentence, NOT a 404. The
// distinction is the point: "no such host" and "this host has nothing to book right now"
// are different answers, and the unknown-host 404 already carries the first one.
func TestTenantIndex_emptyWorkspaceStillRendersThePage(t *testing.T) {
	mux := newIndexMux(t, func(t *testing.T, database *db.DB) {
		seedEventType(t, database, "et-1", "internal-review", "Internal review", true, false, "phone")
	})

	rec := getIndex(t, mux, indexHost)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d; want 200 — an empty workspace is not a missing one", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "There are no meetings available to book right now.") {
		t.Errorf("index does not carry the empty-state sentence:\n%s", body)
	}
	if strings.Contains(body, "/book/") {
		t.Errorf("index links to a booking page with nothing public")
	}
}

// The unknown-host 404 still fires, and it fires in Scoped BEFORE the handler runs — so a
// host that names no workspace cannot get a page at all, empty-state or otherwise.
func TestTenantIndex_unknownHostIsStill404(t *testing.T) {
	mux := newIndexMux(t, func(t *testing.T, database *db.DB) {
		seedEventType(t, database, "et-1", "intro-call", "Intro call", true, true, "phone")
	})

	rec := getIndex(t, mux, "nobody.example")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / on an unknown host = %d; want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "intro-call") {
		t.Errorf("an unknown host was served a workspace's event types")
	}
}

// The page's own headers. The security headers are the root middleware's and are asserted
// in security_headers_test.go; these three are the handler's, and each is the reason this
// page can be served to anyone who knows the hostname.
func TestTenantIndex_headersAndNoindex(t *testing.T) {
	mux := newIndexMux(t, func(t *testing.T, database *db.DB) {
		seedEventType(t, database, "et-1", "intro-call", "Intro call", true, true, "phone")
	})

	rec := getIndex(t, mux, indexHost)

	// ⛔ The STRICT policy, unconditionally: this page renders no operator head HTML and
	// no analytics tag, so there is nothing for publicCSP's relaxation to be for — and
	// reading the tracking settings here would let a stored value widen a policy that has
	// no use for the width.
	const wantCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; " +
		"connect-src 'self'; frame-ancestors 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("CSP = %q; want the strict public policy %q", got, wantCSP)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q; want DENY", got)
	}
	// The body is per-locale, so a shared cache in front of the instance must not serve
	// the first visitor's language to everyone.
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Language") {
		t.Errorf("Vary = %q; want it to name Accept-Language", got)
	}
	// noindex, so a list of a workspace's meeting types does not become a directory
	// assembled by a search engine. The booking pages it links to stay indexable.
	if !strings.Contains(rec.Body.String(), `<meta name="robots" content="noindex">`) {
		t.Errorf("index page is missing its noindex meta")
	}
}

// It is translated through the same plumbing as the booking pages: ?lang= wins, and the
// page's own strings move with it rather than being English under a French <html lang>.
func TestTenantIndex_isTranslated(t *testing.T) {
	mux := newIndexMux(t, func(t *testing.T, database *db.DB) {
		seedEventType(t, database, "et-1", "intro-call", "Intro call", true, true, "phone")
	})

	r := httptest.NewRequest(http.MethodGet, "/?lang=fr", nil)
	r.Host = indexHost
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	body := rec.Body.String()
	if !strings.Contains(body, `<html lang="fr">`) {
		t.Errorf("?lang=fr did not set the document language:\n%.200s", body)
	}
	if !strings.Contains(body, "Réserver une réunion") {
		t.Errorf("the page's own strings are not translated; body:\n%s", body)
	}
	// The event type's NAME is admin-authored content and stays verbatim, like every
	// other admin-authored string on the public surfaces.
	if !strings.Contains(body, "Intro call") {
		t.Errorf("the event type name should not be translated away")
	}
}

// ⛔ With the console ON, the root keeps the behaviour it has always had, in BOTH modes.
// The index is what replaces a 404, not what replaces the console redirect: an operator
// who left ADMIN_SPA alone must still land on /admin/ from the domain root.
func TestTenantIndex_consoleOnKeepsTheRedirect(t *testing.T) {
	for _, mode := range []struct {
		name        string
		multiTenant bool
	}{
		{"single-tenant", false},
		{"multi-tenant", true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			mux := newAdminMux(t, &config.Config{
				MultiTenant:   mode.multiTenant,
				BaseURL:       "https://app.calnode.example",
				PublicBaseURL: "https://app.calnode.example",
				AdminSPA:      true,
			})
			rec := getIndex(t, mux, indexHost)
			if rec.Code != http.StatusFound {
				t.Fatalf("GET / = %d; want 302 to the console", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "/admin/" {
				t.Errorf("Location = %q; want /admin/", loc)
			}
		})
	}
}

// ⛔ And single-tenant never reaches the index at all, because ADMIN_SPA=off is ignored
// there (a self-hoster would otherwise lose the only admin UI they have). Asserted here
// rather than left to adminspa_test.go's redirect check, because "off is ignored" and
// "the root is the console" are now two claims and only the second one is obvious.
func TestTenantIndex_singleTenantOffStillRedirectsToTheConsole(t *testing.T) {
	mux := newAdminMux(t, &config.Config{
		BaseURL:       "https://cal.example.test",
		PublicBaseURL: "https://cal.example.test",
		AdminSPA:      false,
	})

	rec := getIndex(t, mux, "cal.example.test")
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d; want 302 — ADMIN_SPA=off is ignored on a single-tenant instance", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("Location = %q; want /admin/", loc)
	}
}
