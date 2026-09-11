package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// fuzzReturnToOrigins is the allowlist every case below is decided against: one ordinary
// https console and one http loopback, which is the shape config.Validate explicitly
// permits (TestValidate_platformReturnOriginsAcceptsLoopbackHTTPAndPorts) and the one an
// http-blind comparison would get wrong.
var fuzzReturnToOrigins = []string{
	"https://console.example.test",
	"http://127.0.0.1:5173",
}

// FuzzReturnToFromRequest holds the ?return_to= allowlist to its contract for any query
// value at all.
//
// This list is the only thing between the calendar OAuth callback and an open redirect,
// and the comparison it makes is whole-origin equality — a rule that is easy to state and
// easy to lose, because every plausible weakening (prefix, suffix, scheme-blind, host-only)
// still passes the honest cases. TestConnectCalendar_returnTo_originMustMatchWhole lists
// fourteen hosts that a weakened comparison would admit; this asks the same question of
// inputs nobody wrote down.
//
// The harness is the smallest one that exercises the real method: returnToFromRequest reads
// h.platformReturnOrigins and r.URL.Query() and nothing else, so no database, no provider
// and no recorder are involved. It is in package handler because the method is unexported.
func FuzzReturnToFromRequest(f *testing.F) {
	seeds := []string{
		// Accepted by the existing tests, against the console origin.
		"https://console.example.test",
		"https://console.example.test/",
		"https://console.example.test/settings/calendar",
		"https://console.example.test/settings?x=1#frag",
		"https://console.example.test/settings/calendar?tab=connections",
		"http://127.0.0.1:5173/settings",

		// Refused by the existing tests. Every one is a host some weaker comparison admits.
		"",
		"https://console.example.test:8443/x",
		"http://console.example.test/x",
		"https://eu.console.example.test/x",
		"https://example.test/x",
		"https://console.example.evil/x",
		"https://console.example.test.evil.test/x",
		"https://evil.test/https://console.example.test",
		"https://console.example.test@evil.test/x",
		"https://someone@console.example.test/x",
		"/settings/calendar",
		"//console.example.test/x",
		`/\console.example.test`,
		"::::",
		"javascript:alert(1)",
		"https://console.example.test/x" + stateSep + "extra",

		// The shapes the packet names, plus the two the parser is most likely to disagree
		// with a browser about.
		"https://console.example.test/x?y=1",
		"https://console.example.test@evil.example/",
		"https://console.example.test.evil.example/",
		"https://CONSOLE.example.test/",
		"HTTPS://console.example.test/",
		"//console.example.test/",
		"https://console.example.test:443/",
		"https://console.example.te%73t/",
		stateSep,
		"https://console.example.test/" + stateSep,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	// Handler embeds *shared, so the zero value is a nil dereference waiting to happen;
	// an empty shared is all this method needs and is what keeps the harness free of a
	// database. Everything else on it stays zero on purpose: a field this method starts
	// reading should fail loudly here rather than be quietly satisfied.
	h := &Handler{shared: &shared{}}
	h.SetPlatformReturnOrigins(fuzzReturnToOrigins)

	// The same allowlist reversed, and an empty one. Neither is decoration: a comparison
	// that fell out of its loop early would answer differently for the two orders, and
	// "off means refused, not ignored" is a rule with no natural enforcement anywhere else.
	reversed := &Handler{shared: &shared{}}
	reversed.SetPlatformReturnOrigins([]string{fuzzReturnToOrigins[1], fuzzReturnToOrigins[0]})
	off := &Handler{shared: &shared{}}

	f.Fuzz(func(t *testing.T, raw string) {
		got, err := h.returnToFromRequest(returnToRequest(raw))

		if raw == "" {
			// Absent is not refused: the parameter is optional and its absence is the
			// ordinary case for every instance that has no platform in front of it.
			if err != nil || got != "" {
				t.Fatalf("returnToFromRequest(absent) = %q, %v; want \"\", nil", got, err)
			}
			return
		}

		if err != nil {
			if got != "" {
				t.Fatalf("returnToFromRequest(%q) refused with %v but returned %q", raw, err, got)
			}
			return
		}
		if got == "" {
			t.Fatalf("returnToFromRequest(%q) returned \"\" with no error for a present value", raw)
		}

		// Accepted. It is the caller's own string, unchanged — a value this function
		// sanitised would be a value the allowlist was not asked about.
		if got != raw {
			t.Fatalf("returnToFromRequest(%q) = %q; an accepted return_to is returned verbatim", raw, got)
		}

		u, perr := url.Parse(got)
		switch {
		case perr != nil:
			t.Fatalf("returnToFromRequest(%q) accepted a value url.Parse rejects: %v", raw, perr)
		case !u.IsAbs():
			t.Fatalf("returnToFromRequest(%q) accepted a relative URL", raw)
		case u.Host == "":
			t.Fatalf("returnToFromRequest(%q) accepted a URL with no host", raw)
		case u.User != nil:
			t.Fatalf("returnToFromRequest(%q) accepted a URL carrying userinfo %q", raw, u.User)
		}

		origin := u.Scheme + "://" + u.Host
		if origin != fuzzReturnToOrigins[0] && origin != fuzzReturnToOrigins[1] {
			t.Fatalf("returnToFromRequest(%q) accepted origin %q, which is not on the allowlist", raw, origin)
		}

		// Two invariants url.Parse does not carry, and this target is why they are stated
		// separately. The separator would smuggle a fourth field into a two-separator state
		// parse. The control-character scan covers the WHOLE value including the fragment,
		// which url.Parse never examines — the first 30-second run of this target found
		// "https://console.example.test#\x00" accepted in 22s, and the input is kept at
		// testdata/fuzz/FuzzReturnToFromRequest/7ade663e1437fd4c.
		if strings.Contains(got, stateSep) {
			t.Fatalf("returnToFromRequest(%q) accepted a value carrying the state separator", raw)
		}
		for i, c := range got {
			if c < 0x20 || c == 0x7f {
				t.Fatalf("returnToFromRequest(%q) accepted control character %#U at %d", raw, c, i)
			}
		}

		// Order must not matter, and an unconfigured instance must refuse what a
		// configured one accepts.
		if other, oerr := reversed.returnToFromRequest(returnToRequest(raw)); oerr != nil || other != got {
			t.Fatalf("allowlist order changed the answer for %q: %q, %v", raw, other, oerr)
		}
		if _, oerr := off.returnToFromRequest(returnToRequest(raw)); oerr == nil {
			t.Fatalf("an empty allowlist accepted %q; with the feature off a return_to is refused", raw)
		}
	})
}

// returnToRequest is the connect URL carrying raw as ?return_to=. Built by hand rather than
// with httptest.NewRequest because the method reads only the URL, and because
// NewRequest panics on inputs a fuzzer will certainly reach.
func returnToRequest(raw string) *http.Request {
	return &http.Request{
		Method: http.MethodGet,
		URL: &url.URL{
			Path:     "/v1/calendar/connect",
			RawQuery: url.Values{"return_to": {raw}}.Encode(),
		},
	}
}
