package server

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The classification gate.
//
// Every route registered in New must be one of three things, and must SAY which:
//
//	h.Scoped(HostWorkspace, …)        — the tenant comes from the request Host
//	h.Scoped(CredentialWorkspace, …)  — the tenant comes from the credential
//	h.Platform(…)                     — no tenant, on the identity host, on purpose
//
// or it must be on the allowlist below, which holds only handlers that are not
// methods on *handler.Handler at all: the two CORS preflights, the embedded
// frontend's static handlers, and two redirects.
//
// ⛔ This is a SOURCE scan rather than a runtime walk, and that is deliberate:
// http.ServeMux exposes no way to enumerate its patterns, and the thing that has
// to be caught is a registration written without a wrapper — which is a property
// of the text, not of the served response. A future Go that adds mux iteration
// would let this become a runtime assertion; until then the file is the mux.
//
// A new route added without a wrapper fails here, which is the point: on a
// multi-tenant instance an unwrapped handler runs on the UNSCOPED handle, and on
// the unbound application handle that means it reads nothing — or, if someone
// "fixes" that by handing it the platform handle, everything.

// unscopedAllowlist holds the patterns whose handler is not a *handler.Handler
// method. Each entry is a decision, and adding one should be an argument.
var unscopedAllowlist = map[string]string{
	// CORS preflights: an empty function that only needs the headers the cors
	// wrapper already set. It touches no data, so there is no tenant to resolve.
	"OPTIONS /v1/bookings":                     "empty CORS preflight",
	"OPTIONS /v1/event-types/{slug}/assistant": "empty CORS preflight",
	// The embedded frontend and two redirects: static bytes compiled into the
	// binary, identical for every tenant.
	"GET /favicon.svg": "embedded static asset",
	"GET /favicon.ico": "embedded static asset",
	"GET /admin":       "redirect to /admin/",
	"/admin/":          "embedded admin SPA (static bytes, same for every tenant)",
	"GET /{$}":         "redirect to /admin/",
	// The MCP mount is a *mcp.Server behind two middlewares, not a handler
	// method. Its tenant comes from the bearer credential, resolved by
	// MCPCallerMiddleware and applied by MCPServerForRequest — which is asserted
	// separately, below.
	"/mcp": "MCP streamable-HTTP mount; scoped by MCPServerForRequest",
}

var (
	handleFuncRe = regexp.MustCompile(`mux\.HandleFunc\("([^"]+)",\s*(.*)$`)
	handleRe     = regexp.MustCompile(`mux\.Handle\("([^"]+)",\s*(.*)$`)
)

// ClassifiedRoute is one registration as the scan below found it: the mux pattern, and the
// class the registration declares.
//
// It is exported from a _test.go file so the tenancy PROOF suite (package server_test) can
// derive its table from the same scan this gate uses, rather than keeping a second copy of the
// route list. Two lists would drift, and the one that drifted would be the one asserting
// isolation.
type ClassifiedRoute struct {
	Pattern string
	Expr    string
	Class   string // "host" | "credential" | "platform" | "allowlisted"
}

// ScanClassifiedRoutes reads server.go and classifies every registration in it.
//
// ⛔ A source scan rather than a runtime walk because http.ServeMux exposes no way to enumerate
// its patterns, and the thing to catch is a registration written without a wrapper — a property
// of the text. See the file header.
func ScanClassifiedRoutes(src string) []ClassifiedRoute {
	var out []ClassifiedRoute
	for _, line := range strings.Split(src, "\n") {
		for _, re := range []*regexp.Regexp{handleFuncRe, handleRe} {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			r := ClassifiedRoute{Pattern: m[1], Expr: m[2]}
			switch {
			case strings.Contains(r.Expr, "handler.HostWorkspace"):
				r.Class = "host"
			case strings.Contains(r.Expr, "handler.CredentialWorkspace"):
				r.Class = "credential"
			case strings.Contains(r.Expr, "h.Platform("):
				r.Class = "platform"
			default:
				if _, ok := unscopedAllowlist[r.Pattern]; ok {
					r.Class = "allowlisted"
				}
			}
			out = append(out, r)
		}
	}
	return out
}

