package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/calnode/calnode/internal/caldav"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// The `return_to` half of the calendar OAuth round trip.
//
// A platform that has replaced the admin SPA with its own pages still sends the person
// through this instance to connect a calendar, because the OAuth redirect needs the
// session cookie that lives here. `?return_to=` is how it asks for the browser back.
//
// These tests run against a STUB provider rather than gcal, for one reason that matters:
// the failure this feature is mostly about is a failed token exchange, and gcal's Exchange
// is an HTTP POST to Google. A test that reaches the network proves the network. The stub
// keeps the real AES-GCM EncryptState/DecryptState (a caldav.Client, which needs no OAuth
// app to construct), so every state assertion below is against the real crypto.

// stubExchange records one Exchange call.
type stubExchange struct{ userID, code, calendarID string }

// stubExchangeLog is shared by pointer across every ForDB copy of the provider, so the
// test can read what the WORKSPACE-BOUND copy did.
type stubExchangeLog struct {
	mu   sync.Mutex
	seen []stubExchange
	// write inserts a calendar_connections row on a successful Exchange, through
	// whatever handle ForDB last bound. That is the thing the multi-tenant test is
	// actually asking about: which workspace the row lands in.
	write bool
}

func (l *stubExchangeLog) record(e stubExchange) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, e)
}

func (l *stubExchangeLog) calls() []stubExchange {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]stubExchange(nil), l.seen...)
}

// stubCalProvider is a calendar.Provider whose OAuth surface is deterministic. Everything
// it does not override comes from a real caldav.Client.
type stubCalProvider struct {
	calendar.Provider
	handle      *db.DB
	name        string
	exchangeErr error
	log         *stubExchangeLog
}

func newStubCalProvider(t *testing.T, handle *db.DB, name string) *stubCalProvider {
	t.Helper()
	cc, err := caldav.New(handle, testEncKey)
	if err != nil {
		t.Fatalf("caldav.New: %v", err)
	}
	return &stubCalProvider{Provider: cc, handle: handle, name: name, log: &stubExchangeLog{}}
}

func (s *stubCalProvider) Name() string { return s.name }

// AuthURL is a fixed origin with the state on the query, so a test can read the state back
// out of the redirect the way the provider would.
func (s *stubCalProvider) AuthURL(state string) string {
	return "https://oauth.example.test/authorize?state=" + url.QueryEscape(state)
}

func (s *stubCalProvider) ForDB(handle *db.DB) calendar.Provider {
	cp := *s
	cp.Provider = s.Provider.ForDB(handle)
	cp.handle = handle
	return &cp
}

func (s *stubCalProvider) Exchange(ctx context.Context, userID, code, calendarID string) error {
	s.log.record(stubExchange{userID: userID, code: code, calendarID: calendarID})
	if s.exchangeErr != nil {
		return s.exchangeErr
	}
	if !s.log.write {
		return nil
	}
	// No workspace_id column: the bound handle's app.workspace_id fills it, which is
	// exactly how the real providers write and therefore what this has to imitate.
	_, err := s.handle.ExecContext(ctx,
		`INSERT INTO calendar_connections
		   (id, user_id, provider, access_token_enc, refresh_token_enc, calendar_id,
		    check_conflicts, is_destination, created_at, account_email)
		 VALUES (?, ?, ?, 'access', 'refresh', ?, 1, 1, '2026-01-01T00:00:00Z', ?)`,
		userID+"-conn", userID, s.name, calendarID, userID+"@example.com")
	return err
}

// newReturnToHandler is a single-tenant handler on SQLite with the stub registered.
func newReturnToHandler(t *testing.T, origins ...string) (*Handler, *stubCalProvider) {
	t.Helper()
	database := dbtest.Open(t)
	t.Cleanup(func() { database.Close() })

	h := New(database, slog.New(slog.DiscardHandler))
	h.SetBaseURL("https://scheduler.example.test")
	h.SetPlatformReturnOrigins(origins)

	stub := newStubCalProvider(t, database, "stub")
	svc := calendar.NewService(database)
	svc.Register(stub)
	h.SetCalendar(svc)
	return h, stub
}

