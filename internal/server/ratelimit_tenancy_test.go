package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// D14: in multi-tenant mode a rate-limit bucket is (workspace, client IP), not the IP
// alone, and the IP half is the RESOLVED client IP rather than the TCP peer.
//
// Both halves matter and they pull in opposite directions, which is why they are tested
// together rather than as two properties of one key function:
//
//   - Without the workspace, two tenants' bookers behind one address — a shared office,
//     a corporate NAT, a CDN — share a bucket, so the busier workspace spends the
//     quieter one's allowance. One tenant degrades another's service through no fault
//     of either.
//   - Without TRUSTED_PROXY_CIDRS the "IP" behind a proxy is the proxy, so adding the
//     workspace prefix would make things WORSE: a whole tenant would become a single
//     bucket and its first N bookers a minute would exhaust it for everyone else in
//     that tenant. The workspace half is only safe because remoteIP resolves the
//     client's own address when a trusted hop forwarded it.
//
// The limiter runs before any handler, so the workspace comes from the request Host and
// not from a resolved *Workspace: rejecting a request must not cost a database read.

// limiterProbe drives one request through TrustClientIP + RateLimit and reports the
// status. The limit is 1 per minute so the second request against the same bucket is a
// 429 and nothing depends on timing.
type limiterProbe struct {
	serve func(host, peer, forwardedFor string) int
}

func newLimiterProbe(t *testing.T, multiTenant bool, cidrs []string) limiterProbe {
	t.Helper()

	// Package state, restored so the lanes cannot leak into each other. New sets it
	// once from cfg.MultiTenant before any RateLimit call is registered.
	previous := multiTenantLimits
	SetMultiTenantLimits(multiTenant)
	t.Cleanup(func() { SetMultiTenantLimits(previous) })

	trusted, err := ParseTrustedProxies(cidrs)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%v): %v", cidrs, err)
	}

	limited := RateLimit(1, time.Minute)(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	stack := TrustClientIP(trusted)(limited)

	return limiterProbe{serve: func(host, peer, forwardedFor string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
		req.Host = host
		req.RemoteAddr = peer
		if forwardedFor != "" {
			req.Header.Set("X-Forwarded-For", forwardedFor)
		}
		rec := httptest.NewRecorder()
		stack.ServeHTTP(rec, req)
		return rec.Code
	}}
}

// The positive half of D14: one address, one proxy, two tenants, two buckets.
func TestRateLimit_workspacesBehindOneProxyDoNotShareABucket(t *testing.T) {
	const (
		proxy  = "10.0.0.5:41000"
		booker = "203.0.113.7"
	)
	p := newLimiterProbe(t, true, []string{"10.0.0.0/8"})

	if got := p.serve("book.acme.test", proxy, booker); got != http.StatusOK {
		t.Fatalf("A's first request = %d; want 200", got)
	}
	if got := p.serve("book.acme.test", proxy, booker); got != http.StatusTooManyRequests {
		t.Fatalf("A's second request = %d; want 429 — the bucket is spent", got)
	}

	// Same client address, same proxy, different workspace. This is the assertion the
	// whole test exists for: before D14 it was a 429.
	if got := p.serve("book.globex.test", proxy, booker); got != http.StatusOK {
		t.Errorf("B's first request = %d; want 200 — B must not spend A's allowance", got)
	}
}

// The other half: the workspace prefix must not collapse a tenant into one bucket. Two
// of A's bookers arrive through the same proxy and each gets its own allowance, which
// only holds because remoteIP believes the trusted hop's forwarded address.
func TestRateLimit_twoBookersOfOneWorkspaceKeepTheirOwnBuckets(t *testing.T) {
	const proxy = "10.0.0.5:41000"
	p := newLimiterProbe(t, true, []string{"10.0.0.0/8"})

	if got := p.serve("book.acme.test", proxy, "203.0.113.7"); got != http.StatusOK {
		t.Fatalf("first booker = %d; want 200", got)
	}
	if got := p.serve("book.acme.test", proxy, "203.0.113.8"); got != http.StatusOK {
		t.Errorf("second booker = %d; want 200 — one workspace is not one bucket", got)
	}
	if got := p.serve("book.acme.test", proxy, "203.0.113.7"); got != http.StatusTooManyRequests {
		t.Errorf("first booker again = %d; want 429 — the per-IP limit still applies", got)
	}
}

// Single-tenant is unchanged: the key is the client IP alone, so the Host makes no
// difference and one address gets one bucket however many hostnames point at the
// instance.
func TestRateLimit_singleTenantIgnoresTheHost(t *testing.T) {
	const proxy = "10.0.0.5:41000"
	p := newLimiterProbe(t, false, []string{"10.0.0.0/8"})

	if got := p.serve("cal.example.test", proxy, "203.0.113.7"); got != http.StatusOK {
		t.Fatalf("first request = %d; want 200", got)
	}
	if got := p.serve("vanity.example.test", proxy, "203.0.113.7"); got != http.StatusTooManyRequests {
		t.Errorf("second request on another host = %d; want 429 — single-tenant keys on the IP alone", got)
	}
}

// A Host that names no workspace keys on the host string itself rather than falling back
// to the bare IP. That is the safe direction: an attacker rotating Host values gets a
// bucket per value and spends no real tenant's allowance, and the port is stripped so
// book.acme.test and book.acme.test:8443 are one tenant.
func TestRateLimitKey_portIsNotPartOfTheBucket(t *testing.T) {
	previous := multiTenantLimits
	SetMultiTenantLimits(true)
	t.Cleanup(func() { SetMultiTenantLimits(previous) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:1234"

	req.Host = "book.acme.test"
	bare := rateLimitKey(req)
	req.Host = "book.acme.test:8443"
	withPort := rateLimitKey(req)
	req.Host = "BOOK.ACME.TEST"
	upper := rateLimitKey(req)

	if bare != withPort {
		t.Errorf("keys differ by port: %q vs %q", bare, withPort)
	}
	if bare != upper {
		t.Errorf("keys differ by case: %q vs %q", bare, upper)
	}
	// ⚠️ The `ip:` namespace arrived with M2, which added a second kind of caller half.
	// It is there so a credential hash can never collide with an address and so a key
	// read out of a dump says which kind of caller it counts.
	if bare != "book.acme.test|ip:203.0.113.7" {
		t.Errorf("key = %q; want the workspace host and the client IP", bare)
	}
}
