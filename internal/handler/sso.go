package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/uid"
)

const (
	// ssoMaxTokenLifetime caps exp - iat. A hand-off is a redirect the browser follows
	// immediately, so it needs seconds, not minutes.
	ssoMaxTokenLifetime = 60 * time.Second

	// ssoClockSkew is how far the two systems' clocks may disagree before a token is
	// refused, applied to both ends of the window.
	ssoClockSkew = 30 * time.Second

	// ssoDefaultNext is where a hand-off lands when no ?next= is given.
	ssoDefaultNext = "/admin/"
)

// SetSSOSecret configures the shared secret for the SSO hand-off endpoint. An empty
// secret leaves the endpoint off (it 404s). Set once at boot from config, like
// SetBaseURL and SetEncKey, so there is no lock here.
func (h *Handler) SetSSOSecret(secret string) {
	h.ssoSecret = secret
}

// SetAdminSPA records whether the embedded admin console is served, so the hand-off
// below knows whether its default destination exists. Pass config.AdminSPAEnabled(),
// not config.AdminSPA: the single-tenant rule belongs in one place. Set once at boot
// from config, like SetSSOSecret, so there is no lock here.
//
// The argument is the positive sense and the field is the negative one, because the
// default has to be "served" for a handler nobody called this on (see adminSPAOff).
func (h *Handler) SetAdminSPA(on bool) { h.adminSPAOff = !on }

// ssoClaims is the token payload. Every field except wid is required.
type ssoClaims struct {
	Iss  string `json:"iss"`  // issuing system, any non-empty string; logged, never authorised on
	Aud  string `json:"aud"`  // must equal this instance's BASE_URL
	Sub  string `json:"sub"`  // the person's email address
	Name string `json:"name"` // display name, used when the user is created
	Role string `json:"role"` // owner | admin | member
	Iat  int64  `json:"iat"`  // issued at, seconds since the epoch
	Exp  int64  `json:"exp"`  // expires at, seconds since the epoch
	JTI  string `json:"jti"`  // unique per token; replay is refused

	// WID names the workspace the hand-off lands in.
	//
	// Ignored in single-tenant mode, where there is one workspace and nothing to
	// select — a caller written for a multi-tenant fleet therefore works unchanged
	// against a single instance. REQUIRED in multi-tenant mode: /v1/auth/sso is a
	// Platform route, so there is no tenant Host to resolve from and no credential
	// yet — the token IS the credential and this claim is the only statement of
	// which tenant it is for (D11).
	WID string `json:"wid"`
}

