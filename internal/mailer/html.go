package mailer

import (
	"bytes"
	"html/template"
	"strings"
)

// HTML email rendering. Each email type has an html/template that defines a
// "content" block (and a "lead" block, see below); it's cloned onto a shared
// base providing the outer layout plus reusable partials. The plain-text body
// (booking.go) stays the multipart/alternative fallback.
//
// The design is Distronode's transactional letterhead, whose single source is
// distronode-website/src/emails/EmailLayout.tsx. Its three signature motifs are
// reproduced here: the header (logo left, uppercase muted right-aligned lines,
// underlined by a 2.5px slate rule with a 1px hairline 3px below it), the kv
// fact block (sans rows behind a 2px teal LEFT border) and the footer (1px top
// rule, uppercase, letter-spaced, tiny and muted).
//
// ⛔ CONSTRAINTS THAT ARE NOT STYLE PREFERENCES (same as EmailLayout.tsx):
//
//   - TABLE-BASED STRUCTURE. Outlook's Word rendering engine implements neither
//     flexbox nor grid, so a layout built from them collapses to a single column
//     there. There is no display:flex or display:grid anywhere in this file and
//     a test asserts that about the RENDERED output.
//   - INLINE STYLES. Clients strip and rewrite <style> unevenly (Gmail drops it
//     entirely on clipped-message paths), so every load-bearing style is inline
//     on the element. The single <style> block below carries ONLY the wash body
//     background and the mobile media query, neither of which can be expressed
//     inline; losing both degrades to the desktop layout, not a broken email.
//   - THE LOGO IS AN ABSOLUTE URL AND A SMALL FILE. distronode-logo-email.png is
//     ~11 KB at 510x150 and is rendered at 204x60; below ~55px tall the mark's
//     spiral turns to mush, so if it ever needs to look better, grow the box,
//     not the file.
const (
	// Letterhead palette (EMAIL_COLORS). The ONLY accent is teal.
	cInk   = "#1d2432" // primary text, kv values
	cSlate = "#3a4760" // the heavy header rule
	cTeal  = "#166f8a" // links, the kv left border, buttons
	cMuted = "#6d768a" // labels, footer, chrome
	cRule  = "#dde2ea" // hairlines and section dividers
	cWash  = "#f6f8fa" // page background behind the card
	cBody  = "#2a3141" // running body copy (softer than ink)
	cPaper = "#ffffff" // the card itself

	// Serif for CONTENT, sans for CHROME — the letterhead's own split. Email-safe
	// stacks only: a webfont request from an email is both blocked and a tracking
	// signal.
	fSerif = "Georgia, 'Times New Roman', serif"
	fSans  = "'Helvetica Neue', Helvetica, Arial, sans-serif"

	// Deliberately NOT the tenant's BaseURL: the mark is byte-identical on every
	// origin, and a briefly unreachable regional host would otherwise break the
	// image in mail that has already shipped.
	distronodeLogoURL = "https://www.distronode.com/distronode-logo-email.png"

	// 640px is the widest card that survives Outlook's reading pane.
	cardWidth = "640"

	postalAddress = "RBC WaterPark Place, 20 Bay Street, 11th Floor, Toronto, ON M5J 2N8"
)

// preheaderPad is zero-width-space + non-breaking-space padding. Without it a
// client that runs out of preheader text keeps scraping and spills the header
// address block into the inbox preview line, which is the exact thing the
// preheader exists to prevent.
var preheaderPad = strings.Repeat("&#8203;&nbsp;", 60)

// The only <style> block; see the constraint note above.
const styleBlock = `<style>
body { margin:0 !important; padding:0 !important; background-color:` + cWash + `; }
table { border-collapse:collapse; }
img { border:0; outline:none; text-decoration:none; -ms-interpolation-mode:bicubic; }
a { color:` + cTeal + `; }
@media only screen and (max-width:620px) {
.dn-pad { padding-left:22px !important; padding-right:22px !important; }
.dn-stack { display:block !important; width:100% !important; text-align:left !important; padding-left:0 !important; padding-right:0 !important; }
.dn-stack-top { padding-bottom:10px !important; }
}
</style>`

