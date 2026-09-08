package handler_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/uid"
)

const (
	ssoSecret  = "shared-secret-for-tests"
	ssoBaseURL = "https://cal.example.test"
)

// newSSOHandler returns a handler with the hand-off configured and its audience pinned.
func newSSOHandler(t *testing.T) (*handler.Handler, *db.DB) {
	t.Helper()
	h, database := newTestHandlerDB(t)
	h.SetBaseURL(ssoBaseURL)
	h.SetSSOSecret(ssoSecret)
	return h, database
}

// ssoToken mints a compact HS256 JWT the way an external identity system would. Kept
// independent of internal/handler's verifier on purpose: a test that signs with the
// production code would pass even if both halves were wrong in the same way.
func ssoToken(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ssoClaimSet is a valid claim set; cases mutate the one field they are about.
func ssoClaimSet() map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss":  "identity.example.test",
		"aud":  ssoBaseURL,
		"sub":  "handed.over@example.test",
		"name": "Handed Over",
		"role": "member",
		"iat":  now,
		"exp":  now + 30,
		"jti":  uid.New(),
	}
}

func doSSO(h *handler.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.SSOHandoff(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func ssoErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

func TestSSOHandoff_createsUserAndSession(t *testing.T) {
	h, database := newSSOHandler(t)

	rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet()))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302 — %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("Location = %q; want /admin/", loc)
	}

	var userID string
	var isAdmin, isOwner int
	if err := database.QueryRow(
		`SELECT id, is_admin, is_owner FROM users WHERE email = ?`,
		"handed.over@example.test").Scan(&userID, &isAdmin, &isOwner); err != nil {
		t.Fatalf("user was not created: %v", err)
	}
	if isAdmin != 0 || isOwner != 0 {
		t.Errorf("role flags = admin %d owner %d; want 0 0 for a member claim", isAdmin, isOwner)
	}

	// The session cookie must name a live session row for that user.
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "calnode_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no calnode_session cookie was set")
	}
	var sessionUser string
	if err := database.QueryRow(
		`SELECT user_id FROM sessions WHERE id = ?`, cookie.Value).Scan(&sessionUser); err != nil {
		t.Fatalf("session row: %v", err)
	}
	if sessionUser != userID {
		t.Errorf("session belongs to %q; want %q", sessionUser, userID)
	}
}

// A claim asking for owner bootstraps ownership only when nobody holds it. The
// asymmetry is the point: see ssoResolveUser.
func TestSSOHandoff_ownerClaimBootstrapsOnlyWhenUnowned(t *testing.T) {
	h, database := newSSOHandler(t)

	first := ssoClaimSet()
	first["sub"] = "first.owner@example.test"
	first["role"] = "owner"
	if rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, first)); rec.Code != http.StatusFound {
		t.Fatalf("first hand-off: status = %d — %s", rec.Code, rec.Body.String())
	}

	second := ssoClaimSet()
	second["sub"] = "second.owner@example.test"
	second["role"] = "owner"
	if rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, second)); rec.Code != http.StatusFound {
		t.Fatalf("second hand-off: status = %d — %s", rec.Code, rec.Body.String())
	}

	var owners int
	if err := database.QueryRow(`SELECT COUNT(*) FROM users WHERE is_owner = 1`).Scan(&owners); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if owners != 1 {
		t.Errorf("owners = %d; want exactly 1", owners)
	}
	var isAdmin, isOwner int
	if err := database.QueryRow(`SELECT is_admin, is_owner FROM users WHERE email = ?`,
		"second.owner@example.test").Scan(&isAdmin, &isOwner); err != nil {
		t.Fatalf("second user: %v", err)
	}
	if isOwner != 0 || isAdmin != 1 {
		t.Errorf("second user = admin %d owner %d; want admin 1 owner 0", isAdmin, isOwner)
	}
}