// signSSOToken produces the compact HS256 JWS the verifier below accepts. It is the mint
// half of D11, used by the OAuth callbacks.
//
// ⛔ Deliberately NOT a JWT library, for the same reason verifySSOToken is not one: one
// algorithm, one key, a fixed claim set, and no dependency to keep current. The two halves
// live beside each other so a change to either is made looking at the other.
func signSSOToken(claims ssoClaims, secret string) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("sso: marshal claims: %w", err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// The three ways the wid claim fails. All three are 401 with the message in the body:
// the token is the caller's, and so is the mistake. They are sentinels rather than
// strings so SSOHandoff can tell them from a database error, which is a 500.
var (
	errSSOWIDRequired  = errors.New("wid is required")
	errSSOUnknownWID   = errors.New("wid does not name a workspace")
	errSSONoPublicHost = errors.New("wid names a workspace with no public host")
)

// SSOHandoff handles GET /v1/auth/sso?token=<jwt>[&next=/path] — the signed session
// hand-off. An external identity system that has already authenticated a person hands
// them into a Calnode session, without a second login.
//
// The token is a compact HS256 JWT signed with a secret the two systems share
// (CALNODE_SSO_SHARED_SECRET); the endpoint is off unless that is set. It is verified
// here rather than by a JWT library for the same reason internal/livekit signs its own:
// one algorithm, one key, a fixed claim set, and no dependency to keep current. Only
// HS256 is accepted — "none" and every asymmetric alg are refused before the signature
// is looked at, which is the classic JWT downgrade.
//
// Two properties do the security work, and neither is optional:
//
//   - The token is short-lived (exp at most 60s after iat) so a captured URL is not a
//     standing credential. 30s of clock skew is allowed in both directions, because the
//     two systems are separate hosts and NTP is not a guarantee.
//   - The token is single-use: its jti is claimed in sso_nonces before the session is
//     created, so a replay inside the validity window loses on the primary key.
//
// This is the ONE path that creates a user without an invite (see ssoResolveUser).
// Everywhere else an unknown email is refused; the shared secret is what makes this
// different — the caller is the operator's own identity system, not an arbitrary
// visitor with a Google account.
//
// Success is a 302 into the admin app (or ?next=, when that is a same-origin absolute
// path). Every failure is a 401 with a JSON body naming the claim that failed, so an
// operator wiring this up can tell a clock problem from a wrong audience — the body
// never carries the secret, the signature, or the token.
func (h *Handler) SSOHandoff(w http.ResponseWriter, r *http.Request) {
	if h.ssoSecret == "" {
		// Off unless CALNODE_SSO_SHARED_SECRET is set, and 404 rather than 501 so an
		// instance that has not configured SSO is indistinguishable from one that does
		// not implement it. Nothing is disclosed to a prober either way.
		h.writeError(w, http.StatusNotFound, "not found")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, "token is required")
		return
	}

	claims, err := verifySSOToken(token, h.ssoSecret)
	if err != nil {
		h.logger.WarnContext(r.Context(), "sso: token rejected", "reason", err.Error())
		h.writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	// The workspace comes from the token, after the signature and before anything is
	// written. Resolved here rather than in Scoped because this route is Platform: no
	// tenant Host, no credential yet, so the `wid` claim is the only source (D11).
	ws, err := h.ssoWorkspace(r.Context(), r, claims)
	switch {
	case errors.Is(err, errWorkspaceSuspended), errors.Is(err, errUnknownHost), errors.Is(err, errWorkspaceMismatch):
		// 503, 404 and 403 respectively, from the one place that maps a resolution failure
		// to a response. A mismatch is 403 {"error":"workspace mismatch"} (D10), the same
		// answer an API key from another workspace gets on this host.
		h.writeResolveError(w, r, err)
		return
	case errors.Is(err, errSSOWIDRequired), errors.Is(err, errSSOUnknownWID), errors.Is(err, errSSONoPublicHost):
		h.logger.WarnContext(r.Context(), "sso: workspace rejected", "reason", err.Error(), "iss", claims.Iss)
		h.writeError(w, http.StatusUnauthorized, err.Error())
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "sso: resolve workspace", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := claims.validate(h.ssoAudience(ws), time.Now()); err != nil {
		h.logger.WarnContext(r.Context(), "sso: claims rejected", "reason", err.Error(), "iss", claims.Iss)
		h.writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	// ⛔ With the admin console switched off (ADMIN_SPA=off), every route under /admin
	// 404s — ssoDefaultNext included. Refusing here rather than redirecting into it is the
	// difference between a hand-off that failed and a hand-off that succeeded into a dead
	// end: the session cookie would already be set, the jti already spent, and the person
	// would be looking at a 404 on a host whose console is somewhere else entirely. So
	// this runs before the nonce is claimed and before any session exists, for the same
	// reason the ?next= validation below does.
	//
	// ⛔ AN EXPLICIT next UNDER /admin IS REFUSED TOO, AND THIS USED TO SAY THE OPPOSITE
	// (H3). The old rule was "the caller said where to land", which reads as deference and
	// was in fact a bypass: both native clients send `next=/admin/` on every hand-off
	// (iOS SchedulingSSOClient, Android SchedulingRepository), so the guard fired for
	// nobody. Worse, the failure was invisible — the app sees a 302, opens the sheet, and
	// shows a bare 404 with no error path taken, because only a non-3xx opens one. A probe
	// asserting the hand-off answers 3xx stays green straight through the flip.
	//
	// A non-admin next is unchanged, which is the case that matters: the calendar-connect
	// round trip (`?next=/v1/calendar/connect?provider=…&return_to=…`) is why explicit
	// destinations exist at all.
	rawNext := r.URL.Query().Get("next")
	if h.adminSPAOff && (rawNext == "" || nextIsAdminConsole(rawNext)) {
		h.logger.WarnContext(r.Context(), "sso: hand-off would land on a console this instance does not serve",
			"iss", claims.Iss, "had_next", rawNext != "")
		h.writeError(w, http.StatusNotFound,
			"this instance does not serve the admin console; the hand-off needs an explicit next outside /admin")
		return
	}

	// Validated before the nonce is claimed: a bad ?next= is the caller's own bug, and
	// burning the token over it would make the retry fail for a second, unrelated reason.
	next, err := ssoNextPath(rawNext)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Claim the jti before anything is created. On a replay this is the statement that
	// fails, and it fails on the primary key rather than on a read-then-write the second
	// request could interleave with.
	if _, err := h.db.ExecContext(r.Context(),
		`INSERT INTO sso_nonces (jti, expires_at) VALUES (?, ?)`,
		claims.JTI, time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339)); err != nil {
		if db.IsUniqueViolation(err) {
			h.logger.WarnContext(r.Context(), "sso: token replayed", "iss", claims.Iss)
			h.writeError(w, http.StatusUnauthorized, "jti has already been used")
			return
		}
		h.logger.ErrorContext(r.Context(), "sso: claim nonce", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	userID, created, err := h.ssoResolveUser(r.Context(), claims, ws.ID)
	switch {
	case errors.Is(err, errSSOArchived):
		h.writeError(w, http.StatusUnauthorized, "account is archived")
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "sso: resolve user", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The session names its workspace for the same reason the user row does: on a
	// Platform route h.db binds nothing, and a session in the wrong workspace cannot
	// be read or deleted by the bound handles that serve its owner's requests.
	if err := h.createSessionIn(r.Context(), w, userID, ws.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "sso: create session", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.logger.InfoContext(r.Context(), "sso: session handed off",
		"iss", claims.Iss, "user_id", userID, "user_created", created, "workspace_id", ws.ID)
	http.Redirect(w, r, next, http.StatusFound)
}

// ssoWorkspace decides which workspace a hand-off lands in.
//
// Single-tenant: the one workspace, and `wid` is ignored (see the claim's comment).
//
// ⛔ Multi-tenant: the workspace of the REQUEST HOST, with the token's `wid` checked against
// it. Resolving from the host rather than from the token is what makes a stolen or
// misdirected token useless: this endpoint is reached at `https://<public_host>/v1/auth/sso`
// precisely so the session cookie lands on the tenant's own domain, so a token for workspace
// A presented on workspace B's host must be REFUSED (403) rather than quietly creating A's
// session on B's domain. Trusting `wid` alone would do exactly that.
func (h *Handler) ssoWorkspace(ctx context.Context, r *http.Request, claims ssoClaims) (*Workspace, error) {
	if !h.multiTenant {
		return DefaultWorkspace, nil
	}
	ws, err := h.workspaceByHost(ctx, r.Host)
	if err != nil {
		return nil, err
	}
	if ws.Suspended() {
		return nil, fmt.Errorf("%w: %s", errWorkspaceSuspended, ws.ID)
	}
	if ws.PublicHost == "" {
		return nil, errSSONoPublicHost
	}
	wid := strings.TrimSpace(claims.WID)
	if wid == "" {
		return nil, errSSOWIDRequired
	}
	if wid != ws.ID {
		return nil, fmt.Errorf("%w: token names %s, host %s is %s", errWorkspaceMismatch, wid, ws.PublicHost, ws.ID)
	}
	return ws, nil
}

// ssoAudience is the audience a token must name to be spendable here.
//
// Single-tenant: BASE_URL, unchanged — one host, one instance, and binding the token
// to it is what stops a token minted for staging being spent on production when the
// two share a secret by mistake.
//
// Multi-tenant: the WORKSPACE's own public host (D11). The hand-off exists precisely
// because the identity host cannot set a cookie for a tenant's domain, so the token is
// minted for that domain — and a token for tenant A must not be spendable on tenant
// B's, which would seat a session on B for a person A vouched for. Exact match, since
// the minting side is ours.
func (h *Handler) ssoAudience(ws *Workspace) string {
	if h.multiTenant && ws != nil && ws.PublicHost != "" {
		return "https://" + ws.PublicHost
	}
	return h.baseURL
}

// errSSOArchived reports that the token named a real but offboarded user. Archived
// means no login by every other path (see §6), and a shared secret does not change that.
var errSSOArchived = errors.New("sso: user is archived")

// ssoResolveUser maps the token's sub to a user id in workspaceID, creating the user
// when the email is unknown there. It reports whether the user was created.
//
// ⛔ Every statement names workspaceID, and that is not decoration. /v1/auth/sso is a
// Platform route, so h.db is the platform handle: it bypasses the policies, and it
// binds ” so a column left unnamed does not default to this tenant. Since D9 the
// unique on users is (workspace_id, email), which means the same address legitimately
// exists in several workspaces — an unqualified `WHERE email = ?` would resolve an
// arbitrary one of them and hand this token a session on somebody else's tenant. It is
// the same hole reset-admin had, reachable here by anyone holding the shared secret.
//
// Role handling is deliberately asymmetric. On creation the claim's role is applied, so
// the identity system provisions people directly. On an existing user the role is left
// alone — a workspace's roles are the workspace's business, and letting a hand-off
// rewrite them on every sign-in would make the admin UI's role controls advisory. The
// single exception is bootstrapping: a claim asking for owner is honoured when the
// workspace has no owner, because the one-owner invariant TransferOwnership maintains
// means there is nothing to displace.
func (h *Handler) ssoResolveUser(ctx context.Context, claims ssoClaims, workspaceID string) (userID string, created bool, err error) {
	email := strings.ToLower(strings.TrimSpace(claims.Sub))

	var archivedAt sql.NullString
	err = h.db.QueryRowContext(ctx,
		`SELECT id, archived_at FROM users WHERE workspace_id = ? AND email = ?`,
		workspaceID, email).Scan(&userID, &archivedAt)
	switch {
	case err == sql.ErrNoRows:
		// Unknown email: create. This is the one path that creates a user without an
		// invite; it is reachable only by a caller holding the shared secret.
		isAdmin, isOwner := 0, 0
		switch claims.Role {
		case "owner":
			isAdmin = 1
			if !h.ssoOwnerExists(ctx, workspaceID) {
				isOwner = 1
			}
		case "admin":
			isAdmin = 1
		}
		userID = uid.New()
		if _, err := h.db.ExecContext(ctx, `
			INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner, email_login)
			VALUES (?, ?, ?, ?, 'UTC', ?, ?, 0)`,
			userID, workspaceID, email, claims.Name, isAdmin, isOwner); err != nil {
			// ⛔ The ssoOwnerExists check above is a read, and the INSERT is a write, so
			// two hand-offs claiming owner on an unowned workspace can both pass it.
			// idx_users_one_owner_per_workspace decides that case, and losing it means
			// somebody else became the owner between the read and this statement. Retry
			// as an admin rather than failing the sign-in: the person is legitimate and
			// the workspace now has the owner it needed. Failing closed on the ROLE, not
			// on the request.
			if isOwner == 1 && db.IsUniqueViolation(err) {
				h.logger.WarnContext(ctx, "sso: owner bootstrap lost the race; creating as admin",
					"workspace_id", workspaceID)
				if _, err := h.db.ExecContext(ctx, `
					INSERT INTO users (id, workspace_id, email, name, iana_timezone, is_admin, is_owner, email_login)
					VALUES (?, ?, ?, ?, 'UTC', 1, 0, 0)`,
					userID, workspaceID, email, claims.Name); err != nil {
					return "", false, err
				}
				return userID, true, nil
			}
			return "", false, err
		}
		return userID, true, nil

	case err != nil:
		return "", false, err
	}

	if archivedAt.Valid {
		return "", false, errSSOArchived
	}
	if claims.Role == "owner" && !h.ssoOwnerExists(ctx, workspaceID) {
		if _, err := h.db.ExecContext(ctx,
			`UPDATE users SET is_owner = 1, is_admin = 1 WHERE workspace_id = ? AND id = ?`,
			workspaceID, userID); err != nil {
			// Same race as the create path, decided the same way. The admin half of the
			// promotion is dropped along with the owner half, which is deliberate: this
			// branch exists to bootstrap an OWNER, and the claim's role is not otherwise
			// allowed to rewrite an existing user's permissions (see the doc comment).
			if db.IsUniqueViolation(err) {
				h.logger.WarnContext(ctx, "sso: owner bootstrap lost the race; leaving the role alone",
					"workspace_id", workspaceID)
				return userID, false, nil
			}
			return "", false, err
		}
	}
	return userID, false, nil
}

// ssoOwnerExists reports whether workspaceID already has an owner. A read error is
// reported as "yes" so a failed check never hands out ownership.
//
// Scoped for the same reason as the lookup above: counted across the instance, the
// first SSO user of every workspace after the first would silently never be its owner.
func (h *Handler) ssoOwnerExists(ctx context.Context, workspaceID string) bool {
	var n int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE workspace_id = ? AND is_owner = 1 AND archived_at IS NULL`,
		workspaceID).Scan(&n); err != nil {
		h.logger.ErrorContext(ctx, "sso: count owners", "error", err)
		return true
	}
	return n > 0
}

// nextIsAdminConsole reports whether next would land on the embedded admin console —
// which, with ADMIN_SPA=off, is three routes that answer 404 (see server.New).
//
// It normalises the way a browser does before comparing, because the value is compared
// against a mux pattern and the mux sees the normalised path: url.Parse strips the query
// and percent-decodes (`/%61dmin/` is `/admin/`), and path.Clean resolves `.` and `..`
// (so `/admin/../book/x` really does land outside the console and is allowed, while
// `/admin` and `/admin/` collapse to the same string).
//
// ⚠️ Case-SENSITIVE on purpose. http.ServeMux matches paths byte for byte, so `/ADMIN/`
// is not the console — it is an unmatched path that 404s for its own reason. Matching
// case-insensitively here would refuse a hand-off the mux would have served, which is a
// different bug in the same place.
//
// An unparseable value reports true: ssoNextPath refuses it a few lines later anyway,
// and with the console off, "cannot tell" has to read as "might be the console".
func nextIsAdminConsole(next string) bool {
	u, err := url.Parse(next)
	if err != nil {
		return true
	}
	p := path.Clean("/" + strings.TrimPrefix(u.Path, "/"))
	return p == "/admin" || strings.HasPrefix(p, "/admin/")
}

// ssoNextPath validates the optional ?next= target, returning the default when it is
// absent. Only a same-origin absolute path is allowed; anything else is refused rather
// than sanitised, because a redirect built from a partially-cleaned value is how open
// redirects survive their own fix.
func ssoNextPath(next string) (string, error) {
	if next == "" {
		return ssoDefaultNext, nil
	}
	switch {
	case !strings.HasPrefix(next, "/"):
		return "", errors.New("next must be an absolute path")
	case strings.HasPrefix(next, "//"):
		// Protocol-relative: "//evil.example" is another origin, not a local path.
		return "", errors.New("next must not start with //")
	case strings.Contains(next, `\`):
		// Some browsers normalise a backslash to a slash, so "/\evil.example" is a
		// protocol-relative URL wearing a disguise.
		return "", errors.New(`next must not contain a backslash`)
	case strings.Contains(next, "://"):
		return "", errors.New("next must not contain a scheme")
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			// A CR or LF would split the Location header; the rest are never legitimate
			// in a path either.
			return "", errors.New("next must not contain control characters")
		}
	}
	return next, nil
}

// verifySSOToken parses a compact JWS and verifies its HS256 signature with secret,
// returning the claims. The error names what was wrong in terms the operator can act
// on, and never echoes any part of the token back — an error body is attacker-reachable.
func verifySSOToken(token, secret string) (ssoClaims, error) {
	var claims ssoClaims

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errors.New("token is not a three-part JWT")
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, errors.New("token header is not base64url")
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return claims, errors.New("token header is not JSON")
	}
	// Checked before the signature: accepting the token's own choice of algorithm is the
	// JWT downgrade attack ("none", or an RS256 key confusion), and the message stays
	// fixed rather than quoting the value back.
	if header.Alg != "HS256" {
		return claims, errors.New("token alg must be HS256")
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, errors.New("token signature is not base64url")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) { // constant time
		return claims, errors.New("token signature does not verify")
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, errors.New("token payload is not base64url")
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return claims, errors.New("token payload is not JSON")
	}
	return claims, nil
}

// validate checks the claim set against this instance and the current time. aud is the
// instance's BASE_URL: binding the token to one audience is what stops a token minted
// for a staging instance being spent on production, when both share a secret by mistake.
func (c ssoClaims) validate(aud string, now time.Time) error {
	if c.Iss == "" {
		return errors.New("iss is required")
	}
	if c.Aud == "" || c.Aud != aud {
		return errors.New("aud does not match this instance")
	}
	// A full RFC 5322 parse is not the point; the value is looked up against a column
	// whose contents are addresses, so this only rejects the obviously-not-an-address.
	if s := strings.TrimSpace(c.Sub); s == "" || !strings.Contains(s, "@") {
		return errors.New("sub must be an email address")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	switch c.Role {
	case "owner", "admin", "member":
	default:
		return errors.New("role must be owner, admin or member")
	}
	if c.JTI == "" {
		return errors.New("jti is required")
	}
	if c.Iat == 0 {
		return errors.New("iat is required")
	}
	if c.Exp == 0 {
		return errors.New("exp is required")
	}

	iat, exp := time.Unix(c.Iat, 0), time.Unix(c.Exp, 0)
	if !exp.After(iat) {
		return errors.New("exp must be after iat")
	}
	if exp.Sub(iat) > ssoMaxTokenLifetime {
		return errors.New("exp is more than 60s after iat")
	}
	if iat.After(now.Add(ssoClockSkew)) {
		return errors.New("iat is in the future")
	}
	if exp.Before(now.Add(-ssoClockSkew)) {
		return errors.New("exp is in the past")
	}
	return nil
}
