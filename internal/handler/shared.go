package handler

import (
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/livekit"
	"github.com/calnode/calnode/internal/llm"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/stripe"
	"github.com/calnode/calnode/internal/zoom"
)

// shared is everything one process has exactly one of: the logger, the mailer,
// the hot-swappable integration clients and the mutexes guarding them, the
// configured hosts, and the demo-mode bookkeeping.
//
// It exists so Handler can be a cheap VALUE. A tenant-scoped request needs a
// handler whose db is bound to its workspace, and the only way to give 314
// methods a bound `h.db` without editing all of them is to hand them a receiver
// that differs in that field — so Handler is copied per request. A struct
// holding six sync.RWMutex cannot be copied (go vet copylocks, and rightly:
// copying a mutex copies its state), so the mutexes live behind this pointer,
// which every copy shares.
//
// Embedding it as *shared rather than naming it keeps `h.logger`, `h.mailer`,
// `h.livekitMu` and the rest compiling unchanged across the package.
type shared struct {
	logger *slog.Logger
	live   *mailer.Live // non-nil in production; nil in tests using a direct stub
	encKey [32]byte     // AES-256 key for encrypting secrets stored in the DB

	// envMailer is the transport EMAIL_SMTP_* configured at boot, kept as the
	// REGION DEFAULT for multi-tenant workspaces that carry no email settings of
	// their own. Nil when the process was started without one.
	//
	// ⛔ Not h.live, deliberately, even though live wraps the same object at boot.
	// live is process-wide AND hot-swappable: the email-settings save path calls
	// live.Swap with whatever the saving workspace persisted, so reading live at
	// send time would hand one tenant's SMTP credentials and From address to every
	// tenant that has none. This field is written once, at boot, and never swapped.
	envMailer mailer.Mailer

	// The per-workspace runtime state (D7). Each is built lazily from THAT
	// workspace's server_settings row through the bound handle, and replaced when
	// that workspace saves its settings. One entry keyed "" in single-tenant mode.
	mailerCache  *tenantCache[mailer.Mailer]
	llmCache     *tenantCache[*llm.Client]
	zoomCache    *tenantCache[*zoom.Client]
	stripeCache  *tenantCache[*stripe.Client]
	livekitCache *tenantCache[*livekit.Client]
	// settingsCache holds the two multi-tenant-only server_settings columns
	// (embed origins, STT host) per workspace; see tenant_settings.go.
	settingsCache *tenantCache[tenantSettings]

	// calBase is the PLATFORM-level provider registry: one Service holding the
	// instance's Google/Microsoft OAuth apps and the CalDAV client, per D7. It is
	// never used to read a tenant table directly — calCache holds the per-workspace
	// copies that Service.ForDB produces, and getCal is the only reader.
	calMu    sync.RWMutex
	calBase  *calendar.Service
	calCache *tenantCache[*calendar.Service]
	calNudge chan struct{} // buffered(1): wakes the calendar reconciler after a failed inline op

	baseURL       string
	publicBaseURL string
	dataDir       string

	// ssoSecret, metricsToken and sttBaseURLCfg arrive with feat/platform-hooks. They
	// live on shared, not on the per-request Handler: each is one process-wide
	// setting read from the environment at boot, none is per workspace, and putting
	// them on the value would copy three strings per request for nothing.
	//
	// ⚠️ ssoSecret is deliberately platform-level even though the token it signs
	// carries a `wid` claim. The secret identifies the INSTANCE that minted the
	// token; the workspace is inside the payload, which is what lets one identity
	// host hand a session to any of its tenants' public hosts (D11).
	// appDB is the APPLICATION handle as server.New built it: unbound, and belonging to the
	// role the row-level-security policies constrain.
	//
	// ⛔ forWorkspace binds from THIS, never from h.db, and the difference is the whole
	// isolation guarantee on a Platform route. Platform() replaces h.db with the platform
	// handle, whose role BYPASSES RLS — so a handle derived from it with ForWorkspace binds
	// app.workspace_id and is then not FILTERED by it. A `WHERE id = 1` read through such a
	// handle matches every workspace's row and returns an arbitrary one; a write lands
	// wherever the statement says. Both are silent.
	//
	// Found by a vendor-webhook test: the hand-off from a Platform route to
	// forWorkspace(ws).getLiveKit() read the DEFAULT workspace's empty settings row instead of
	// the resolved tenant's, so the handler answered 200 and verified nothing.
	appDB *db.DB

	// platformToken authorises the platform API (D12). Like the two below it is one
	// process-wide env-var secret, and it is what makes an instance a control plane
	// rather than just a tenant: with it unset every /v1/platform/* route 404s.
	platformToken string

	// platformReturnOrigins is PLATFORM_RETURN_ORIGINS: the origins a platform console
	// may ask the calendar OAuth round trip to return the browser to. Process-wide and
	// written once at boot, like the three below it — the allowlist identifies the
	// PLATFORM that embeds this instance, which is not a per-workspace fact. Empty ⇒ the
	// feature is off and a `return_to` is refused rather than ignored.
	platformReturnOrigins []string

	ssoSecret     string // HMAC key for the signed session hand-off; empty ⇒ /v1/auth/sso is off
	metricsToken  string // bearer token for GET /metrics; empty ⇒ that endpoint 404s
	sttBaseURLCfg string // STT_BASE_URL override; empty ⇒ stt.DefaultBaseURL (see sttBaseURL)

	authMu        sync.RWMutex
	googleAuth    *oauth2.Config
	microsoftAuth *oauth2.Config
	secureCookie  bool

	demoMode          bool // true on the public demo instance: disables calendar/Zoom connect
	demoResetInterval time.Duration
	demoMu            sync.RWMutex
	demoNextResetAt   time.Time

	// mcpServers caches one MCP server per workspace, keyed by workspace id ("" in
	// single-tenant mode). The tools close over their handler, so one shared
	// instance would run every tenant's tool calls on one workspace's handle.
	mcpMu      sync.RWMutex
	mcpServers map[string]*mcp.Server

	// multiTenant mirrors config.MultiTenant. It is what turns host and
	// credential resolution on; unset, every request runs on the default
	// workspace and the handler behaves exactly as it did before.
	multiTenant bool

	// adminSPAOff records that /admin/ is not served on this instance, and it is
	// stored NEGATED on purpose: the zero value has to mean "the console is there",
	// which is what every handler built without SetAdminSPA — the tests, and any
	// future entry point that forgets the setter — must keep believing. Stored the
	// other way round, forgetting one call would 404 SSO hand-offs on a perfectly
	// ordinary instance. See SetAdminSPA.
	adminSPAOff bool
}
