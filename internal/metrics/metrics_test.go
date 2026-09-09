package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/metrics"
)

// scrape renders the exposition and returns its lines.
func scrape(t *testing.T, q metrics.Queue) []string {
	t.Helper()
	var sb strings.Builder
	if err := metrics.Write(&sb, q); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func findLine(t *testing.T, lines []string, prefix string) string {
	t.Helper()
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starting %q in:\n%s", prefix, strings.Join(lines, "\n"))
	return ""
}

// A deliberately parser-free check: assert the exact lines an operator's scrape config
// depends on. Pulling in a Prometheus parser to test this would defeat the reason the
// exposition is hand-written.
func TestWrite_exposesEveryDocumentedSeries(t *testing.T) {
	metrics.Reset()
	metrics.HTTPRequest(metrics.ClassAPI, 200, 12*time.Millisecond)
	metrics.HTTPRequest(metrics.ClassAPI, 200, 8*time.Millisecond)
	metrics.HTTPRequest(metrics.ClassPublic, 404, 3*time.Millisecond)
	metrics.BookingEvent(metrics.BookingCreated)

	lines := scrape(t, metrics.Queue{Pending: 4, Failed: 2})

	for _, want := range []string{
		`calnode_http_requests_total{class="api",status="200"} 2`,
		`calnode_http_requests_total{class="public",status="404"} 1`,
		`calnode_bookings_total{event="created"} 1`,
		// Emitted at zero rather than omitted, so a rate() over a quiet window returns 0
		// instead of nothing.
		`calnode_bookings_total{event="cancelled"} 0`,
		`calnode_bookings_total{event="rescheduled"} 0`,
		`calnode_jobs_pending 4`,
		`calnode_jobs_failed 2`,
		`calnode_http_request_duration_seconds_count 3`,
	} {
		if !hasLine(lines, want) {
			t.Errorf("missing line:\n  %s\ngot:\n%s", want, strings.Join(lines, "\n"))
		}
	}

	// The ones whose values are not fixed: assert shape, not value.
	for _, prefix := range []string{
		"calnode_build_info{version=",
		"process_start_time_seconds ",
		"go_goroutines ",
		"go_memstats_alloc_bytes ",
	} {
		findLine(t, lines, prefix)
	}
	if !strings.HasSuffix(findLine(t, lines, "calnode_build_info{version="), " 1") {
		t.Error("calnode_build_info must always be 1")
	}
}

// Every series needs its HELP and TYPE, or a scrape is a wall of unlabelled numbers.
func TestWrite_everyMetricHasHelpAndType(t *testing.T) {
	metrics.Reset()
	lines := scrape(t, metrics.Queue{})

	names := []string{
		"calnode_build_info",
		"calnode_http_requests_total",
		"calnode_http_request_duration_seconds",
		"calnode_jobs_pending",
		"calnode_jobs_failed",
		"calnode_bookings_total",
		"process_start_time_seconds",
		"go_goroutines",
		"go_memstats_alloc_bytes",
	}
	for _, name := range names {
		if !hasPrefixLine(lines, "# HELP "+name+" ") {
			t.Errorf("%s has no HELP", name)
		}
		if !hasPrefixLine(lines, "# TYPE "+name+" ") {
			t.Errorf("%s has no TYPE", name)
		}
	}
}

func hasPrefixLine(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// Every non-comment line must be "name value" or "name{labels} value" — one metric per
// line, exactly two fields once the label set is closed.
func TestWrite_everySampleLineIsWellFormed(t *testing.T) {
	metrics.Reset()
	metrics.HTTPRequest(metrics.ClassOps, 200, time.Millisecond)
	lines := scrape(t, metrics.Queue{Pending: 1})

	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			continue
		}
		if l == "" {
			t.Error("blank sample line")
			continue
		}
		// A sample is `name value` or `name{labels} value`. Count the fields after the
		// label set (if any), which is the one place a stray space or a missing value
		// would show up.
		want := 2
		rest := l
		if close := strings.LastIndex(l, "}"); close >= 0 {
			rest = l[close+1:]
			want = 1
		}
		if got := len(strings.Fields(rest)); got != want {
			t.Errorf("line %q has %d fields after its name/labels; want %d", l, got, want)
		}
	}
}