// connectReq builds an authenticated GET /v1/calendar/connect. The user comes from the
// context rather than from the auth middleware because what is under test is the handler,
// and the middleware has its own tests.
func connectReq(userID, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/calendar/connect"+query, nil)
	return r.WithContext(context.WithValue(r.Context(), ctxKeyUser, AuthUser{ID: userID}))
}

// stateFromRedirect pulls the OAuth state out of the Location the connect handler wrote.
func stateFromRedirect(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302 — %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location %q: %v", rec.Header().Get("Location"), err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatalf("Location %q carries no state", loc)
	}
	return state
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response %q is not the JSON error shape: %v", rec.Body.String(), err)
	}
	return body.Error
}

const testConsoleOrigin = "https://console.example.test"

// ---------------------------------------------------------------------------
// ConnectCalendar — the allowlist
// ---------------------------------------------------------------------------

// The accepted value goes into the state, and it goes in WHOLE: path, query and all. The
// origin is what the allowlist decides; the rest is the platform's own page.
func TestConnectCalendar_returnTo_isCarriedInTheEncryptedState(t *testing.T) {
	h, stub := newReturnToHandler(t, testConsoleOrigin)
	const returnTo = testConsoleOrigin + "/settings/calendar?tab=connections"

	rec := httptest.NewRecorder()
	h.ConnectCalendar(rec, connectReq("user-1", "?return_to="+url.QueryEscape(returnTo)))

	raw, err := stub.DecryptState(stateFromRedirect(t, rec))
	if err != nil {
		t.Fatalf("DecryptState: %v", err)
	}
	provider, userID, got := parseCalendarState(raw)
	if provider != "stub" || userID != "user-1" {
		t.Errorf("state = %q/%q; want stub/user-1", provider, userID)
	}
	if got != returnTo {
		t.Errorf("return_to in state = %q; want %q", got, returnTo)
	}
}

// Exact origin match only. Every case here is a host a prefix, suffix or scheme-blind
// comparison would have let through, which is the whole reason this list exists.
func TestConnectCalendar_returnTo_originMustMatchWhole(t *testing.T) {
	cases := map[string]string{
		"a different port":       "https://console.example.test:8443/x",
		"http instead of https":  "http://console.example.test/x",
		"a subdomain":            "https://eu.console.example.test/x",
		"a parent domain":        "https://example.test/x",
		"a suffix lookalike":     "https://console.example.evil/x",
		"a longer lookalike":     "https://console.example.test.evil.test/x",
		"the origin as a path":   "https://evil.test/https://console.example.test",
		"the origin as userinfo": "https://console.example.test@evil.test/x",
		"credentials on the way": "https://someone@console.example.test/x",
		"a relative path":        "/settings/calendar",
		"protocol relative":      "//console.example.test/x",
		"a backslash disguise":   `/\console.example.test`,
		"not a URL":              "::::",
		"a javascript URL":       "javascript:alert(1)",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := newReturnToHandler(t, testConsoleOrigin)
			rec := httptest.NewRecorder()
			h.ConnectCalendar(rec, connectReq("user-1", "?return_to="+url.QueryEscape(value)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d for %q; want 400", rec.Code, value)
			}
			if got := errorBody(t, rec); got != "return_to origin not allowed" {
				t.Errorf("error = %q; want %q", got, "return_to origin not allowed")
			}
		})
	}
}

// The allowed origin itself, with and without a path, is accepted — otherwise the test
// above would pass against a handler that refuses everything.
func TestConnectCalendar_returnTo_acceptsTheAllowedOrigin(t *testing.T) {
	h, stub := newReturnToHandler(t, "https://other.example.test", testConsoleOrigin)
	for _, value := range []string{
		testConsoleOrigin,
		testConsoleOrigin + "/",
		testConsoleOrigin + "/settings/calendar",
		testConsoleOrigin + "/settings?x=1#frag",
		"https://other.example.test/second-entry",
	} {
		t.Run(value, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ConnectCalendar(rec, connectReq("user-1", "?return_to="+url.QueryEscape(value)))
			raw, err := stub.DecryptState(stateFromRedirect(t, rec))
			if err != nil {
				t.Fatalf("DecryptState: %v", err)
			}
			if _, _, got := parseCalendarState(raw); got != value {
				t.Errorf("return_to in state = %q; want %q", got, value)
			}
		})
	}
}

