package handler

import (
	_ "embed"
	"html/template"
	"net/http"

	"github.com/calnode/calnode/internal/i18n"
)

//go:embed templates/index.html
var indexTmplSrc string

// indexTmpl is parsed with the shared partials first, exactly as book and manage are, so
// this page gets the same footer, the same language switcher and the same colour tokens
// rather than a third copy of any of them.
var indexTmpl = template.Must(template.Must(template.New("index").Funcs(template.FuncMap{
	"supportedLocales": i18n.SupportedLocales,
}).Parse(sharedPartialsSrc)).Parse(indexTmplSrc))

// indexEventType is one bookable event type as the index lists it: enough to choose
// between them, and nothing more. No description, no host names, no price — this page is
// reachable by anyone who knows the hostname, and the booking page each row links to is
// where the workspace has already decided what a stranger may see.
type indexEventType struct {
	Slug          string
	Name          string
	DurationLabel string
	LocationLabel string
}

// indexPageData drives templates/index.html.
//
// GTMContainerID and GA4MeasurementID exist only to satisfy the shared legalFooter
// partial, which hides the "Cookie settings" button unless a tag is configured. They are
// deliberately never populated: this page renders no analytics and no operator head HTML,
// which is what lets it carry the strict CSP unconditionally.
type indexPageData struct {
	Locale string
	T      func(string) string

	CSSVersion string

	BusinessName  string
	LogoURL       string
	LogoHeight    int
	LogoOpacity   string
	BannerURL     string
	BannerOpacity string
	PrivacyURL    string
	TermsURL      string

	GTMContainerID   string
	GA4MeasurementID string

	EventTypes []indexEventType
}

// TenantIndex renders the neutral root of a workspace's public host: a list of its
// public, active event types, each linking to its booking page (M9).
//
// ⛔ It replaces a BARE 404, and only where one existed. With the admin console on, the
// root is still the redirect to /admin/ it has always been — this is the handler behind
// `GET /{$}` when ADMIN_SPA=off, which is a multi-tenant-only switch. So a self-hosted
// single-tenant instance is untouched, and no route was added: the registration in
// server.New is the same line, with a different handler behind it.
//
// ⚠️ WHAT IT DELIBERATELY IS NOT is a marketing page or a workspace profile. A tenant
// host answering 404 at its root reads as broken to anyone who trims a booking link back
// to the domain, which is the whole finding; a page that lists what is bookable answers
// that without publishing anything the booking pages do not already publish to the same
// audience. Hence: no host names, no descriptions, no counts, and `noindex` so the list
// does not become a directory of the operator's tenants in a search index.
//
// A workspace with nothing public renders the same page with one sentence rather than a
// 404 — the distinction between "no such host" and "this host has nothing to book right
// now" is one the visitor needs and the unknown-host 404 (which still fires, in Scoped,
// before this handler runs) already carries.
func (h *Handler) TenantIndex(w http.ResponseWriter, r *http.Request) {
	brand := h.loadBranding(r.Context())
	loc := h.resolveLocaleWithFallback(r, brand.FallbackLocale)

	// The same visibility predicate as BookPage, join included: an event type whose
	// owner row has gone would 404 there, so listing it here would send the visitor
	// to a dead link. Ordered by name (then slug, so the order is total and a test
	// can assert it) — there is no author-controlled ordering column to honour.
	//
	// ⛔ h.db — the WORKSPACE-BOUND handle Scoped built — and there is no workspace
	// predicate in this statement, deliberately, because that is D1: the binding is the
	// filter. This is the one public surface where getting it wrong shows up as somebody
	// else's data ON THE PAGE rather than as a 404, since every other host-scoped page
	// names its subject in the URL and an unbound read there returns the wrong single row.
	// Pinned by TestTenancy_theIndexListsOnlyItsOwnWorkspace against a real NOBYPASSRLS
	// role; swapping this for Platform() makes that test list both tenants.
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT et.slug, et.name, et.duration_minutes, et.location_type, COALESCE(et.location_value, '')
		FROM event_types et
		JOIN users u ON u.id = et.user_id
		WHERE et.is_active = 1 AND et.is_public = 1
		ORDER BY et.name ASC, et.slug ASC`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "tenant index: db query", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var events []indexEventType
	for rows.Next() {
		var slug, name, locType, locValue string
		var durMins int
		if err := rows.Scan(&slug, &name, &durMins, &locType, &locValue); err != nil {
			h.logger.ErrorContext(r.Context(), "tenant index: scan event type", "error", err)
			continue
		}
		events = append(events, indexEventType{
			Slug:          slug,
			Name:          name,
			DurationLabel: durationLabel(durMins, loc),
			LocationLabel: locationLabel(locType, locValue, loc),
		})
	}
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "tenant index: event type rows", "error", err)
	}

	data := indexPageData{
		Locale:        loc.Code,
		T:             loc.T,
		CSSVersion:    bookingCSSVersion,
		BusinessName:  brand.BusinessName,
		LogoURL:       brand.LogoURL,
		LogoHeight:    pageLogoHeight(brand.LogoHeight),
		LogoOpacity:   opacityCSS(brand.LogoOpacity),
		BannerURL:     brand.BannerURL,
		BannerOpacity: opacityCSS(brand.BannerOpacity),
		PrivacyURL:    brand.PrivacyURL,
		TermsURL:      brand.TermsURL,
		EventTypes:    events,
	}

	h.persistLangOverride(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// ⛔ strictPublicCSP, not publicCSP(track), and it is not an oversight. publicCSP
	// relaxes to fit an operator's head_html and their GA4/GTM tag, neither of which this
	// page renders — so reading the tracking settings here would let a stored value widen
	// the policy of a page that has nothing to widen it for.
	w.Header().Set("Content-Security-Policy", strictPublicCSP)
	w.Header().Set("X-Frame-Options", "DENY")
	// The body varies by resolved locale, exactly as the booking page's does: without
	// this a shared cache in front of the instance serves the first visitor's language
	// to everyone.
	w.Header().Set("Vary", "Accept-Language, Cookie")
	if err := indexTmpl.Execute(w, data); err != nil {
		h.logger.ErrorContext(r.Context(), "tenant index: template", "error", err)
	}
}
