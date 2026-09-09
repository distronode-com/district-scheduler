package handler

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/i18n"
)

// TestBookingSurfacesShareStructuralHooks pins the structural contract across the
// THREE booking surfaces — book.html and manage.html (Go templates) and embed.js
// (a vanilla-JS web component). All three load the shared booking.css and implement
// the same calendar/slot-picker, but their markup is authored separately (Go
// template vs JS DOM-building), so they drift — the exact hazard CLAUDE.md warns
// about. Go template partials can't reach the JS embed, so this is the cross-language
// safety net: change the calendar/slots structure on one surface and forget another,
// and CI fails here.
//
// The pages are rendered WITHOUT the shared booking-logic.js inlined (BookingLogicJS
// left empty) on purpose: each hook must be present in the surface's OWN markup/script,
// otherwise the shared module would mask per-surface drift.
//
// Hooks that legitimately differ are deliberately excluded: the mobile step-flow uses
// .cal-back buttons in the templates vs .step-cal/.step-right card classes in the
// embed, and month nav uses #prev-btn/#next-btn in the templates vs the embed's own.
func TestBookingSurfacesShareStructuralHooks(t *testing.T) {
	var bookBuf, manageBuf bytes.Buffer
	if err := bookTmpl.Execute(&bookBuf, bookPageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("book render: %v", err)
	}
	// Zero value → TokenInvalid false + Status "" (not "cancelled"), so the reschedule
	// calendar branch renders.
	if err := manageTmpl.Execute(&manageBuf, managePageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("manage render: %v", err)
	}

	surfaces := map[string]string{
		"book.html":   bookBuf.String(),
		"manage.html": manageBuf.String(),
		"embed.js":    string(embedJS),
	}

	// Shared calendar/slots hooks every surface must expose — booking.css styles these
	// and the pickers depend on them. Verified present in all three when authored.
	hooks := []string{
		"cal-nav",      // month-navigation row
		"cal-grid",     // the day grid
		"month-label",  // current-month label
		"cal-col",      // calendar column
		"right-col",    // slots/form column
		"slots-list",   // slot-button container
		"slots-header", // selected-day header
		"slot-btn",     // a time-slot button
	}

	for _, h := range hooks {
		for name, src := range surfaces {
			if !strings.Contains(src, h) {
				t.Errorf("structural hook %q missing from %s — the booking surfaces have drifted; "+
					"add it to all three (book.html, manage.html, embed.js) or adjust this contract", h, name)
			}
		}
	}
}

// ── F3b: the three accessibility fixes, pinned on all three surfaces ──────────────
//
// A browser audit of the live booking pages found three axe violations. Each fix is
// markup or a token value, which means each can be silently undone by an edit that
// looks unrelated — so each is asserted here, where the three-surface rule already
// lives, rather than in three separate places.

