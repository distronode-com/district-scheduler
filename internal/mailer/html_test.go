package mailer

import (
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/i18n"
)

func sampleBookingData() BookingData {
	start := time.Date(2026, 6, 22, 21, 0, 0, 0, time.UTC) // 9am NZST
	return BookingData{
		BookingID:         "abc-123",
		EventTypeName:     "20-minute call",
		EventTypeSlug:     "intro",
		HostName:          "Wynne Pirini",
		HostEmail:         "host@example.com",
		OrganizerName:     "Alex Johnson",
		OrganizerEmail:    "alex@example.com",
		OrganizerTimezone: "Pacific/Auckland",
		StartAt:           start,
		EndAt:             start.Add(20 * time.Minute),
		PreviousStartAt:   start.AddDate(0, 0, -1),
		PreviousEndAt:     start.AddDate(0, 0, -1).Add(20 * time.Minute),
		ManageURL:         "https://booking.example.com/manage/tok",
		BaseURL:           "https://booking.example.com",
		BrandName:         "Orchestratr",
	}
}

// renderedVariants is every booking mail this package can produce, keyed by the
// name used in test failures.
//
// There are SEVEN, not eight: reminders go to the attendee only (SendReminder
// has no host counterpart and there is no htmlReminderHost), so "attendee and
// host variants of all four mails" describes the intent, not the code. Any new
// variant must be added here — these are the templates the whole file sweeps.
func renderedVariants(d BookingData) map[string]string {
	return map[string]string{
		"confirm-org":     renderHTML(htmlConfirmOrg, d),
		"confirm-host":    renderHTML(htmlConfirmHost, d),
		"cancel-org":      renderHTML(htmlCancelOrg, d),
		"cancel-host":     renderHTML(htmlCancelHost, d),
		"reschedule-org":  renderHTML(htmlRescheduleOrg, d),
		"reschedule-host": renderHTML(htmlRescheduleHost, d),
		"reminder-org":    renderHTML(htmlReminderOrg, d),
	}
}

// Every HTML template must render to non-empty output (renderHTML returns "" on
// any execution error, so empty means a broken template).
func TestRenderHTML_allTemplates(t *testing.T) {
	d := sampleBookingData()
	cases := renderedVariants(d)
	for name, out := range cases {
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s: rendered empty HTML (template error)", name)
			continue
		}
		if !strings.Contains(out, "20-minute call") {
			t.Errorf("%s: missing event name", name)
		}
		// CHANGED 2026-09-06 (letterhead restyle): this used to assert that no
		// <img> rendered at all when LogoURL was empty, because the old design
		// had no mark of its own. The letterhead always carries a mark — the
		// Distronode one when the tenant has not set theirs — so the assertion
		// is inverted deliberately rather than deleted. The tenant/Distronode
		// switch itself is pinned by TestHTMLHeaderSwitchesOnTenantLogo.
		if !strings.Contains(out, distronodeLogoURL) {
			t.Errorf("%s: no Distronode logo with LogoURL unset", name)
		}
		if strings.Contains(out, "cdn.example.com") {
			t.Errorf("%s: rendered a tenant asset with no LogoURL set", name)
		}
	}

	// Attendee confirmation must carry the calendar buttons and manage link.
	conf := cases["confirm-org"]
	if !strings.Contains(conf, "calendar.google.com") || !strings.Contains(conf, "outlook.office.com") {
		t.Error("confirm-org: missing add-to-calendar links")
	}
	if !strings.Contains(conf, "/manage/tok") {
		t.Error("confirm-org: missing manage link")
	}

	// With a logo, the header renders the image with the brand as alt text.
	d.LogoURL = "https://cdn.example.com/logo.png"
	withLogo := renderHTML(htmlConfirmOrg, d)
	if !strings.Contains(withLogo, `src="https://cdn.example.com/logo.png"`) {
		t.Error("confirm-org: logo image not rendered when LogoURL set")
	}
	if !strings.Contains(withLogo, `alt="Orchestratr"`) {
		t.Error("confirm-org: logo alt should be the brand name")
	}

	// With a banner, it renders full-width below the logo, with no border/padding.
	d.BannerURL = "https://cdn.example.com/banner.png"
	d.BannerOpacity = 60
	withBanner := renderHTML(htmlConfirmOrg, d)
	if !strings.Contains(withBanner, `src="https://cdn.example.com/banner.png"`) {
		t.Error("confirm-org: banner image not rendered when BannerURL set")
	}
	if !strings.Contains(withBanner, `opacity:0.6`) {
		t.Error("confirm-org: banner opacity not applied")
	}
	if strings.Index(withBanner, "cdn.example.com/logo.png") > strings.Index(withBanner, "cdn.example.com/banner.png") {
		t.Error("confirm-org: banner should render after the logo, not before")
	}
}