// ⛔ Off means REFUSED, not ignored. A platform pointed at an instance nobody configured
// for it has to find out at the first attempt; ignoring the parameter would land the
// person on this instance's /admin/calendar and look like a bug in the platform.
func TestConnectCalendar_returnTo_refusedWhenTheFeatureIsOff(t *testing.T) {
	h, _ := newReturnToHandler(t) // no origins configured

	rec := httptest.NewRecorder()
	h.ConnectCalendar(rec, connectReq("user-1", "?return_to="+url.QueryEscape(testConsoleOrigin+"/x")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 with the feature off", rec.Code)
	}
	if got := errorBody(t, rec); got != "return_to origin not allowed" {
		t.Errorf("error = %q; want %q", got, "return_to origin not allowed")
	}
}

// The state separator is refused BEFORE the value is encoded. url.Parse rejects ASCII
// control characters on its own, so this pins the explicit guard rather than the accident
// that currently backs it up.
func TestConnectCalendar_returnTo_refusesTheStateSeparator(t *testing.T) {
	h, _ := newReturnToHandler(t, testConsoleOrigin)

	rec := httptest.NewRecorder()
	h.ConnectCalendar(rec, connectReq("user-1",
		"?return_to="+url.QueryEscape(testConsoleOrigin+"/x"+stateSep+"extra")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for a return_to carrying the state separator", rec.Code)
	}
	if got := errorBody(t, rec); got != "return_to origin not allowed" {
		t.Errorf("error = %q; want %q", got, "return_to origin not allowed")
	}
}

// The direct unit, because the handler test above cannot distinguish the explicit guard
// from url.Parse's control-character refusal.
func TestReturnToFromRequest_refusesTheSeparatorItself(t *testing.T) {
	h, _ := newReturnToHandler(t, testConsoleOrigin)
	r := httptest.NewRequest(http.MethodGet, "/v1/calendar/connect", nil)
	r.URL.RawQuery = url.Values{"return_to": {testConsoleOrigin + "/x" + stateSep}}.Encode()

	if _, err := h.returnToFromRequest(r); err == nil {
		t.Error("returnToFromRequest accepted a return_to carrying the state separator")
	}
}

// ---------------------------------------------------------------------------
// The state encoding
// ---------------------------------------------------------------------------

// ⚠️ With no return_to the state is the two-field value it has always been. A trailing
// separator would be read by an OLDER binary — the other half of a rolling deploy — as
// part of the user id.
func TestConnectCalendar_withoutReturnTo_mintsTheTwoFieldState(t *testing.T) {
	h, stub := newReturnToHandler(t, testConsoleOrigin)

	rec := httptest.NewRecorder()
	h.ConnectCalendar(rec, connectReq("user-1", ""))

	raw, err := stub.DecryptState(stateFromRedirect(t, rec))
	if err != nil {
		t.Fatalf("DecryptState: %v", err)
	}
	if raw != "stub"+stateSep+"user-1" {
		t.Errorf("state plaintext = %q; want exactly %q", raw, "stub"+stateSep+"user-1")
	}
	if got := strings.Count(raw, stateSep); got != 1 {
		t.Errorf("state carries %d separators; want 1", got)
	}
}

// Three fields, through the real EncryptState/DecryptState rather than the encoder alone.
func TestCalendarState_threeFieldRoundTripThroughTheRealCrypto(t *testing.T) {
	_, stub := newReturnToHandler(t, testConsoleOrigin)
	const returnTo = testConsoleOrigin + "/settings?a=1&b=2#frag"

	state, err := stub.EncryptState(encodeCalendarState("stub", "user-1", returnTo))
	if err != nil {
		t.Fatalf("EncryptState: %v", err)
	}
	raw, err := stub.DecryptState(state)
	if err != nil {
		t.Fatalf("DecryptState: %v", err)
	}
	provider, userID, got := parseCalendarState(raw)
	if provider != "stub" || userID != "user-1" || got != returnTo {
		t.Errorf("round trip = %q/%q/%q; want stub/user-1/%q", provider, userID, got, returnTo)
	}
}

// The two shapes that predate the third field. A round trip already in flight when the
// deploy lands still has to complete.
func TestParseCalendarState_legacyShapesStillParse(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		raw                        string
		provider, userID, returnTo string
	}{
		{name: "one field, the oldest shape", raw: "user-1", userID: "user-1"},
		{name: "two fields, the current shape", raw: "google" + stateSep + "user-1",
			provider: "google", userID: "user-1"},
		{name: "three fields", raw: "google" + stateSep + "user-1" + stateSep + "https://c.test/x",
			provider: "google", userID: "user-1", returnTo: "https://c.test/x"},
		{name: "an empty third field parses as absent", raw: "google" + stateSep + "user-1" + stateSep,
			provider: "google", userID: "user-1"},
		{name: "at most two separators, so the third field keeps its own",
			raw:      "google" + stateSep + "user-1" + stateSep + "a" + stateSep + "b",
			provider: "google", userID: "user-1", returnTo: "a" + stateSep + "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, userID, returnTo := parseCalendarState(tc.raw)
			if provider != tc.provider || userID != tc.userID || returnTo != tc.returnTo {
				t.Errorf("parseCalendarState(%q) = %q/%q/%q; want %q/%q/%q",
					tc.raw, provider, userID, returnTo, tc.provider, tc.userID, tc.returnTo)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CalendarCallback — with a return_to in the state
// ---------------------------------------------------------------------------

// mintState produces the real encrypted state for a round trip, the way ConnectCalendar
// would have. Going through EncryptState rather than hand-building one is what makes these
// callback tests exercise the parse the deployed code runs.
func mintState(t *testing.T, stub *stubCalProvider, userID, returnTo string) string {
	t.Helper()
	state, err := stub.EncryptState(encodeCalendarState(stub.name, userID, returnTo))
	if err != nil {
		t.Fatalf("EncryptState: %v", err)
	}
	return state
}

func callback(h *Handler, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.CalendarCallback(rec, httptest.NewRequest(http.MethodGet, "/v1/calendar/callback"+query, nil))
	return rec
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302 — %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
}

// Every way the round trip can fail, and the code each one carries. The platform console
// reads these; they are the reason it can say something better than "it didn't work".
func TestCalendarCallback_returnTo_everyFailureCarriesItsReason(t *testing.T) {
	const returnTo = testConsoleOrigin + "/settings/calendar"

	t.Run("provider_denied", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		rec := callback(h, "?error=access_denied&state="+url.QueryEscape(mintState(t, stub, "user-1", returnTo)))
		wantRedirect(t, rec, returnTo+"?calendar=error&reason=provider_denied")
	})

	t.Run("missing_code", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		rec := callback(h, "?state="+url.QueryEscape(mintState(t, stub, "user-1", returnTo)))
		wantRedirect(t, rec, returnTo+"?calendar=error&reason=missing_code")
	})

	t.Run("exchange_failed", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		stub.exchangeErr = errors.New("the provider refused the code")
		rec := callback(h, "?code=abc&state="+url.QueryEscape(mintState(t, stub, "user-1", returnTo)))
		wantRedirect(t, rec, returnTo+"?calendar=error&reason=exchange_failed")
		if len(stub.log.calls()) != 1 {
			t.Errorf("Exchange ran %d times; want 1 — the reason must come from a real attempt", len(stub.log.calls()))
		}
	})

	// A state that decrypts but names nobody. The return_to in it is still trustworthy —
	// it came out of the ciphertext — so the person goes home with a reason rather than
	// looking at a JSON body on an OAuth callback URL.
	t.Run("invalid_state", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		rec := callback(h, "?code=abc&state="+url.QueryEscape(mintState(t, stub, "", returnTo)))
		wantRedirect(t, rec, returnTo+"?calendar=error&reason=invalid_state")
	})
}

func TestCalendarCallback_returnTo_successCarriesConnected(t *testing.T) {
	h, stub := newReturnToHandler(t, testConsoleOrigin)
	const returnTo = testConsoleOrigin + "/settings/calendar"

	rec := callback(h, "?code=abc&state="+url.QueryEscape(mintState(t, stub, "user-1", returnTo)))

	wantRedirect(t, rec, returnTo+"?calendar=connected")
	calls := stub.log.calls()
	if len(calls) != 1 || calls[0].userID != "user-1" || calls[0].code != "abc" {
		t.Errorf("Exchange calls = %+v; want one for user-1 with code abc", calls)
	}
}

// The console's own query survives, verbatim and in order, and the fragment stays last. A
// "?" / "&" concatenation would append after the fragment and lose both.
func TestCalendarCallback_returnTo_appendsToAnExistingQuery(t *testing.T) {
	for _, tc := range []struct{ name, returnTo, want string }{
		{
			name:     "no query",
			returnTo: testConsoleOrigin + "/settings",
			want:     testConsoleOrigin + "/settings?calendar=connected",
		},
		{
			name:     "an existing query",
			returnTo: testConsoleOrigin + "/settings?x=1",
			want:     testConsoleOrigin + "/settings?x=1&calendar=connected",
		},
		{
			name:     "two existing parameters",
			returnTo: testConsoleOrigin + "/settings?x=1&y=2",
			want:     testConsoleOrigin + "/settings?x=1&y=2&calendar=connected",
		},
		{
			name:     "a fragment stays last",
			returnTo: testConsoleOrigin + "/settings?x=1#calendars",
			want:     testConsoleOrigin + "/settings?x=1&calendar=connected#calendars",
		},
		{
			name:     "the bare origin",
			returnTo: testConsoleOrigin,
			want:     testConsoleOrigin + "?calendar=connected",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, stub := newReturnToHandler(t, testConsoleOrigin)
			rec := callback(h, "?code=abc&state="+url.QueryEscape(mintState(t, stub, "user-1", tc.returnTo)))
			wantRedirect(t, rec, tc.want)
		})
	}
}

