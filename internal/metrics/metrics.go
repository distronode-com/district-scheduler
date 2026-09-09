// Package metrics collects the handful of process counters Calnode exposes at /metrics
// in Prometheus text format.
//
// It is hand-written rather than backed by prometheus/client_golang for the reason
// internal/livekit signs its own tokens and internal/stripe speaks HTTP directly: the
// exposition format is a stable, one-page text protocol, and a scrape endpoint is not
// worth a dependency tree in a binary an operator is expected to self-host. What is given
// up is the client library's process/GC collectors and its label-cardinality tooling;
// what is kept is a metrics endpoint that cannot break on a library upgrade.
//
// State is package-level, which is what a metrics registry is: one process, one set of
// counters, no way to hand a registry down through every call site that needs to record
// something. The counters are guarded by a single mutex rather than sharded atomics — the
// hot path already takes the rate limiter's mutex per request, and one uncontended
// Lock/Unlock around three integer updates is not what will bound this server.
package metrics

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calnode/calnode/internal/buildinfo"
)

// Request classes. A request is classified by its path prefix alone, so the set is fixed
// and a label can never be attacker-chosen — the thing that turns a metrics endpoint into
// an out-of-memory vector.
const (
	ClassPublic = "public" // booking pages, the embed widget, rooms, branding assets
	ClassAdmin  = "admin"  // the embedded admin SPA under /admin/
	ClassAPI    = "api"    // /v1/…
	ClassMCP    = "mcp"    // /mcp and its OAuth authorization server
	ClassOps    = "ops"    // /healthz, /readyz, /version, /metrics
)

// Booking event names for BookingEvent.
const (
	BookingCreated     = "created"
	BookingCancelled   = "cancelled"
	BookingRescheduled = "rescheduled"
)

// durationBuckets are the cumulative upper bounds, in seconds, of
// calnode_http_request_duration_seconds. They are client_golang's default set: fixed at
// compile time because a histogram's buckets are part of its identity, and changing them
// on a live instance discards the history in every dashboard built on it.
var durationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// startTime is when the process began, for process_start_time_seconds. Prometheus'
// standard way of deriving uptime, and the only reliable signal that an instance
// restarted between two scrapes.
var startTime = time.Now()

type requestKey struct {
	class  string
	status int
}

var (
	mu            sync.Mutex
	requests      = map[requestKey]uint64{}
	bookings      = map[string]uint64{}
	bucketCounts  = make([]uint64, len(durationBuckets))
	durationSum   float64
	durationCount uint64
)

// HTTPRequest records one served request. Called from the logging middleware, which
// already has the class, the final status and the elapsed time in hand.
func HTTPRequest(class string, status int, d time.Duration) {
	seconds := d.Seconds()
	mu.Lock()
	requests[requestKey{class: class, status: status}]++
	durationSum += seconds
	durationCount++
	for i, upper := range durationBuckets {
		if seconds <= upper {
			// Buckets are cumulative, so an observation counts in this bound and every
			// larger one. Incrementing only the first matching bucket here and summing on
			// the way out keeps this loop short.
			bucketCounts[i]++
			break
		}
	}
	mu.Unlock()
}

// BookingEvent records a booking lifecycle event: one of the Booking* constants.
func BookingEvent(event string) {
	mu.Lock()
	bookings[event]++
	mu.Unlock()
}

// Reset clears every counter. For tests only — a scrape endpoint whose numbers depend on
// which other tests ran first is not testable.
func Reset() {
	mu.Lock()
	requests = map[requestKey]uint64{}
	bookings = map[string]uint64{}
	bucketCounts = make([]uint64, len(durationBuckets))
	durationSum, durationCount = 0, 0
	mu.Unlock()
}

// Queue is the job-queue depth, which lives in the database rather than in this process
// (any instance can claim any job), so the caller reads it and passes it in.
type Queue struct {
	Pending int64
	Failed  int64
}