// The letterhead palette must be what actually reaches the wire, and the old
// zinc palette must be gone from it. Asserting the absence matters as much as
// the presence: a half-converted template renders perfectly well and simply
// looks like a different product.
func TestHTMLCarriesTheLetterheadPaletteAndNotTheOldZinc(t *testing.T) {
	want := map[string]string{
		"ink": cInk, "slate": cSlate, "teal": cTeal, "muted": cMuted,
		"rule": cRule, "wash": cWash, "body": cBody, "paper": cPaper,
	}
	// The retired zinc palette. #f4f4f5 was the wash, #18181b the ink and the
	// button fill, #e4e4e7 the hairline, #a1a1aa the muted/struck-through text.
	unwanted := []string{"#f4f4f5", "#18181b", "#e4e4e7", "#a1a1aa", "#d4d4d8", "#71717a", "#52525b"}

	d := sampleBookingData()
	d.LocationValue = "Level 3, 12 Example Street"
	d.CancellationReason = "Something came up"
	d.CustomNote = "Bring the deck."
	for name, out := range renderedVariants(d) {
		for role, hex := range want {
			if !strings.Contains(out, hex) {
				t.Errorf("%s: palette colour %s (%s) never reaches the output", name, role, hex)
			}
		}
		for _, hex := range unwanted {
			if strings.Contains(out, hex) {
				t.Errorf("%s: retired zinc colour %s still rendered", name, hex)
			}
		}
		if !strings.Contains(out, fSerif) {
			t.Errorf("%s: serif stack missing from the content cell", name)
		}
		if !strings.Contains(out, fSans) {
			t.Errorf("%s: sans stack missing from the chrome", name)
		}
	}
}

// Outlook's Word rendering engine implements neither flexbox nor grid, so a
// layout built from them collapses to a single column there. The constraint is
// on the RENDERED output, not on the source: a template can smuggle either in
// through a partial.
func TestHTMLHasNoFlexOrGrid(t *testing.T) {
	d := sampleBookingData()
	d.LogoURL = "https://cdn.example.com/logo.png"
	d.BannerURL = "https://cdn.example.com/banner.png"
	d.LocationValue = "Level 3, 12 Example Street"
	d.CancellationReason = "Something came up"
	d.CustomNote = "Bring the deck."
	for name, out := range renderedVariants(d) {
		flat := strings.ReplaceAll(out, " ", "")
		for _, bad := range []string{"display:flex", "display:grid", "display:inline-flex", "display:inline-grid"} {
			if strings.Contains(flat, bad) {
				t.Errorf("%s: %s in rendered output; Outlook cannot lay it out", name, bad)
			}
		}
		// One <style> block only, and nothing pulled over the network.
		if n := strings.Count(out, "<style"); n != 1 {
			t.Errorf("%s: %d <style> blocks; want exactly 1", name, n)
		}
		if strings.Contains(out, "<link") || strings.Contains(out, "@import") {
			t.Errorf("%s: external stylesheet reference in an email", name)
		}
		// The card carries the width ATTRIBUTE as well as the style: Outlook
		// reads the attribute and ignores max-width.
		if !strings.Contains(out, `width="`+cardWidth+`"`) || !strings.Contains(out, "max-width:"+cardWidth+"px") {
			t.Errorf("%s: card must carry both width=%s and max-width:%spx", name, cardWidth, cardWidth)
		}
	}
}

