package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/server"
)

// The end-to-end tenancy proof: two workspaces on distinct public hosts, one
// process, the real mux, and a real db.OpenPair whose application handle is a
// NOBYPASSRLS role that owns nothing.
//
// ⛔ Everything below would pass through a superuser handle whether the policies
// existed or not, which is why dbtest.RequireTenantPair skips LOUDLY rather than
// falling back. What is asserted is not "the handler filtered correctly" — it is
// that B's rows are not reachable from A's request even though no handler in the
// tree carries a workspace predicate.

const (
	hostA = "book.acme.example"
	hostB = "book.globex.example"
)

type tenant struct {
	id, host, apiKey, userID, eventSlug, bookingID string
}

type tenancyFixture struct {
	mux  http.Handler
	app  *db.DB
	plat *db.DB
	a, b tenant
}

func newTenancyFixture(t *testing.T) *tenancyFixture {
	t.Helper()

	app, platform := dbtest.RequireTenantPair(t)

	cfg := &config.Config{
		MultiTenant:         true,
		BaseURL:             "https://app.calnode.example",
		PublicBaseURL:       "https://app.calnode.example",
		DatabaseURL:         "postgres://app", // not dialled; New takes the handle
		EmbedAllowedOrigins: nil,
	}
	// ⚠️ The worker's context has to be cancelled BEFORE drain: drain blocks until
	// the worker finishes its current poll cycle, and the worker only stops when
	// its context is done. main.go does the same pair; passing
	// context.Background() here hangs the test in Worker.Wait with an otherwise
	// green body.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	mux, drain := server.New(workerCtx, cfg, app, slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		stopWorker()
		drain()
	})

	f := &tenancyFixture{mux: mux, app: app, plat: platform}
	f.a = seedTenant(t, app, platform, "acme", hostA)
	f.b = seedTenant(t, app, platform, "globex", hostB)
	return f
}

// seedTenant provisions one workspace end to end: the workspaces row and the
// server_settings row through the PLATFORM handle (naming workspace_id, because
// the platform handle binds ” and the column default would otherwise put the row
// in 'default'), then everything else through the workspace-bound handle with no
// column named, which is D1's whole point.
func seedTenant(t *testing.T, app, platform *db.DB, id, host string) tenant {
	t.Helper()
	ctx := context.Background()

	if _, err := platform.ExecContext(ctx,
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
		id, id, host); err != nil {
		t.Fatalf("create workspace %s: %v", id, err)
	}
	if _, err := platform.ExecContext(ctx,
		`INSERT INTO server_settings (workspace_id, id) VALUES (?, 1)`, id); err != nil {
		t.Fatalf("seed settings for %s: %v", id, err)
	}

	h := app.ForWorkspace(id)
	tn := tenant{
		id: id, host: host,
		userID:    id + "-user",
		eventSlug: id + "-intro",
		bookingID: id + "-booking",
	}

	if _, err := h.ExecContext(ctx,
		`INSERT INTO users (id, email, name, is_admin, is_owner) VALUES (?, ?, ?, 1, 1)`,
		tn.userID, id+"@example.com", strings.ToUpper(id)); err != nil {
		t.Fatalf("create user for %s: %v", id, err)
	}

	// cno_ keys are hashed with SHA-256 and api_keys.key_hash stays globally
	// unique on purpose (D9): the credential has to resolve before a tenant is
	// known.
	tn.apiKey = "cno_" + id + "_key"
	sum := sha256.Sum256([]byte(tn.apiKey))
	if _, err := h.ExecContext(ctx,
		`INSERT INTO api_keys (id, user_id, name, key_hash) VALUES (?, ?, 'test', ?)`,
		id+"-key", tn.userID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("create api key for %s: %v", id, err)
	}

	if _, err := h.ExecContext(ctx,
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, slot_interval_minutes,
		    min_notice_minutes, max_future_days, is_active, is_public)
		 VALUES (?, ?, ?, ?, 30, 30, 0, 60, 1, 1)`,
		id+"-et", tn.userID, tn.eventSlug, strings.ToUpper(id)+" intro"); err != nil {
		t.Fatalf("create event type for %s: %v", id, err)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority)
		 VALUES (?, ?, ?, 'required', 0)`,
		id+"-eth", id+"-et", tn.userID); err != nil {
		t.Fatalf("host row for %s: %v", id, err)
	}
	// Availability every day, so slots are generated whatever day the test runs.
	for dow := 0; dow < 7; dow++ {
		if _, err := h.ExecContext(ctx,
			`INSERT INTO availability_rules (id, user_id, day_of_week, start_time, end_time)
			 VALUES (?, ?, ?, '09:00', '17:00')`,
			fmt.Sprintf("%s-av-%d", id, dow), tn.userID, dow); err != nil {
			t.Fatalf("availability for %s: %v", id, err)
		}
	}

	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	if _, err := h.ExecContext(ctx,
		`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status)
		 VALUES (?, ?, ?, ?, ?, 'confirmed')`,
		tn.bookingID, id+"-et", tn.userID,
		start.Format(time.RFC3339), start.Add(30*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("create booking for %s: %v", id, err)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO booking_hosts (id, booking_id, user_id, is_primary) VALUES (?, ?, ?, 1)`,
		id+"-bh", tn.bookingID, tn.userID); err != nil {
		t.Fatalf("booking host for %s: %v", id, err)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO booking_attendees (id, booking_id, name, email, is_organizer)
		 VALUES (?, ?, ?, ?, 1)`,
		id+"-att", tn.bookingID, strings.ToUpper(id)+" booker", "booker-"+id+"@example.com"); err != nil {
		t.Fatalf("attendee for %s: %v", id, err)
	}

	return tn
}