// ContentType is the exposition format's media type, for the response header.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Write renders the exposition. Ordering is deterministic — labelled series are sorted —
// so a diff of two scrapes shows what changed rather than what got re-mapped.
func Write(w io.Writer, q Queue) error {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	goroutines := runtime.NumGoroutine()

	// Snapshot under the lock, render outside it: rendering does formatting and I/O, and
	// holding the counters' mutex across a write to a possibly-slow client would stall
	// every request in the process.
	mu.Lock()
	reqSnapshot := make(map[requestKey]uint64, len(requests))
	for k, v := range requests {
		reqSnapshot[k] = v
	}
	bookSnapshot := make(map[string]uint64, len(bookings))
	for k, v := range bookings {
		bookSnapshot[k] = v
	}
	buckets := append([]uint64(nil), bucketCounts...)
	sum, count := durationSum, durationCount
	mu.Unlock()

	bi := buildinfo.Get()
	b := &errWriter{w: w}

	b.line("# HELP calnode_build_info Build identity of the running binary; always 1.")
	b.line("# TYPE calnode_build_info gauge")
	b.linef("calnode_build_info{version=%q,commit=%q} 1", bi.Version, bi.Commit)

	b.line("# HELP calnode_http_requests_total Requests served, by path class and response status.")
	b.line("# TYPE calnode_http_requests_total counter")
	for _, k := range sortedRequestKeys(reqSnapshot) {
		b.linef("calnode_http_requests_total{class=%q,status=%q} %d",
			k.class, strconv.Itoa(k.status), reqSnapshot[k])
	}

	b.line("# HELP calnode_http_request_duration_seconds Time to serve a request.")
	b.line("# TYPE calnode_http_request_duration_seconds histogram")
	var cumulative uint64
	for i, upper := range durationBuckets {
		cumulative += buckets[i]
		b.linef("calnode_http_request_duration_seconds_bucket{le=%q} %d",
			formatFloat(upper), cumulative)
	}
	b.linef("calnode_http_request_duration_seconds_bucket{le=\"+Inf\"} %d", count)
	b.linef("calnode_http_request_duration_seconds_sum %s", formatFloat(sum))
	b.linef("calnode_http_request_duration_seconds_count %d", count)

	b.line("# HELP calnode_jobs_pending Background jobs waiting to run.")
	b.line("# TYPE calnode_jobs_pending gauge")
	b.linef("calnode_jobs_pending %d", q.Pending)

	// ⛔ A gauge, and named without the _total suffix, because the value is
	// COUNT(*) WHERE status = 'failed' — the size of the failed backlog right now,
	// exactly like calnode_jobs_pending above it. It goes DOWN when those rows are
	// cleared or retried.
	//
	// It was declared a counter, and the convention is not cosmetic: a counter
	// promises monotonic growth, so rate() over it is read as failures per second.
	// Over a value that can fall, rate() drops the decrease as a counter reset and
	// reports nonsense. A true lifetime failure count would be a separate series
	// incremented where a job is marked failed, which is a different measurement
	// from this one rather than a renaming of it.
	b.line("# HELP calnode_jobs_failed Background jobs that have exhausted their retries and are still in the queue.")
	b.line("# TYPE calnode_jobs_failed gauge")
	b.linef("calnode_jobs_failed %d", q.Failed)

	b.line("# HELP calnode_bookings_total Booking lifecycle events since this process started.")
	b.line("# TYPE calnode_bookings_total counter")
	// Always emit all three, at zero if need be: a series that only appears once it is
	// non-zero makes a rate() over a quiet window return nothing instead of 0, which
	// reads on a dashboard as "no data" rather than "nothing happened".
	for _, event := range []string{BookingCreated, BookingCancelled, BookingRescheduled} {
		b.linef("calnode_bookings_total{event=%q} %d", event, bookSnapshot[event])
	}

	b.line("# HELP process_start_time_seconds Start time of the process since the unix epoch.")
	b.line("# TYPE process_start_time_seconds gauge")
	b.linef("process_start_time_seconds %d", startTime.Unix())

	b.line("# HELP go_goroutines Number of goroutines that currently exist.")
	b.line("# TYPE go_goroutines gauge")
	b.linef("go_goroutines %d", goroutines)

	b.line("# HELP go_memstats_alloc_bytes Bytes allocated and still in use.")
	b.line("# TYPE go_memstats_alloc_bytes gauge")
	b.linef("go_memstats_alloc_bytes %d", ms.Alloc)

	return b.err
}

// sortedRequestKeys orders series by class then status.
func sortedRequestKeys(m map[requestKey]uint64) []requestKey {
	keys := make([]requestKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].class != keys[j].class {
			return keys[i].class < keys[j].class
		}
		return keys[i].status < keys[j].status
	})
	return keys
}

// formatFloat renders a float the way the exposition format wants it: shortest form that
// round-trips, never scientific notation for the values in play here.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// errWriter collects the first write error so the renderer above stays free of
// per-line error handling. A scrape that fails mid-body is reported to the caller and
// logged; there is nothing to roll back.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) line(s string) {
	if e.err != nil {
		return
	}
	_, e.err = io.WriteString(e.w, s+"\n")
}

func (e *errWriter) linef(format string, args ...any) {
	e.line(fmt.Sprintf(format, args...))
}

// ClassifyPath maps a request path to one of the five classes, by prefix only.
//
// ⚠️ Syntactic on purpose: POST /v1/bookings is a public, unauthenticated endpoint and
// still counts as "api" here, because the alternative is a per-route table that drifts
// silently the first time someone adds a route. Read the classes as "which surface of the
// URL space", not "how the request was authenticated".
func ClassifyPath(path string) string {
	switch {
	case path == "/healthz" || path == "/readyz" || path == "/version" || path == "/metrics":
		return ClassOps
	case path == "/mcp" || strings.HasPrefix(path, "/mcp/") ||
		strings.HasPrefix(path, "/oauth/") || strings.HasPrefix(path, "/.well-known/"):
		return ClassMCP
	case path == "/admin" || strings.HasPrefix(path, "/admin/"):
		return ClassAdmin
	case strings.HasPrefix(path, "/v1/"):
		return ClassAPI
	default:
		return ClassPublic
	}
}