// emailButton is the "bulletproof" button from EmailButton: a one-cell table
// with the fill on the TD and the padding on the anchor. Outlook ignores padding
// on an inline <a> and ignores border-radius everywhere, so the fill has to be
// the cell (square corners there, rounded elsewhere) and the click target has to
// be the anchor's own box, which only exists at display:inline-block.
// href/label are template source fragments, not values.
func emailButton(margin, href, label string) string {
	return `<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="border-collapse:separate;margin:` + margin + `;">` +
		`<tr><td align="center" style="background-color:` + cTeal + `;border:1px solid ` + cTeal + `;border-radius:4px;text-align:center;">` +
		`<a href="` + href + `" style="display:inline-block;padding:13px 30px;font-family:` + fSans + `;font-size:14px;font-weight:700;letter-spacing:0.4px;color:` + cPaper + `;text-decoration:none;">` +
		label + `</a></td></tr></table>`
}

// kv row cells. The label column is a fixed 34% so values line up down the block
// the way they do on the printed letterhead.
const (
	kvLabelTD = `<td class="dn-stack" valign="top" width="34%" style="vertical-align:top;width:34%;padding:3px 10px 3px 14px;font-family:` + fSans + `;font-size:13px;line-height:20px;color:` + cMuted + `;">`
	kvValueTD = `<td class="dn-stack" valign="top" style="vertical-align:top;padding:3px 0;font-family:` + fSans + `;font-size:13px;line-height:20px;font-weight:700;color:` + cInk + `;">`
	// Long location values (a room address, a meeting URL) must wrap rather than
	// widen the card.
	kvValueWrapTD = `<td class="dn-stack" valign="top" style="vertical-align:top;padding:3px 0;font-family:` + fSans + `;font-size:13px;line-height:20px;font-weight:700;color:` + cInk + `;word-break:break-word;">`
	// The superseded time on a reschedule: struck through and muted on both cells.
	kvWasLabelTD = `<td class="dn-stack" valign="top" width="34%" style="vertical-align:top;width:34%;padding:3px 10px 3px 14px;font-family:` + fSans + `;font-size:13px;line-height:20px;color:` + cMuted + `;text-decoration:line-through;">`
	kvWasValueTD = `<td class="dn-stack" valign="top" style="vertical-align:top;padding:3px 0;font-family:` + fSans + `;font-size:13px;line-height:20px;color:` + cMuted + `;text-decoration:line-through;">`
)

