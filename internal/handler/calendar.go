package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/calnode/calnode/internal/caldav"
	"github.com/calnode/calnode/internal/calendar"
)

// stateSep separates the fields inside the (encrypted) OAuth state, so the shared
// callback can route to the right provider and, optionally, back to the platform console
// that started the round trip.
const stateSep = "\x1f"

// errReturnToNotAllowed is the one answer every rejected ?return_to= gets. It is
// deliberately a single message: an operator debugging a misconfiguration has the
// allowlist in front of them, and a caller who is probing learns nothing from a more
// specific one.
var errReturnToNotAllowed = errors.New("return_to origin not allowed")

// returnToFromRequest validates the optional ?return_to= on the connect URL and returns
// the value to carry in the state ("" when absent).
//
// Two rules, and the second is the one that is easy to get wrong:
//
//   - The origin must be in PLATFORM_RETURN_ORIGINS, compared WHOLE. A prefix match would
//     accept https://console.example.com.evil.test, and this list is the only thing
//     between the OAuth callback and an open redirect.
//   - With the list empty the feature is off and a return_to is REFUSED, not ignored. A
//     platform pointed at an instance nobody configured for it then finds out on the first
//     attempt, rather than on a landing page that silently belongs to someone else.
func (h *Handler) returnToFromRequest(r *http.Request) (string, error) {
	raw := r.URL.Query().Get("return_to")
	if raw == "" {
		return "", nil
	}
	if len(h.platformReturnOrigins) == 0 {
		return "", errReturnToNotAllowed
	}
	// Refused before it is encoded rather than after it is decoded: a separator inside the
	// value would let a return_to carry a fourth field into a two-separator parse. (Go's
	// url.Parse already rejects ASCII control characters, so this is the belt behind the
	// braces — and it is the belt that is checked by a test.)
	if strings.Contains(raw, stateSep) {
		return "", errReturnToNotAllowed
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil {
		return "", errReturnToNotAllowed
	}
	origin := u.Scheme + "://" + u.Host
	for _, allowed := range h.platformReturnOrigins {
		if origin == allowed {
			return raw, nil
		}
	}
	return "", errReturnToNotAllowed
}

// encodeCalendarState builds the plaintext that goes into the provider's EncryptState:
// provider\x1fuserID, plus \x1freturnTo when there is one.
//
// ⚠️ The third field is appended only when it is non-empty, so an instance with the
// feature off mints exactly the two-field state it always did. That is not tidiness: a
// state minted by this code and handed back to an OLDER binary mid-deploy is parsed by a
// single strings.Index, which would read a trailing separator as part of the user id and
// exchange against a user that does not exist. The parse side treats one, two and three
// fields alike, so nothing here depends on the third field always being present.
func encodeCalendarState(provider, userID, returnTo string) string {
	s := provider + stateSep + userID
	if returnTo != "" {
		s += stateSep + returnTo
	}
	return s
}

// parseCalendarState splits a decrypted state into its three fields.
//
// At most two separators, so the third field keeps any it contains rather than being
// truncated, and so the two shapes that predate this function still parse: a bare userID
// (before the provider was encoded) and provider\x1fuserID (an OAuth round trip that was
// in flight across the deploy that added the third field).
func parseCalendarState(raw string) (provider, userID, returnTo string) {
	parts := strings.SplitN(raw, stateSep, 3)
	switch len(parts) {
	case 1:
		return "", parts[0], ""
	case 2:
		return parts[0], parts[1], ""
	default:
		return parts[0], parts[1], parts[2]
	}
}

// ConnectCalendar handles GET /v1/calendar/connect (auth required).
// Redirects the browser to the chosen provider's OAuth consent page.
// Optional ?provider=<name> selects a provider; defaults to the primary.
//
// Optional ?return_to=<absolute URL> asks the callback to finish on a platform console
// instead of this instance's /admin/calendar. It is checked against
// PLATFORM_RETURN_ORIGINS HERE, on an authenticated request, and then carried inside the
// encrypted state — which is what makes the callback's redirect safe: the destination was
// chosen and checked at connect time, not read off the callback's own URL.
func (h *Handler) ConnectCalendar(w http.ResponseWriter, r *http.Request) {
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}
	p := svc.Primary()
	if name := r.URL.Query().Get("provider"); name != "" {
		if pr := svc.Provider(name); pr != nil {
			p = pr
		} else {
			h.writeError(w, http.StatusBadRequest, "unknown calendar provider")
			return
		}
	}
	returnTo, err := h.returnToFromRequest(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, _ := userFromContext(r.Context())
	// Encode the provider in the state so the shared callback routes correctly, and the
	// return_to so it knows where to finish.
	state, err := p.EncryptState(encodeCalendarState(p.Name(), user.ID, returnTo))
	if err != nil {
		h.logger.ErrorContext(r.Context(), "calendar connect: encrypt state", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, p.AuthURL(state), http.StatusFound) // #nosec G710 -- AuthURL is the provider's own fixed OAuth authorize endpoint (oauth2.Config.AuthCodeURL); only our own encrypted state is appended, no attacker-controlled destination
}

// The reason codes appended to a return_to when the round trip fails. Short, stable and
// machine-readable: the platform console is what reads them, and it shows its own copy.
const (
	reasonProviderDenied = "provider_denied" // the provider sent ?error= instead of a code
	reasonMissingCode    = "missing_code"    // no ?code= to exchange
	reasonExchangeFailed = "exchange_failed" // the provider refused the code
	reasonInvalidState   = "invalid_state"   // the state decrypted but named nobody reachable
)

// returnToWith appends one result to a validated return_to.
//
// The existing query is kept VERBATIM rather than re-encoded through url.Values, so a
// platform's own parameters come back exactly as it wrote them, and the fragment stays
// last — a naive "?" / "&" concatenation would append after a fragment and lose the lot.
func returnToWith(returnTo, result string) string {
	u, err := url.Parse(returnTo)
	if err != nil {
		// Unreachable: this value parsed at connect time, before it was encrypted. Answer
		// with what we have rather than inventing a destination.
		return returnTo
	}
	if u.RawQuery == "" {
		u.RawQuery = result
	} else {
		u.RawQuery += "&" + result
	}
	return u.String()
}

// finishReturnTo sends the browser back to the platform console. Only ever called with a
// returnTo that came out of the DECRYPTED state, which is the whole safety argument: it
// was checked against PLATFORM_RETURN_ORIGINS at connect time by an authenticated user,
// and nothing on the callback's own URL can influence it.
func (h *Handler) finishReturnTo(w http.ResponseWriter, r *http.Request, returnTo, result string) {
	http.Redirect(w, r, returnToWith(returnTo, result), http.StatusFound) // #nosec G710 -- returnTo comes out of the encrypted OAuth state, allowlisted at connect time; the callback's query cannot reach it
}

// CalendarCallback handles GET /v1/calendar/callback (public — browser redirect from the provider).
// Validates the encrypted state, exchanges the auth code, and persists tokens.
//
// ⛔ The state is decrypted FIRST, before anything else about the request is acted on,
// because the state is the only trustworthy thing on this URL — it is what says where the
// person should end up. When it carries a return_to, every outcome is a 302 back to the
// platform console carrying `?calendar=connected` or `?calendar=error&reason=<code>`;
// without one, every outcome is exactly what it has always been (JSON errors, and the
// redirect to this workspace's own /admin/calendar).
//
// ⚠️ The provider's ?error= branch is handled after the decryption but BEFORE the state is
// judged invalid. Order matters twice: after, so a denied consent with a return_to goes
// home instead of answering JSON; before, so a denial that arrives with no state at all —
// which is what a user clicking "Cancel" produces — still answers the same
// "OAuth error: <param>" it always did rather than "invalid or missing state".
func (h *Handler) CalendarCallback(w http.ResponseWriter, r *http.Request) {
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}

	// All providers share the encryption key, so any provider's DecryptState
	// recovers the state; we then resolve the provider it encodes.
	raw, stateErr := svc.Primary().DecryptState(r.URL.Query().Get("state"))
	p := svc.Primary()
	var userID, returnTo string
	if stateErr == nil && raw != "" {
		var providerName string
		providerName, userID, returnTo = parseCalendarState(raw)
		if pr := svc.Provider(providerName); pr != nil {
			p = pr
		}
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		if returnTo != "" {
			h.finishReturnTo(w, r, returnTo, "calendar=error&reason="+reasonProviderDenied)
			return
		}
		h.writeError(w, http.StatusBadRequest, "OAuth error: "+errParam)
		return
	}

	if stateErr != nil || raw == "" {
		// No usable state means no usable return_to either: there is nowhere to send the
		// browser that did not come off this request's own query.
		h.writeError(w, http.StatusBadRequest, "invalid or missing state")
		return
	}
	if userID == "" {
		if returnTo != "" {
			h.finishReturnTo(w, r, returnTo, "calendar=error&reason="+reasonInvalidState)
			return
		}
		h.writeError(w, http.StatusBadRequest, "invalid or missing state")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		if returnTo != "" {
			h.finishReturnTo(w, r, returnTo, "calendar=error&reason="+reasonMissingCode)
			return
		}
		h.writeError(w, http.StatusBadRequest, "missing code")
		return
	}

	// ⛔ The workspace comes from the STATE, via the user it names, and the exchange runs
	// bound to it. This route is Platform-wrapped — the callback arrives on the identity host
	// with no tenant Host and no session — so h and its calendar providers hold the UNBOUND
	// handle, which under the policies writes nothing: the connection would appear to succeed
	// and no calendar_connections row would exist.
	//
	// The state is encrypted (the provider's own EncryptState), so the user id inside it
	// cannot be forged, and workspaceOfUser resolves the tenant from it on the platform
	// handle. That is why the workspace is not ALSO carried in the state: it would be a second
	// copy of a fact the user id already settles, and two sources of one truth is how they
	// come to disagree.
	scoped := h
	if h.multiTenant {
		wsID, wsErr := h.workspaceOfUser(r.Context(), userID)
		if wsErr != nil {
			h.logger.ErrorContext(r.Context(), "calendar callback: resolve workspace",
				"error", wsErr, "user_id", userID)
			if returnTo != "" {
				h.finishReturnTo(w, r, returnTo, "calendar=error&reason="+reasonInvalidState)
				return
			}
			h.writeError(w, http.StatusBadRequest, "invalid or missing state")
			return
		}
		ws, wsErr := h.workspaceByID(r.Context(), wsID)
		if wsErr != nil {
			h.logger.ErrorContext(r.Context(), "calendar callback: read workspace",
				"error", wsErr, "workspace_id", wsID)
			// Also invalid_state: from the console's point of view the state named a
			// workspace this instance cannot resolve, which is the same answer whether the
			// user row or the workspace row is the one missing. The JSON path keeps its 500,
			// because there the distinction is an operator's to act on.
			if returnTo != "" {
				h.finishReturnTo(w, r, returnTo, "calendar=error&reason="+reasonInvalidState)
				return
			}
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		scoped = h.forWorkspace(ws)
		// The provider has to be the workspace's own, not the process-wide registry's: each
		// captures a *db.DB at construction (B5, calendar.Provider.ForDB).
		if svcScoped := scoped.getCal(); svcScoped != nil {
			if pr := svcScoped.Provider(p.Name()); pr != nil {
				p = pr
			}
		}
	}

	if err := p.Exchange(r.Context(), userID, code, "primary"); err != nil {
		h.logger.ErrorContext(r.Context(), "calendar callback: exchange", "error", err, "user_id", userID)
		if returnTo != "" {
			h.finishReturnTo(w, r, returnTo, "calendar=error&reason="+reasonExchangeFailed)
			return
		}
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Multi-calendar: connecting is additive. The first connection becomes the destination
	// (handled in the provider's saveToken); subsequent ones are conflict-check only.

	if returnTo != "" {
		h.finishReturnTo(w, r, returnTo, "calendar=connected")
		return
	}

	// Back to the workspace's own admin UI, which is on its public host — h.baseURL is the
	// identity host and would send the person somewhere their session does not exist.
	http.Redirect(w, r, scoped.publicURL()+"/admin/calendar?connected=true", http.StatusFound)
}

// ConnectCalDAV handles POST /v1/calendar/caldav/connect (auth required). CalDAV is
// credential-based (no OAuth redirect): the host supplies a server (a preset like "icloud"/
// "fastmail" or a full server URL for Nextcloud/custom), their username, and an app-specific
// password. We discover their calendar, validate the credentials, and store the connection.
// Body: {"preset": "...", "server_url": "...", "username": "...", "app_password": "..."}.
func (h *Handler) ConnectCalDAV(w http.ResponseWriter, r *http.Request) {
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}
	cc, ok := svc.Provider("caldav").(*caldav.Client)
	if !ok || cc == nil {
		h.writeError(w, http.StatusNotImplemented, "CalDAV is not available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Preset    string `json:"preset"`
		ServerURL string `json:"server_url"`
		Username  string `json:"username"`
		Password  string `json:"app_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	server := strings.TrimSpace(req.ServerURL)
	if server == "" {
		if p, okp := caldav.Presets[strings.ToLower(strings.TrimSpace(req.Preset))]; okp {
			server = p
		}
	}
	if server == "" {
		h.writeError(w, http.StatusBadRequest, "choose a provider or enter a server URL")
		return
	}
	// SSRF guard: a self-hosted CalDAV server on the operator's own private network (or
	// localhost) is a legitimate destination, so this only blocks the cloud-metadata
	// range — see validateBYOServerURL (shared with the BYO-LLM and LiveKit URL checks).
	// The caldav.Client's own http.Client re-validates at dial time
	// (internal/caldav/caldav.go), and the cc.Connect below immediately exercises the
	// URL, so a save-time DNS blip surfaces there as a user-actionable error.
	//
	// ⛔ THE SCHEME LIST IS PER MODE (M1). CalDAV Basic auth puts the person's
	// app-specific password on the wire in every request, so `http://` is a credential
	// disclosure — but on a single-tenant instance it is the operator's own password
	// going to their own server on their own network, which is a call they are entitled
	// to make and have always been able to. On a multi-tenant instance the password
	// belongs to a TENANT and the hop leaves the operator's network, so only https is
	// accepted. Same refusal shape either way: validateBYOServerURL names the field and
	// the schemes it will take.
	schemes := []string{"http", "https"}
	if h.multiTenant {
		schemes = []string{"https"}
	}
	if err := validateBYOServerURL(r.Context(), server, "server URL", schemes...); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, _ := userFromContext(r.Context())
	email, _, err := cc.Connect(r.Context(), user.ID, server, req.Username, req.Password)
	if err != nil {
		// Discovery/auth failures are user-actionable — surface the message to the form.
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"connected": true, "account_email": email})
}

// unconfiguredProviders reports which OAuth calendar providers Calnode supports but this
// instance hasn't registered — so the admin UI can show "not set up yet" rather than
// silently omitting them. svc may be nil (calendar entirely unconfigured), in which case
// every OAuth provider is unconfigured. CalDAV needs no instance-level credentials (each
// host supplies their own at connect-time), so it's never listed here.
func unconfiguredProviders(svc *calendar.Service) []string {
	var out []string
	if svc == nil || svc.Provider("google") == nil {
		out = append(out, "google")
	}
	if svc == nil || svc.Provider("microsoft") == nil {
		out = append(out, "microsoft")
	}
	return out
}

// CalendarStatus handles GET /v1/calendar/status (auth required). Returns the user's full
// list of connected calendars (many may be checked for conflicts; exactly one is the
// destination), which providers are available to connect, and which supported providers
// this instance hasn't been configured with credentials for yet.
func (h *Handler) CalendarStatus(w http.ResponseWriter, r *http.Request) {
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeJSON(w, http.StatusOK, map[string]any{
			"connected": false, "configured": false, "connections": []any{},
			"unconfigured_providers": unconfiguredProviders(svc),
		})
		return
	}
	user, _ := userFromContext(r.Context())
	conns, err := svc.Connections(r.Context(), user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "calendar status", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if conns == nil {
		conns = []calendar.Connection{}
	}
	// Back-compat fields: `connected` + `provider` reflect the destination connection.
	var destProvider string
	for _, c := range conns {
		if c.IsDestination {
			destProvider = c.Provider
		}
	}
	resp := map[string]any{
		"connected":              len(conns) > 0,
		"configured":             true,
		"providers":              svc.ProviderNames(),
		"connections":            conns,
		"unconfigured_providers": unconfiguredProviders(svc),
	}
	if destProvider != "" {
		resp["provider"] = destProvider
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// SetCalendarDestination handles POST /v1/calendar/connections/{id}/destination (auth) —
// choose which connected calendar bookings are written to.
func (h *Handler) SetCalendarDestination(w http.ResponseWriter, r *http.Request) {
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}
	user, _ := userFromContext(r.Context())
	// Account identity, not the {id} path value: that id is recreated on every token
	// refresh, and opening the calendar picker can trigger one, so a page loaded moments
	// earlier holds a dead id. The {id} stays in the route for URL shape only.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var destReq struct {
		Provider string `json:"provider"`
		Account  string `json:"account_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&destReq); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if destReq.Provider == "" {
		h.writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if err := svc.SetDestination(r.Context(), user.ID, destReq.Provider, destReq.Account); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.writeError(w, http.StatusNotFound, "calendar connection not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "calendar set destination", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DisconnectCalendarConnection handles DELETE /v1/calendar/connections/{id} (auth) —
// disconnect one calendar; promotes another to destination if this was it.
func (h *Handler) DisconnectCalendarConnection(w http.ResponseWriter, r *http.Request) {
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}
	user, _ := userFromContext(r.Context())
	// Account identity, not the volatile {id} - same reason as SetCalendarDestination.
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		h.writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if err := svc.DisconnectOne(r.Context(), user.ID, provider, r.URL.Query().Get("account")); err != nil {
		h.logger.ErrorContext(r.Context(), "calendar disconnect one", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DisconnectCalendar handles DELETE /v1/calendar (auth required) — disconnects ALL of the
// user's calendars (kept for convenience / "remove everything").
func (h *Handler) DisconnectCalendar(w http.ResponseWriter, r *http.Request) {
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}
	user, _ := userFromContext(r.Context())
	if err := svc.Disconnect(r.Context(), user.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "calendar disconnect", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetConnectionCalendars handles GET /v1/calendar/connections/{id}/calendars (auth) —
// lists the account's calendars, each with the user's saved conflict/destination choice.
func (h *Handler) GetConnectionCalendars(w http.ResponseWriter, r *http.Request) {
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}
	user, _ := userFromContext(r.Context())
	// Keyed on account identity, not the {id} path value: a calendar_connections.id is
	// volatile (recreated on token refresh, which listing calendars can itself trigger).
	provider := r.URL.Query().Get("provider")
	account := r.URL.Query().Get("account")
	if provider == "" {
		h.writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	cals, err := svc.AccountCalendars(r.Context(), user.ID, provider, account)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.writeError(w, http.StatusNotFound, "calendar connection not found")
			return
		}
		if calendar.IsReauthErr(err) {
			// The stored OAuth grant is dead (revoked/expired/security interrupt) — this is a
			// reconnect, not an outage. 409 so the UI can flag the account distinctly.
			h.writeError(w, http.StatusConflict, "This calendar needs reconnecting — disconnect it and connect again.")
			return
		}
		h.logger.ErrorContext(r.Context(), "list connection calendars", "error", err)
		h.writeError(w, http.StatusBadGateway, "could not reach the calendar provider")
		return
	}
	if cals == nil {
		cals = []calendar.CalendarSelection{}
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"calendars": cals})
}

// PutConnectionCalendars handles PUT /v1/calendar/connections/{id}/calendars (auth) —
// saves which of the account's calendars count for conflicts and which is the write target.
func (h *Handler) PutConnectionCalendars(w http.ResponseWriter, r *http.Request) {
	svc := h.getCal()
	if svc == nil || !svc.Any() {
		h.writeError(w, http.StatusNotImplemented, "Calendar integration not configured")
		return
	}
	user, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Provider  string                       `json:"provider"`
		Account   string                       `json:"account_email"`
		Calendars []calendar.CalendarSelection `json:"calendars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Provider == "" {
		h.writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	// Account identity, not the volatile {id} path value (see GetConnectionCalendars).
	if err := svc.SetAccountCalendars(r.Context(), user.ID, req.Provider, req.Account, req.Calendars); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.writeError(w, http.StatusNotFound, "calendar connection not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "set connection calendars", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
