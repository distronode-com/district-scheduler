package handler

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/i18n"
)

// The Distronode palette, as declared by the hosted pages. Values come from
// distronode-website/src/app/globals.css (its HSL tokens converted to hex).
var districtPalette = map[string]string{
	"--bk-fg":            "#14141a",
	"--bk-primary":       "#3f36e2",
	"--bk-primary-hover": "#2c24cc",
	"--bk-muted":         "#60606c",
	"--bk-border":        "#e0e0e6",
	"--bk-surface":       "#f3f3f7",
	"--bk-page-bg":       "#f9f9fb",
}

// The neutral defaults booking.css ships, which the embed widget keeps.
var neutralDefaults = map[string]string{
	"--bk-fg":      "#111827",
	"--bk-primary": "#111827",
	"--bk-border":  "#e5e7eb",
	"--bk-surface": "#f3f4f6",
}

// TestBookingCSSDeclaresTokensOnRootAndHost pins the one thing that makes the
// shared sheet safe in a Shadow DOM.
//
// ⛔ `:host` IS NOT REDUNDANT. The embed widget injects booking.css into its shadow
// root, where `:root` matches NOTHING — it selects the document root element, which
// is outside the shadow tree. Declared on `:root` alone, every var() in the sheet
// resolves to nothing inside the widget: the pages would look correct and the widget
// would render unstyled, on customers' sites, with no error anywhere. The failure is
// invisible to every test that only renders the pages, which is why it is pinned here.
func TestBookingCSSDeclaresTokensOnRootAndHost(t *testing.T) {
	css := string(bookingCSS)
	if !strings.Contains(css, ":root, :host {") {
		t.Fatal("booking.css must declare its tokens on `:root, :host` — `:root` alone leaves the Shadow-DOM widget unstyled")
	}
	for tok, want := range neutralDefaults {
		if !strings.Contains(css, tok+": "+want) {
			t.Errorf("booking.css must default %s to the neutral %s (the widget keeps these)", tok, want)
		}
	}
}

// TestBookingCSSHoldsNoHardcodedPaletteColour keeps the sheet themeable.
//
// Every colour must be a token, or a page theme cannot reach it: a hardcoded hex
// deeper in the sheet silently opts that one rule out of theming and shows up as a
// single stubbornly grey control on an otherwise branded page. The token block itself
// is exempt, since that is where the defaults live.
func TestBookingCSSHoldsNoHardcodedPaletteColour(t *testing.T) {
	css := string(bookingCSS)
	start := strings.Index(css, ":root, :host {")
	if start < 0 {
		t.Fatal("token block not found")
	}
	end := strings.Index(css[start:], "\n}")
	if end < 0 {
		t.Fatal("token block not terminated")
	}
	body := css[:start] + css[start+end:]

	// The palette greys and the ink/indigo pair. State colours (success/amber/sky
	// accents on the assistant pill) are deliberately not tokenised yet.
	palette := regexp.MustCompile(`(?i)#(111827|1f2937|374151|4b5563|6b7280|9ca3af|e5e7eb|d1d5db|f3f4f6|f9fafb|dc2626)\b`)
	if hits := palette.FindAllString(body, -1); len(hits) > 0 {
		t.Errorf("booking.css has %d hardcoded palette colours outside the token block (%v) — a page theme cannot override these", len(hits), hits)
	}
}

// TestHostedPagesCarryDistrictThemeAndWidgetDoesNot is the scoping contract.
//
// The hosted booking and manage pages wear the Distronode palette; the shared sheet
// the customer-embedded widget loads must NOT, because that widget sits inside the
// customer's own design. Asserted in BOTH directions on purpose: presence alone would
// still pass if someone "simplified" the theme by moving it into booking.css, which is
// exactly the change that would repaint every customer's widget indigo.
func TestHostedPagesCarryDistrictThemeAndWidgetDoesNot(t *testing.T) {
	var bookBuf, manageBuf bytes.Buffer
	if err := bookTmpl.Execute(&bookBuf, bookPageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("book render: %v", err)
	}
	if err := manageTmpl.Execute(&manageBuf, managePageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("manage render: %v", err)
	}

	for name, page := range map[string]string{"book.html": bookBuf.String(), "manage.html": manageBuf.String()} {
		for tok, want := range districtPalette {
			if !strings.Contains(page, tok+": "+want) {
				t.Errorf("%s: missing Distronode token %s: %s", name, tok, want)
			}
		}
		// The ink/indigo split is the substance of the theme: the neutral sheet uses
		// one ink for both text and buttons, the site does not.
		if strings.Contains(page, "--bk-primary: #14141a") {
			t.Errorf("%s: primary must be the site indigo, not the text ink", name)
		}
	}

	css := string(bookingCSS)
	for tok, val := range districtPalette {
		if strings.Contains(css, tok+": "+val) {
			t.Errorf("booking.css carries the Distronode value for %s — the embedded widget would inherit our brand onto a customer's site", tok)
		}
	}
	if strings.Contains(css, "--bk-page-bg") {
		t.Error("booking.css must not define --bk-page-bg; it is page chrome, not a widget primitive")
	}
}
