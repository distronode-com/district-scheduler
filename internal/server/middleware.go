package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/calnode/calnode/internal/metrics"
)

type contextKey string

const requestIDKey contextKey = "request_id"

// clientIPKey carries the client IP resolved by TrustClientIP. Absent unless
// TRUSTED_PROXY_CIDRS is configured, in which case remoteIP falls back to the peer.
const clientIPKey contextKey = "client_ip"

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func Logging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		// Counted here rather than in a middleware of its own: this is already the one
		// place that knows the final status and the elapsed time, and a second wrapper
		// measuring the same thing would drift from the log line it is supposed to match.
		elapsed := time.Since(start)
		metrics.HTTPRequest(metrics.ClassifyPath(r.URL.Path), rw.status, elapsed)

		reqID, _ := r.Context().Value(requestIDKey).(string)
		logger.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", elapsed.Milliseconds(),
			"remote_addr", r.RemoteAddr,
			"request_id", reqID,
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// Flush keeps this wrapper transparent to streaming handlers (SSE): embedding the
// http.ResponseWriter interface does not promote Flush, so without this the underlying
// flusher would be hidden and a streamed response (e.g. the booking assistant) would be
// buffered until the handler returns.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// SameOriginCheck is CSRF defense-in-depth layered on the session cookie's
// SameSite=Lax: for a state-changing request that carries the admin session cookie,
// it rejects the request when a present Origin (or, as a fallback, Referer) header
// names a different host than the one the request was sent to.
//
// Scope is deliberately narrow — it only fires when the `calnode_session` cookie is
// present, so the public booking POST, API-key clients, and manage-token actions
// (none of which carry that cookie) are untouched, as are all GET/HEAD requests.
// When neither Origin nor Referer is present it allows the request: SameSite=Lax is
// the primary guard, and non-browser clients legitimately omit both. The comparison
// is against the request Host header, so a reverse proxy must forward the original
// Host (the common default).
func SameOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStateChanging(r.Method) {
			if _, err := r.Cookie("calnode_session"); err == nil {
				if src := requestOriginHost(r); src != "" && !strings.EqualFold(src, r.Host) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":"cross-origin request blocked"}`)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// requestOriginHost returns the host[:port] of the request's Origin header, or its
// Referer host as a fallback, or "" if neither is present/usable (a "null" Origin
// from an opaque/sandboxed context counts as absent here).
func requestOriginHost(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			return u.Host
		}
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return ""
}

// PublicCORS wraps a public, unauthenticated endpoint to allow cross-origin browser
// access (for the embeddable booking widget). allowedOrigins empty ⇒ any origin
// (`*`); otherwise only a request whose Origin is in the list gets an
// Access-Control-Allow-Origin header (others are blocked browser-side). Credentials
// are never permitted — these endpoints carry no session — so a malicious page can't
// ride a logged-in admin's cookie. Note CORS only constrains browsers; it is not an
// access-control boundary (the routes are rate-limited regardless). Handles the
// OPTIONS preflight itself (returns 204).
func PublicCORS(allowedOrigins []string) func(http.HandlerFunc) http.HandlerFunc {
	return PublicCORSFor(func(*http.Request) ([]string, bool) { return allowedOrigins, true })
}

// PublicCORSFor is PublicCORS with the allowlist chosen per request. originsFor
// answers the list that applies to this request and whether one applies at all:
// known=false means the request names no tenant, and then NO
// Access-Control-Allow-Origin header is sent — not `*`, because "no workspace"
// must never read as "any origin". An empty list with known=true keeps
// PublicCORS's meaning: any origin.
//
// This is what lets a multi-tenant instance honour each workspace's own
// `embed_allowed_origins` (handler.EmbedOriginsFor) while a single-tenant instance
// keeps one process-wide list. The preflight is answered either way; the resolver
// only decides the origin header.
func PublicCORSFor(originsFor func(*http.Request) ([]string, bool)) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if origin := r.Header.Get("Origin"); origin != "" {
				if allowedOrigins, known := originsFor(r); known {
					if len(allowedOrigins) == 0 {
						w.Header().Set("Access-Control-Allow-Origin", "*")
					} else {
						want := strings.ToLower(strings.TrimRight(origin, "/"))
						for _, o := range allowedOrigins {
							if strings.ToLower(strings.TrimRight(o, "/")) == want {
								w.Header().Set("Access-Control-Allow-Origin", origin)
								w.Header().Add("Vary", "Origin")
								break
							}
						}
					}
				}
			}
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next(w, r)
		}
	}
}