// The error shape gets the same treatment, since it appends two parameters rather than one.
func TestCalendarCallback_returnTo_errorAppendsToAnExistingQuery(t *testing.T) {
	h, stub := newReturnToHandler(t, testConsoleOrigin)
	const returnTo = testConsoleOrigin + "/settings?x=1"

	rec := callback(h, "?error=access_denied&state="+url.QueryEscape(mintState(t, stub, "user-1", returnTo)))

	wantRedirect(t, rec, testConsoleOrigin+"/settings?x=1&calendar=error&reason=provider_denied")
}

// ⛔ The destination comes out of the CIPHERTEXT and nowhere else. A return_to on the
// callback's own URL is attacker-controlled — the callback is a public route — so it must
// not be able to steer the redirect even when the state is otherwise perfectly valid.
func TestCalendarCallback_ignoresAReturnToOnItsOwnQuery(t *testing.T) {
	h, stub := newReturnToHandler(t, testConsoleOrigin)

	rec := callback(h, "?code=abc&return_to="+url.QueryEscape(testConsoleOrigin+"/evil")+
		"&state="+url.QueryEscape(mintState(t, stub, "user-1", "")))

	// The state carried no return_to, so this is the unchanged admin redirect.
	wantRedirect(t, rec, "https://scheduler.example.test/admin/calendar?connected=true")
}