// ReadServerSource returns server.go's text, for a caller in another package that cannot reach
// the file by a relative path of its own.
func ReadServerSource() (string, error) {
	b, err := os.ReadFile("server.go")
	return string(b), err
}

// TestEveryRouteIsClassified reads server.go and insists on it.
func TestEveryRouteIsClassified(t *testing.T) {
	src, err := ReadServerSource()
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	type route struct{ pattern, expr string }
	var routes []route
	for _, r := range ScanClassifiedRoutes(src) {
		routes = append(routes, route{pattern: r.Pattern, expr: r.Expr})
	}

	// A floor, so a regexp that stopped matching cannot pass vacuously.
	if len(routes) < 150 {
		t.Fatalf("found only %d registrations in server.go; the scan has stopped working", len(routes))
	}

	var host, credential, platform, allowed int
	seen := map[string]bool{}
	for _, r := range routes {
		if seen[r.pattern] {
			// Two registrations of one pattern is a panic at boot, so it is worth
			// catching here — except for the deliberate LiveKit webhook alias,
			// which is a different pattern for the same handler.
			t.Errorf("pattern %q is registered twice", r.pattern)
		}
		seen[r.pattern] = true

		switch {
		case strings.Contains(r.expr, "handler.HostWorkspace"):
			host++
		case strings.Contains(r.expr, "handler.CredentialWorkspace"):
			credential++
		case strings.Contains(r.expr, "h.Platform("):
			platform++
		default:
			if _, ok := unscopedAllowlist[r.pattern]; ok {
				allowed++
				continue
			}
			t.Errorf("route %q is unclassified: %s\n"+
				"    wrap it in h.Scoped(handler.HostWorkspace, (*H).Method), "+
				"h.Scoped(handler.CredentialWorkspace, (*H).Method) or h.Platform((*H).Method), "+
				"or add it to unscopedAllowlist with a reason", r.pattern, strings.TrimSuffix(r.expr, ")"))
		}
	}

	// Every allowlist entry must still be a registered route, so a stale
	// exemption cannot sit there silently permitting nothing.
	for pattern := range unscopedAllowlist {
		if !seen[pattern] {
			t.Errorf("unscopedAllowlist holds %q, which is no longer registered — delete the entry", pattern)
		}
	}

	t.Logf("%d routes: %d host-scoped, %d credential-scoped, %d platform, %d allowlisted",
		len(routes), host, credential, platform, allowed)

	// Each bucket has to be non-trivially populated. A refactor that accidentally
	// classified everything one way would otherwise satisfy every assertion above.
	for name, n := range map[string]int{"host-scoped": host, "credential-scoped": credential, "platform": platform} {
		if n < 3 {
			t.Errorf("only %d %s routes; that is not a plausible classification", n, name)
		}
	}
}

// TestCredentialScopedRoutesAuthenticateFirst: CredentialWorkspace reads the
// caller out of the request context, so the auth middleware has to have run.
// Registered the other way round it would resolve nothing and 500 — a 500 on
// every authenticated route, which is loud, but the compiler cannot see it and a
// reviewer easily can't either.
func TestCredentialScopedRoutesAuthenticateFirst(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	// The bearer-authenticated MCP mount is the one exception: its caller is bound
	// by MCPCallerMiddleware, not RequireAuth, and it is not a CredentialWorkspace
	// registration at all.
	var checked int
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "handler.CredentialWorkspace") {
			continue
		}
		checked++
		if !strings.Contains(line, "h.RequireAuth(") {
			t.Errorf("a CredentialWorkspace route does not authenticate first:\n    %s", strings.TrimSpace(line))
		}
		// And the order has to be RequireAuth OUTSIDE Scoped.
		if strings.Index(line, "h.RequireAuth(") > strings.Index(line, "h.Scoped(") {
			t.Errorf("RequireAuth must wrap Scoped, not the other way round:\n    %s", strings.TrimSpace(line))
		}
	}
	if checked < 90 {
		t.Fatalf("only %d CredentialWorkspace routes found; the scan has stopped working", checked)
	}
	t.Logf("checked %d credential-scoped routes", checked)
}