// RateLimit returns middleware that allows limit requests per period per bucket, and
// rateLimitKey is what a bucket is: the remote IP in single-tenant mode, and
// (workspace host, credential-or-IP) in multi-tenant mode. Exceeding the limit returns
// 429 with a Retry-After header. The IP is the TCP remote address only — see remoteIP
// for why proxy headers are never trusted here.
//
// The body is deliberately fixed English, even though the public booking form renders it
// verbatim in its error slot. Translating it needs a locale, which this middleware has no
// good source for (no DB, so no operator fallback-locale, and these requests carry no
// ?lang=) — and RateLimit also guards login/token/invite/avatar/settings, where a
// translated body would put a stray non-English string into the untranslated admin SPA and
// make machine consumers' 429s vary by Accept-Language. A split constructor to thread a
// locale through only the public limiters was tried and reverted: too much shared-middleware
// surface for one string on an abuse path a booker reaches only by hammering the endpoint.
func RateLimit(limit int, period time.Duration) func(http.HandlerFunc) http.HandlerFunc {
	rl := &rateLimiter{
		windows: make(map[string]*rlWindow),
		limit:   limit,
		period:  period,
	}
	go rl.cleanup()
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !rl.allow(rateLimitKey(r)) {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(period.Seconds())))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"error":"rate limit exceeded"}`)
				return
			}
			next(w, r)
		}
	}
}

// rateLimitKey is the bucket a request counts against (D14).
//
// ⛔ In multi-tenant mode it is (workspace, caller), not the IP alone. Two tenants'
// bookers routinely arrive from the same address — a shared office, a corporate NAT, a
// CDN — and with one bucket the busier workspace would spend the quieter one's
// allowance, which is one tenant degrading another's service through no fault of either.
//
// ⛔ AND THE CALLER HALF IS THE CREDENTIAL WHEN THERE IS ONE, NOT THE IP (M2). The
// address is the right identity for an anonymous booker and the wrong one for an API
// caller: the platform's dashboard reaches every tenant host from ONE egress IP per
// region, so keyed on the address, an entire region's authenticated dashboard traffic
// shared one 20-per-minute settings budget — and the busiest workspace in the region
// would have spent it on everyone else, which is the exact failure the workspace prefix
// was added to prevent, reappearing one dimension over.
//
// A SHA-256 prefix of the credential VALUE, because the limiter runs before RequireAuth
// and must not do a database read to decide whether to reject: hashing what the request
// already carries needs no lookup, and it keeps the raw bearer out of a map key that
// outlives the request. The prefix is 16 hex characters — 64 bits, far past collision
// for a per-process window map, and short enough that the key stays readable in a dump.
//
// ⚠️ It keys on the credential the caller PRESENTED, not on the user it resolves to. An
// invalid key therefore gets its own bucket rather than falling in with the anonymous
// ones, which is the safe direction (a rotated-key retry storm cannot spend a booker's
// allowance) and the cheap one. Two live keys for one user are two budgets: they are two
// callers, which is what the platform mints them as.
//
// ⚠️ The IP half — still used for anonymous requests — has to be the RESOLVED client IP,
// not the peer, or the workspace prefix makes things WORSE: behind a shared proxy every
// request from a workspace would key on the proxy's address, so one workspace would
// become one bucket and its first 20 bookers a minute would exhaust it for everyone else
// in that tenant. TrustClientIP (TRUSTED_PROXY_CIDRS) is what makes remoteIP the
// client's own address; without it configured, remoteIP is the peer and that half
// degrades to exactly the behaviour that existed before.
//
// The workspace comes from the Host rather than from a resolved *Workspace, for the same
// no-database-read reason. An unrecognised host keys on the host string itself, which is
// the safe direction: an attacker rotating Host values gets a bucket per value and no
// tenant's allowance.
func rateLimitKey(r *http.Request) string {
	if !multiTenantLimits {
		// Single-tenant is byte-for-byte what it always was: one instance, one
		// operator, and no fleet of origins sharing an egress address.
		return remoteIP(r)
	}
	host := strings.ToLower(r.Host)
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i+1:], ".") {
		host = host[:i]
	}
	if cred := requestCredential(r); cred != "" {
		return host + "|c:" + hashCredential(cred)
	}
	// The two halves are namespaced so a hash can never collide with an address, and
	// so a key read out of a heap dump says which kind of caller it counts.
	return host + "|ip:" + remoteIP(r)
}

// requestCredential returns the credential value a request carries, or "" when it
// carries none.
//
// The three RequireAuth accepts, in its own order: an X-API-Key header, an
// `Authorization: Bearer` value, and the session cookie. Order matters only for a
// request carrying two, and matching RequireAuth's precedence means the limiter and the
// authenticator agree on who the caller is.
func requestCredential(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		return k
	}
	if a := strings.TrimSpace(r.Header.Get("Authorization")); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		if v := strings.TrimSpace(a[7:]); v != "" {
			return v
		}
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// hashCredential is the 64-bit prefix of the credential's SHA-256, hex encoded.
func hashCredential(cred string) string {
	sum := sha256.Sum256([]byte(cred))
	return hex.EncodeToString(sum[:8])
}

// sessionCookieName duplicates internal/handler's constant rather than importing it:
// internal/server already imports internal/handler, and the reverse edge does not exist.
// SameOriginCheck above spells the same literal for the same reason.
const sessionCookieName = "calnode_session"

// multiTenantLimits mirrors config.MultiTenant. It is a package variable rather than
// a parameter because RateLimit is called ~15 times at registration and every caller
// would otherwise thread the same boolean; SetMultiTenantLimits is called once from
// New before any of them.
var multiTenantLimits bool

// SetMultiTenantLimits switches rate-limit buckets to (workspace host, client IP).
func SetMultiTenantLimits(v bool) { multiTenantLimits = v }

type rlWindow struct {
	count     int
	expiresAt time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rlWindow
	limit   int
	period  time.Duration
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	w, ok := rl.windows[key]
	if !ok || now.After(w.expiresAt) {
		rl.windows[key] = &rlWindow{count: 1, expiresAt: now.Add(rl.period)}
		return true
	}
	if w.count >= rl.limit {
		return false
	}
	w.count++
	return true
}

// cleanup removes expired windows every period to bound memory usage.
func (rl *rateLimiter) cleanup() {
	for range time.Tick(rl.period) {
		rl.mu.Lock()
		now := time.Now()
		for k, w := range rl.windows {
			if now.After(w.expiresAt) {
				delete(rl.windows, k)
			}
		}
		rl.mu.Unlock()
	}
}

// FrameAncestors returns middleware that sets `Content-Security-Policy:
// frame-ancestors <origins>` on the responses it wraps, so an operator can embed the
// admin SPA in their own console. An empty list is a pass-through.
//
// ⛔ Scoped to the admin SPA on purpose, and it must stay that way. The public booking
// pages set `frame-ancestors 'none'` plus `X-Frame-Options: DENY` in their own handlers
// (book.go, manage_handler.go, tracking_settings.go's publicCSP) and this must never
// reach them: they are unauthenticated pages that collect names, emails and card
// details, and clickjacking one is worth more to an attacker than framing an admin UI
// nobody can reach without a session.
//
// No `X-Frame-Options` is set beside the CSP. That header has no allow-list form — its
// `ALLOW-FROM` was only ever implemented by one browser and is dead — so the only value
// it could carry here is `SAMEORIGIN`, which every browser that reads it would apply
// INSTEAD of honouring the CSP, breaking the embedding this exists to enable. Every
// browser that can frame anything today supports frame-ancestors.
//
// ⚠️ With the list empty the wrapped handler sends no frame header at all, which is what
// /admin/ has always sent: this middleware does not add a default deny, because that
// would be a behaviour change smuggled in on an opt-in setting. See
// TestAdminSPA_sendsNoFrameHeadersWhenUnset, which pins it.
func FrameAncestors(origins []string) func(http.Handler) http.Handler {
	if len(origins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	policy := "frame-ancestors " + strings.Join(origins, " ")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Set before next writes: a header added after the first Write is dropped.
			w.Header().Set("Content-Security-Policy", policy)
			next.ServeHTTP(w, r)
		})
	}
}

