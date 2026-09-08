package server

import (
	"regexp"
	"strings"
	"testing"
)

// The platform-managed gate (H1), in the same shape as the classification gate beside it
// and for the same reason: what has to be caught is a REGISTRATION written without a
// wrapper, which is a property of server.go's text rather than of any served response.
//
// ⛔ These routes read or write an INSTANCE credential — the SMTP account, the Google
// OAuth client, the Zoom app, the LiveKit server, the Stripe account, the model provider
// — every one of which is shared by every workspace on the process. A tenant admin holds
// a `cno_` key their own Developer tab minted, and that key is accepted on every
// credential route, so the platform dashboard's own allowlist protects the dashboard and
// not the instance. h.PlatformManaged is what protects the instance, and this test is
// what keeps it attached.
//
// It asserts in BOTH directions. A guarded path that loses its wrapper fails; and a
// /v1/settings route that GAINS one without being added here fails too, because the
// tenant-safe settings (branding, storage, notetaker, tracking) are what the platform's
// dashboard calls, and taking one of them away would break the console in multi-tenant
// mode only — the mode nobody runs locally.

// platformManagedGuards maps a route pattern to the guard expression its registration
// must contain. Two guards exist: the blanket one, and the field-scoped one for the two
// PATCHes whose body carries the tenant's own fields alongside the platform's credential
// ones (`/settings/llm` and `/settings/notetaker`).
var platformManagedGuards = map[string]string{
	"GET /v1/settings/email":       "h.PlatformManaged(",
	"PATCH /v1/settings/email":     "h.PlatformManaged(",
	"POST /v1/settings/email/test": "h.PlatformManaged(",
	"GET /v1/settings/google":      "h.PlatformManaged(",
	"PATCH /v1/settings/google":    "h.PlatformManaged(",
	"GET /v1/settings/zoom":        "h.PlatformManaged(",
	"PATCH /v1/settings/zoom":      "h.PlatformManaged(",
	"GET /v1/settings/livekit":     "h.PlatformManaged(",
	"PATCH /v1/settings/livekit":   "h.PlatformManaged(",
	"GET /v1/settings/stripe":      "h.PlatformManaged(",
	"PATCH /v1/settings/stripe":    "h.PlatformManaged(",
	"POST /v1/settings/llm/test":   "h.PlatformManaged(",
	"PATCH /v1/settings/llm":       `h.PlatformManagedFields("endpoint", "model", "api_key")`,
	"PATCH /v1/settings/notetaker": `h.PlatformManagedFields("stt_api_key")`,
}

// tenantSafeSettingsRoutes are the /v1/settings paths a tenant legitimately owns. Named
// rather than inferred, so adding a settings route forces a decision about which side of
// the line it is on instead of defaulting to "unguarded".
var tenantSafeSettingsRoutes = map[string]string{
	"GET /v1/settings/storage":            "a per-workspace recording-retention toggle",
	"PATCH /v1/settings/storage":          "a per-workspace recording-retention toggle",
	"GET /v1/settings/notetaker":          "reads only; stt_api_key_set and stt_base_url are omitted in the handler",
	"GET /v1/settings/tracking":           "the workspace's own GA4/GTM ids (head_html is refused separately, L6)",
	"PATCH /v1/settings/tracking":         "the workspace's own GA4/GTM ids (head_html is refused separately, L6)",
	"GET /v1/settings/branding":           "the workspace's own logo, colours and links",
	"PATCH /v1/settings/branding":         "the workspace's own logo, colours and links",
	"POST /v1/settings/branding/logo":     "the workspace's own logo",
	"DELETE /v1/settings/branding/logo":   "the workspace's own logo",
	"POST /v1/settings/branding/banner":   "the workspace's own banner",
	"DELETE /v1/settings/branding/banner": "the workspace's own banner",
	"GET /v1/settings/llm":                "reads only; the provider fields are omitted in the handler (H1)",
}