// newRequest builds a request bound to a host, for the cases that need to add
// cookies or headers before it is served.
func newRequest(t *testing.T, method, host, path string, body io.Reader) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.Host = host
	return r
}

// serve runs a prepared request through the real mux.
func (f *tenancyFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec
}

// postForm sends a urlencoded form through the real mux.
func (f *tenancyFixture) postForm(t *testing.T, host, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := newRequest(t, http.MethodPost, host, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.serve(r)
}

// do sends a request through the real mux.
func (f *tenancyFixture) do(t *testing.T, method, host, path, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = host
	if apiKey != "" {
		r.Header.Set("X-API-Key", apiKey)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec
}

// TestTenancy_readSurfaces covers the four read shapes the packet names, each
// asserted in both directions: A sees its own, and A cannot see B's.
func TestTenancy_readSurfaces(t *testing.T) {
	f := newTenancyFixture(t)

	t.Run("event types list", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, f.a.host, "/v1/event-types", f.a.apiKey, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, f.a.eventSlug) {
			t.Errorf("A's own event type is missing: %s", body)
		}
		if strings.Contains(body, f.b.eventSlug) {
			t.Errorf("A's event type list contains B's %q: %s", f.b.eventSlug, body)
		}
	})

	t.Run("bookings list", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, f.a.host, "/v1/bookings?scope=all", f.a.apiKey, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, f.a.bookingID) {
			t.Errorf("A's own booking is missing: %s", body)
		}
		if strings.Contains(body, f.b.bookingID) {
			t.Errorf("A's booking list contains B's %q: %s", f.b.bookingID, body)
		}
	})

	t.Run("slots for B's event type on A's host", func(t *testing.T) {
		// A's own slug resolves.
		day := time.Now().UTC().Add(48 * time.Hour).Format("2006-01-02")
		own := f.do(t, http.MethodGet, f.a.host,
			"/v1/event-types/"+f.a.eventSlug+"/slots?date="+day, "", "")
		if own.Code != http.StatusOK {
			t.Fatalf("A's own slots: status = %d: %s", own.Code, own.Body.String())
		}

		// B's does not, on A's host, even though the slug exists in the database.
		other := f.do(t, http.MethodGet, f.a.host,
			"/v1/event-types/"+f.b.eventSlug+"/slots?date="+day, "", "")
		if other.Code == http.StatusOK {
			t.Errorf("B's event type produced slots on A's host: %s", other.Body.String())
		}
		if strings.Contains(other.Body.String(), f.b.eventSlug) &&
			!strings.Contains(strings.ToLower(other.Body.String()), "not found") {
			t.Errorf("B's slug leaked into the response: %s", other.Body.String())
		}
	})

	t.Run("public event type page for B's slug on A's host", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, f.a.host, "/book/"+f.b.eventSlug, "", "")
		if rec.Code == http.StatusOK {
			t.Errorf("B's booking page rendered on A's host: status 200")
		}
	})
}