// An existing user's role is not rewritten by a hand-off (owner-bootstrap aside).
func TestSSOHandoff_doesNotDemoteAnExistingUser(t *testing.T) {
	h, database, _, ownerID := setupWorkspaceWithDB(t)
	h.SetBaseURL(ssoBaseURL)
	h.SetSSOSecret(ssoSecret)

	var email string
	if err := database.QueryRow(`SELECT email FROM users WHERE id = ?`, ownerID).Scan(&email); err != nil {
		t.Fatalf("load owner email: %v", err)
	}

	claims := ssoClaimSet()
	claims["sub"] = email
	claims["role"] = "member"
	if rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, claims)); rec.Code != http.StatusFound {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}

	var isAdmin, isOwner int
	if err := database.QueryRow(`SELECT is_admin, is_owner FROM users WHERE id = ?`, ownerID).
		Scan(&isAdmin, &isOwner); err != nil {
		t.Fatalf("reload owner: %v", err)
	}
	if isAdmin != 1 || isOwner != 1 {
		t.Errorf("owner = admin %d owner %d; want 1 1 (a member claim must not demote)", isAdmin, isOwner)
	}
}

func TestSSOHandoff_replayedJTIIsRejected(t *testing.T) {
	h, _ := newSSOHandler(t)
	token := ssoToken(t, ssoSecret, ssoClaimSet())

	if rec := doSSO(h, "/v1/auth/sso?token="+token); rec.Code != http.StatusFound {
		t.Fatalf("first use: status = %d — %s", rec.Code, rec.Body.String())
	}
	rec := doSSO(h, "/v1/auth/sso?token="+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay: status = %d; want 401", rec.Code)
	}
	if got := ssoErrorBody(t, rec); got != "jti has already been used" {
		t.Errorf("error = %q; want the jti to be named", got)
	}
}

func TestSSOHandoff_rejectsBadTokens(t *testing.T) {
	valid := ssoClaimSet()

	expired := ssoClaimSet()
	expired["iat"] = time.Now().Add(-10 * time.Minute).Unix()
	expired["exp"] = time.Now().Add(-10 * time.Minute).Add(30 * time.Second).Unix()

	longLived := ssoClaimSet()
	longLived["exp"] = longLived["iat"].(int64) + 3600

	wrongAud := ssoClaimSet()
	wrongAud["aud"] = "https://other.example.test"

	noIss := ssoClaimSet()
	noIss["iss"] = ""

	badRole := ssoClaimSet()
	badRole["role"] = "superuser"

	cases := []struct {
		name      string
		token     string
		wantError string
	}{
		{"expired", ssoToken(t, ssoSecret, expired), "exp is in the past"},
		{"lifetime too long", ssoToken(t, ssoSecret, longLived), "exp is more than 60s after iat"},
		{"wrong aud", ssoToken(t, ssoSecret, wrongAud), "aud does not match this instance"},
		{"missing iss", ssoToken(t, ssoSecret, noIss), "iss is required"},
		{"bad role", ssoToken(t, ssoSecret, badRole), "role must be owner, admin or member"},
		{"bad signature", ssoToken(t, "not-the-shared-secret", valid), "token signature does not verify"},
		{"not a jwt", "nonsense", "token is not a three-part JWT"},
		{"no token", "", "token is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, database := newSSOHandler(t)
			rec := doSSO(h, "/v1/auth/sso?token="+tc.token)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401 — %s", rec.Code, rec.Body.String())
			}
			if got := ssoErrorBody(t, rec); got != tc.wantError {
				t.Errorf("error = %q; want %q", got, tc.wantError)
			}
			var users int
			if err := database.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
				t.Fatalf("count users: %v", err)
			}
			if users != 0 {
				t.Errorf("users = %d; a rejected token must not create anyone", users)
			}
		})
	}
}

// An "alg": "none" token with an empty signature is the classic downgrade. It must be
// refused on the algorithm, before the signature is even compared.
func TestSSOHandoff_rejectsAlgNone(t *testing.T) {
	h, _ := newSSOHandler(t)
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	token := enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(ssoClaimSet()) + "."

	rec := doSSO(h, "/v1/auth/sso?token="+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if got := ssoErrorBody(t, rec); got != "token alg must be HS256" {
		t.Errorf("error = %q; want the alg to be named", got)
	}
}

func TestSSOHandoff_nextMustBeSameOriginPath(t *testing.T) {
	cases := []struct {
		name string
		next string
	}{
		{"absolute url", "https://evil.example.test/"},
		{"protocol relative", "//evil.example.test/"},
		{"backslash", `/\evil.example.test`},
		{"scheme inside", "/redirect?to=https://evil.example.test"},
		{"not a path", "admin/"},
		{"header injection", "/admin/\r\nX-Injected: 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, database := newSSOHandler(t)
			token := ssoToken(t, ssoSecret, ssoClaimSet())
			rec := doSSO(h, "/v1/auth/sso?token="+token+"&next="+url.QueryEscape(tc.next))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 — %s", rec.Code, rec.Body.String())
			}
			// The token was refused before its nonce was claimed, so the caller can
			// retry the same one with a sane next.
			var nonces int
			if err := database.QueryRow(`SELECT COUNT(*) FROM sso_nonces`).Scan(&nonces); err != nil {
				t.Fatalf("count nonces: %v", err)
			}
			if nonces != 0 {
				t.Errorf("nonces = %d; a bad next must not burn the token", nonces)
			}
		})
	}
}