func TestInstanceCredentialRoutesAreGuardedByPlatformManaged(t *testing.T) {
	src, err := ReadServerSource()
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	seen := map[string]string{}
	for _, line := range strings.Split(src, "\n") {
		m := handleFuncRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		seen[m[1]] = m[2]
	}

	for pattern, guard := range platformManagedGuards {
		expr, registered := seen[pattern]
		if !registered {
			t.Errorf("platformManagedGuards names %q, which is not registered in server.go — "+
				"delete the entry, or restore the route", pattern)
			continue
		}
		if !strings.Contains(expr, guard) {
			t.Errorf("route %q is not guarded: its registration does not contain %s\n"+
				"    it configures an INSTANCE credential every workspace on this process shares; "+
				"a tenant credential must get 403 managed_by_platform in multi-tenant mode", pattern, guard)
		}
	}

	// The other direction: nothing tenant-safe may acquire a guard by accident.
	for pattern, why := range tenantSafeSettingsRoutes {
		expr, registered := seen[pattern]
		if !registered {
			t.Errorf("tenantSafeSettingsRoutes names %q, which is not registered — delete the entry", pattern)
			continue
		}
		if strings.Contains(expr, "h.PlatformManaged") {
			t.Errorf("route %q is guarded but is tenant-safe (%s)\n"+
				"    guarding it takes the surface away from the platform's own dashboard, "+
				"in multi-tenant mode only", pattern, why)
		}
	}
}

// Every /v1/settings registration has to appear in one of the two tables above. Without
// this the gate is only as good as somebody remembering to extend it: a settings route
// added later would be unguarded, unlisted, and silently outside both assertions.
func TestEverySettingsRouteHasATenancyDecision(t *testing.T) {
	src, err := ReadServerSource()
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	var checked int
	for _, line := range strings.Split(src, "\n") {
		m := handleFuncRe.FindStringSubmatch(line)
		if m == nil || !strings.Contains(m[1], "/v1/settings/") {
			continue
		}
		checked++
		_, guarded := platformManagedGuards[m[1]]
		_, safe := tenantSafeSettingsRoutes[m[1]]
		switch {
		case guarded && safe:
			t.Errorf("route %q is in BOTH tables; it cannot be two things", m[1])
		case !guarded && !safe:
			t.Errorf("route %q is in neither platformManagedGuards nor tenantSafeSettingsRoutes.\n"+
				"    Decide: does it configure an INSTANCE credential (wrap it in h.PlatformManaged "+
				"and list it) or the workspace's own data (list it as tenant-safe, with the reason)?", m[1])
		}
	}
	if want := len(platformManagedGuards) + len(tenantSafeSettingsRoutes); checked != want {
		t.Errorf("scanned %d /v1/settings routes but the two tables hold %d; "+
			"one of them names a pattern the scan cannot see", checked, want)
	}
	t.Logf("checked %d /v1/settings routes: %d platform-managed, %d tenant-safe",
		checked, len(platformManagedGuards), len(tenantSafeSettingsRoutes))
}

// A stray guard outside the settings tree would be a route silently 403ing every tenant
// in multi-tenant mode, and nothing else in this file would notice.
func TestNoPlatformManagedGuardOutsideItsTable(t *testing.T) {
	src, err := ReadServerSource()
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	guardRe := regexp.MustCompile(`h\.PlatformManaged`)
	for _, line := range strings.Split(src, "\n") {
		if !guardRe.MatchString(line) {
			continue
		}
		m := handleFuncRe.FindStringSubmatch(line)
		if m == nil {
			// A comment or a helper, not a registration. Registrations are one line
			// each (routes_classified_test.go relies on the same rule).
			if strings.Contains(line, "mux.Handle") {
				t.Errorf("a guarded route is registered in a shape this scan cannot read:\n    %s",
					strings.TrimSpace(line))
			}
			continue
		}
		if _, ok := platformManagedGuards[m[1]]; !ok {
			t.Errorf("route %q carries h.PlatformManaged but is not in platformManagedGuards.\n"+
				"    Add it with the reason, or remove the guard: it refuses every tenant credential "+
				"on that route in multi-tenant mode.", m[1])
		}
	}
}