// Same, for a state that never decrypts: there is nowhere trustworthy to send the browser,
// so it gets the JSON it always got rather than the query's suggestion.
func TestCalendarCallback_undecryptableState_neverRedirects(t *testing.T) {
	h, _ := newReturnToHandler(t, testConsoleOrigin)

	rec := callback(h, "?code=abc&return_to="+url.QueryEscape(testConsoleOrigin+"/evil")+
		"&state=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	if got := errorBody(t, rec); got != "invalid or missing state" {
		t.Errorf("error = %q; want %q", got, "invalid or missing state")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q; an undecryptable state must not redirect anywhere", loc)
	}
}

// ---------------------------------------------------------------------------
// CalendarCallback — without a return_to, nothing moved
// ---------------------------------------------------------------------------

// ⚠️ The ?error= branch moved to AFTER the decryption, and this is the case that pins what
// that move must not change: a denial arriving with no state at all (a user clicking
// "Cancel") still answers "OAuth error: …", not "invalid or missing state".
func TestCalendarCallback_noReturnTo_keepsTodaysAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		status      int
		body        string
	}{
		{name: "a denial with no state at all", query: "?error=access_denied",
			status: http.StatusBadRequest, body: "OAuth error: access_denied"},
		{name: "no state and no code", query: "",
			status: http.StatusBadRequest, body: "invalid or missing state"},
		{name: "a tampered state", query: "?code=abc&state=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			status: http.StatusBadRequest, body: "invalid or missing state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newReturnToHandler(t, testConsoleOrigin)
			rec := callback(h, tc.query)
			if rec.Code != tc.status {
				t.Fatalf("status = %d; want %d — %s", rec.Code, tc.status, rec.Body.String())
			}
			if got := errorBody(t, rec); got != tc.body {
				t.Errorf("error = %q; want %q", got, tc.body)
			}
		})
	}
}