// TestBookingCalendarIsAGroupNotAGrid. The calendar carried role="grid" on the two Go
// templates (via the shared "calendarGrid" partial) and no role at all on the widget.
//
// ⛔ What the renderers emit is a FLAT list — seven .ch header cells, blank spacers for
// the month's start offset, then one <button class="cd"> per day — arranged into weeks
// by grid-template-columns and nothing else. `grid` requires role="row" children
// holding gridcells, so a screen reader was told to expect rows and found none and
// announced an empty grid (axe: aria-required-children, critical). It also implies an
// arrow-key roving-tabindex contract that no surface implements.
//
// The fix is role="group" with the visible month as the accessible name, which is what
// the markup honestly is. This test holds BOTH halves: the group must be there, and
// `grid` must not come back.
func TestBookingCalendarIsAGroupNotAGrid(t *testing.T) {
	var bookBuf, manageBuf bytes.Buffer
	if err := bookTmpl.Execute(&bookBuf, bookPageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("book render: %v", err)
	}
	if err := manageTmpl.Execute(&manageBuf, managePageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("manage render: %v", err)
	}

	pages := map[string]string{"book.html": bookBuf.String(), "manage.html": manageBuf.String()}
	for name, src := range pages {
		if !strings.Contains(src, `role="group" aria-labelledby="month-label"`) {
			t.Errorf("%s: the calendar is not a group named by the month label — expected "+
				`role="group" aria-labelledby="month-label" on #cal (templates/_shared.html)`, name)
		}
		// The name has to resolve to something live, or the group is unnamed.
		if !strings.Contains(src, `id="month-label" class="month-label" aria-live="polite"`) {
			t.Errorf("%s: #month-label lost its aria-live, so the group's name no longer "+
				"announces a month change", name)
		}
	}

	// The widget builds its DOM in JS, so the same two facts read differently: a group
	// role, and a month string as its label.
	js := string(embedJS)
	if !strings.Contains(js, `role: 'group'`) || !strings.Contains(js, `'aria-label': monthLabel`) {
		t.Error("embed.js: the calendar grid is not a group labelled by the visible month; " +
			"the widget must match the pages (see calPane)")
	}
	if !strings.Contains(js, `'aria-live': 'polite'`) {
		t.Error("embed.js: the month label is not a live region, so a month change is silent " +
			"where the pages announce it")
	}

	// ⛔ Both directions. Asserting only the group would let role="grid" be re-added
	// alongside it, and the violation is the presence of `grid`, not the absence of
	// `group`.
	for name, src := range map[string]string{"book.html": bookBuf.String(), "manage.html": manageBuf.String(), "embed.js": js} {
		for _, bad := range []string{`role="grid"`, `role: 'grid'`, `role="row"`, `role="gridcell"`} {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains %s. The day buttons are a flat list with no row in the DOM, "+
					"so a grid role is a claim the markup does not honour. If real rows were added, "+
					"update this test in the same commit and say how the arrow-key contract is met.", name, bad)
			}
		}
	}
}

// TestBookingPagesHaveExactlyOneMain — axe: landmark-one-main (moderate). Neither page
// had a <main> at all.
//
// ⚠️ The count matters as much as the presence. manage.html renders one of two cards
// from the arms of a single {{if .TokenInvalid}}, and both are now <main class="card">;
// making them siblings instead of branches would produce two mains and trade one
// violation for another. Both arms are rendered here and each is counted.
//
// ⛔ And the embed must have NONE: it renders into a customer's own document, which has
// its own <main>. A second one there would be this same violation, on someone else's
// page, where nobody looking at this repo would find it.
func TestBookingPagesHaveExactlyOneMain(t *testing.T) {
	renders := map[string]func() (string, error){
		"book.html": func() (string, error) {
			var b bytes.Buffer
			err := bookTmpl.Execute(&b, bookPageData{T: i18n.Default().T})
			return b.String(), err
		},
		"manage.html (valid token)": func() (string, error) {
			var b bytes.Buffer
			err := manageTmpl.Execute(&b, managePageData{T: i18n.Default().T})
			return b.String(), err
		},
		"manage.html (expired token)": func() (string, error) {
			var b bytes.Buffer
			err := manageTmpl.Execute(&b, managePageData{T: i18n.Default().T, TokenInvalid: true})
			return b.String(), err
		},
		"manage.html (cancelled booking)": func() (string, error) {
			var b bytes.Buffer
			err := manageTmpl.Execute(&b, managePageData{T: i18n.Default().T, Status: "cancelled"})
			return b.String(), err
		},
	}

	for name, render := range renders {
		src, err := render()
		if err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
		if n := strings.Count(src, "<main"); n != 1 {
			t.Errorf("%s has %d <main> elements; want exactly 1 (axe: landmark-one-main)", name, n)
		}
		if n := strings.Count(src, "</main>"); n != 1 {
			t.Errorf("%s has %d </main> closing tags; want exactly 1", name, n)
		}
	}

	if strings.Contains(string(embedJS), "'main'") || strings.Contains(string(embedJS), `"main"`) {
		t.Error("embed.js appears to create a <main>. It renders inside a host page that has " +
			"its own, so a second one is landmark-one-main on the customer's document — see the " +
			"comment on calPane's returned <section>.")
	}
}