// The header is the whole "whose mail is this" decision. With no tenant logo it
// is Distronode's; with one it is the tenant's and ours disappears entirely.
func TestHTMLHeaderSwitchesOnTenantLogo(t *testing.T) {
	d := sampleBookingData()
	for name, out := range renderedVariants(d) {
		if !strings.Contains(out, `src="`+distronodeLogoURL+`"`) {
			t.Errorf("%s: Distronode logo URL missing", name)
		}
		if !strings.Contains(out, `alt="Distronode"`) {
			t.Errorf("%s: Distronode logo alt missing", name)
		}
		// 204x60 as attributes AND inline style — clients disagree about which
		// they honour, and an unsized logo reflows the header.
		for _, want := range []string{`width="204"`, `height="60"`, "width:204px", "height:60px", "display:block"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: Distronode logo missing %q", name, want)
			}
		}
		if !strings.Contains(out, "District AI Scheduling") || !strings.Contains(out, "distronode.com") {
			t.Errorf("%s: Distronode header lines missing", name)
		}
		if strings.Contains(out, ">Orchestratr<") {
			t.Errorf("%s: tenant brand in the header with no tenant logo set", name)
		}
		// The double rule: 2.5px slate, a 3px gap, then the 1px hairline.
		if !strings.Contains(out, "border-bottom:2.5px solid "+cSlate) {
			t.Errorf("%s: header slate rule missing", name)
		}
		if !strings.Contains(out, `height="3"`) || !strings.Contains(out, `height="1"`) {
			t.Errorf("%s: the 3px gap + 1px hairline below the slate rule is missing", name)
		}
	}

	d.LogoURL = "https://cdn.example.com/logo.png"
	d.LogoHeight = 40
	d.LogoOpacity = 90
	for name, out := range renderedVariants(d) {
		if strings.Contains(out, distronodeLogoURL) {
			t.Errorf("%s: Distronode logo still rendered alongside the tenant's", name)
		}
		if strings.Contains(out, "District AI Scheduling") {
			t.Errorf("%s: Distronode header line still rendered for a tenant-branded mail", name)
		}
		if !strings.Contains(out, `src="https://cdn.example.com/logo.png"`) {
			t.Errorf("%s: tenant logo missing", name)
		}
		if !strings.Contains(out, `height="40"`) || !strings.Contains(out, "height:40px") {
			t.Errorf("%s: tenant LogoPx not applied to the header image", name)
		}
		if !strings.Contains(out, "opacity:0.9") {
			t.Errorf("%s: tenant logo opacity not applied", name)
		}
		if !strings.Contains(out, ">Orchestratr</div>") {
			t.Errorf("%s: tenant brand missing from the header lines", name)
		}
	}
}

// The footer says who sent the mail, and it must not claim to be Distronode
// Corporation on a mail wearing a tenant's mark — the postal address is ours,
// and publishing it under someone else's brand is wrong in both directions.
func TestHTMLFooterSwitchesOnTenantLogo(t *testing.T) {
	d := sampleBookingData()
	for name, out := range renderedVariants(d) {
		if !strings.Contains(out, "Distronode Corporation") {
			t.Errorf("%s: footer missing the corporation line", name)
		}
		if !strings.Contains(out, postalAddress) {
			t.Errorf("%s: footer missing the postal address", name)
		}
		if strings.Contains(out, "Scheduling by District AI") {
			t.Errorf("%s: tenant footer line on a Distronode-branded mail", name)
		}
		if !strings.Contains(out, "border-top:1px solid "+cRule) {
			t.Errorf("%s: footer rule missing", name)
		}
	}

	d.LogoURL = "https://cdn.example.com/logo.png"
	for name, out := range renderedVariants(d) {
		if strings.Contains(out, "Distronode Corporation") {
			t.Errorf("%s: our corporation name on a tenant-branded mail", name)
		}
		if strings.Contains(out, postalAddress) {
			t.Errorf("%s: our postal address on a tenant-branded mail", name)
		}
		if !strings.Contains(out, "Scheduling by District AI") {
			t.Errorf("%s: tenant footer missing the attribution line", name)
		}
		if !strings.Contains(out, ">Orchestratr</div>") {
			t.Errorf("%s: tenant footer missing the brand line", name)
		}
	}
}