// TestTenancy_bookingByIDRequiresACredentialOfItsOwnWorkspace.
//
// ⛔ This test asserted the opposite until F4, and was named
// TestTenancy_bookingByIDIsNotFoundAcrossWorkspaces: GET /v1/bookings/{id} was
// host-scoped and carried no auth middleware, so its first assertion was that an
// ANONYMOUS request on A's public host answered 200 with A's booking. Reaching a
// tenant's host is not a credential, so the booking id was the entire capability
// protecting an attendee's name, email and intake answers. The route is now
// RequireAuth + CredentialWorkspace, and there are two distinct refusals to hold
// apart: no credential is 401, and a credential belonging to another workspace is
// 404 — the row is invisible under that credential's bind, not forbidden, which is
// what stops the response from confirming the id exists.
func TestTenancy_bookingByIDRequiresACredentialOfItsOwnWorkspace(t *testing.T) {
	f := newTenancyFixture(t)

	anon := f.do(t, http.MethodGet, f.a.host, "/v1/bookings/"+f.a.bookingID, "", "")
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("anonymous read of A's booking on A's host: status = %d, want 401: %s",
			anon.Code, anon.Body.String())
	}
	if strings.Contains(anon.Body.String(), "booker-acme") {
		t.Errorf("the 401 leaked A's attendee: %s", anon.Body.String())
	}

	own := f.do(t, http.MethodGet, f.a.host, "/v1/bookings/"+f.a.bookingID, f.a.apiKey, "")
	if own.Code != http.StatusOK {
		t.Fatalf("A's own booking with A's key: status = %d: %s", own.Code, own.Body.String())
	}

	// B's id under A's credential. On the identity host, which names no workspace, so
	// the 404 is the credential's bind and not D10's host/credential mismatch 403.
	other := f.do(t, http.MethodGet, "app.calnode.example", "/v1/bookings/"+f.b.bookingID, f.a.apiKey, "")
	if other.Code != http.StatusNotFound {
		t.Errorf("B's booking id under A's key: status = %d, want 404: %s", other.Code, other.Body.String())
	}
	if strings.Contains(other.Body.String(), "booker-globex") {
		t.Errorf("B's attendee leaked: %s", other.Body.String())
	}

	// And the mirror image, which is the half that would still pass if the route had
	// merely gained RequireAuth without moving off HostWorkspace: B's own key must not
	// read B's booking through A's host either.
	crossHost := f.do(t, http.MethodGet, f.a.host, "/v1/bookings/"+f.b.bookingID, f.b.apiKey, "")
	if crossHost.Code == http.StatusOK {
		t.Errorf("B's key read B's booking on A's host: %s", crossHost.Body.String())
	}
	if strings.Contains(crossHost.Body.String(), "booker-globex") {
		t.Errorf("B's attendee leaked through A's host: %s", crossHost.Body.String())
	}
}

// TestTenancy_credentialOnTheWrongHostIs403 is D10's mismatch rule, body included.
func TestTenancy_credentialOnTheWrongHostIs403(t *testing.T) {
	f := newTenancyFixture(t)

	rec := f.do(t, http.MethodGet, f.b.host, "/v1/bookings", f.a.apiKey, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("A's key on B's host: status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body["error"] != "workspace mismatch" {
		t.Errorf("error = %q; want \"workspace mismatch\"", body["error"])
	}

	// And the same key on the identity host is fine: that host names no workspace,
	// so there is nothing to disagree with. API and MCP callers arrive there.
	ok := f.do(t, http.MethodGet, "app.calnode.example", "/v1/bookings", f.a.apiKey, "")
	if ok.Code != http.StatusOK {
		t.Errorf("A's key on the identity host: status = %d: %s", ok.Code, ok.Body.String())
	}
}

// TestTenancy_writeLandsInItsOwnWorkspace is the write half: a public booking
// created on A's host must carry A's workspace_id, and B's row count must not move.
func TestTenancy_writeLandsInItsOwnWorkspace(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()

	before := f.countBookings(t, f.b.id)

	// 10:00 UTC three days out: inside the 09:00-17:00 availability the fixture
	// seeds for every weekday, clear of the min-notice window, and on the 30-minute
	// interval the event type offers.
	d := time.Now().UTC().Add(72 * time.Hour)
	start := time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":%q,"name":"Nia","email":"nia@example.com","iana_timezone":"UTC"}`,
		f.a.eventSlug, start.Format(time.RFC3339))
	rec := f.do(t, http.MethodPost, f.a.host, "/v1/bookings", "", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create on A's host: status = %d: %s", rec.Code, rec.Body.String())
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if created.ID == "" {
		t.Fatalf("no id in %s", rec.Body.String())
	}

	// Read it back through the PLATFORM handle, which sees every workspace, so the
	// assertion is about where the row IS rather than about what A can see.
	var ws string
	if err := f.plat.QueryRowContext(ctx,
		`SELECT workspace_id FROM bookings WHERE id = ?`, created.ID).Scan(&ws); err != nil {
		t.Fatalf("read the created booking: %v", err)
	}
	if ws != f.a.id {
		t.Errorf("the booking landed in workspace %q; want %q", ws, f.a.id)
	}
	// ⛔ Not 'default' either: the column default is
	// COALESCE(current_setting(...), 'default'), so a write that escaped the
	// binding would land there rather than failing visibly.
	if ws == db.DefaultWorkspaceID {
		t.Error("the booking landed in the default workspace — the statement was not bound")
	}

	if after := f.countBookings(t, f.b.id); after != before {
		t.Errorf("B's booking count moved from %d to %d", before, after)
	}
}

// TestTenancy_mcpToolCallOverHTTP drives a real tools/call through the streamable
// HTTP transport, which is the surface a remote agent uses. /mcp is on the identity
// host and carries no tenant Host, so the bearer credential is the only source of
// the workspace (D10) — and the tools close over their handler, which is why
// MCPServerForRequest builds one server per workspace.
func TestTenancy_mcpToolCallOverHTTP(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()

	srv := httptest.NewServer(f.mux)
	t.Cleanup(srv.Close)

	callList := func(apiKey string) string {
		t.Helper()
		transport := &mcp.StreamableClientTransport{
			Endpoint:             srv.URL + "/mcp",
			HTTPClient:           &http.Client{Transport: bearerTransport{key: apiKey}},
			DisableStandaloneSSE: true,
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "tenancy-test", Version: "0"}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			t.Fatalf("connect to /mcp: %v", err)
		}
		defer session.Close()

		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_bookings",
			Arguments: map[string]any{"status": "confirmed"},
		})
		if err != nil {
			t.Fatalf("list_bookings: %v", err)
		}
		if res.IsError {
			t.Fatalf("list_bookings returned an error result: %+v", res.Content)
		}
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String()
	}

	got := callList(f.a.apiKey)
	if !strings.Contains(got, f.a.bookingID) {
		t.Errorf("A's MCP list_bookings is missing A's own booking: %s", got)
	}
	if strings.Contains(got, f.b.bookingID) {
		t.Errorf("A's MCP list_bookings returned B's booking %q: %s", f.b.bookingID, got)
	}

	// The mirror, which is what catches a cached server built for whichever
	// workspace happened to call first.
	gotB := callList(f.b.apiKey)
	if !strings.Contains(gotB, f.b.bookingID) {
		t.Errorf("B's MCP list_bookings is missing B's own booking: %s", gotB)
	}
	if strings.Contains(gotB, f.a.bookingID) {
		t.Errorf("B's MCP list_bookings returned A's booking %q: %s", f.a.bookingID, gotB)
	}
}

