package handler_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/metrics"
)

const metricsToken = "metrics-token-for-tests"

func doMetrics(h *handler.Handler, auth string) *httptest.ResponseRecorder {
	return doMetricsFrom(h, auth, "")
}

// doMetricsFrom is doMetrics with control over the TCP peer. peer is "ip:port"; empty
// leaves httptest's own 192.0.2.1:1234, which is outside every CIDR used below.
func doMetricsFrom(h *handler.Handler, auth, peer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if peer != "" {
		req.RemoteAddr = peer
	}
	rec := httptest.NewRecorder()
	h.Metrics(rec, req)
	return rec
}

// mustCIDRs parses a CIDR list the way server.ParseTrustedProxies does, without
// importing internal/server (which imports this package).
func mustCIDRs(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, e := range entries {
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			t.Fatalf("parse %q: %v", e, err)
		}
		out = append(out, n)
	}
	return out
}

// Unset METRICS_TOKEN ⇒ 404, byte-identical to the mux's own not-found. A 401 would
// confirm the endpoint exists on an instance whose operator never opted in to publishing
// its booking rate.
func TestMetrics_404WithoutAToken(t *testing.T) {
	h, _ := newTestHandlerDB(t)

	rec := doMetrics(h, "Bearer "+metricsToken)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 — %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "calnode_") {
		t.Error("body leaked metrics despite the endpoint being off")
	}
}

func TestMetrics_rejectsWrongOrMissingBearer(t *testing.T) {
	cases := map[string]string{
		"no header":              "",
		"empty bearer":           "Bearer ",
		"wrong token":            "Bearer nope",
		"right token, no scheme": metricsToken,
		"basic auth":             "Basic " + metricsToken,
		// A prefix of the real token must not pass: the comparison is over digests, so
		// length tells an attacker nothing either.
		"token prefix": "Bearer " + metricsToken[:10],
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestHandlerDB(t)
			h.SetMetricsToken(metricsToken)

			rec := doMetrics(h, auth)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d; want 404 for %q", rec.Code, auth)
			}
		})
	}
}