// relativeLuminance is WCAG 2.x's, so the ratios below are computed from the shipped hex
// rather than trusted from a comment.
func relativeLuminance(hexColor string) (float64, error) {
	h := strings.TrimPrefix(strings.TrimSpace(hexColor), "#")
	// ⚠️ The shipped palettes mix both forms — booking.css writes --bk-on-primary as
	// #fff while the pages write #ffffff. A 6-digit-only parser rejected the shared
	// sheet and reported "the palette moved", which reads like a real drift.
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return 0, fmt.Errorf("want a 3- or 6-digit hex colour, got %q", hexColor)
	}
	var out float64
	for i, weight := range []float64{0.2126, 0.7152, 0.0722} {
		v, err := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
		if err != nil {
			return 0, fmt.Errorf("parse %q: %w", hexColor, err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		out += weight * c
	}
	return out, nil
}

func contrastRatio(t *testing.T, fg, bg string) float64 {
	t.Helper()
	lf, err := relativeLuminance(fg)
	if err != nil {
		t.Fatalf("%v", err)
	}
	lb, err := relativeLuminance(bg)
	if err != nil {
		t.Fatalf("%v", err)
	}
	hi, lo := math.Max(lf, lb), math.Min(lf, lb)
	return (hi + 0.05) / (lo + 0.05)
}

// tokenValue pulls one `--bk-x: #rrggbb;` declaration out of a stylesheet or a page's
// <style> block. Reading the shipped bytes is the point: a test holding its own copy of
// the palette would keep passing after the palette changed.
func tokenValue(t *testing.T, src, token string) string {
	t.Helper()
	m := regexp.MustCompile(`--` + token + `:\s*(#[0-9a-fA-F]{6}|#[0-9a-fA-F]{3})\s*;`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find --%s in the source; the palette moved and this test is now blind", token)
	}
	return m[1]
}

// TestBookingSubtleTextMeetsContrast — axe: colour contrast (serious).
//
// --bk-subtle is the text colour of .ch (the day-of-week headers), .tz-label, .hint, the
// timezone <select>, and manage's .scheduled-label / .row-label: real 11px copy on the
// card's #fff, not decoration. It shipped at #9ca3af on the widget's neutral default
// palette (2.54:1) and #8b8b9c on the pages' Distronode palette (3.35:1), both well under
// the 4.5:1 that normal-size text needs.
//
// ⚠️ The primary button's hover fill was the state the packet brief named, and it does
// not reproduce on either palette: #ffffff on --bk-primary-hover measures 14.68:1
// (widget, #1f2937) and 9.41:1 (pages, #2c24cc). It is asserted below so that stays true.
//
// Not asserted, and deliberately: .cd:disabled and .slot-btn.taken are disabled controls,
// which WCAG 1.4.3 exempts, and both must keep reading as unavailable.
func TestBookingSubtleTextMeetsContrast(t *testing.T) {
	var bookBuf, manageBuf bytes.Buffer
	if err := bookTmpl.Execute(&bookBuf, bookPageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("book render: %v", err)
	}
	if err := manageTmpl.Execute(&manageBuf, managePageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("manage render: %v", err)
	}

	palettes := map[string]string{
		"booking.css defaults (the embed widget)": string(bookingCSS),
		"book.html (Distronode palette)":          bookBuf.String(),
		"manage.html (Distronode palette)":        manageBuf.String(),
	}

	const (
		card    = "#ffffff" // .card's background, where every --bk-subtle string sits
		textMin = 4.5
	)

	for name, src := range palettes {
		subtle := tokenValue(t, src, "bk-subtle")
		if r := contrastRatio(t, subtle, card); r < textMin {
			t.Errorf("%s: --bk-subtle %s on the card's %s is %.2f:1, want >= %.1f. "+
				"It is the colour of .ch, .tz-label and .hint — 11px text, not decoration.",
				name, subtle, card, r, textMin)
		}
		// The subtlest tier must not overtake the tier above it, or the hierarchy the
		// palette encodes has quietly inverted.
		muted := tokenValue(t, src, "bk-muted")
		if contrastRatio(t, subtle, card) > contrastRatio(t, muted, card) {
			t.Errorf("%s: --bk-subtle (%s) is now darker than --bk-muted (%s); "+
				"raising subtle for contrast must not invert the two", name, subtle, muted)
		}
		// The button hover the brief named. Kept green rather than assumed green.
		hover, onPrimary := tokenValue(t, src, "bk-primary-hover"), tokenValue(t, src, "bk-on-primary")
		if r := contrastRatio(t, onPrimary, hover); r < textMin {
			t.Errorf("%s: .btn-primary:hover is %s on %s = %.2f:1, want >= %.1f",
				name, onPrimary, hover, r, textMin)
		}
	}

	// ⛔ The widget's own "powered by" line held the #9ca3af literal rather than the
	// token, so correcting the shared sheet alone would have left exactly one string on
	// one surface still failing.
	if !strings.Contains(string(embedJS), ".powered{text-align:center;font-size:.6875rem;color:var(--bk-subtle);") {
		t.Error("embed.js .powered no longer takes its colour from --bk-subtle; a literal " +
			"there is a contrast failure that the shared stylesheet cannot fix")
	}
}

// TestBookingSurfacesExplainEmptyDaysAndMinNotice is the same safety net for the strings
// and payload field that answer "why can't I see those times" (#20). All three surfaces
// have to name the day on an empty one, name the host when there is exactly one, and
// surface the minimum-notice policy when that is what removed the nearest starts — and
// each does it in its own separately-authored code, so forgetting one is silent.
func TestBookingSurfacesExplainEmptyDaysAndMinNotice(t *testing.T) {
	var bookBuf, manageBuf bytes.Buffer
	if err := bookTmpl.Execute(&bookBuf, bookPageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("book render: %v", err)
	}
	if err := manageTmpl.Execute(&manageBuf, managePageData{T: i18n.Default().T}); err != nil {
		t.Fatalf("manage render: %v", err)
	}

	surfaces := map[string]string{
		"book.html":   bookBuf.String(),
		"manage.html": manageBuf.String(),
		"embed.js":    string(embedJS),
	}

	required := []string{
		"no_available_times",      // the empty-day message, which now names the date
		"no_available_times_host", // its "<host> has no available times on <date>" form
		"min_notice_hint",         // the minimum-notice explanation
		"min_notice",              // the GET /slots field saying which days it applied to
	}
	for _, key := range required {
		for name, src := range surfaces {
			if !strings.Contains(src, key) {
				t.Errorf("%q missing from %s — an empty or thinned day there will not explain "+
					"itself; add it to all three surfaces (#20)", key, name)
			}
		}
	}

	// Every locale must actually carry the keys the surfaces look up. i18n's own key-parity
	// test compares locales against English; this checks English has them at all, so a
	// renamed key can't leave three surfaces rendering their own key names at visitors.
	en := i18n.Default()
	for _, key := range []string{"no_available_times", "no_available_times_host", "min_notice_hint"} {
		if en.T(key) == key {
			t.Errorf("locale key %q is missing from en.json — Locale.T falls back to the key "+
				"itself, so the booking page would show %q to a visitor", key, key)
		}
	}
}

// TestEmbedJSDoesNotDependOnBookingLogic pins the trap that the shared module's own header
// used to get wrong: embed.js is served as standalone bytes (EmbedJS writes the embedded
// file unmodified), so `BookingLogic` does not exist inside the widget. It carries its own
// copies of the few helpers it needs. A well-meant de-duplication onto BookingLogic would
// throw a ReferenceError on a customer's site, where nothing here would see it.
func TestEmbedJSDoesNotDependOnBookingLogic(t *testing.T) {
	for i, line := range strings.Split(string(embedJS), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") { // comments may reference it by name
			continue
		}
		if strings.Contains(code, "BookingLogic") {
			t.Errorf("embed.js:%d calls into BookingLogic, which is not loaded in the widget: %s",
				i+1, code)
		}
	}
}