// The same three-plus outcomes, this time with a valid two-field state and no return_to in
// it. Each has to be byte for byte what it was before the parameter existed.
func TestCalendarCallback_noReturnTo_withAValidState(t *testing.T) {
	t.Run("a denial answers JSON, not a redirect", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		rec := callback(h, "?error=access_denied&state="+url.QueryEscape(mintState(t, stub, "user-1", "")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d; want 400", rec.Code)
		}
		if got := errorBody(t, rec); got != "OAuth error: access_denied" {
			t.Errorf("error = %q; want %q", got, "OAuth error: access_denied")
		}
	})

	t.Run("missing code", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		rec := callback(h, "?state="+url.QueryEscape(mintState(t, stub, "user-1", "")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d; want 400", rec.Code)
		}
		if got := errorBody(t, rec); got != "missing code" {
			t.Errorf("error = %q; want %q", got, "missing code")
		}
	})

	t.Run("a failed exchange is still a 500", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		stub.exchangeErr = errors.New("the provider refused the code")
		rec := callback(h, "?code=abc&state="+url.QueryEscape(mintState(t, stub, "user-1", "")))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", rec.Code)
		}
		if got := errorBody(t, rec); got != "internal error" {
			t.Errorf("error = %q; want %q", got, "internal error")
		}
	})

	t.Run("success still lands on the admin UI", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		rec := callback(h, "?code=abc&state="+url.QueryEscape(mintState(t, stub, "user-1", "")))
		wantRedirect(t, rec, "https://scheduler.example.test/admin/calendar?connected=true")
	})

	// The oldest state shape of all — a bare user id, no provider — reaches the same place.
	t.Run("a one-field legacy state still completes", func(t *testing.T) {
		h, stub := newReturnToHandler(t, testConsoleOrigin)
		state, err := stub.EncryptState("user-1")
		if err != nil {
			t.Fatalf("EncryptState: %v", err)
		}
		rec := callback(h, "?code=abc&state="+url.QueryEscape(state))
		wantRedirect(t, rec, "https://scheduler.example.test/admin/calendar?connected=true")
		if calls := stub.log.calls(); len(calls) != 1 || calls[0].userID != "user-1" {
			t.Errorf("Exchange calls = %+v; want one for user-1", calls)
		}
	})
}
