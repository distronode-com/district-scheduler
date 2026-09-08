package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port           string
	DatabaseURL    string
	EncryptionKey  string // platform secret (KEK input); vault derives the real AES key
	RecoverySecret string // CALNODE_RECOVERY_SECRET — escrow key stored in keystore
	BaseURL        string // identity host: OAuth callbacks, admin UI, team invites
	PublicBaseURL  string // booker-facing host: booking links, emails; defaults to BaseURL
	LogLevel       slog.Level

	// MultiTenant serves many isolated workspaces from one process: the tenant of
	// a request is resolved from its Host or from the credential it carries, and
	// PostgreSQL row-level security — not the query author — is what keeps one
	// workspace out of another's rows.
	//
	// Unset is the default and changes nothing: one workspace with the literal id
	// "default", row-level security never enabled, SQLite still supported.
	MultiTenant bool

	// DatabaseAdminURL is the PLATFORM role's DSN: the owner of the schema, with
	// BYPASSRLS, which runs migrations, the worker's cross-tenant claim loop, the
	// reconciler's workspace enumeration and the platform API. DatabaseURL is then
	// the APPLICATION role, which must be NOBYPASSRLS and must not own the tables —
	// that is the whole of the isolation guarantee, since a role that owns a table
	// or bypasses RLS is not constrained by its policy.
	//
	// Required when MultiTenant is set, ignored otherwise: in single-tenant mode
	// both handles are the same one.
	DatabaseAdminURL string

	// PlatformToken authenticates the platform API (workspace provisioning,
	// export/import, erasure) on the identity host. Empty means the platform API
	// is not mounted at all — a 404, not a 401, so an instance that does not
	// provision workspaces does not advertise that it could.
	PlatformToken string

	// Email / SMTP
	SMTPHost      string
	SMTPPort      string
	SMTPUser      string
	SMTPPass      string
	SMTPTLS       bool // implicit TLS (port 465)
	SMTPStartTLS  bool // STARTTLS (port 587)
	EmailFrom     string
	EmailFromName string

	// Google OAuth (calendar + sign-in)
	GoogleClientID     string
	GoogleClientSecret string

	// Microsoft 365 / Outlook (calendar) — env-only; tenant defaults to "common".
	MicrosoftClientID     string
	MicrosoftClientSecret string
	MicrosoftTenant       string

	// Zoom OAuth app (per-host meeting links) — DB settings take priority over these.
	ZoomClientID     string
	ZoomClientSecret string

	// SSOSharedSecret is the HMAC key for the signed session hand-off
	// (GET /v1/auth/sso). Empty ⇒ that endpoint is off and 404s. Env-only and
	// deliberately not settable from the admin UI: it can create users and mint
	// sessions, so it belongs with the platform secrets rather than in a settings
	// page an admin session can reach.
	SSOSharedSecret string

	// MetricsToken is the bearer token that authorises GET /metrics. Empty ⇒ that
	// endpoint 404s, so an instance never publishes its request volume, booking rate or
	// queue depth by accident. Env-only, for the same reason as SSOSharedSecret.
	MetricsToken string

	// STTBaseURL overrides the speech-to-text endpoint host for the notetaker, e.g. a
	// regional endpoint so recordings are transcribed inside one jurisdiction. Empty ⇒
	// stt.DefaultBaseURL. Only the host is configurable; the path, model and options are
	// Calnode's. Surfaced read-only in GET /v1/settings/notetaker so an operator can see
	// where audio is being sent without reading the environment of a running container.
	STTBaseURL string

	// DataDir is where uploaded files (avatars, branding assets) are written.
	// Defaults to the relative directory "data", which is what every existing
	// deployment has always used; set DATA_DIR when the process runs somewhere
	// its working directory is not writable, such as a read-only container image
	// that mounts a volume elsewhere.
	DataDir string

	// CookieSecure sets the Secure flag on session cookies. Defaults to true
	// when BASE_URL starts with https://, but can be overridden explicitly via
	// COOKIE_SECURE=false for HTTPS-terminated-at-proxy setups where the binary
	// itself listens on plain HTTP and BASE_URL is set correctly.
	CookieSecure bool

	// EmbedAllowedOrigins lists the origins permitted to call the public booking
	// endpoints cross-origin (for the embeddable widget). Empty ⇒ allow any origin
	// (`*`). CORS only constrains browsers; it is not an access-control boundary —
	// the public endpoints are rate-limited regardless. Comma-separated.
	EmbedAllowedOrigins []string

	// TrustedProxyCIDRs lists the networks whose forwarded headers are believed when
	// resolving the client IP for per-IP rate limiting. Empty (the default) ⇒ the limit
	// keys on the TCP peer and CF-Connecting-IP / X-Forwarded-For are ignored entirely,
	// because a header from an unvetted peer is a client-chosen value. Comma-separated
	// CIDRs; a bare address is taken as a single host.
	TrustedProxyCIDRs []string

	// FrameAncestors lists the origins allowed to embed the admin SPA in a frame, as a
	// Content-Security-Policy frame-ancestors source list. Space-separated, matching the
	// CSP syntax it becomes. Empty (the default) ⇒ nothing is sent and /admin/ behaves
	// exactly as it did. Each entry must be `https://host[:port]` or `'self'`; anything
	// else fails Validate and the process refuses to start, because a directive the
	// browser cannot parse is a directive that silently allows everything.
	//
	// Only the admin SPA is affected. The public booking pages keep their
	// `frame-ancestors 'none'` + `X-Frame-Options: DENY` unconditionally — those are
	// unauthenticated pages that take payment details, and no operator convenience is
	// worth making them embeddable.
	FrameAncestors []string

	// PlatformReturnOrigins lists the origins a platform console may ask an OAuth round
	// trip to come back to. Comma-separated absolute origins (`scheme://host[:port]`,
	// no path, query or fragment). Empty (the default) ⇒ the feature is off and
	// `?return_to=` is refused rather than ignored, so a platform that sends one against
	// an instance that was never configured for it hears about it at the first attempt.
	//
	// It exists because a platform embedding this scheduler wants the person back on ITS
	// page after connecting a calendar, and the callback otherwise lands on the operator's
	// own console. The value is carried inside the ENCRYPTED OAuth state, so what the
	// callback redirects to was chosen at connect time by an authenticated user and
	// checked against this list then; nothing on the callback URL can steer it.
	//
	// The security rule is the same one FrameAncestors states: an entry must be
	// `https://host[:port]` (plain http only for localhost/127.0.0.1, for a developer
	// running both halves on a laptop), never a wildcard, and the match at connect time is
	// the WHOLE origin, byte for byte. A prefix match would accept
	// `https://console.example.com.evil.test`, and an open redirect out of an OAuth
	// callback is worth more to an attacker than most bugs in this file.
	PlatformReturnOrigins []string

	// DemoMode turns this instance into a public, self-resetting demo: seeds sample
	// data on every boot (there's no persistent volume, so every boot is a fresh DB),
	// disables calendar/Zoom connect, serves a disallow-all robots.txt, and exposes
	// demo_mode/next_reset_at via the public auth-status endpoint so the frontend can
	// show the reset banner. Never set this on a real deployment.
	DemoMode bool
	// DemoResetInterval is how often DemoMode wipes and re-seeds the DB. Configurable
	// (not hardcoded to 30m) so local verification doesn't require waiting half an hour.
	DemoResetInterval time.Duration

	// DBMaxOpenConns / DBMaxIdleConns size the PostgreSQL connection pool.
	// PostgreSQL's own max_connections is the thing they have to fit inside, and
	// that is a property of the server a self-hoster runs, not of Calnode — a
	// small instance behind a PgBouncer wants a different number from one talking
	// to a 200-connection server directly.
	//
	// They do NOT apply to SQLite, which is pinned at 1/1 in internal/db because
	// the single writer connection is a correctness guarantee (ARCHITECTURE §17),
	// not a tuning choice.
	DBMaxOpenConns int
	DBMaxIdleConns int
}