// Buckets are cumulative and the last one equals _count. Getting this wrong yields a
// histogram that renders as a plausible but wrong latency distribution.
func TestWrite_histogramBucketsAreCumulative(t *testing.T) {
	metrics.Reset()
	metrics.HTTPRequest(metrics.ClassAPI, 200, 1*time.Millisecond)   // ≤ 0.005
	metrics.HTTPRequest(metrics.ClassAPI, 200, 30*time.Millisecond)  // ≤ 0.05
	metrics.HTTPRequest(metrics.ClassAPI, 200, 400*time.Millisecond) // ≤ 0.5
	metrics.HTTPRequest(metrics.ClassAPI, 200, 30*time.Second)       // over the top bucket

	lines := scrape(t, metrics.Queue{})

	for _, want := range []string{
		`calnode_http_request_duration_seconds_bucket{le="0.005"} 1`,
		`calnode_http_request_duration_seconds_bucket{le="0.01"} 1`,
		`calnode_http_request_duration_seconds_bucket{le="0.05"} 2`,
		`calnode_http_request_duration_seconds_bucket{le="0.5"} 3`,
		`calnode_http_request_duration_seconds_bucket{le="10"} 3`,
		// The 30s observation only ever appears in +Inf and _count.
		`calnode_http_request_duration_seconds_bucket{le="+Inf"} 4`,
		`calnode_http_request_duration_seconds_count 4`,
	} {
		if !hasLine(lines, want) {
			t.Errorf("missing line:\n  %s\ngot:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

func TestClassifyPath(t *testing.T) {
	cases := map[string]string{
		"/healthz":                  metrics.ClassOps,
		"/readyz":                   metrics.ClassOps,
		"/version":                  metrics.ClassOps,
		"/metrics":                  metrics.ClassOps,
		"/mcp":                      metrics.ClassMCP,
		"/oauth/token":              metrics.ClassMCP,
		"/.well-known/oauth-server": metrics.ClassMCP,
		"/admin":                    metrics.ClassAdmin,
		"/admin/bookings":           metrics.ClassAdmin,
		"/v1/bookings":              metrics.ClassAPI,
		"/v1/event-types/x/slots":   metrics.ClassAPI,
		"/book/intro":               metrics.ClassPublic,
		"/manage/tok":               metrics.ClassPublic,
		"/room/abc":                 metrics.ClassPublic,
		"/embed.js":                 metrics.ClassPublic,
		"/":                         metrics.ClassPublic,
		// Not /admin/: a path that merely starts with those letters is a public 404, and
		// must not be filed under the console.
		"/administrivia": metrics.ClassPublic,
	}
	for path, want := range cases {
		if got := metrics.ClassifyPath(path); got != want {
			t.Errorf("ClassifyPath(%q) = %q; want %q", path, got, want)
		}
	}
}

// The label set has to stay closed, or a metrics endpoint becomes a memory vector: one
// series per distinct label value, and paths are attacker-chosen.
func TestClassifyPath_labelSetIsClosed(t *testing.T) {
	allowed := map[string]bool{
		metrics.ClassPublic: true, metrics.ClassAdmin: true, metrics.ClassAPI: true,
		metrics.ClassMCP: true, metrics.ClassOps: true,
	}
	for _, path := range []string{"", "/", "//", "/../etc/passwd", "/v1", "/mcpx", strings.Repeat("/a", 500)} {
		if got := metrics.ClassifyPath(path); !allowed[got] {
			t.Errorf("ClassifyPath(%q) = %q, which is outside the five classes", path, got)
		}
	}
}