func TestSSOHandoff_nextHonoursALocalPath(t *testing.T) {
	h, _ := newSSOHandler(t)
	rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet())+"&next=%2Fadmin%2Fbookings")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302 — %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/bookings" {
		t.Errorf("Location = %q; want /admin/bookings", loc)
	}
}

// Unset CALNODE_SSO_SHARED_SECRET ⇒ 404, indistinguishable from a build without the
// feature. Documented in DEPLOY.md and ARCHITECTURE.md §6.
func TestSSOHandoff_disabledWithoutASecret(t *testing.T) {
	h, _ := newTestHandlerDB(t)
	h.SetBaseURL(ssoBaseURL)

	rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 — %s", rec.Code, rec.Body.String())
	}
}

func TestSSOHandoff_archivedUserCannotSignIn(t *testing.T) {
	h, database, _, ownerID := setupWorkspaceWithDB(t)
	h.SetBaseURL(ssoBaseURL)
	h.SetSSOSecret(ssoSecret)

	var email string
	if err := database.QueryRow(`SELECT email FROM users WHERE id = ?`, ownerID).Scan(&email); err != nil {
		t.Fatalf("load owner email: %v", err)
	}
	if _, err := database.Exec(`UPDATE users SET archived_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), ownerID); err != nil {
		t.Fatalf("archive user: %v", err)
	}

	claims := ssoClaimSet()
	claims["sub"] = email
	rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, claims))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 — %s", rec.Code, rec.Body.String())
	}
	if got := ssoErrorBody(t, rec); got != "account is archived" {
		t.Errorf("error = %q; want the archive to be named", got)
	}
}

// ⛔ ssoDefaultNext is /admin/, which 404s on an instance running with ADMIN_SPA=off.
// The refusal has to come BEFORE the session, or the hand-off "succeeds" into a dead
// end: cookie set, jti spent, and a 404 on a host whose console lives elsewhere. The
// caller cannot retry, because the token is single-use.
func TestSSOHandoff_withoutAdminSPAAHandoffWithNoNextIs404(t *testing.T) {
	h, database := newSSOHandler(t)
	h.SetAdminSPA(false)

	rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 — %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q; want no redirect into a route that does not exist", loc)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "calnode_session" {
			t.Fatal("a session cookie was minted for a hand-off that could not land")
		}
	}

	// Nothing was written: no session row, and the jti is unspent, so the same token
	// could still be presented with an explicit next.
	var sessions, nonces int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM sso_nonces`).Scan(&nonces); err != nil {
		t.Fatalf("count nonces: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions = %d; want 0 — the refusal must precede the session", sessions)
	}
	if nonces != 0 {
		t.Errorf("nonces = %d; a refused hand-off must not burn the token", nonces)
	}
}