func Load() *Config {
	cfg := &Config{
		Port:        getEnv("PORT", "3000"),
		DatabaseURL: getEnv("DATABASE_URL", "sqlite://./data/calnode.db"),
		BaseURL:     getEnv("BASE_URL", "http://localhost:3000"),

		SMTPHost:      getEnv("EMAIL_SMTP_HOST", ""),
		SMTPPort:      getEnv("EMAIL_SMTP_PORT", "587"),
		SMTPUser:      getEnv("EMAIL_SMTP_USER", ""),
		SMTPPass:      getEnv("EMAIL_SMTP_PASS", ""),
		SMTPTLS:       getBool("EMAIL_SMTP_TLS", false),
		SMTPStartTLS:  getBool("EMAIL_SMTP_STARTTLS", false),
		EmailFrom:     getEnv("EMAIL_FROM_ADDRESS", "bookings@localhost"),
		EmailFromName: getEnv("EMAIL_FROM_NAME", "Calnode"),

		GoogleClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),

		MicrosoftClientID:     getEnv("MICROSOFT_CLIENT_ID", ""),
		MicrosoftClientSecret: getEnv("MICROSOFT_CLIENT_SECRET", ""),
		MicrosoftTenant:       getEnv("MICROSOFT_TENANT", "common"),

		ZoomClientID:     getEnv("ZOOM_CLIENT_ID", ""),
		ZoomClientSecret: getEnv("ZOOM_CLIENT_SECRET", ""),

		EmbedAllowedOrigins: splitCSV(getEnv("EMBED_ALLOWED_ORIGINS", "")),
		TrustedProxyCIDRs:   splitCSV(getEnv("TRUSTED_PROXY_CIDRS", "")),
		STTBaseURL:          getEnv("STT_BASE_URL", ""),
		DataDir:             getEnv("DATA_DIR", "data"),
		// Space-separated, not comma: the value goes into a CSP source list verbatim, so
		// it reads the same in the env var as it does in the header.
		FrameAncestors: strings.Fields(getEnv("FRAME_ANCESTORS", "")),
		// Comma-separated, unlike FRAME_ANCESTORS: these are URLs an operator pastes,
		// not a CSP source list, so they follow TRUSTED_PROXY_CIDRS' shape.
		PlatformReturnOrigins: splitCSV(getEnv("PLATFORM_RETURN_ORIGINS", "")),
	}

	cfg.EncryptionKey = os.Getenv("CALNODE_ENCRYPTION_KEY")
	cfg.RecoverySecret = os.Getenv("CALNODE_RECOVERY_SECRET")
	cfg.SSOSharedSecret = os.Getenv("CALNODE_SSO_SHARED_SECRET")
	cfg.MetricsToken = os.Getenv("METRICS_TOKEN")
	// PUBLIC_BASE_URL overrides the booker-facing host (custom/vanity domain).
	// Unset → inherits BASE_URL, so single-domain deploys need only set BASE_URL.
	cfg.PublicBaseURL = getEnv("PUBLIC_BASE_URL", cfg.BaseURL)
	cfg.LogLevel = parseLogLevel(getEnv("LOG_LEVEL", "info"))
	cfg.CookieSecure = getBool("COOKIE_SECURE", strings.HasPrefix(cfg.BaseURL, "https://"))
	cfg.DemoMode = getBool("DEMO_MODE", false)
	cfg.DemoResetInterval = getDuration("DEMO_RESET_INTERVAL", 30*time.Minute)
	cfg.DBMaxOpenConns, cfg.DBMaxIdleConns = PoolFromEnv()

	cfg.MultiTenant = getBool("MULTI_TENANT", false)
	cfg.DatabaseAdminURL = os.Getenv("DATABASE_ADMIN_URL")
	cfg.PlatformToken = os.Getenv("CALNODE_PLATFORM_TOKEN")

	return cfg
}