// remoteIP returns the IP a per-IP limit keys on.
//
// By default that is the TCP-level remote address, stripped of its port, and the
// forwarded headers are ignored: without a configured trusted-proxy allowlist those
// headers can be forged by any client and would bypass the rate limit entirely.
// Operators behind a reverse proxy should strip proxy headers at the proxy and rely on
// the TCP address the proxy connects with. See audit/claims.yaml's
// rate-limit-keys-on-tcp-source-address claim, recorded specifically because a prior
// Layer 2 audit pass mistook this deliberate behavior for a spoofable gap.
//
// When TRUSTED_PROXY_CIDRS is set, TrustClientIP has already resolved the client IP for
// this request and left it in the context — see resolveClientIP for what that means and
// what it deliberately does not do. This function reads that value when it is there, so
// the untrusted-peer path stays byte-for-byte the old behaviour.
func remoteIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey).(string); ok && ip != "" {
		return ip
	}
	return peerIP(r)
}

// peerIP returns the TCP-level remote address, stripped of its port. This is the only
// value in a request that a client cannot choose.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ParseTrustedProxies parses TRUSTED_PROXY_CIDRS entries. A bare address is accepted and
// treated as a single-host range (/32 or /128), because "10.0.0.7" is what an operator
// naming one proxy will write.
//
// Every parseable entry is returned even when others fail, and the error names the ones
// that did not: one typo should cost that hop's trust, not the whole list's. The caller
// is expected to log the error — an unparsed entry means that proxy's headers are NOT
// believed, which degrades to per-proxy rate limiting rather than to trusting a forgery.
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	var bad []string
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		bad = append(bad, e)
	}
	if len(bad) > 0 {
		return nets, fmt.Errorf("not a CIDR or IP address: %s", strings.Join(bad, ", "))
	}
	return nets, nil
}

