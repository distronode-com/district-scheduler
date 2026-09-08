package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/calnode/calnode/internal/metrics"
)

// SetMetricsToken configures the bearer token that authorises GET /metrics. Empty leaves
// the endpoint off. Set once at boot from config, like SetSSOSecret.
func (h *Handler) SetMetricsToken(token string) {
	h.metricsToken = token
}

// SetMetricsAnonymousNetworks configures METRICS_ALLOW_UNAUTHENTICATED_FROM: the networks
// a request may come FROM and scrape /metrics with no bearer. Empty (the default) leaves
// the bearer as the only way in. Set once at boot, like SetMetricsToken.
//
// It takes parsed networks rather than strings because internal/server already owns the
// parser (ParseTrustedProxies) and the handler package does not import it. Passing the
// parsed value also means an unparseable entry is reported once, at boot, by the code that
// read the environment — rather than silently never matching on every scrape.
func (h *Handler) SetMetricsAnonymousNetworks(nets []*net.IPNet) {
	h.metricsAnonymousNets = nets
}

// Metrics handles GET /metrics — Prometheus text exposition of the counters in
// internal/metrics plus the job-queue depth read from the database.
//
// ⛔ Gated on `Authorization: Bearer <METRICS_TOKEN>`, and it answers **404** — byte-identical
// to the mux's own not-found — when the token is unset or wrong. Not 401: a 401 confirms the
// endpoint exists and invites a guess, and the numbers here are a business feed (bookings
// created per hour, request volume by surface) on an instance whose whole point is being
// publicly reachable. An operator who has not configured a token has not opted in to
// publishing any of that, so there is nothing to advertise.
//
// The second way in is METRICS_ALLOW_UNAUTHENTICATED_FROM, off unless configured: a request
// whose TCP peer is inside one of those networks is served without a bearer (I6). It exists
// because a Prometheus collector cannot hold this kind of secret — Grafana Alloy sends one
// bearer file to every target it scrapes — so the fleet's scrape got 404 about 5,755 times a
// day and the fork shipped no metrics at all. Nothing else about the route changed: the
// bearer still works, and outside those networks the answer is what it always was.
//
// The response is not rate-limited: a scrape runs every few seconds by design, and a
// limiter tuned for humans would drop samples and produce gaps that look like downtime.
// The token is the control.
func (h *Handler) Metrics(w http.ResponseWriter, r *http.Request) {
	if !h.metricsAuthorized(r) {
		http.NotFound(w, r)
		return
	}

	// Job depth lives in the database because any instance can claim any job, so it is
	// read per scrape rather than counted in this process. One grouped query; a failure is
	// logged and reported as zero rather than failing the whole scrape, since the process
	// counters are still worth having when the database is the thing that is unwell (and
	// /readyz is the endpoint that answers "is the database reachable").
	var q metrics.Queue
	// ⛔ Platform(), not h.db. jobs is a tenant table (00060): the queue depth of an
	// INSTANCE is an instance-level number, so a bound read would report one
	// workspace's backlog as if it were the whole queue and the unbound handle would
	// report zero. Both are wrong in a way a dashboard cannot show you. /metrics is
	// registered through h.Platform, so h.db is already the platform handle — this is
	// belt and braces against someone re-registering the route Scoped.
	rows, err := h.db.Platform().QueryContext(r.Context(),
		`SELECT status, COUNT(*) FROM jobs WHERE status IN ('pending', 'failed') GROUP BY status`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "metrics: count jobs", "error", err)
	} else {
		for rows.Next() {
			var status string
			var n int64
			if err := rows.Scan(&status, &n); err != nil {
				h.logger.ErrorContext(r.Context(), "metrics: scan job count", "error", err)
				continue
			}
			switch status {
			case "pending":
				q.Pending = n
			case "failed":
				q.Failed = n
			}
		}
		if err := rows.Err(); err != nil {
			h.logger.ErrorContext(r.Context(), "metrics: job count rows", "error", err)
		}
		rows.Close() // #nosec G104 -- rows already fully consumed; nothing actionable on close error
	}

	w.Header().Set("Content-Type", metrics.ContentType)
	// Cache-Control matters here: a scrape must never be answered from an intermediary,
	// or a dashboard shows a frozen instance as a healthy one.
	w.Header().Set("Cache-Control", "no-store")
	if err := metrics.Write(w, q); err != nil {
		h.logger.ErrorContext(r.Context(), "metrics: write exposition", "error", err)
	}
}

// metricsAuthorized reports whether this request may read the exposition: either it comes
// from an allowed network, or it carries the configured bearer token. Everything else is
// answered exactly as it was before the network path existed.
func (h *Handler) metricsAuthorized(r *http.Request) bool {
	return h.metricsPeerAllowed(r) || h.metricsBearerValid(r)
}

// metricsBearerValid reports whether the request carries the configured bearer token.
//
// The comparison is over SHA-256 digests rather than the raw strings: subtle.ConstantTimeCompare
// returns early when the lengths differ, so comparing the values directly would leak the
// token's length. Hashing makes both sides 32 bytes whatever was sent.
func (h *Handler) metricsBearerValid(r *http.Request) bool {
	if h.metricsToken == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	presented := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
	expected := sha256.Sum256([]byte(h.metricsToken))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

// metricsPeerAllowed reports whether the request's TCP peer falls inside
// METRICS_ALLOW_UNAUTHENTICATED_FROM (I6).
//
// ⛔ THE PEER, AND NOT ANY FORWARDED HEADER — not X-Forwarded-For, not CF-Connecting-IP,
// and NOT the trusted-proxy-resolved client IP the rate limiter uses. r.RemoteAddr is the
// only address in a request that the client cannot choose, and here the address IS the
// credential: reading a header instead would let anyone who can reach the endpoint claim
// to be the collector by asserting it, which is not a weaker control but no control at
// all. The rate limiter can afford the opposite trade because the worst outcome of a
// forged value there is a shared bucket.
//
// ⚠️ The consequence to know before deploying it: this endpoint must NOT be reachable
// THROUGH a proxy that sits inside the allowed range, because then every request arrives
// with that proxy as its peer. On the fleet the value is the cluster's pod CIDR and the
// booking hosts terminate at Caddy on the node — a scrape is a pod dialling the pod IP
// directly, and outside traffic reaches :3000 through neither.
func (h *Handler) metricsPeerAllowed(r *http.Request) bool {
	if len(h.metricsAnonymousNets) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port: RemoteAddr is not the "ip:port" a TCP listener sets, so this is a
		// synthetic request rather than a connection. Try it as a bare address and
		// refuse if that fails too — an unreadable peer is not an allowed one.
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range h.metricsAnonymousNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
