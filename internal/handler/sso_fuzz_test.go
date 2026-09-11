package handler

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzSSONextPath holds the ?next= validator to its contract for any input at all.
//
// ssoNextPath is one of the three places in this tree where an attacker-supplied string
// becomes a Location header, and the refusal is written as a list of separate `case`
// clauses. A list is exactly the shape that grows a hole: every clause is right on its own,
// and the question is whether they are right TOGETHER. A table test can only ask that of
// the rows someone thought to write.
//
// The property is the one the header needs rather than the one the code checks: whatever
// comes back must be a same-origin absolute path, stated twice — once as the byte-level
// rules the function enforces, and once as "url.Parse sees no scheme and no host", which is
// how a browser decides where a redirect goes.
//
// In package handler rather than handler_test because ssoNextPath is unexported. It touches
// no Handler and no database, so there is no harness here.
func FuzzSSONextPath(f *testing.F) {
	// The cases the existing tests reach through the HTTP surface
	// (TestSSOHandoff_nextMustBeSameOriginPath, …nextHonoursALocalPath,
	// …withoutAdminSPAAnExplicitNextIsUnchanged, …AnExplicitAdminNextIs404).
	seeds := []string{
		"",
		"https://evil.example.test/",
		"//evil.example.test/",
		`/\evil.example.test`,
		"/redirect?to=https://evil.example.test",
		"admin/",
		"/admin/\r\nX-Injected: 1",
		"/admin/bookings",
		"/admin/",
		"/admin",
		"/admin/settings/video",
		"/admin/?tab=hosts",
		"/admin/settings/../bookings",
		"/admin/../book/intro",
		"/book/intro",
		"/v1/calendar/connect?provider=google",
		"/v1/calendar/connect?provider=google&return_to=https%3A%2F%2Fconsole.example.test",

		// The shapes a validator of this kind is usually broken by.
		"/",
		"/admin/",
		"//evil",
		`/\evil`,
		"/a://b",
		"/a\r\nb",
		"/%2F%2Fevil",
		"/..//evil",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, next string) {
		got, err := ssoNextPath(next)

		if next == "" {
			// The documented default, and the only input that may produce a value the
			// caller did not supply.
			if err != nil || got != ssoDefaultNext {
				t.Fatalf("ssoNextPath(%q) = %q, %v; want %q, nil", next, got, err, ssoDefaultNext)
			}
			return
		}

		if err != nil {
			// A refusal is always allowed, but it must not also hand back a value: the
			// caller checks err, and a non-empty string beside a non-nil error is how a
			// later refactor starts using the refused value.
			if got != "" {
				t.Fatalf("ssoNextPath(%q) refused with %v but returned %q", next, err, got)
			}
			return
		}

		// Accepted. Everything below is what the Location header is allowed to carry.
		switch {
		case !strings.HasPrefix(got, "/"):
			t.Fatalf("ssoNextPath(%q) = %q, which is not an absolute path", next, got)
		case strings.HasPrefix(got, "//"):
			t.Fatalf("ssoNextPath(%q) = %q, which is protocol-relative", next, got)
		case strings.Contains(got, `\`):
			t.Fatalf("ssoNextPath(%q) = %q, which contains a backslash", next, got)
		case strings.Contains(got, "://"):
			t.Fatalf("ssoNextPath(%q) = %q, which contains a scheme", next, got)
		}
		for _, c := range got {
			if c < 0x20 || c == 0x7f {
				t.Fatalf("ssoNextPath(%q) = %q, which contains control character %#U", next, got, c)
			}
		}

		// The same-origin rule stated the way a browser reads it. A parse error is not a
		// violation: a string url.Parse refuses has no scheme and no host by construction,
		// and as a Location it is resolved relative to this origin — which is the outcome
		// the checks above are for. What would be a violation is a value that parses into
		// somewhere else.
		if u, perr := url.Parse(got); perr == nil {
			if u.Scheme != "" || u.Host != "" {
				t.Fatalf("ssoNextPath(%q) = %q, which parses to scheme %q host %q — another origin",
					next, got, u.Scheme, u.Host)
			}
		}
	})
}