// The preheader exists so the inbox preview shows the mail's own first sentence
// instead of the header address block, which only works if it comes FIRST in
// the body. It also has to stay invisible, and to carry the pad — a client that
// runs out of preheader text keeps scraping.
func TestHTMLPreheaderIsHiddenAndPrecedesTheHeader(t *testing.T) {
	leads := map[string]string{
		"confirm-org":     "Your booking is confirmed.",
		"confirm-host":    "You have a new booking.",
		"cancel-org":      "Your booking has been cancelled.",
		"cancel-host":     "A booking has been cancelled.",
		"reschedule-org":  "Your booking has been rescheduled.",
		"reschedule-host": "A booking has been rescheduled.",
		"reminder-org":    "This is a reminder that your booking is coming up.",
	}
	d := sampleBookingData()
	for name, out := range renderedVariants(d) {
		body := strings.Index(out, "<body")
		pre := strings.Index(out, "display:none;overflow:hidden")
		header := strings.Index(out, distronodeLogoURL)
		if pre < 0 {
			t.Errorf("%s: no hidden preheader div", name)
			continue
		}
		if !(body < pre && pre < header) {
			t.Errorf("%s: preheader at %d must sit between <body> (%d) and the header (%d)", name, pre, body, header)
		}
		for _, want := range []string{"display:none", "overflow:hidden", "opacity:0", "max-height:0"} {
			if !strings.Contains(out[pre-40:pre+120], want) {
				t.Errorf("%s: preheader missing %q", name, want)
			}
		}
		if !strings.Contains(out, preheaderPad) {
			t.Errorf("%s: preheader pad missing; the inbox preview will spill into the header", name)
		}
		if n := strings.Count(out, "&#8203;&nbsp;"); n != 60 {
			t.Errorf("%s: preheader pad repeated %d times; want 60", name, n)
		}
		// The preheader must be the mail's OWN lead, not a generic string, and
		// the same sentence must open the body.
		lead := leads[name]
		if got := out[pre:strings.Index(out, preheaderPad)]; !strings.Contains(got, lead) {
			t.Errorf("%s: preheader does not carry the lead %q", name, lead)
		}
		if strings.Count(out, lead) != 2 {
			t.Errorf("%s: lead %q should appear exactly twice (preheader + body)", name, lead)
		}
	}
}

// The kv fact block is the letterhead's second motif and the one that carries
// the actual booking. Its geometry is load-bearing: a 2px teal LEFT border, a
// fixed 34% label column so values line up, muted labels, bold ink values.
func TestHTMLKvBlockIsTheLetterheadFactBlock(t *testing.T) {
	d := sampleBookingData()
	d.LocationValue = "Level 3, 12 Example Street"
	for name, out := range renderedVariants(d) {
		if !strings.Contains(out, "margin:16px 0 18px;border-left:2px solid "+cTeal) {
			t.Errorf("%s: kv block missing its 2px teal left border / margin", name)
		}
		if !strings.Contains(out, `width="34%"`) || !strings.Contains(out, "width:34%") {
			t.Errorf("%s: kv label column must be a fixed 34%%", name)
		}
		if !strings.Contains(out, "font-size:13px;line-height:20px;color:"+cMuted) {
			t.Errorf("%s: kv labels must be sans 13px muted", name)
		}
		if !strings.Contains(out, "font-size:13px;line-height:20px;font-weight:700;color:"+cInk) {
			t.Errorf("%s: kv values must be bold ink", name)
		}
	}

	// The superseded time on a reschedule keeps the struck-through muted
	// treatment it has always had — deliberately NOT the bold ink value style,
	// because the point of the row is that it no longer applies.
	for _, name := range []string{"reschedule-org", "reschedule-host"} {
		out := renderedVariants(d)[name]
		if !strings.Contains(out, "text-decoration:line-through") {
			t.Errorf("%s: the superseded 'was' time must render struck through", name)
		}
	}
}

// Every clickable thing is the bulletproof pattern: fill on the TD (Outlook
// ignores background and border-radius on an inline <a>), padding on an
// inline-block anchor (Outlook ignores padding on an inline one).
func TestHTMLButtonsAreBulletproof(t *testing.T) {
	d := sampleBookingData()
	buttons := map[string][]string{
		"confirm-org":    {"Manage booking", "Google Calendar", "Outlook"},
		"cancel-org":     {"Book again"},
		"reschedule-org": {"Manage booking", "Google Calendar", "Outlook"},
		"reminder-org":   {"Manage booking", "Google Calendar", "Outlook"},
	}
	rendered := renderedVariants(d)
	for name, labels := range buttons {
		out := rendered[name]
		for _, label := range labels {
			if !strings.Contains(out, label+"</a>") {
				t.Errorf("%s: button %q missing", name, label)
			}
		}
		if !strings.Contains(out, "background-color:"+cTeal+";border:1px solid "+cTeal) {
			t.Errorf("%s: button fill must be teal on the TD", name)
		}
		if !strings.Contains(out, "display:inline-block;padding:13px 30px;font-family:"+fSans+";font-size:14px;font-weight:700") {
			t.Errorf("%s: button anchor must be an inline-block with padding, sans 14px bold", name)
		}
		if !strings.Contains(out, "color:"+cPaper+";text-decoration:none") {
			t.Errorf("%s: button label must be paper and undecorated", name)
		}
	}
	// Both calendar hrefs survive the restyle, on every mail that had them.
	for _, name := range []string{"confirm-org", "reschedule-org", "reminder-org"} {
		if !strings.Contains(rendered[name], "calendar.google.com") || !strings.Contains(rendered[name], "outlook.office.com") {
			t.Errorf("%s: lost a calendar href", name)
		}
	}
}