// htmlLayout holds the shared outer document plus every partial the content
// templates call. Each content template must also define a "lead" block: it is
// rendered both as the opening body sentence and as the hidden preheader, so the
// inbox preview line is the mail's own first sentence rather than scraped chrome.
var htmlLayout = `{{define "layout"}}<!doctype html>
<html lang="{{.LocaleCode}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
` + styleBlock + `</head>
<body style="margin:0;padding:0;background-color:` + cWash + `;">
<div style="display:none;overflow:hidden;line-height:1px;opacity:0;max-height:0;max-width:0;">{{template "lead" .}}` + preheaderPad + `</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%;background-color:` + cWash + `;border-collapse:collapse;"><tr><td align="center" style="padding:32px 12px;">
<table role="presentation" width="` + cardWidth + `" cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:` + cardWidth + `px;background-color:` + cPaper + `;border:1px solid ` + cRule + `;border-collapse:collapse;text-align:left;">
{{template "header" .}}
{{if .BannerURL}}<tr><td style="padding:0;line-height:0;font-size:0;">
<img src="{{.BannerURL}}" alt="" width="` + cardWidth + `" style="width:100%;max-width:100%;height:auto;opacity:{{.BannerOpacityCSS}};display:block;border:0;">
</td></tr>{{end}}
<tr><td class="dn-pad" style="padding:28px 36px 32px;font-family:` + fSerif + `;font-size:15px;line-height:1.6;color:` + cBody + `;">
{{template "content" .}}
</td></tr>
{{template "footer" .}}
</table>
</td></tr></table>
</body></html>{{end}}

{{define "header"}}<tr><td class="dn-pad" style="padding:28px 36px 13px;border-bottom:2.5px solid ` + cSlate + `;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%;border-collapse:collapse;"><tr>
<td class="dn-stack dn-stack-top" valign="top" style="vertical-align:top;">
{{if .LogoURL}}<img src="{{.LogoURL}}" alt="{{.Brand}}" height="{{.LogoPx}}" style="display:block;height:{{.LogoPx}}px;width:auto;max-width:100%;opacity:{{.LogoOpacityCSS}};border:0;">
{{else}}<img src="` + distronodeLogoURL + `" alt="Distronode" width="204" height="60" style="display:block;width:204px;height:60px;border:0;">{{end}}
</td>
<td class="dn-stack" align="right" valign="top" style="vertical-align:top;text-align:right;font-family:` + fSans + `;font-size:10px;line-height:17px;letter-spacing:0.8px;text-transform:uppercase;color:` + cMuted + `;">
{{if .LogoURL}}<div>{{.Brand}}</div>{{else}}<div>District AI Scheduling</div><div>distronode.com</div>{{end}}
</td>
</tr></table>
</td></tr>
<tr><td height="3" style="height:3px;font-size:1px;line-height:3px;">&nbsp;</td></tr>
<tr><td height="1" style="height:1px;font-size:1px;line-height:1px;background-color:` + cRule + `;">&nbsp;</td></tr>{{end}}

{{define "footer"}}<tr><td class="dn-pad" style="padding:12px 36px 22px;border-top:1px solid ` + cRule + `;font-family:` + fSans + `;font-size:11px;line-height:18px;letter-spacing:0.8px;text-transform:uppercase;color:` + cMuted + `;">
{{if .LogoURL}}<div>{{.Brand}}</div><div style="margin:8px 0 0;">Scheduling by District AI</div>
{{else}}<div>Distronode Corporation</div><div style="margin:8px 0 0;">` + postalAddress + `</div>{{end}}
</td></tr>{{end}}

{{define "kvOpen"}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%;border-collapse:collapse;margin:16px 0 18px;border-left:2px solid ` + cTeal + `;">{{end}}
{{define "kvClose"}}</table>{{end}}

{{define "mngBtn"}}{{if .ManageURL}}` + emailButton("24px 0 0", `{{.ManageURL}}`, `{{.T "email_manage_button"}}`) + `{{end}}{{end}}

{{define "calBtns"}}<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;margin:14px 0 0;"><tr>
<td class="dn-stack dn-stack-top" valign="top" style="vertical-align:top;padding-right:10px;">` + emailButton("0", `{{.GoogleCalURL}}`, `{{.T "email_calendar_google"}}`) + `</td>
<td class="dn-stack" valign="top" style="vertical-align:top;">` + emailButton("0", `{{.OutlookCalURL}}`, `{{.T "email_calendar_outlook"}}`) + `</td>
</tr></table>{{end}}

{{define "rebook"}}` + emailButton("24px 0 0", `{{.BaseURL}}/book/{{.EventTypeSlug}}`, `{{.T "email_rebook_button"}}`) + `{{end}}

{{define "ref"}}<p style="margin:24px 0 0;font-family:` + fSans + `;font-size:12px;line-height:18px;color:` + cMuted + `;">{{.Tf "email_booking_reference" .BookingID}}</p>{{end}}

{{define "note"}}{{if .CustomNote}}<div style="margin:24px 0 0;padding-top:20px;border-top:1px solid ` + cRule + `;font-family:` + fSerif + `;font-size:14px;line-height:1.6;color:` + cBody + `;white-space:pre-line;">{{.CustomNote}}</div>{{end}}{{end}}`

