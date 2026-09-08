package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/config"
)

// The global security headers (M9). The finding was not that one page was missing one
// header — it was that the headers existed only where a handler had remembered to write
// them, so /embed.js, /booking.css, every JSON error and every 404 carried none.
//
// These run against the REAL mux for that reason: a test on the middleware alone would
// prove the function works and say nothing about whether it is reached by the surfaces
// that were bare. The stub-based cases at the bottom cover the two things a mux cannot
// show — the set-if-absent rule, and a real TLS connection.

// securityHeaderPaths are one of each KIND of response, chosen because each was
// previously served by a different amount of header-setting code.
var securityHeaderPaths = []struct {
	name string
	path string
}{
	// A public, server-rendered surface. booking.css is the one the embed widget on a
	// customer's site loads, and it set nothing at all before this.
	{"public asset", "/booking.css"},
	// The JSON API: an unauthenticated /v1 route, which answers 401 and never went
	// near a handler that sets headers.
	{"v1 json", "/v1/event-types"},
	// A path nothing is registered at, so the response comes from ServeMux itself.
	{"not found", "/no-such-path-exists"},
}

func TestSecurityHeaders_onEverySurfaceInBothModes(t *testing.T) {
	// The headers do not vary by tenancy, and that is the assertion: multi-tenant is
	// where an unknown Host makes the mux answer 404 out of the resolver rather than
	// out of a handler, which is exactly the path a per-handler header would miss.
	for _, mode := range []struct {
		name string
		cfg  *config.Config
	}{
		{"single-tenant", &config.Config{
			BaseURL:       "https://cal.example.test",
			PublicBaseURL: "https://cal.example.test",
			AdminSPA:      true,
		}},
		{"multi-tenant", &config.Config{
			MultiTenant:   true,
			BaseURL:       "https://app.calnode.example",
			PublicBaseURL: "https://app.calnode.example",
			AdminSPA:      true,
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			mux := newAdminMux(t, mode.cfg)
			for _, c := range securityHeaderPaths {
				t.Run(c.name, func(t *testing.T) {
					rec := getAdminPath(t, mux, c.path)
					assertHeader(t, rec, "X-Content-Type-Options", "nosniff")
					assertHeader(t, rec, "Referrer-Policy", "strict-origin-when-cross-origin")
					assertHeader(t, rec, "Permissions-Policy", "camera=(), microphone=(), geolocation=()")
					// No TLS and no forwarded header on this request: HSTS must be absent,
					// not empty-valued.
					if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
						t.Errorf("GET %s over plain http carries HSTS %q; a self-hoster on a LAN would be pinned for a year", c.path, got)
					}
				})
			}
		})
	}
}

// HSTS is the one conditional header, and both directions matter: absent it is not sent
// on http (the lockout), present it is not sent at all behind a TLS-terminating proxy
// (the whole fleet, since the binary itself listens on plain http there).
func TestSecurityHeaders_HSTSFollowsTheRequestScheme(t *testing.T) {
	mux := newAdminMux(t, &config.Config{
		BaseURL:       "https://cal.example.test",
		PublicBaseURL: "https://cal.example.test",
		AdminSPA:      true,
	})

	cases := []struct {
		name      string
		forwarded string
		want      string
	}{
		{"no forwarded header", "", ""},
		{"forwarded https", "https", "max-age=31536000; includeSubDomains"},
		{"forwarded http", "http", ""},
		// Case is not significant in the header value, and a chain appends: the
		// LEFTMOST entry is the scheme the client used, the rest are hops behind
		// termination that would read as http.
		{"forwarded HTTPS uppercase", "HTTPS", "max-age=31536000; includeSubDomains"},
		{"forwarded chain, client on https", "https, http", "max-age=31536000; includeSubDomains"},
		{"forwarded chain, client on http", "http, https", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/booking.css", nil)
			if c.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", c.forwarded)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if got := rec.Header().Get("Strict-Transport-Security"); got != c.want {
				t.Errorf("HSTS = %q; want %q", got, c.want)
			}
		})
	}
}

// A real TLS connection needs no forwarded header. httptest.NewRequest leaves r.TLS nil,
// so this is the one case the mux table above cannot express.
func TestSecurityHeaders_HSTSOnADirectTLSConnection(t *testing.T) {
	rec := serveThroughSecurityHeaders(t, "/booking.css", func(r *http.Request) {
		r.TLS = &tls.ConnectionState{}
	}, nil)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Errorf("HSTS on a direct TLS request = %q; want it set", got)
	}
}

// The LiveKit room is a video meeting: the blanket camera=()/microphone=() denial would
// break it, and a Permissions-Policy refusal cannot be recovered from in JavaScript —
// getUserMedia just rejects. Asserted through the mux, because what picks the value is
// the request PATH rather than anything the room handler does (it 404s here, LiveKit
// being unconfigured, and the header is correct anyway — which is the point).
func TestSecurityHeaders_theRoomPageKeepsCameraAndMicrophone(t *testing.T) {
	mux := newAdminMux(t, &config.Config{
		BaseURL:       "https://cal.example.test",
		PublicBaseURL: "https://cal.example.test",
		AdminSPA:      true,
	})

	rec := getAdminPath(t, mux, "/room/some-room-token")
	assertHeader(t, rec, "Permissions-Policy", "camera=(self), microphone=(self), geolocation=()")

	// And the exception is scoped to that prefix, not to anything room-ish.
	rec = getAdminPath(t, mux, "/booking.css")
	assertHeader(t, rec, "Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// Set-if-absent, proved with a handler that has its own opinion. The live example is the
// LiveKit room, which sets `Referrer-Policy: no-referrer` and must keep it: the room URL
// carries the opaque room token in its query string, so leaking even the origin-plus-path
// to an outbound link is worse there than on a booking page.
func TestSecurityHeaders_aHandlerKeepsItsOwnValue(t *testing.T) {
	rec := serveThroughSecurityHeaders(t, "/room/abc", nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(http.StatusOK)
	})

	assertHeader(t, rec, "Referrer-Policy", "no-referrer")
	// The headers it did NOT set still arrive.
	assertHeader(t, rec, "X-Content-Type-Options", "nosniff")
	assertHeader(t, rec, "Permissions-Policy", "camera=(self), microphone=(self), geolocation=()")
}

// serveThroughSecurityHeaders drives the middleware alone, for the two cases the mux
// cannot produce: a request whose TLS field is set, and a handler that writes a header
// this middleware also writes.
func serveThroughSecurityHeaders(t *testing.T, path string, tweak func(*http.Request), inner http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	if inner == nil {
		inner = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if tweak != nil {
		tweak(req)
	}
	rec := httptest.NewRecorder()
	SecurityHeaders(inner).ServeHTTP(rec, req)
	return rec
}

func assertHeader(t *testing.T, rec *httptest.ResponseRecorder, key, want string) {
	t.Helper()
	if got := rec.Header().Get(key); got != want {
		t.Errorf("%s = %q; want %q", key, got, want)
	}
}