// Validate reports the configuration errors an operator has to fix before the process
// can safely serve traffic. Called from main after Load; a non-nil error is fatal.
//
// It is separate from Load because Load has no error return and every optional knob in
// it deliberately falls back on a typo rather than refusing to boot (see PoolFromEnv).
// Nothing here is a typo. It holds two families, and both share the property that a
// wrong value is worse than an absent one:
//
//   - settings whose malformed value weakens a defence silently. A bad CSP directive is
//     the example: browsers drop a source list they cannot parse, so the admin UI would
//     end up MORE embeddable than with the setting unset, and nothing in the response
//     would say so.
//   - multi-tenant combinations whose only possible outcome is silent data loss or
//     silent cross-tenant exposure.
func (c *Config) Validate() error {
	for _, origin := range c.FrameAncestors {
		if err := validFrameAncestor(origin); err != nil {
			return fmt.Errorf("FRAME_ANCESTORS: %w", err)
		}
	}

	// Same family as the one above: a malformed entry here does not fail closed on its
	// own. It sits in the allowlist matching nothing, so every return_to is refused and
	// the platform's calendar connect looks broken for a reason no response body names.
	for _, origin := range c.PlatformReturnOrigins {
		if err := validReturnOrigin(origin); err != nil {
			return fmt.Errorf("PLATFORM_RETURN_ORIGINS: %w", err)
		}
	}

	if !c.MultiTenant {
		return nil
	}

	// SQLite has no row-level security, so there is nothing to enforce isolation
	// with. Refusing is the only honest answer: the alternative is an instance
	// that looks multi-tenant and separates nothing.
	if !isPostgresURL(c.DatabaseURL) {
		return errors.New("MULTI_TENANT requires a postgres:// DATABASE_URL — " +
			"tenant isolation is PostgreSQL row-level security, which SQLite has no equivalent of")
	}
	if c.DatabaseAdminURL == "" {
		return errors.New("MULTI_TENANT requires DATABASE_ADMIN_URL — " +
			"the platform role that owns the schema and runs migrations, distinct from the application role in DATABASE_URL")
	}
	if !isPostgresURL(c.DatabaseAdminURL) {
		return errors.New("DATABASE_ADMIN_URL must be a postgres:// DSN")
	}
	// Same DSN means one role, which means the application role owns the schema
	// and every policy is inert against it. This is the misconfiguration that
	// would be hardest to notice, because everything works — including reading
	// other tenants' rows.
	if c.DatabaseAdminURL == c.DatabaseURL {
		return errors.New("DATABASE_ADMIN_URL must differ from DATABASE_URL — " +
			"the application role must not own the tables or its row-level-security policies do not apply to it")
	}
	// Demo mode wipes and re-seeds the whole database every DemoResetInterval.
	// Against a multi-tenant database that is every tenant's data.
	if c.DemoMode {
		return errors.New("DEMO_MODE and MULTI_TENANT are mutually exclusive — " +
			"demo mode periodically wipes the entire database")
	}
	return nil
}

