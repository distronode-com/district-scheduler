package server_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/server"
)

// B7 — the cost of a tenant, measured rather than assumed.
//
// The design's claim was "a few MB per tenant". This provisions 200 workspaces through the real
// platform API in one process, serves one request to each, and reports process RSS before and
// after, plus the pool's open connections.
//
// ⛔ Opt-in: it needs a PostgreSQL server and takes tens of seconds, so it runs only with
// CALNODE_RSS_PROOF=1. It is a test rather than a script on purpose — the number wanted is the
// RSS of a process that has the handler, the caches and the pool in it, and a separate script
// would measure a different process.
//
//	CALNODE_RSS_PROOF=1 CALNODE_TEST_POSTGRES_DSN='postgres://…' \
//	  go test ./internal/server/ -run TestRSS_200Workspaces -v
func TestRSS_200Workspaces(t *testing.T) {
	if os.Getenv("CALNODE_RSS_PROOF") != "1" {
		t.Skip("set CALNODE_RSS_PROOF=1 to run the 200-workspace RSS measurement")
	}
	const workspaces = 200

	app, platform := dbtest.RequireTenantPair(t)
	cfg := &config.Config{
		MultiTenant:   true,
		AdminSPA:      true, // Load's default; a literal skips Load (see newTenancyFixture)
		BaseURL:       "https://app.calnode.example",
		PublicBaseURL: "https://app.calnode.example",
		PlatformToken: "rss-proof-token",
		EncryptionKey: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	mux, drain := server.New(workerCtx, cfg, app, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopWorker(); drain() })

	baselineRSS := vmRSSKB(t)
	var baselineMem runtime.MemStats
	runtime.ReadMemStats(&baselineMem)

	// Provision through the API, not through INSERTs: the point is the cost of a tenant as the
	// platform actually creates one, settings row, owner, key, webhook, event type and all.
	for i := 0; i < workspaces; i++ {
		id := fmt.Sprintf("ws%03d", i)
		body := fmt.Sprintf(`{"id":%q,"slug":%q,"public_host":%q,"region":"us",
			"owner_email":"owner@%s.example","owner_name":"Owner","owner_timezone":"UTC",
			"defaults":{"embed_allowed_origins":[],"webhook":{"url":"https://hooks.%s.example/in"},
			"event_type":{"slug":"intro","name":"Intro","duration_minutes":30,
			"min_notice_minutes":0,"max_future_days":60,
			"availability":[{"day_of_week":1,"start_time":"09:00","end_time":"17:00"}]}}}`,
			id, id, id+".example", id, id)
		req := httptest.NewRequest(http.MethodPost, "/v1/platform/workspaces", strings.NewReader(body))
		req.Host = "app.calnode.example"
		req.Header.Set("Authorization", "Bearer "+cfg.PlatformToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("provision %s: %d — %s", id, rec.Code, rec.Body.String())
		}
	}
	provisionedRSS := vmRSSKB(t)

	// One request per workspace, on its own public host, so every tenant's caches and bound
	// handle are exercised at least once.
	for i := 0; i < workspaces; i++ {
		id := fmt.Sprintf("ws%03d", i)
		req := httptest.NewRequest(http.MethodGet, "/v1/event-types/intro/slots?days=1", nil)
		req.Host = id + ".example"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("request to %s: %d — %s", id, rec.Code, rec.Body.String())
		}
	}
	servedRSS := vmRSSKB(t)
	var servedMem runtime.MemStats
	runtime.ReadMemStats(&servedMem)

	appStats, platStats := app.Stats(), platform.Stats()

	perTenantKB := float64(servedRSS-baselineRSS) / float64(workspaces)
	t.Logf("RSS: baseline %d KB, after provisioning %d workspaces %d KB, after one request each %d KB",
		baselineRSS, workspaces, provisionedRSS, servedRSS)
	t.Logf("growth: %d KB total, %.1f KB per tenant", servedRSS-baselineRSS, perTenantKB)
	t.Logf("runtime.MemStats.Sys: %d KB -> %d KB", baselineMem.Sys/1024, servedMem.Sys/1024)
	t.Logf("pool: application open=%d inUse=%d idle=%d, platform open=%d inUse=%d idle=%d",
		appStats.OpenConnections, appStats.InUse, appStats.Idle,
		platStats.OpenConnections, platStats.InUse, platStats.Idle)

	// ⚠️ A ceiling rather than an equality: RSS is noisy and the GC is not asked to run. This
	// fails only if a tenant costs something the design did not anticipate — a per-workspace
	// allocation that never comes back.
	if perTenantKB > 1024 {
		t.Errorf("%.1f KB per tenant; the design assumed a few MB for HUNDREDS, so anything "+
			"approaching 1 MB each means something is retained per workspace", perTenantKB)
	}
	// The pool must not grow with tenants: handles are values over one pool (D5).
	if appStats.OpenConnections > 20 {
		t.Errorf("the application pool holds %d connections after %d workspaces; ForWorkspace "+
			"must not open one per tenant", appStats.OpenConnections, workspaces)
	}
}

// vmRSSKB reads VmRSS out of /proc/self/status. Linux only, which is where this runs.
func vmRSSKB(t *testing.T) int {
	t.Helper()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status on this platform: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("unparsable VmRSS line %q", line)
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("VmRSS %q: %v", fields[1], err)
		}
		return kb
	}
	t.Fatal("no VmRSS in /proc/self/status")
	return 0
}