var htmlBase = template.Must(template.New("base").Parse(htmlLayout))

func content(def string) *template.Template {
	return template.Must(template.Must(htmlBase.Clone()).Parse(def))
}

// renderHTML executes the layout of a content template against d. On error it
// returns "" so the caller falls back to a text-only message.
func renderHTML(t *template.Template, d BookingData) string {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", d); err != nil {
		return ""
	}
	return buf.String()
}

// greeting + lead, shared by every content template. The lead is a separate
// block so the layout can reuse it as the preheader.
const leadBlock = `<p style="margin:0 0 4px;color:` + cInk + `;">{{.Tf "email_greeting" .OrganizerName}}</p>
<p style="margin:0 0 18px;">{{template "lead" .}}</p>`

// hostGreeting is the English host-facing equivalent; host mail is never
// translated (see BookingData.Locale).
const hostGreeting = `<p style="margin:0 0 4px;color:` + cInk + `;">Hi {{.HostName}},</p>
<p style="margin:0 0 18px;">{{template "lead" .}}</p>`

var htmlConfirmOrg = content(`{{define "lead"}}{{.T "email_confirmed_lead"}}{{end}}
{{define "content"}}
` + leadBlock + `
{{template "kvOpen" .}}
<tr>` + kvLabelTD + `{{.T "email_label_event"}}</td>` + kvValueTD + `{{.EventTypeName}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_with"}}</td>` + kvValueTD + `{{.HostName}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_when"}}</td>` + kvValueTD + `{{.WhenFmt}}</td></tr>
{{if .LocationValue}}<tr>` + kvLabelTD + `{{.T "email_label_location"}}</td>` + kvValueWrapTD + `{{.LocationValue}}</td></tr>{{end}}
{{template "kvClose" .}}
{{template "mngBtn" .}}
{{template "calBtns" .}}
{{template "ref" .}}
{{template "note" .}}
{{end}}`)

var htmlConfirmHost = content(`{{define "lead"}}You have a new booking.{{end}}
{{define "content"}}
` + hostGreeting + `
{{template "kvOpen" .}}
<tr>` + kvLabelTD + `Event</td>` + kvValueTD + `{{.EventTypeName}}</td></tr>
<tr>` + kvLabelTD + `With</td>` + kvValueTD + `{{.OrganizerName}} &lt;{{.OrganizerEmail}}&gt;</td></tr>
<tr>` + kvLabelTD + `When</td>` + kvValueTD + `{{.WhenFmt}}</td></tr>
{{if .LocationValue}}<tr>` + kvLabelTD + `Location</td>` + kvValueWrapTD + `{{.LocationValue}}</td></tr>{{end}}
{{template "kvClose" .}}
{{template "ref" .}}
{{end}}`)

var htmlCancelOrg = content(`{{define "lead"}}{{.T "email_cancelled_lead"}}{{end}}
{{define "content"}}
` + leadBlock + `
{{template "kvOpen" .}}
<tr>` + kvLabelTD + `{{.T "email_label_event"}}</td>` + kvValueTD + `{{.EventTypeName}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_with"}}</td>` + kvValueTD + `{{.HostName}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_when"}}</td>` + kvValueTD + `{{.WhenFmt}}</td></tr>
{{if .CancellationReason}}<tr>` + kvLabelTD + `{{.T "email_label_reason"}}</td>` + kvValueWrapTD + `{{.CancellationReason}}</td></tr>{{end}}
{{template "kvClose" .}}
{{template "rebook" .}}
{{template "note" .}}
{{end}}`)