func TestMetrics_servesExpositionWithTheToken(t *testing.T) {
	metrics.Reset()
	h, database := newTestHandlerDB(t)
	h.SetMetricsToken(metricsToken)

	// Two pending jobs and one failed one, so the gauges are read from the table rather
	// than reported as zero. The payload differs per row: jobs carries UNIQUE(type,
	// payload), which is what makes an enqueue idempotent.
	for _, row := range []struct{ id, status string }{
		{"job-1", "pending"}, {"job-2", "pending"}, {"job-3", "failed"}, {"job-4", "done"},
	} {
		if _, err := database.Exec(
			`INSERT INTO jobs (id, type, payload, run_at, status) VALUES (?, 'webhook.deliver', ?, '2026-01-01T00:00:00Z', ?)`,
			row.id, `{"webhook_delivery_id":"`+row.id+`"}`, row.status); err != nil {
			t.Fatalf("seed job %s: %v", row.id, err)
		}
	}

	rec := doMetrics(h, "Bearer "+metricsToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != metrics.ContentType {
		t.Errorf("Content-Type = %q; want %q", ct, metrics.ContentType)
	}
	// A cached scrape shows a frozen instance as a healthy one.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store", cc)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"calnode_jobs_pending 2",
		"calnode_jobs_failed 1",
		"# TYPE calnode_build_info gauge",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// METRICS_ALLOW_UNAUTHENTICATED_FROM (I6). Alloy's scrape got 404 about 5,755 times a day
// across the fleet because a Prometheus collector cannot hold this endpoint's kind of
// secret: the chart sends ONE bearer token file to every target it discovers, so pointing
// it at METRICS_TOKEN would hand this instance's token to every other scraped pod. The
// opt-in here is the network position instead.

// Inside the range, no bearer, no token configured at all: 200 with the real exposition.
// The "no token configured" half is the deployment this is for — the fleet does set one,
// but an operator who wants ONLY the in-cluster scrape should not have to invent a secret
// nothing will ever present.
func TestMetrics_anonymousScrapeFromAnAllowedNetwork(t *testing.T) {
	metrics.Reset()
	h, _ := newTestHandlerDB(t)
	h.SetMetricsAnonymousNetworks(mustCIDRs(t, "10.4.0.0/14"))

	rec := doMetricsFrom(h, "", "10.4.1.9:41234")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "calnode_") {
		t.Errorf("body carries no exposition:\n%s", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store — a cached scrape shows a frozen instance as a healthy one", cc)
	}
}

// Outside the range the answer is exactly today's, in every combination. The token half
// matters as much as the 404 half: the setting adds a way in, it does not replace one.
func TestMetrics_outsideTheRangeNothingChanged(t *testing.T) {
	cases := []struct {
		name  string
		token string
		auth  string
		peer  string
		want  int
	}{
		{"outside, no bearer, no token", "", "", "203.0.113.7:5000", http.StatusNotFound},
		{"outside, no bearer, token set", metricsToken, "", "203.0.113.7:5000", http.StatusNotFound},
		{"outside, wrong bearer", metricsToken, "Bearer nope", "203.0.113.7:5000", http.StatusNotFound},
		{"outside, right bearer still works", metricsToken, "Bearer " + metricsToken, "203.0.113.7:5000", http.StatusOK},
		{"inside, wrong bearer is still served", metricsToken, "Bearer nope", "10.4.1.9:41234", http.StatusOK},
		// An address one bit outside the mask. 10.8.0.1 is not in 10.4.0.0/14.
		{"just outside the mask", "", "", "10.8.0.1:41234", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			metrics.Reset()
			h, _ := newTestHandlerDB(t)
			h.SetMetricsAnonymousNetworks(mustCIDRs(t, "10.4.0.0/14"))
			if c.token != "" {
				h.SetMetricsToken(c.token)
			}

			if rec := doMetricsFrom(h, c.auth, c.peer); rec.Code != c.want {
				t.Errorf("status = %d; want %d", rec.Code, c.want)
			}
		})
	}
}

// ⛔ THE ADDRESS IS THE CREDENTIAL, SO IT MUST BE THE ONE THE CLIENT CANNOT CHOOSE.
// Every header a proxy might set is asserted individually rather than as a group: this is
// the single assertion that keeps the setting from being no control at all, and a future
// refactor that reached for the rate limiter's resolved client IP would pass a test that
// only checked X-Forwarded-For.
func TestMetrics_aForwardedHeaderCannotSatisfyTheAllowlist(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "CF-Connecting-IP", "X-Real-IP", "Forwarded"} {
		t.Run(header, func(t *testing.T) {
			h, _ := newTestHandlerDB(t)
			h.SetMetricsAnonymousNetworks(mustCIDRs(t, "10.4.0.0/14"))

			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.RemoteAddr = "203.0.113.7:5000"
			req.Header.Set(header, "10.4.1.9")
			rec := httptest.NewRecorder()
			h.Metrics(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d; want 404 — %s claiming an in-range address must not authorise a scrape", rec.Code, header)
			}
		})
	}
}

// Unset (the default, and every deployment that exists today) leaves the bearer as the
// only way in, from any address.
func TestMetrics_emptyAllowlistIsTodaysBehaviour(t *testing.T) {
	for _, peer := range []string{"10.4.1.9:41234", "127.0.0.1:41234", "203.0.113.7:5000"} {
		t.Run(peer, func(t *testing.T) {
			h, _ := newTestHandlerDB(t)
			// No SetMetricsAnonymousNetworks call at all, which is what BuildHandler does
			// when the variable is empty.
			if rec := doMetricsFrom(h, "", peer); rec.Code != http.StatusNotFound {
				t.Errorf("status = %d; want 404 with no allowlist configured", rec.Code)
			}
			h.SetMetricsToken(metricsToken)
			if rec := doMetricsFrom(h, "Bearer "+metricsToken, peer); rec.Code != http.StatusOK {
				t.Errorf("status = %d; want 200 — the bearer is unaffected", rec.Code)
			}
		})
	}
}