// Every optional row must still appear only when its field is set. The restyle
// moved the markup around these conditionals; nothing about when they fire
// changed, and this is what proves it.
func TestHTMLConditionalRowsStillToggle(t *testing.T) {
	bare := sampleBookingData()
	bare.ManageURL = ""
	bareOut := renderedVariants(bare)
	full := sampleBookingData()
	full.LocationValue = "Level 3, 12 Example Street"
	full.CancellationReason = "Something came up"
	full.CustomNote = "Bring the deck."
	full.BannerURL = "https://cdn.example.com/banner.png"
	fullOut := renderedVariants(full)

	cases := []struct {
		variant string
		marker  string
		what    string
	}{
		{"confirm-org", ">Location</td>", "location row"},
		{"confirm-host", ">Location</td>", "location row"},
		{"reschedule-org", ">Location</td>", "location row"},
		{"reschedule-host", ">Location</td>", "location row"},
		{"reminder-org", ">Location</td>", "location row"},
		{"cancel-org", ">Reason</td>", "cancellation reason row"},
		{"cancel-host", ">Reason</td>", "cancellation reason row"},
		{"confirm-org", "Bring the deck.", "custom note"},
		{"cancel-org", "Bring the deck.", "custom note"},
		{"reschedule-org", "Bring the deck.", "custom note"},
		{"reminder-org", "Bring the deck.", "custom note"},
		{"confirm-org", "cdn.example.com/banner.png", "banner row"},
	}
	for _, c := range cases {
		if strings.Contains(bareOut[c.variant], c.marker) {
			t.Errorf("%s: %s rendered with the field unset", c.variant, c.what)
		}
		if !strings.Contains(fullOut[c.variant], c.marker) {
			t.Errorf("%s: %s missing with the field set", c.variant, c.what)
		}
	}

	// The manage button follows ManageURL, on the three mails that offer it.
	for _, name := range []string{"confirm-org", "reschedule-org", "reminder-org"} {
		if strings.Contains(bareOut[name], "/manage/tok") {
			t.Errorf("%s: manage button rendered with no ManageURL", name)
		}
		if !strings.Contains(fullOut[name], "/manage/tok") {
			t.Errorf("%s: manage button missing with ManageURL set", name)
		}
	}
	// Cancellation offers a rebook button instead, and it does not depend on
	// ManageURL — it is built from BaseURL and the event-type slug.
	for _, out := range []string{bareOut["cancel-org"], fullOut["cancel-org"]} {
		if !strings.Contains(out, "https://booking.example.com/book/intro") {
			t.Error("cancel-org: rebook button href missing")
		}
	}
	// The booking reference survives on every mail that carried it.
	for _, name := range []string{"confirm-org", "confirm-host", "cancel-host", "reschedule-org", "reschedule-host", "reminder-org"} {
		if !strings.Contains(fullOut[name], "abc-123") {
			t.Errorf("%s: booking reference missing", name)
		}
	}
}