// TrustClientIP returns middleware that resolves the client IP once per request and
// stores it in the request context for remoteIP to read. With no trusted proxies it is a
// pass-through, so an instance that has not configured any is unchanged.
//
// It is middleware rather than a package-level setting because the trust list belongs to
// one server instance: a mutable global would leak between instances in a test binary
// and would make "which requests are affected" unanswerable from the wiring.
func TrustClientIP(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(trusted) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), clientIPKey, resolveClientIP(r, trusted))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveClientIP applies the forwarded headers, but only for a peer inside a trusted
// CIDR. A header from an untrusted peer is never read at all — that is the whole point,
// and it is why the default (no trusted proxies) cannot be weakened by a header.
//
// Preference order for a trusted peer:
//
//  1. CF-Connecting-IP, which a single fronting CDN sets to one value and does not append
//     to, so there is no chain to reason about.
//  2. X-Forwarded-For, walked RIGHT TO LEFT past trusted hops, returning the first
//     untrusted address. ⛔ Not the leftmost entry: the left of that header is whatever
//     the original client sent, so a client that pre-seeds "X-Forwarded-For: 1.2.3.4"
//     gets it prepended and preserved by every well-behaved proxy. The rightmost
//     non-trusted hop is the last address a trusted proxy actually observed, which is the
//     only one in the header that anything vouched for.
//  3. The peer, whenever the headers are absent or unusable.
//
// A hop that does not parse ends the walk and falls back to the peer rather than being
// skipped. Skipping it would let a client inject one malformed entry to push the walk
// past the real hop and onto a value it chose. Note this also means a proxy that appends
// "ip:port" (rather than a bare address) reads as malformed and lands on the peer, which
// is the safe direction to be wrong in.
func resolveClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := peerIP(r)
	if !ipInAny(peer, trusted) {
		return peer
	}

	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		if ip := net.ParseIP(cf); ip != nil {
			return ip.String()
		}
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(hops[i]))
			if ip == nil {
				break
			}
			if ipInAny(ip.String(), trusted) {
				continue // a trusted hop of our own; keep walking left
			}
			return ip.String()
		}
	}

	return peer
}

// ipInAny reports whether ip (a textual address) falls inside any of nets.
func ipInAny(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}
