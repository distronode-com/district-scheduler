package server

import (
	"net/http"
	"strings"
)

// The four header values, spelled once so the middleware and its tests cannot
// disagree about them.
const (
	// contentTypeOptions stops a browser from re-sniffing a declared type. It matters
	// most on the surfaces that were carrying nothing: /embed.js and /booking.css are
	// served to third-party sites, and a mis-sniffed stylesheet is a script.
	contentTypeOptions = "nosniff"

	// referrerPolicy sends the origin cross-site and the full URL same-site. The
	// stricter no-referrer was rejected as the global default because a booking page
	// legitimately links out to the operator's own privacy policy and terms, and an
	// operator reading their own referrer log is not a leak. The LiveKit room keeps
	// its own no-referrer — see the set-if-absent rule below.
	referrerPolicy = "strict-origin-when-cross-origin"

	// permissionsPolicy denies the three powerful features no booking surface uses.
	// ⛔ It is a DENY list of what we ship, not of everything a browser offers: an
	// unnamed feature is unaffected, so adding a feature to a page means adding it
	// here too rather than assuming the default covers it.
	permissionsPolicy = "camera=(), microphone=(), geolocation=()"

	// roomPermissionsPolicy is the LiveKit room's exception. It is a video meeting:
	// denying camera and microphone there would break the product, and a Permissions-Policy
	// denial is not recoverable from JavaScript — getUserMedia rejects and the page has
	// no way to ask again. Geolocation stays denied; the room has never wanted it.
	roomPermissionsPolicy = "camera=(self), microphone=(self), geolocation=()"

	// hstsPolicy is one year with subdomains. No `preload`: preloading is a submission
	// to a browser-maintained list that is slow and awkward to leave, and this header
	// is set by a binary a self-hoster runs on domains we know nothing about.
	hstsPolicy = "max-age=31536000; includeSubDomains"
)

// roomPathPrefix is the LiveKit room page, registered as `GET /room/{room}`.
const roomPathPrefix = "/room/"

// SecurityHeaders sets the response headers every surface of this instance should
// carry, at the mux root so that /book, /manage, /embed.js, /booking.css, the whole
// /v1 tree, the admin SPA and every 404 get them (M9).
//
// ⛔ It is at the ROOT rather than in the handlers, because the finding was that the
// handlers were the only place they existed: book.go, manage_handler.go and the SPA
// each set a subset, and everything else — the static assets a customer's site loads,
// every JSON error, every not-found — carried none. A per-handler header is a header
// that is missing from whatever nobody remembered to edit.
//
// ⚠️ SET BEFORE next, WHICH IS WHAT MAKES IT SET-IF-ABSENT. A handler that Sets one
// of these keys overwrites the value here, so a surface with a considered opinion keeps
// it: the LiveKit room's `Referrer-Policy: no-referrer` survives this middleware, and
// so does anything a future handler tightens. The alternative — a wrapping
// ResponseWriter that fills gaps at WriteHeader time — buys nothing and costs the
// Flush/Hijack transparency the logging wrapper already had to restore by hand.
//
// ⛔ HSTS IS CONDITIONAL, AND THE CONDITION IS NOT COSMETIC. Sent on a plain-http
// response it would still be ignored by browsers, but this binary is also run by
// self-hosters on a LAN, behind a proxy that terminates TLS on some hostnames and not
// others, and an accidental `includeSubDomains` pinned against a hostname reachable
// only over http locks that operator out of their own installation for a year with no
// way to retract it. So: a real TLS connection, or an explicit X-Forwarded-Proto of
// https, and otherwise nothing.
//
// ⚠️ The forwarded header is believed here WITHOUT consulting TRUSTED_PROXY_CIDRS,
// unlike the rate limiter's client IP, and that is a deliberate asymmetry. Forging it
// buys an attacker one thing: an HSTS header on a response, which a browser honours
// only when the response arrived over https in the first place — i.e. only when the
// header was true anyway. There is nothing to gain, and requiring a trusted-proxy
// allowlist would leave HSTS off on every deployment that has not configured one,
// which is most of them.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", contentTypeOptions)
		h.Set("Referrer-Policy", referrerPolicy)
		if strings.HasPrefix(r.URL.Path, roomPathPrefix) {
			h.Set("Permissions-Policy", roomPermissionsPolicy)
		} else {
			h.Set("Permissions-Policy", permissionsPolicy)
		}
		if requestIsTLS(r) {
			h.Set("Strict-Transport-Security", hstsPolicy)
		}
		next.ServeHTTP(w, r)
	})
}

// requestIsTLS reports whether this request reached us over https: either the
// connection itself is TLS, or a proxy in front said so.
//
// The comparison is case-insensitive and takes the FIRST value of a comma-separated
// X-Forwarded-Proto, which is what a chain of proxies appends to — the leftmost entry
// is the scheme the client used, and the ones after it describe hops that are behind
// TLS termination and would read as http.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}
