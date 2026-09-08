package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// M2: in multi-tenant mode an AUTHENTICATED request keys on its credential, not on the
// address it came from.
//
// ⛔ The address is the right identity for an anonymous booker and the wrong one for an
// API caller. The platform's dashboard reaches every tenant host from ONE egress IP per
// region, so keyed on the address, an entire region's authenticated dashboard traffic
// shared one 20-per-minute settings budget — the busiest workspace in the region would
// have spent it on everyone else. That is exactly the failure the workspace prefix (D14)
// was added to prevent, reappearing one dimension over, and the workspace prefix does
// not help because the traffic to ONE tenant host is what shares the address.
//
// The credential is hashed rather than used raw, and it is read from the request rather
// than resolved: the limiter runs before RequireAuth and must not do a database read to
// decide whether to reject.

// credLimiterProbe drives one request through TrustClientIP + RateLimit with a caller
// identity attached. The limit is 1 per minute so the second request against a bucket is
// a 429 and nothing depends on timing.
type credLimiterProbe struct {
	serve func(host, peer string, headers map[string]string, cookie string) int
}

func newCredLimiterProbe(t *testing.T, multiTenant bool) credLimiterProbe {
	t.Helper()

	previous := multiTenantLimits
	SetMultiTenantLimits(multiTenant)
	t.Cleanup(func() { SetMultiTenantLimits(previous) })

	limited := RateLimit(1, time.Minute)(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	stack := TrustClientIP(nil)(limited)

	return credLimiterProbe{serve: func(host, peer string, headers map[string]string, cookie string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/settings/branding", nil)
		req.Host = host
		req.RemoteAddr = peer
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		stack.ServeHTTP(rec, req)
		return rec.Code
	}}
}

func bearer(v string) map[string]string { return map[string]string{"Authorization": "Bearer " + v} }

// The case M2 exists for: the whole region's dashboard arrives from one egress address,
// and two members' keys must not share a budget.
func TestRateLimit_twoBearersFromOneAddressGetTwoBudgets(t *testing.T) {
	const egress = "35.245.161.119:52000" // one origin's egress, as the fork sees it
	p := newCredLimiterProbe(t, true)

	if got := p.serve("book.acme.test", egress, bearer("cno_alpha"), ""); got != http.StatusOK {
		t.Fatalf("first key's first request = %d; want 200", got)
	}
	if got := p.serve("book.acme.test", egress, bearer("cno_alpha"), ""); got != http.StatusTooManyRequests {
		t.Fatalf("first key again = %d; want 429 — its own bucket is spent", got)
	}
	if got := p.serve("book.acme.test", egress, bearer("cno_beta"), ""); got != http.StatusOK {
		t.Errorf("second key = %d; want 200 — one member must not spend another's allowance", got)
	}
}

// The session cookie is a credential too: the browser hand-off path reaches the fork the
// same way and must not fall in with the anonymous bucket for its origin's address.
func TestRateLimit_twoSessionsFromOneAddressGetTwoBudgets(t *testing.T) {
	const egress = "35.245.161.119:52000"
	p := newCredLimiterProbe(t, true)

	if got := p.serve("book.acme.test", egress, nil, "sess-one"); got != http.StatusOK {
		t.Fatalf("first session = %d; want 200", got)
	}
	if got := p.serve("book.acme.test", egress, nil, "sess-one"); got != http.StatusTooManyRequests {
		t.Fatalf("first session again = %d; want 429", got)
	}
	if got := p.serve("book.acme.test", egress, nil, "sess-two"); got != http.StatusOK {
		t.Errorf("second session = %d; want 200", got)
	}
}

// ⛔ Anonymous requests keep the IP key, which is the half that must not change: the
// public booking endpoints are the abuse surface the limiter was written for, and a
// booker carries no credential to key on. Two addresses, two budgets.
func TestRateLimit_anonymousRequestsStillKeyOnTheAddress(t *testing.T) {
	p := newCredLimiterProbe(t, true)

	if got := p.serve("book.acme.test", "203.0.113.7:1234", nil, ""); got != http.StatusOK {
		t.Fatalf("first booker = %d; want 200", got)
	}
	if got := p.serve("book.acme.test", "203.0.113.7:9999", nil, ""); got != http.StatusTooManyRequests {
		t.Fatalf("same booker on another port = %d; want 429 — the port is not part of the key", got)
	}
	if got := p.serve("book.acme.test", "203.0.113.8:1234", nil, ""); got != http.StatusOK {
		t.Errorf("second booker = %d; want 200 — two addresses are two budgets", got)
	}
}

// A credentialed caller and an anonymous one from the SAME address are different
// buckets, which is what the `c:`/`ip:` namespacing is for. Without it a booker could be
// throttled by dashboard traffic that happened to hash to their address string.
func TestRateLimit_credentialAndAnonymousDoNotShareABucket(t *testing.T) {
	const egress = "35.245.161.119:52000"
	p := newCredLimiterProbe(t, true)

	if got := p.serve("book.acme.test", egress, bearer("cno_alpha"), ""); got != http.StatusOK {
		t.Fatalf("keyed request = %d; want 200", got)
	}
	if got := p.serve("book.acme.test", egress, nil, ""); got != http.StatusOK {
		t.Errorf("anonymous request from the same address = %d; want 200", got)
	}
}

// The workspace dimension survives: one credential presented on two tenant hosts is two
// buckets, so D14 still holds after M2. (A credential resolving one workspace on
// another's host is a 403 at CredentialWorkspace; the limiter runs earlier and cannot
// know that, so counting them apart is the conservative reading.)
func TestRateLimit_credentialKeysStillCarryTheWorkspace(t *testing.T) {
	const egress = "35.245.161.119:52000"
	p := newCredLimiterProbe(t, true)

	if got := p.serve("book.acme.test", egress, bearer("cno_alpha"), ""); got != http.StatusOK {
		t.Fatalf("on A = %d; want 200", got)
	}
	if got := p.serve("book.globex.test", egress, bearer("cno_alpha"), ""); got != http.StatusOK {
		t.Errorf("on B = %d; want 200 — the workspace is still part of the key", got)
	}
}

// ⛔ Single-tenant is untouched. There is one instance, one operator and no fleet of
// origins sharing an egress address, so the key stays the client IP alone — which means
// two credentials from one address still share a bucket there, deliberately.
func TestRateLimit_singleTenantIgnoresTheCredential(t *testing.T) {
	p := newCredLimiterProbe(t, false)

	if got := p.serve("cal.example.test", "203.0.113.7:1234", bearer("cno_alpha"), ""); got != http.StatusOK {
		t.Fatalf("first request = %d; want 200", got)
	}
	if got := p.serve("cal.example.test", "203.0.113.7:1234", bearer("cno_beta"), ""); got != http.StatusTooManyRequests {
		t.Errorf("another credential from the same address = %d; want 429 — "+
			"single-tenant keys on the IP alone", got)
	}
}

// The key's shape, asserted directly rather than only through the 200/429 dance: the
// credential is HASHED (a raw bearer must never sit in a map key that outlives the
// request), the hash is a fixed-width hex prefix, and the namespace says which kind of
// caller it counts.
func TestRateLimitKey_credentialIsHashedAndNamespaced(t *testing.T) {
	previous := multiTenantLimits
	SetMultiTenantLimits(true)
	t.Cleanup(func() { SetMultiTenantLimits(previous) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "book.acme.test"
	req.RemoteAddr = "203.0.113.7:1234"
	req.Header.Set("Authorization", "Bearer cno_supersecret")

	key := rateLimitKey(req)
	if strings.Contains(key, "cno_supersecret") {
		t.Fatalf("key = %q; the raw credential must never appear in a bucket key", key)
	}
	if strings.Contains(key, "203.0.113.7") {
		t.Errorf("key = %q; a credentialed request must not key on the address", key)
	}
	prefix := "book.acme.test|c:"
	if !strings.HasPrefix(key, prefix) {
		t.Fatalf("key = %q; want the workspace host and the c: namespace", key)
	}
	if hash := strings.TrimPrefix(key, prefix); len(hash) != 16 {
		t.Errorf("hash = %q (%d chars); want a 16-character hex prefix", hash, len(hash))
	}
}

// RequireAuth's precedence, mirrored: X-API-Key wins over Authorization, which wins over
// the cookie. It matters only for a request carrying two, and matching means the limiter
// and the authenticator agree on who the caller is rather than counting one identity and
// authenticating another.
func TestRequestCredential_followsRequireAuthsPrecedence(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "from-header")
	req.Header.Set("Authorization", "Bearer from-bearer")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "from-cookie"})
	if got := requestCredential(req); got != "from-header" {
		t.Errorf("credential = %q; want the X-API-Key header", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer from-bearer") // lower case: the scheme is case-insensitive
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "from-cookie"})
	if got := requestCredential(req); got != "from-bearer" {
		t.Errorf("credential = %q; want the bearer value", got)
	}

	// Shapes that carry no credential at all, each of which must fall through to the
	// address rather than key everyone who sends it into one bucket.
	for _, tc := range []struct{ name, header string }{
		{"empty bearer", "Bearer "},
		{"bearer with no value", "Bearer"},
		{"another scheme", "Basic dXNlcjpwYXNz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", tc.header)
			if got := requestCredential(req); got != "" {
				t.Errorf("credential = %q; want none", got)
			}
		})
	}
}