// mcpSession opens an MCP session as one workspace, lists the tools the server offers, and
// returns a caller for them. Both halves are DERIVED from the running server: the proof suite
// asks what tools exist rather than naming them, so a tool added without scoping is covered the
// moment it is registered.
//
// The caller passes no arguments: a tool that needs one returns an error result, which is fine
// — the assertion is that nothing of the other workspace's appears in whatever comes back, and
// an error body is as good a place for a leak as a success body.
func (f *tenancyFixture) mcpSession(t *testing.T, apiKey string) (tools []string, call func(*testing.T, string) string) {
	t.Helper()
	ctx := context.Background()

	srv := httptest.NewServer(f.mux)
	t.Cleanup(srv.Close)

	connect := func(t *testing.T) *mcp.ClientSession {
		t.Helper()
		transport := &mcp.StreamableClientTransport{
			Endpoint:             srv.URL + "/mcp",
			HTTPClient:           &http.Client{Transport: bearerTransport{key: apiKey}},
			DisableStandaloneSSE: true,
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "tenancy-proof", Version: "0"}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			t.Fatalf("connect to /mcp: %v", err)
		}
		return session
	}

	listing := connect(t)
	res, err := listing.ListTools(ctx, nil)
	if err != nil {
		listing.Close()
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		tools = append(tools, tool.Name)
	}
	listing.Close()

	call = func(t *testing.T, name string) string {
		t.Helper()
		session := connect(t)
		defer session.Close()
		out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil {
			// A transport-level failure is not a leak; report it and let the caller scan the
			// message, which is all this function promises.
			return err.Error()
		}
		var sb strings.Builder
		for _, c := range out.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String()
	}
	return tools, call
}

func (f *tenancyFixture) countBookings(t *testing.T, workspace string) int {
	t.Helper()
	var n int
	if err := f.plat.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM bookings WHERE workspace_id = ?`, workspace).Scan(&n); err != nil {
		t.Fatalf("count bookings in %s: %v", workspace, err)
	}
	return n
}

// bearerTransport adds the API key to every MCP request. The streamable transport
// takes an *http.Client but no header hook, and /mcp accepts a cno_ key as a
// bearer token (VerifyMCPBearer) as well as an OAuth access token.
type bearerTransport struct{ key string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.key)
	res, err := http.DefaultTransport.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	return res, nil
}