var htmlCancelHost = content(`{{define "lead"}}A booking has been cancelled.{{end}}
{{define "content"}}
` + hostGreeting + `
{{template "kvOpen" .}}
<tr>` + kvLabelTD + `Event</td>` + kvValueTD + `{{.EventTypeName}}</td></tr>
<tr>` + kvLabelTD + `With</td>` + kvValueTD + `{{.OrganizerName}} &lt;{{.OrganizerEmail}}&gt;</td></tr>
<tr>` + kvLabelTD + `When</td>` + kvValueTD + `{{.WhenFmt}}</td></tr>
{{if .CancellationReason}}<tr>` + kvLabelTD + `Reason</td>` + kvValueWrapTD + `{{.CancellationReason}}</td></tr>{{end}}
{{template "kvClose" .}}
{{template "ref" .}}
{{end}}`)

var htmlRescheduleOrg = content(`{{define "lead"}}{{.T "email_rescheduled_lead"}}{{end}}
{{define "content"}}
` + leadBlock + `
{{template "kvOpen" .}}
<tr>` + kvLabelTD + `{{.T "email_label_event"}}</td>` + kvValueTD + `{{.EventTypeName}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_with"}}</td>` + kvValueTD + `{{.HostName}}</td></tr>
<tr>` + kvWasLabelTD + `{{.T "email_label_was"}}</td>` + kvWasValueTD + `{{.PreviousStartFmt}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_now"}}</td>` + kvValueTD + `{{.WhenFmt}}</td></tr>
{{if .LocationValue}}<tr>` + kvLabelTD + `{{.T "email_label_location"}}</td>` + kvValueWrapTD + `{{.LocationValue}}</td></tr>{{end}}
{{template "kvClose" .}}
{{template "mngBtn" .}}
{{template "calBtns" .}}
{{template "ref" .}}
{{template "note" .}}
{{end}}`)

var htmlRescheduleHost = content(`{{define "lead"}}A booking has been rescheduled.{{end}}
{{define "content"}}
` + hostGreeting + `
{{template "kvOpen" .}}
<tr>` + kvLabelTD + `Event</td>` + kvValueTD + `{{.EventTypeName}}</td></tr>
<tr>` + kvLabelTD + `With</td>` + kvValueTD + `{{.OrganizerName}} &lt;{{.OrganizerEmail}}&gt;</td></tr>
<tr>` + kvWasLabelTD + `Was</td>` + kvWasValueTD + `{{.PreviousStartFmt}}</td></tr>
<tr>` + kvLabelTD + `Now</td>` + kvValueTD + `{{.WhenFmt}}</td></tr>
{{if .LocationValue}}<tr>` + kvLabelTD + `Location</td>` + kvValueWrapTD + `{{.LocationValue}}</td></tr>{{end}}
{{template "kvClose" .}}
{{template "ref" .}}
{{end}}`)

var htmlReminderOrg = content(`{{define "lead"}}{{.T "email_reminder_lead"}}{{end}}
{{define "content"}}
` + leadBlock + `
{{template "kvOpen" .}}
<tr>` + kvLabelTD + `{{.T "email_label_event"}}</td>` + kvValueTD + `{{.EventTypeName}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_with"}}</td>` + kvValueTD + `{{.HostName}}</td></tr>
<tr>` + kvLabelTD + `{{.T "email_label_when"}}</td>` + kvValueTD + `{{.WhenFmt}}</td></tr>
{{if .LocationValue}}<tr>` + kvLabelTD + `{{.T "email_label_location"}}</td>` + kvValueWrapTD + `{{.LocationValue}}</td></tr>{{end}}
{{template "kvClose" .}}
{{template "mngBtn" .}}
{{template "calBtns" .}}
{{template "ref" .}}
{{template "note" .}}
{{end}}`)

// htmlByType maps RenderBody email types to their HTML template (attendee-facing).
func htmlByType(emailType string) *template.Template {
	switch emailType {
	case "confirmation":
		return htmlConfirmOrg
	case "cancellation":
		return htmlCancelOrg
	case "reschedule":
		return htmlRescheduleOrg
	case "reminder":
		return htmlReminderOrg
	}
	return nil
}