// The other half, and the one the platform actually uses: the calendar connect round
// trip hands off with an explicit next. Nothing about a next OUTSIDE /admin changes when
// the console is off — that is what explicit destinations exist for.
func TestSSOHandoff_withoutAdminSPAAnExplicitNextIsUnchanged(t *testing.T) {
	// wantLocation differs from next only for the climbing case: http.Redirect runs
	// path.Clean on a rooted target itself, so the header carries the resolved path —
	// which is the same normalisation nextIsAdminConsole applies to decide, and the
	// agreement between the two is worth pinning rather than assuming.
	for next, wantLocation := range map[string]string{
		"/v1/calendar/connect?provider=google":                                              "/v1/calendar/connect?provider=google",
		"/v1/calendar/connect?provider=google&return_to=https%3A%2F%2Fconsole.example.test": "/v1/calendar/connect?provider=google&return_to=https%3A%2F%2Fconsole.example.test",
		"/book/intro": "/book/intro",
		// ⚠️ It CLIMBS OUT of the console, so it lands outside it and is allowed. The
		// browser resolves it the same way; refusing on the prefix alone would refuse a
		// destination that is not the console.
		"/admin/../book/intro": "/book/intro",
	} {
		t.Run(next, func(t *testing.T) {
			h, _ := newSSOHandler(t)
			h.SetAdminSPA(false)

			rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet())+
				"&next="+url.QueryEscape(next))

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d; want 302 — %s", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != wantLocation {
				t.Errorf("Location = %q; want %q", loc, wantLocation)
			}
		})
	}
}

// ⛔ H3: AN EXPLICIT next UNDER /admin IS REFUSED TOO, AND THIS FILE ASSERTED THE
// OPPOSITE UNTIL F6.
//
// The old rule — "the caller said where to land" — read as deference and was a bypass:
// both native clients send `next=/admin/` on every hand-off, so with the console off the
// guard fired for nobody and the person got a 404 with a session cookie already set and
// the single-use token already spent. The apps could not even report it, because only a
// non-3xx response opens their error path.
//
// The jti assertion is the load-bearing half. A refusal that burned the token would make
// "try again" impossible for a reason nothing on screen could explain.
func TestSSOHandoff_withoutAdminSPAAnExplicitAdminNextIs404(t *testing.T) {
	for _, next := range []string{
		"/admin/",
		"/admin",
		"/admin/bookings",
		"/admin/settings/video",
		"/admin/?tab=hosts",
		// Percent-encoded: url.Parse decodes before the comparison, because the mux
		// matches the decoded path.
		"/%61dmin/",
		// Normalisation: a browser resolves this to /admin/bookings.
		"/admin/settings/../bookings",
	} {
		t.Run(next, func(t *testing.T) {
			h, database := newSSOHandler(t)
			h.SetAdminSPA(false)

			rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet())+
				"&next="+url.QueryEscape(next))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d; want 404 — %s", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("Location = %q; want no redirect into a console that is not served", loc)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == "calnode_session" {
					t.Fatal("a session cookie was minted for a hand-off that could not land")
				}
			}

			var sessions, nonces int
			if err := database.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
				t.Fatalf("count sessions: %v", err)
			}
			if err := database.QueryRow(`SELECT COUNT(*) FROM sso_nonces`).Scan(&nonces); err != nil {
				t.Fatalf("count nonces: %v", err)
			}
			if sessions != 0 {
				t.Errorf("sessions = %d; want 0 — the refusal must precede the session", sessions)
			}
			if nonces != 0 {
				t.Errorf("nonces = %d; a refused hand-off must not burn the token", nonces)
			}
		})
	}
}

// With the console ON — every single-tenant instance, and every multi-tenant one before
// the flip — a next under /admin is the ordinary case and must keep working. Without
// this, H3 would read as "refuse /admin" rather than "refuse /admin when it 404s".
func TestSSOHandoff_withTheConsoleOnAnAdminNextStillLands(t *testing.T) {
	for _, next := range []string{"/admin/", "/admin/bookings"} {
		t.Run(next, func(t *testing.T) {
			h, _ := newSSOHandler(t) // SetAdminSPA never called: the console is served

			rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet())+
				"&next="+url.QueryEscape(next))

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d; want 302 — %s", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != next {
				t.Errorf("Location = %q; want %q", loc, next)
			}
		})
	}
}

// ⚠️ The flag is stored negated so that the zero value serves the console: a handler
// nobody called SetAdminSPA on — every other test in this package, and any entry point
// that forgets the call — must behave as it always did. Stored the other way round,
// one forgotten call would 404 the hand-off on an ordinary single-tenant instance.
func TestSSOHandoff_defaultsToServingTheConsole(t *testing.T) {
	h, _ := newSSOHandler(t) // SetAdminSPA never called

	rec := doSSO(h, "/v1/auth/sso?token="+ssoToken(t, ssoSecret, ssoClaimSet()))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302 — %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("Location = %q; want /admin/", loc)
	}
}