// The restyle is markup only: the attendee-facing copy still comes from the
// locale files, and every variant must render for every language we ship. en
// and fr-CA are the two the brief names; sweeping the rest is free.
func TestHTMLRendersInEveryLocale(t *testing.T) {
	for _, opt := range i18n.SupportedLocales() {
		loc := i18n.Get(opt.Code)
		if loc == nil {
			t.Fatalf("locale %q not loaded", opt.Code)
		}
		d := sampleBookingData()
		d.Locale = loc
		d.LocationValue = "Level 3, 12 Example Street"
		d.CancellationReason = "Something came up"
		for name, out := range renderedVariants(d) {
			if strings.TrimSpace(out) == "" {
				t.Errorf("%s/%s: rendered empty HTML", name, opt.Code)
				continue
			}
			if !strings.Contains(out, `lang="`+opt.Code+`"`) {
				t.Errorf("%s/%s: wrong <html lang>", name, opt.Code)
			}
			// Brand chrome stays English by design — it is a company name and a
			// domain, not copy — so it must be present in every locale.
			if !strings.Contains(out, "Distronode Corporation") || !strings.Contains(out, "District AI Scheduling") {
				t.Errorf("%s/%s: brand chrome missing", name, opt.Code)
			}
		}
	}

	// Spot-check that the localised strings really are reaching the markup, not
	// just that it rendered: fr-CA labels and lead on the attendee mails.
	d := sampleBookingData()
	d.Locale = i18n.Get("fr-CA")
	d.LocationValue = "Level 3, 12 Example Street"
	fr := renderedVariants(d)
	for _, want := range []string{"Bonjour Alex Johnson,", "Votre réservation est confirmée.", ">Événement</td>", ">Quand</td>", ">Lieu</td>", "Gérer la réservation</a>"} {
		if !strings.Contains(fr["confirm-org"], want) {
			t.Errorf("confirm-org/fr-CA: missing %q", want)
		}
	}
	for _, want := range []string{"Votre réservation a été reportée.", ">Avant</td>", ">Maintenant</td>"} {
		if !strings.Contains(fr["reschedule-org"], want) {
			t.Errorf("reschedule-org/fr-CA: missing %q", want)
		}
	}
	if !strings.Contains(fr["cancel-org"], "Réserver à nouveau</a>") {
		t.Error("cancel-org/fr-CA: rebook button not translated")
	}
	// Host mail is the operator's, never translated — the Send* functions clear
	// the locale, so a host template rendered WITH one must still be English.
	if !strings.Contains(fr["confirm-host"], "Hi Wynne Pirini,") || !strings.Contains(fr["confirm-host"], ">Event</td>") {
		t.Error("confirm-host: host copy must stay English")
	}
}

// TestTextTemplates_signOffWithBrand is the regression guard for a split that lived
// inside a single message. All seven HTML variants take their wordmark from the shared
// header/footer, which reads {{.Brand}}; the three HOST text templates hardcoded the
// product name instead, so an operator who set a business name got attendee mail signed
// with it and host mail signed with ours, and the text and HTML alternatives of one host
// email disagreed with each other. Nothing rendered the text templates in a test, which
// is why it survived.
func TestTextTemplates_signOffWithBrand(t *testing.T) {
	d := sampleBookingData() // BrandName: "Orchestratr"
	texts := map[string]string{
		"confirm-org":     render(confirmOrgTmpl, d),
		"confirm-host":    render(confirmHostTmpl, d),
		"cancel-org":      render(cancelOrgTmpl, d),
		"cancel-host":     render(cancelHostTmpl, d),
		"reschedule-org":  render(rescheduleOrgTmpl, d),
		"reschedule-host": render(rescheduleHostTmpl, d),
		"reminder-org":    render(reminderOrgTmpl, d),
	}
	if len(texts) != 7 {
		t.Fatalf("expected the seven text templates, got %d", len(texts))
	}
	for name, out := range texts {
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s: rendered empty text (template error)", name)
			continue
		}
		if !strings.Contains(out, "\u2014 Orchestratr") {
			t.Errorf("%s: does not sign off with the configured brand; tail was %q",
				name, lastLines(out, 2))
		}
		if strings.Contains(out, "Calnode") {
			t.Errorf("%s: still names the upstream product in body copy", name)
		}
	}
}

// lastLines returns the final n non-empty lines of s, for failure messages.
func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " / ")
}

func TestBookingData_Brand(t *testing.T) {
	// The fallback is the same literal html.go's header writes with no tenant logo
	// set, so one message's text and HTML parts agree on the sender.
	if got := (BookingData{}).Brand(); got != "District AI Scheduling" {
		t.Errorf("Brand() empty = %q; want District AI Scheduling", got)
	}
	if got := (BookingData{BrandName: "Acme"}).Brand(); got != "Acme" {
		t.Errorf("Brand() set = %q; want Acme", got)
	}
}