// TestPlatformRoutesAreTheIdentityHostSet pins the list against D11, which names
// exactly which surfaces belong to BASE_URL rather than to a workspace's public
// host. A route joining or leaving this set is a decision about which host serves
// it, so it should not be possible to make silently.
func TestPlatformRoutesAreTheIdentityHostSet(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	want := []string{
		// Ops (D11).
		"GET /healthz", "GET /readyz", "GET /version",
		// Prometheus exposition: an instance-level number, and its jobs read goes
		// through Platform() inside the handler (feat/platform-hooks + D11).
		"GET /metrics",
		// The signed session hand-off. No tenant Host and no credential yet — the
		// token IS the credential and the workspace is its `wid` claim, which
		// SSOHandoff resolves itself (D11).
		"GET /v1/auth/sso",
		// The platform API: provisioning is an instance-level operation on the identity
		// host, authorised by CALNODE_PLATFORM_TOKEN rather than by any tenant's
		// credential (D12).
		"POST /v1/platform/workspaces",
		"GET /v1/platform/workspaces/{id}",
		"PATCH /v1/platform/workspaces/{id}",
		"DELETE /v1/platform/workspaces/{id}",
		"POST /v1/platform/workspaces/{id}/export",
		"POST /v1/platform/workspaces/{id}/import",
		"DELETE /v1/platform/workspaces/{id}/attendees",
		// The member API (F1): the identity provider owns roles and mints each of its
		// people a managed key. Platform for the same reason — the tenant is in the URL,
		// authorised by CALNODE_PLATFORM_TOKEN rather than by any tenant's credential.
		"POST /v1/platform/workspaces/{id}/users",
		"POST /v1/platform/workspaces/{id}/users/{uid}/api-keys",
		"DELETE /v1/platform/workspaces/{id}/users/{uid}/api-keys/{keyId}",
		"POST /v1/platform/workspaces/{id}/users/{uid}/archive",
		"PATCH /v1/platform/workspaces/{id}/webhooks",
		// Bootstrap: creates the first user of a single-tenant instance.
		"POST /v1/setup",
		// OAuth login and its callbacks live on the identity host, because that is
		// the redirect URI registered with Google and Microsoft (D11). The
		// hand-off back to the workspace's public host is TODO(integration),
		// blocked on feat/platform-hooks.
		"GET /v1/auth/login", "GET /v1/auth/callback",
		"GET /v1/auth/microsoft/login", "GET /v1/auth/microsoft/callback",
		// The MCP authorization server (D11).
		"GET /.well-known/oauth-protected-resource",
		"GET /.well-known/oauth-authorization-server",
		"POST /oauth/register", "GET /oauth/authorize", "POST /oauth/authorize",
		"POST /oauth/token",
		// Calendar and Zoom connect callbacks: same registered-redirect-URI
		// reason. Storing the tokens under the state's workspace is B6.
		"GET /v1/calendar/callback", "GET /v1/zoom/callback",
		// Vendor webhooks. They arrive at whatever host the vendor was given and
		// carry no tenant Host, so they cannot be host-scoped; each has to resolve
		// its own workspace from the room or session it names. TODO(B6).
		"POST /v1/livekit/webhook", "POST /v1/livekit/egress-webhook",
		"POST /v1/stripe/webhook",
		// Vendored room assets: static bytes, identical for every tenant.
		"GET /assets/livekit-client.js", "GET /assets/livekit-room.js",
		// Demo mode, which is mutually exclusive with multi-tenant mode (D13).
		"GET /robots.txt", "GET /v1/demo/enter", "POST /v1/demo/reset",
	}

	var got []string
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "h.Platform((*H).") {
			continue
		}
		m := handleFuncRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("a Platform route is registered in a shape this test cannot read:\n    %s", strings.TrimSpace(line))
			continue
		}
		got = append(got, m[1])
	}

	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the platform route set has changed.\n got: %v\nwant: %v\n"+
			"A route moving onto or off the identity host is a decision about which host serves it; "+
			"update this list in the same commit and say why.", got, want)
	}
}
