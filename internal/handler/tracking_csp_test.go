package handler

import (
	"strings"
	"testing"
)

// The strict default is pinned as a LITERAL, not compared to its own constant: every
// other test here checks publicCSP against strictPublicCSP, so an edit to the constant
// moved both sides at once and nothing said what the policy actually was. M9 shipped
// through that gap — `script-src 'unsafe-inline'` with no `'self'` refused every
// same-origin script, which is why Cloudflare's injected jsd loader was a console error
// on every booking page, and the whole suite stayed green.
func TestStrictPublicCSP_isTheStringWeThinkItIs(t *testing.T) {
	const want = "default-src 'self'; script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; " +
		"connect-src 'self'; frame-ancestors 'none'"
	if strictPublicCSP != want {
		t.Errorf("strictPublicCSP changed.\n got: %q\nwant: %q\n"+
			"Every directive here is a decision; update this literal in the same commit and say why.",
			strictPublicCSP, want)
	}
}

// ⛔ Same-origin scripts must be ALLOWED. Asserted on its own, in the terms of the bug,
// because the literal above would still pass if someone "tidied" `'self' 'unsafe-inline'`
// into a shape that dropped one of them and updated the literal to match.
func TestStrictPublicCSP_allowsSameOriginScripts(t *testing.T) {
	if !strings.Contains(strictPublicCSP, "script-src 'self'") {
		t.Errorf("the strict policy must allow same-origin scripts; got %q", strictPublicCSP)
	}
	// And the relaxed policy, which always had it, must not lose it either — the two
	// disagreeing is what M9 was.
	if got := publicCSP(trackingSettings{HeadHTML: "<script>gtm</script>"}); !strings.Contains(got, "script-src 'self'") {
		t.Errorf("the relaxed policy must allow same-origin scripts; got %q", got)
	}
}

func TestPublicCSP_strictWhenNoInjection(t *testing.T) {
	if got := publicCSP(trackingSettings{}); got != strictPublicCSP {
		t.Errorf("no code injection must keep the strict CSP; got %q", got)
	}
	// dataLayer alone (inline) also needs nothing external.
	if got := publicCSP(trackingSettings{DataLayerEnabled: true}); got != strictPublicCSP {
		t.Errorf("dataLayer-only must keep the strict CSP; got %q", got)
	}
}

func TestPublicCSP_broadHttpsWhenHeadSet(t *testing.T) {
	got := publicCSP(trackingSettings{HeadHTML: "<script>gtm</script>"})
	if !strings.Contains(got, "script-src 'self' 'unsafe-inline' https:") {
		t.Errorf("expected broad https script-src; got %q", got)
	}
	if !strings.Contains(got, "connect-src 'self' https:") {
		t.Errorf("expected broad https connect-src; got %q", got)
	}
	if !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("must still forbid framing; got %q", got)
	}
}

func TestPublicCSP_allowlistTightens(t *testing.T) {
	got := publicCSP(trackingSettings{
		HeadHTML: "<script>gtm</script>",
		CSPAllow: "https://www.googletagmanager.com https://*.google-analytics.com",
	})
	if !strings.Contains(got, "https://www.googletagmanager.com") {
		t.Errorf("allowlisted origin missing; got %q", got)
	}
	if strings.Contains(got, "'unsafe-inline' https:;") {
		t.Errorf("broad https: should be replaced by the allowlist; got %q", got)
	}
}