// isPostgresURL mirrors the classification internal/db does on the same string.
// Duplicated rather than imported because db imports config, and one three-line
// prefix check is a smaller price than a cycle.
func isPostgresURL(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "postgres://") || strings.HasPrefix(l, "postgresql://")
}

// Pool defaults. Deliberately modest: one Calnode instance is one small process,
// and a self-hoster's PostgreSQL is usually sized to match.
const (
	DefaultDBMaxOpenConns = 10
	DefaultDBMaxIdleConns = 5
)

// PoolFromEnv reads DB_MAX_OPEN_CONNS and DB_MAX_IDLE_CONNS.
//
// It is exported separately from Load because internal/db calls it directly:
// OpenDB has to know the pool size, every entry point that opens a database
// would otherwise have to remember to pass it, and forgetting would silently
// give that entry point the defaults. config imports nothing from the app, so
// db → config is not a cycle.
//
// Validation, rather than handing database/sql whatever the environment said:
//
//   - unset, unparsable or not positive → the default, with a warning. This
//     matches getBool/getDuration above, which also fall back rather than
//     failing a boot over a typo in an optional knob.
//   - idle > open → idle is clamped to open. database/sql silently reduces the
//     idle limit to the open limit in that case, so the pair is the honest
//     description of what the pool will do.
func PoolFromEnv() (maxOpen, maxIdle int) {
	maxOpen = getPositiveInt("DB_MAX_OPEN_CONNS", DefaultDBMaxOpenConns)
	maxIdle = getPositiveInt("DB_MAX_IDLE_CONNS", DefaultDBMaxIdleConns)
	if maxIdle > maxOpen {
		slog.Warn("DB_MAX_IDLE_CONNS exceeds DB_MAX_OPEN_CONNS; clamping",
			"idle", maxIdle, "open", maxOpen)
		maxIdle = maxOpen
	}
	return maxOpen, maxIdle
}

func getPositiveInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ignoring unparsable integer environment variable", "key", key, "value", v, "default", def)
		return def
	}
	if n < 1 {
		slog.Warn("ignoring non-positive environment variable", "key", key, "value", n, "default", def)
		return def
	}
	return n
}

// validFrameAncestor accepts 'self' or an https origin with no path, credentials, query
// or fragment.
//
// Wildcards are deliberately refused even though CSP allows them. `https://*.example.com`
// trusts every host any subdomain of that name ever points at, including one taken over
// later; an operator who needs two hosts can name two hosts. Plain http is refused for
// the same reason the admin session cookie is Secure — the framing page would be able to
// read nothing, but its own compromise becomes a foothold.
func validFrameAncestor(origin string) error {
	if origin == "'self'" {
		return nil
	}
	if strings.HasPrefix(origin, "'") {
		// 'none', 'unsafe-inline' and friends are keywords this setting has no use for:
		// 'none' is not "unset" (see FrameAncestors) and the rest are not source
		// expressions at all. Refusing them keeps the accepted grammar one line long.
		return fmt.Errorf("%q is not a supported keyword; use 'self' or an https:// origin", origin)
	}
	u, err := url.Parse(origin)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a URL: %w", origin, err)
	case u.Scheme != "https":
		return fmt.Errorf("%q must use https://", origin)
	case u.Host == "":
		return fmt.Errorf("%q has no host", origin)
	case strings.Contains(u.Host, "*"):
		return fmt.Errorf("%q must name one host, not a wildcard", origin)
	case u.User != nil:
		return fmt.Errorf("%q must not carry credentials", origin)
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("%q must be an origin, with no path", origin)
	case u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("%q must be an origin, with no query or fragment", origin)
	}
	return nil
}

// validReturnOrigin checks one PLATFORM_RETURN_ORIGINS entry.
//
// The grammar is deliberately narrower than validFrameAncestor's: no CSP keywords (there
// is nothing to say `'self'` about — a return_to to this instance's own host would be the
// existing /admin redirect), and http is allowed for localhost only, because a developer
// runs the platform and the scheduler on one laptop and there is no session to steal over
// loopback. Everything else is refused for the same reason a frame-ancestor is: this list
// is the ONLY thing standing between the OAuth callback and an open redirect, so a value
// nobody can read exactly is a value that should not boot.
func validReturnOrigin(origin string) error {
	u, err := url.Parse(origin)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a URL: %w", origin, err)
	case u.Host == "":
		return fmt.Errorf("%q has no host; an entry is scheme://host[:port]", origin)
	case u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Host)):
		return fmt.Errorf("%q must use https:// (http:// only for localhost or 127.0.0.1)", origin)
	case strings.Contains(u.Host, "*"):
		return fmt.Errorf("%q must name one host, not a wildcard", origin)
	case u.User != nil:
		return fmt.Errorf("%q must not carry credentials", origin)
	case u.Path != "":
		// Not `!= "/"` as FrameAncestors allows: this value is compared to a request's
		// origin, which never carries the trailing slash, so accepting one here would
		// make an entry that matches nothing look correct.
		return fmt.Errorf("%q must be an origin, with no path (not even a trailing slash)", origin)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return fmt.Errorf("%q must be an origin, with no query or fragment", origin)
	}
	return nil
}

// isLoopbackHost reports whether host (which may carry a port) is the loopback the
// http:// exemption above is for. Named hosts other than "localhost" are not accepted
// even when they resolve to 127.0.0.1: what a name resolves to is not a property of the
// configuration file.
func isLoopbackHost(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	return name == "localhost" || name == "127.0.0.1" || name == "[::1]" || name == "::1"
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitCSV parses a comma-separated value into trimmed, non-empty entries.
// Returns nil for an empty string.
func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
