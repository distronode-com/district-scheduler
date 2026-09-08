package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/calnode/calnode/frontend"
	"github.com/calnode/calnode/internal/caldav"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/calendar/microsoft"
	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/demo"
	"github.com/calnode/calnode/internal/gcal"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/livekit"
	"github.com/calnode/calnode/internal/llm"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/secret"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtime"
	"github.com/calnode/calnode/internal/stripe"
	"github.com/calnode/calnode/internal/webhook"
	"github.com/calnode/calnode/internal/worker"
	"github.com/calnode/calnode/internal/zoom"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// New builds the HTTP mux and starts background services. It returns the
// handler and a drain function that blocks until the background worker has
// finished its current poll cycle — call drain before httpServer.Shutdown.
// BuildHandler constructs the fully-wired Handler — mailer, webhook worker, calendar
// providers, OAuth — without registering HTTP routes or starting an HTTP server. New
// uses it to back the HTTP server; the `calnode mcp` subcommand uses it to serve the
// MCP server over stdio. The returned drain func blocks until the background worker
// has finished its current poll cycle.
func BuildHandler(ctx context.Context, cfg *config.Config, db *db.DB, logger *slog.Logger) (*handler.Handler, func()) {
	// Before any RateLimit call below: the bucket key depends on it (D14).
	SetMultiTenantLimits(cfg.MultiTenant)

	h := handler.New(db, logger)
	// Before anything else: it decides whether every route resolves a tenant.
	h.SetMultiTenant(cfg.MultiTenant)
	h.SetBaseURL(cfg.BaseURL)
	h.SetPublicBaseURL(cfg.PublicBaseURL)
	// DATA_DIR, defaulting to the relative "data" every deployment has always used.
	// The fallback is repeated here because tests build a Config literal that skips
	// Load, and an empty dir would put uploads beside the binary.
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "data"
	}
	h.SetDataDir(dataDir)
	h.SetEncKey(cfg.EncryptionKey)
	h.SetSSOSecret(cfg.SSOSharedSecret)
	// ADMIN_SPA is a multi-tenant switch: a single-tenant instance has no other admin
	// UI, so an operator who set it there is told it did nothing rather than being left
	// to wonder why /admin/ still answers. AdminSPAEnabled is the only expression that
	// decides this, here and at the route registration in New.
	if !cfg.AdminSPA && !cfg.MultiTenant {
		logger.Warn("ADMIN_SPA=off ignored: it applies to multi-tenant instances only, " +
			"and this one has no other admin UI")
	}
	h.SetAdminSPA(cfg.AdminSPAEnabled())
	h.SetPlatformToken(cfg.PlatformToken)
	h.SetPlatformReturnOrigins(cfg.PlatformReturnOrigins)
	h.SetMetricsToken(cfg.MetricsToken)
	h.SetSTTBaseURL(cfg.STTBaseURL)
	h.SetDemoMode(cfg.DemoMode)
	h.SetDemoResetInterval(cfg.DemoResetInterval)

	if cfg.DemoMode {
		// There's no persistent volume in demo mode, so the DB is always empty on
		// boot — this seed doubles as "first boot" and "after a container restart".
		if err := demo.Seed(ctx, db); err != nil {
			logger.Error("demo: seed failed", "error", err)
		} else {
			logger.Info("demo: seeded")
		}
		h.SetDemoNextResetAt(time.Now().Add(cfg.DemoResetInterval))
		go func() {
			ticker := time.NewTicker(cfg.DemoResetInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := demo.Reset(ctx, db); err != nil {
						logger.Error("demo: scheduled reset failed", "error", err)
					} else {
						logger.Info("demo: scheduled reset complete")
					}
					h.SetDemoNextResetAt(time.Now().Add(cfg.DemoResetInterval))
				}
			}
		}()
	}

	encKey, _ := secret.ParseKey(cfg.EncryptionKey)

	// Create the hot-swappable mailer wrapper. All sends everywhere use this
	// reference, so changing SMTP settings in the UI takes effect immediately.
	live := mailer.NewLive(&mailer.Noop{})

	// DB settings take priority over env vars — they're what the UI controls.
	// ⛔ Every LoadXSettingsFromDB below reads server_settings, which is a TENANT
	// table, through the UNBOUND application handle. In multi-tenant mode that
	// matches no row, so priming would install nothing and log "not configured" for
	// an instance whose tenants are all configured — a misleading boot log and a
	// pointless round trip. The per-workspace caches (D7) build each client from its
	// own workspace's row on first use instead, so there is nothing to prime.
	//
	// Made single-tenant-only rather than removed: it is the only path that seeds
	// env-var SMTP into the database on first boot, and it is what makes a
	// single-tenant instance's first request fast rather than lazy.
	primeFromDB := !cfg.MultiTenant

	dbSMTP, dbErr := handler.LoadEmailSettingsFromDB(db, encKey)
	if !primeFromDB {
		dbSMTP, dbErr = nil, nil
		logger.Info("multi-tenant mode: per-workspace settings are built on first use, not primed at boot")
	}
	if dbErr != nil {
		logger.Warn("mailer: could not load settings from database", "error", dbErr)
	}

	switch {
	case dbSMTP != nil:
		// BuildMailer, not NewSMTP directly, so boot and the settings-save path pick the
		// transport by the same rule. A Resend API key here means HTTPS delivery.
		m, transport := handler.BuildMailer(*dbSMTP)
		live.Swap(m)
		logger.Info("mailer: configured from database",
			"transport", string(transport), "host", dbSMTP.Host, "port", dbSMTP.Port)

	case cfg.SMTPHost != "":
		env := mailer.NewSMTP(
			cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPass,
			cfg.SMTPTLS, cfg.SMTPStartTLS, cfg.EmailFrom, cfg.EmailFromName,
		)
		live.Swap(env)
		// The same object again, kept unswappable, as the region default for
		// multi-tenant workspaces that carry no email settings of their own. Without
		// it a provisioned tenancy resolves to Noop and confirms its bookings to
		// nobody; live cannot serve that purpose because the settings-save path swaps
		// it to whichever workspace saved last.
		h.SetEnvMailer(env)
		logger.Info("mailer: configured from environment", "host", cfg.SMTPHost, "port", cfg.SMTPPort)
		// Seed env-var settings into DB so they appear in the UI on first boot.
		seedSMTPToDB(db, cfg, encKey, logger)

	default:
		logger.Info("mailer: not configured — configure SMTP in Settings or set EMAIL_SMTP_HOST")
	}

	h.SetMailer(live, cfg.BaseURL)

	drain := func() {}
	whs, err := webhook.New(db, cfg.EncryptionKey)
	if err != nil {
		logger.Error("webhook: init failed", "error", err)
	} else {
		h.SetWebhookSvc(whs)
		// Pass live so the worker picks up SMTP changes automatically.
		// ⛔ The PLATFORM handle. jobs is a tenant table, so a worker holding the
		// application handle claims nothing on a multi-tenant instance: reminders and
		// webhook deliveries would simply never fire, with no error anywhere.
		wrk := worker.New(db.Platform(), whs, logger,
			worker.WithMailer(live),
			worker.WithTenantResolver(func(workspaceID string) worker.TenantDeps {
				bound, m, wh := h.TenantRuntime(workspaceID)
				return worker.TenantDeps{DB: bound, Mailer: m, Webhook: wh}
			}))
		// Notetaker jobs live in the handler package (they need LLM/S3/encKey).
		wrk.RegisterHandler("notetaker.transcribe", h.JobNotetakerTranscribe)
		wrk.RegisterHandler("notetaker.summarize", h.JobNotetakerSummarize)
		go wrk.Run(ctx)
		drain = wrk.Wait
		logger.Info("webhook worker started")
	}

	// DB Google settings take priority over env vars.
	googleClientID := cfg.GoogleClientID
	googleClientSecret := cfg.GoogleClientSecret
	if dbGoogle, dbGoogleErr := loadIfSingleTenant(primeFromDB, func() (*handler.GoogleOAuthConfig, error) { return handler.LoadGoogleSettingsFromDB(db, encKey) }); dbGoogleErr != nil {
		logger.Warn("google settings: could not load from database", "error", dbGoogleErr)
	} else if dbGoogle != nil {
		googleClientID = dbGoogle.ClientID
		googleClientSecret = dbGoogle.ClientSecret
		logger.Info("Google OAuth: credentials loaded from database")
	}

	// Build one calendar Service and register every configured provider into it.
	calSvc := calendar.NewService(db)
	calRedirect := cfg.BaseURL + "/v1/calendar/callback"

	if googleClientID != "" {
		authRedirect := cfg.BaseURL + "/v1/auth/callback"
		h.SetGoogleAuth(googleClientID, googleClientSecret, authRedirect, cfg.CookieSecure)
		logger.Info("Google OAuth login configured", "redirect_url", authRedirect)

		gc, err := gcal.New(db, googleClientID, googleClientSecret, calRedirect, cfg.EncryptionKey)
		if err != nil {
			logger.Error("gcal: init failed", "error", err)
		} else {
			calSvc.Register(gc)
			logger.Info("Google Calendar configured", "redirect_url", calRedirect)
		}
	} else {
		logger.Info("Google OAuth not configured — add credentials in Settings or set GOOGLE_CLIENT_ID")
	}

	if cfg.MicrosoftClientID != "" && cfg.MicrosoftClientSecret != "" {
		msAuthRedirect := cfg.BaseURL + "/v1/auth/microsoft/callback"
		h.SetMicrosoftAuth(cfg.MicrosoftClientID, cfg.MicrosoftClientSecret, cfg.MicrosoftTenant, msAuthRedirect, cfg.CookieSecure)
		logger.Info("Microsoft OAuth login configured", "redirect_url", msAuthRedirect)

		mc, err := microsoft.New(db, cfg.MicrosoftClientID, cfg.MicrosoftClientSecret, cfg.MicrosoftTenant, calRedirect, cfg.EncryptionKey)
		if err != nil {
			logger.Error("microsoft: init failed", "error", err)
		} else {
			calSvc.Register(mc)
			logger.Info("Microsoft 365 calendar configured", "tenant", cfg.MicrosoftTenant)
		}
	}

	// CalDAV (Apple iCloud / Fastmail / Nextcloud / generic): unlike Google/Microsoft it
	// needs no instance-level OAuth app — each host connects their own server with an
	// app-specific password — so it's always available. Registered last so it never displaces
	// Google/Microsoft as the OAuth-callback primary.
	if cdav, err := caldav.New(db, cfg.EncryptionKey); err != nil {
		logger.Error("caldav: init failed", "error", err)
	} else {
		calSvc.Register(cdav)
		logger.Info("CalDAV calendar configured")
	}

	if calSvc.Any() {
		h.SetCalendar(calSvc)
		h.StartCalendarReconciler(ctx)
	}

	// Optional LLM layer (PRD §8.11) — off unless configured + enabled in Settings.
	if llmCfg, err := loadIfSingleTenant(primeFromDB, func() (*handler.LLMConfig, error) { return handler.LoadLLMSettingsFromDB(db, encKey) }); err != nil {
		logger.Warn("llm: could not load settings from database", "error", err)
	} else if llmCfg != nil && llmCfg.Enabled && llmCfg.Endpoint != "" {
		h.SetLLM(llm.New(llm.Config{Endpoint: llmCfg.Endpoint, Model: llmCfg.Model, APIKey: llmCfg.APIKey}))
		logger.Info("LLM layer enabled", "endpoint", llmCfg.Endpoint, "model", llmCfg.Model)
	} else {
		logger.Info("LLM layer not enabled — configure it in Settings → AI")
	}

	// Optional Zoom integration — each host connects their own Zoom account to
	// auto-mint meeting links. DB settings take priority over env vars.
	zoomClientID := cfg.ZoomClientID
	zoomClientSecret := cfg.ZoomClientSecret
	if dbZoom, dbZoomErr := loadIfSingleTenant(primeFromDB, func() (*handler.ZoomOAuthConfig, error) { return handler.LoadZoomSettingsFromDB(db, encKey) }); dbZoomErr != nil {
		logger.Warn("zoom settings: could not load from database", "error", dbZoomErr)
	} else if dbZoom != nil {
		zoomClientID = dbZoom.ClientID
		zoomClientSecret = dbZoom.ClientSecret
		logger.Info("Zoom OAuth: credentials loaded from database")
	}
	if zoomClientID != "" && zoomClientSecret != "" {
		zoomRedirect := cfg.BaseURL + "/v1/zoom/callback"
		if zc, err := zoom.New(db, zoomClientID, zoomClientSecret, zoomRedirect, cfg.EncryptionKey); err != nil {
			logger.Error("zoom: init failed", "error", err)
		} else {
			h.SetZoom(zc)
			logger.Info("Zoom integration configured", "redirect_url", zoomRedirect)
		}
	} else {
		logger.Info("Zoom not configured — add credentials in Settings → Zoom")
	}

	// Optional Stripe payments — paid bookings. Off unless configured in Settings → Payments.
	if dbStripe, dbStripeErr := loadIfSingleTenant(primeFromDB, func() (*handler.StripeConfig, error) { return handler.LoadStripeSettingsFromDB(db, encKey) }); dbStripeErr != nil {
		logger.Warn("stripe settings: could not load from database", "error", dbStripeErr)
	} else if dbStripe != nil {
		if sc, err := stripe.New(dbStripe.SecretKey, dbStripe.PublishableKey, dbStripe.WebhookSecret); err != nil {
			logger.Error("stripe: init failed", "error", err)
		} else {
			h.SetStripe(sc)
			logger.Info("Stripe payments configured", "webhook_secret_set", dbStripe.WebhookSecret != "")
		}
	} else {
		logger.Info("Stripe not configured — add credentials in Settings → Payments")
	}

	// Optional LiveKit video — built-in meeting rooms. Off unless configured in Settings → Video.
	if dbLK, dbLKErr := loadIfSingleTenant(primeFromDB, func() (*handler.LiveKitConfig, error) { return handler.LoadLiveKitSettingsFromDB(db, encKey) }); dbLKErr != nil {
		logger.Warn("livekit settings: could not load from database", "error", dbLKErr)
	} else if dbLK != nil {
		h.SetLiveKit(livekit.New(dbLK.URL, dbLK.APIKey, dbLK.APISecret, encKey))
		logger.Info("LiveKit video configured", "url", dbLK.URL)
	} else {
		logger.Info("LiveKit not configured — add a server in Settings → Video")
	}

	return h, drain
}

// New wires services via BuildHandler, then registers all HTTP routes. It returns the
// http.Handler and the worker drain func.
// loadIfSingleTenant runs a boot-time settings loader only when this instance has
// one tenant. In multi-tenant mode it returns (nil, nil), which every caller
// already treats as "not configured in the database" — so the per-workspace cache
// builds it on first use instead.
func loadIfSingleTenant[T any](prime bool, load func() (*T, error)) (*T, error) {
	if !prime {
		return nil, nil
	}
	return load()
}

// H is handler.Handler under a shorter name, so a registration reads
// (*H).ListBookings rather than (*handler.Handler).ListBookings 163 times.
//
// The method EXPRESSION is the point, not the brevity: h.Scoped takes
// func(*Handler, http.ResponseWriter, *http.Request), so passing h.ListBookings —
// a bound method value on the UNSCOPED handler, which is exactly the bug Scoped
// exists to prevent — does not compile.
type H = handler.Handler

func New(ctx context.Context, cfg *config.Config, db *db.DB, logger *slog.Logger) (http.Handler, func()) {
	h, drain := BuildHandler(ctx, cfg, db, logger)
	mux := http.NewServeMux()

	// Ops
	mux.HandleFunc("GET /healthz", h.Platform((*H).Healthz))
	mux.HandleFunc("GET /readyz", h.Platform((*H).Readyz))
	mux.HandleFunc("GET /version", h.Platform((*H).Version))
	// Prometheus exposition. Registered unconditionally and gated inside the handler on
	// METRICS_TOKEN, which answers 404 when it is unset — so an instance never publishes
	// its request volume or booking rate by accident, and never advertises the endpoint.
	//
	// ⛔ Platform, and its jobs read goes through Platform() inside the handler. The
	// queue depth of an instance is an instance-level number: on the bound handle it
	// would report one workspace's backlog as if it were the whole queue, and on the
	// unbound handle it would report zero. Both are wrong in a way a dashboard cannot
	// show you.
	mux.HandleFunc("GET /metrics", h.Platform((*H).Metrics))

	// Platform API (D12) — workspace provisioning on the identity host. Registered
	// unconditionally and gated inside the handlers on CALNODE_PLATFORM_TOKEN *and*
	// multi-tenant mode, both of which answer 404 when absent: a single-tenant instance
	// has no workspaces to provision, and a prober should not be able to tell which of
	// the two reasons applies.
	//
	// ⛔ Platform-wrapped, so h.db is the platform handle — which is the only handle that
	// CAN create a tenant (the application role cannot see a workspace that does not
	// exist yet, and RLS would refuse its first INSERT). Every INSERT in platform.go
	// names workspace_id for that reason.
	mux.HandleFunc("POST /v1/platform/workspaces", h.Platform((*H).CreateWorkspace))
	mux.HandleFunc("GET /v1/platform/workspaces/{id}", h.Platform((*H).GetWorkspace))
	mux.HandleFunc("PATCH /v1/platform/workspaces/{id}", h.Platform((*H).PatchWorkspace))
	mux.HandleFunc("DELETE /v1/platform/workspaces/{id}", h.Platform((*H).DeleteWorkspace))
	// Export, import and erasure. Export and import are POSTs because each is an operation
	// on the workspace rather than a representation of it, and because an export is far too
	// large and too sensitive to be a cacheable GET.
	mux.HandleFunc("POST /v1/platform/workspaces/{id}/export", h.Platform((*H).ExportWorkspace))
	mux.HandleFunc("POST /v1/platform/workspaces/{id}/import", h.Platform((*H).ImportWorkspace))
	mux.HandleFunc("DELETE /v1/platform/workspaces/{id}/attendees", h.Platform((*H).EraseAttendee))
	// The member API (F1). The identity provider owns who is in a workspace and what they
	// may do there; each of its people acts here as their own user with a platform-minted
	// managed key. Platform-wrapped for the same reason the four above are: the tenant is
	// named in the URL rather than resolved from a Host or a credential, and every
	// statement in platform_members.go names it.
	mux.HandleFunc("POST /v1/platform/workspaces/{id}/users", h.Platform((*H).UpsertWorkspaceUser))
	mux.HandleFunc("POST /v1/platform/workspaces/{id}/users/{uid}/api-keys", h.Platform((*H).MintWorkspaceUserAPIKey))
	mux.HandleFunc("DELETE /v1/platform/workspaces/{id}/users/{uid}/api-keys/{keyId}", h.Platform((*H).DeleteWorkspaceUserAPIKey))
	mux.HandleFunc("POST /v1/platform/workspaces/{id}/users/{uid}/archive", h.Platform((*H).ArchiveWorkspaceUser))
	mux.HandleFunc("PATCH /v1/platform/workspaces/{id}/webhooks", h.Platform((*H).MarkWorkspaceWebhooksManaged))

	// Bootstrap — public, once-only
	mux.HandleFunc("POST /v1/setup", h.Platform((*H).Setup))

	// Auth status (public — drives login page rendering).
	mux.HandleFunc("GET /v1/auth/status", h.Scoped(handler.HostWorkspace, (*H).AuthStatus))

	// First-user claim (public — only succeeds when no users exist).
	claimRL := RateLimit(5, time.Minute)
	mux.HandleFunc("POST /v1/auth/claim", claimRL(h.Scoped(handler.HostWorkspace, (*H).Claim)))

	// Email + password login.
	loginRL := RateLimit(10, time.Minute)
	mux.HandleFunc("POST /v1/auth/login/email", loginRL(h.Scoped(handler.HostWorkspace, (*H).LoginEmail)))
	mux.HandleFunc("POST /v1/auth/magic-link/request", loginRL(h.Scoped(handler.HostWorkspace, (*H).RequestMagicLink)))
	mux.HandleFunc("GET /v1/auth/magic-link/verify", loginRL(h.Scoped(handler.HostWorkspace, (*H).VerifyMagicLink)))

	// OAuth login (browser sessions for admin UI).
	authRL := RateLimit(10, time.Minute)
	mux.HandleFunc("GET /v1/auth/login", authRL(h.Platform((*H).LoginGoogle)))
	mux.HandleFunc("GET /v1/auth/callback", authRL(h.Platform((*H).CallbackGoogle)))
	mux.HandleFunc("GET /v1/auth/microsoft/login", authRL(h.Platform((*H).LoginMicrosoft)))
	mux.HandleFunc("GET /v1/auth/microsoft/callback", authRL(h.Platform((*H).CallbackMicrosoft)))
	// Signed session hand-off from an external identity system. Registered
	// unconditionally and gated inside the handler on the shared secret, so an
	// unconfigured instance answers 404 rather than exposing whether the route exists
	// at all. Same limiter as the OAuth callbacks: it is an unauthenticated endpoint
	// that does an HMAC and a write.
	//
	// ⛔ Platform, on the identity host, because there is no tenant Host to resolve
	// from and no credential yet — the TOKEN is the credential, and the workspace is
	// the `wid` claim inside it. SSOHandoff therefore resolves its own workspace from
	// that claim and names workspace_id explicitly on every row it writes; the
	// platform handle binds '', so an omitted column would seat the user and their
	// session in the default workspace (D11).
	mux.HandleFunc("GET /v1/auth/sso", authRL(h.Platform((*H).SSOHandoff)))
	mux.HandleFunc("POST /v1/auth/logout", h.Scoped(handler.HostWorkspace, (*H).Logout))
	// Sign out everywhere. Own sessions for anyone; someone else's for an admin, which
	// is the offboarding half. Also cuts that user's MCP OAuth tokens. Credential
	// scoped: whose sessions to cut is a question about one workspace's users.
	//
	// ⚠️ One line, like every other registration: routes_classified_test.go scans this
	// file line by line, so a wrapper on a continuation line reads as an unclassified
	// route — which is what it caught when this arrived from feat/platform-hooks.
	mux.HandleFunc("POST /v1/auth/sessions/revoke-all", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).RevokeAllSessions)))

	// MCP server (Model Context Protocol) — Streamable HTTP transport for remote
	// agents. One server instance reused across requests. Guarded by a bearer token:
	// an OAuth access token (the "Connect" flow) or a cno_ API key, both resolved by
	// verifyMCPBearer. A 401 advertises the OAuth discovery doc so clients can connect.
	// One server per workspace, not one per process: the tools close over their
	// handler, so a shared instance would run every tenant's tool calls on
	// whichever handler built it. MCPCallerMiddleware below has resolved the
	// caller's workspace into the request context by the time the factory runs.
	mcpHTTP := mcp.NewStreamableHTTPHandler(h.MCPServerForRequest, nil)
	mcpAuth := auth.RequireBearerToken(h.VerifyMCPBearer, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: cfg.BaseURL + "/.well-known/oauth-protected-resource",
	})
	// RequireBearerToken authenticates; MCPCallerMiddleware then binds the user+role so
	// the tools scope by role (members → only their own bookings).
	mux.Handle("/mcp", mcpAuth(h.MCPCallerMiddleware(mcpHTTP)))

	// OAuth 2.1 authorization server for MCP (discovery + dynamic client registration;
	// the interactive /oauth/authorize + /oauth/token live with the consent flow). All
	// public — the security gate is the user login + consent at /oauth/authorize.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", h.Platform((*H).OAuthProtectedResourceMetadata))
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.Platform((*H).OAuthAuthServerMetadata))
	oauthRegRL := RateLimit(20, time.Minute)
	mux.HandleFunc("POST /oauth/register", oauthRegRL(h.Platform((*H).RegisterOAuthClient)))
	mux.HandleFunc("GET /oauth/authorize", h.Platform((*H).AuthorizeMCP))
	mux.HandleFunc("POST /oauth/authorize", h.Platform((*H).AuthorizeMCPDecision))
	tokenRL := RateLimit(30, time.Minute)
	mux.HandleFunc("POST /oauth/token", tokenRL(h.Platform((*H).TokenMCP)))

	// Password management.
	mux.HandleFunc("POST /v1/users/me/password", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ChangePassword)))
	mux.HandleFunc("POST /v1/users/{id}/password", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).AdminSetPassword)))

	// Invite management.
	inviteRL := RateLimit(20, time.Minute)
	mux.HandleFunc("POST /v1/invites", inviteRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateInvite))))
	mux.HandleFunc("GET /v1/invites", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListInvites)))
	mux.HandleFunc("DELETE /v1/invites/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).RevokeInvite)))
	mux.HandleFunc("POST /v1/invites/{id}/resend", inviteRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ResendInvite))))
	mux.HandleFunc("GET /v1/invites/{token}", h.Scoped(handler.HostWorkspace, (*H).GetInvite))
	mux.HandleFunc("POST /v1/invites/{token}/claim", inviteRL(h.Scoped(handler.HostWorkspace, (*H).ClaimInvite)))

	// Users
	mux.HandleFunc("GET /v1/users", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListUsers)))
	mux.HandleFunc("DELETE /v1/users/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteUser)))
	mux.HandleFunc("PATCH /v1/users/{id}/role", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).SetUserRole)))
	mux.HandleFunc("POST /v1/users/{id}/transfer-ownership", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).TransferOwnership)))
	mux.HandleFunc("POST /v1/users/{id}/archive", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ArchiveUser)))
	mux.HandleFunc("POST /v1/users/{id}/restore", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).RestoreUser)))
	mux.HandleFunc("GET /v1/users/{id}/upcoming-bookings", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListUserUpcomingBookings)))

	// Teams
	mux.HandleFunc("POST /v1/teams", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateTeam)))
	mux.HandleFunc("GET /v1/teams", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListTeams)))
	mux.HandleFunc("GET /v1/teams/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetTeam)))
	mux.HandleFunc("PATCH /v1/teams/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchTeam)))
	mux.HandleFunc("DELETE /v1/teams/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteTeam)))
	mux.HandleFunc("POST /v1/teams/{id}/members", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).AddTeamMember)))
	mux.HandleFunc("PATCH /v1/teams/{id}/members/{userId}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).UpdateTeamMember)))
	mux.HandleFunc("DELETE /v1/teams/{id}/members/{userId}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).RemoveTeamMember)))
	mux.HandleFunc("GET /v1/users/me", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetMe)))
	mux.HandleFunc("PATCH /v1/users/me", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchMe)))
	avatarRL := RateLimit(20, time.Minute)
	mux.HandleFunc("POST /v1/users/me/avatar", avatarRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).UploadAvatar))))
	mux.HandleFunc("DELETE /v1/users/me/avatar", avatarRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteAvatar))))
	mux.HandleFunc("GET /avatars/{userID}", h.Scoped(handler.HostWorkspace, (*H).ServeAvatar))
	mux.HandleFunc("GET /branding/logo", h.Scoped(handler.HostWorkspace, (*H).ServeBrandingLogo))
	mux.HandleFunc("GET /branding/banner", h.Scoped(handler.HostWorkspace, (*H).ServeBrandingBanner))

	// Server settings — email (SMTP) and Google OAuth
	//
	// ⛔ THE h.PlatformManaged ROUTES BELOW HOLD AN INSTANCE CREDENTIAL, NOT A TENANT'S
	// DATA (H1). `server_settings` is per workspace, so most of these are self-scoped —
	// but the credential each one names is the PROCESS's: the SMTP account every tenancy
	// sends through, the Google OAuth client every tenancy's calendar connect and login
	// uses, the Zoom app, the LiveKit server, the Stripe account. On a multi-tenant
	// instance the platform provisions all five, so a tenant credential (a session or a
	// `cno_` key its own Developer tab minted) gets 403 `managed_by_platform` instead.
	// Single-tenant is untouched — there the operator is the instance.
	//
	// The wrapper is at the REGISTRATION rather than inside each handler so
	// routes_platform_managed_test.go can read this file and fail on one of these paths
	// registered without it. Everything NOT wrapped here is deliberately tenant-safe and
	// is what the platform's own dashboard calls: branding, the storage toggle, the
	// notetaker toggle, the tracking ids, and the two LLM fields below.
	settingsRL := RateLimit(20, time.Minute)
	mux.HandleFunc("GET /v1/settings/email", h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetEmailSettings))))
	mux.HandleFunc("PATCH /v1/settings/email", settingsRL(h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchEmailSettings)))))
	mux.HandleFunc("POST /v1/settings/email/test", settingsRL(h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).TestEmailConnection)))))
	mux.HandleFunc("GET /v1/settings/google", h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetGoogleSettings))))
	mux.HandleFunc("PATCH /v1/settings/google", settingsRL(h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchGoogleSettings)))))
	mux.HandleFunc("GET /v1/settings/zoom", h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetZoomSettings))))
	mux.HandleFunc("PATCH /v1/settings/zoom", settingsRL(h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchZoomSettings)))))
	mux.HandleFunc("GET /v1/settings/livekit", h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetLiveKitSettings))))
	mux.HandleFunc("PATCH /v1/settings/livekit", settingsRL(h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchLiveKitSettings)))))
	mux.HandleFunc("GET /v1/settings/storage", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetStorageSettings)))
	mux.HandleFunc("PATCH /v1/settings/storage", settingsRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchStorageSettings))))
	mux.HandleFunc("GET /v1/settings/notetaker", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetNotetakerSettings)))
	mux.HandleFunc("PATCH /v1/settings/notetaker", settingsRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchNotetakerSettings))))
	mux.HandleFunc("GET /v1/bookings/{id}/notes", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetBookingNotes)))
	mux.HandleFunc("POST /v1/bookings/{id}/notes/regenerate", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).RegenerateBookingNotes)))
	mux.HandleFunc("GET /v1/bookings/{id}/transcript", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetBookingTranscript)))
	mux.HandleFunc("GET /v1/settings/stripe", h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetStripeSettings))))
	mux.HandleFunc("PATCH /v1/settings/stripe", settingsRL(h.PlatformManaged(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchStripeSettings)))))
	mux.HandleFunc("GET /v1/settings/tracking", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetTrackingSettings)))
	mux.HandleFunc("PATCH /v1/settings/tracking", settingsRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchTrackingSettings))))
	mux.HandleFunc("GET /v1/settings/llm", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetLLMSettings)))
	// ⛔ Guarded by FIELD, not by route: `enabled` and `extra_instructions` belong to the
	// workspace (they are the only two the platform's catalog offers), while `endpoint`,
	// `model` and `api_key` name the model provider the PLATFORM pays for. A body naming
	// any of the three is 403 `managed_by_platform`; a body naming neither is unchanged.
	mux.HandleFunc("PATCH /v1/settings/llm", settingsRL(h.PlatformManagedFields("endpoint", "model", "api_key")(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchLLMSettings)))))
	mux.HandleFunc("POST /v1/settings/llm/test", settingsRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).TestLLMSettings))))
	mux.HandleFunc("GET /v1/settings/branding", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetBranding)))
	mux.HandleFunc("PATCH /v1/settings/branding", settingsRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchBranding))))
	mux.HandleFunc("POST /v1/settings/branding/logo", settingsRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).UploadBrandingLogo))))
	mux.HandleFunc("DELETE /v1/settings/branding/logo", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteBrandingLogo)))
	mux.HandleFunc("POST /v1/settings/branding/banner", settingsRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).UploadBrandingBanner))))
	mux.HandleFunc("DELETE /v1/settings/branding/banner", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteBrandingBanner)))

	// Event types
	mux.HandleFunc("POST /v1/event-types", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateEventType)))
	mux.HandleFunc("GET /v1/event-types", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListEventTypes)))
	mux.HandleFunc("GET /v1/event-types/{slug}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetEventType)))
	mux.HandleFunc("PATCH /v1/event-types/{slug}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchEventType)))
	mux.HandleFunc("DELETE /v1/event-types/{slug}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteEventType)))
	mux.HandleFunc("GET /v1/event-types/{slug}/hosts", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListEventTypeHosts)))
	mux.HandleFunc("PUT /v1/event-types/{slug}/hosts", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).SetEventTypeHosts)))
	testEmailRL := RateLimit(10, time.Minute)
	mux.HandleFunc("POST /v1/event-types/{slug}/test-email", testEmailRL(h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).SendTestEmail))))

	// Availability rules
	mux.HandleFunc("POST /v1/availability-rules", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateAvailabilityRule)))
	mux.HandleFunc("GET /v1/availability-rules", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListAvailabilityRules)))
	mux.HandleFunc("PATCH /v1/availability-rules/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).UpdateAvailabilityRule)))
	mux.HandleFunc("DELETE /v1/availability-rules/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteAvailabilityRule)))

	// Availability overrides
	mux.HandleFunc("POST /v1/availability-overrides", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateAvailabilityOverride)))
	mux.HandleFunc("GET /v1/availability-overrides", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListAvailabilityOverrides)))
	mux.HandleFunc("PATCH /v1/availability-overrides/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).UpdateAvailabilityOverride)))
	mux.HandleFunc("DELETE /v1/availability-overrides/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteAvailabilityOverride)))
	mux.HandleFunc("DELETE /v1/availability-overrides/group/{groupId}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteAvailabilityOverrideGroup)))

	// Slots — public, rate-limited per IP. Browsed more than booked (so a higher cap
	// than booking), but each call fans out Google free/busy per host, so leaving it
	// cors wraps the public booking endpoints so the embeddable widget can call them
	// cross-origin from a customer's site. Scoped to these unauthenticated routes
	// only — admin/auth routes never get CORS. Default (empty allowlist) = any origin.
	cors := PublicCORS(cfg.EmbedAllowedOrigins)
	if cfg.MultiTenant {
		// Per workspace: the allowlist is the one the platform API stored for the
		// workspace whose public host the request names, and an unknown host gets
		// no CORS header at all. EMBED_ALLOWED_ORIGINS is not consulted in this
		// mode — one process-wide list would let tenant A's embed origins call
		// tenant B's booking endpoints.
		cors = PublicCORSFor(h.EmbedOriginsFor)
	}

	// Public event-type display info for the widget (name/duration/location/brand).
	mux.HandleFunc("GET /v1/event-types/{slug}/public", cors(h.Scoped(handler.HostWorkspace, (*H).PublicEventType)))

	// unthrottled is a CPU + API-quota abuse vector on an openly-public page.
	slotsRL := RateLimit(60, time.Minute)
	mux.HandleFunc("GET /v1/event-types/{slug}/slots", cors(slotsRL(h.Scoped(handler.HostWorkspace, (*H).GetSlots))))

	// Conversational booking assistant (optional AI; public, anonymous → tighter limit).
	assistantRL := RateLimit(15, time.Minute)
	mux.HandleFunc("POST /v1/event-types/{slug}/assistant", cors(assistantRL(h.Scoped(handler.HostWorkspace, (*H).BookingAssistant))))
	mux.HandleFunc("OPTIONS /v1/event-types/{slug}/assistant", cors(func(http.ResponseWriter, *http.Request) {}))

	// Intake questions
	mux.HandleFunc("GET /v1/event-types/{slug}/questions", cors(h.Scoped(handler.HostWorkspace, (*H).ListQuestions)))
	mux.HandleFunc("GET /v1/event-types/{slug}/questions/admin", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListQuestionsAdmin)))
	mux.HandleFunc("POST /v1/event-types/{slug}/questions", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateQuestion)))
	mux.HandleFunc("PATCH /v1/event-types/{slug}/questions/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).UpdateQuestion)))
	mux.HandleFunc("DELETE /v1/event-types/{slug}/questions/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteQuestion)))

	bookingRL := RateLimit(20, time.Minute)
	manageRL := RateLimit(30, time.Minute)

	// Bookings — public create is CORS-enabled for the widget; the JSON body makes it
	// a non-simple request, so the OPTIONS preflight is handled too.
	mux.HandleFunc("POST /v1/bookings", cors(bookingRL(h.Scoped(handler.HostWorkspace, (*H).CreateBooking))))
	mux.HandleFunc("OPTIONS /v1/bookings", cors(func(http.ResponseWriter, *http.Request) {}))
	// F4. This read was host-scoped and unauthenticated: the booking id was the whole
	// capability, so anyone who could reach a tenant's public host and guess or capture
	// one read the attendee's name, email and intake answers. It is now credential-scoped
	// like every sibling under /v1/bookings/{id}/*.
	//
	// ⛔ Nothing public reads it — proven, not assumed. The booking page and the widget
	// render their confirmation from the POST /v1/bookings response body (book.html,
	// embed.js), and the cancel/reschedule links in emails go to /manage/{token}, which
	// is a separate token-bearing surface. The only callers of /v1/bookings/{id}* in the
	// tree are the authenticated admin SPA's sub-path calls.
	mux.HandleFunc("GET /v1/bookings/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetBooking)))
	mux.HandleFunc("GET /v1/bookings", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListBookings)))
	mux.HandleFunc("POST /v1/bookings/{id}/cancel", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CancelBooking)))
	mux.HandleFunc("PATCH /v1/bookings/{id}/reschedule", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).RescheduleBooking)))
	mux.HandleFunc("POST /v1/bookings/{id}/reassign", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ReassignBooking)))
	mux.HandleFunc("GET /v1/bookings/{id}/answers", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetBookingAnswers)))

	// Public booking page
	mux.HandleFunc("GET /embed.js", h.Scoped(handler.HostWorkspace, (*H).EmbedJS))
	mux.HandleFunc("GET /booking.css", h.Scoped(handler.HostWorkspace, (*H).BookingCSS))
	mux.HandleFunc("GET /book/{slug}", h.Scoped(handler.HostWorkspace, (*H).BookPage))

	// Built-in LiveKit video room (public): the page, its vendored assets, and the token
	// exchange. The signed room token in the join URL is the capability — no auth.
	mux.HandleFunc("GET /room/{room}", h.Scoped(handler.HostWorkspace, (*H).LiveKitRoom))
	mux.HandleFunc("GET /assets/livekit-client.js", h.Platform((*H).LiveKitSDKAsset))
	mux.HandleFunc("GET /assets/livekit-room.js", h.Platform((*H).LiveKitRoomJSAsset))
	mux.HandleFunc("POST /v1/livekit/token", bookingRL(h.Scoped(handler.HostWorkspace, (*H).LiveKitToken)))
	mux.HandleFunc("POST /v1/livekit/room/end", bookingRL(h.Scoped(handler.HostWorkspace, (*H).EndRoom)))
	mux.HandleFunc("POST /v1/livekit/room/reassign-host", bookingRL(h.Scoped(handler.HostWorkspace, (*H).ReassignHost)))
	mux.HandleFunc("POST /v1/livekit/room/screenshare", bookingRL(h.Scoped(handler.HostWorkspace, (*H).ScreenShareToggle)))
	mux.HandleFunc("POST /v1/livekit/record/start", bookingRL(h.Scoped(handler.HostWorkspace, (*H).RecordStart)))
	mux.HandleFunc("POST /v1/livekit/record/stop", bookingRL(h.Scoped(handler.HostWorkspace, (*H).RecordStop)))
	mux.HandleFunc("POST /v1/livekit/consent", bookingRL(h.Scoped(handler.HostWorkspace, (*H).RecordConsent)))
	mux.HandleFunc("POST /v1/livekit/webhook", h.Platform((*H).LiveKitWebhook))
	mux.HandleFunc("POST /v1/livekit/egress-webhook", h.Platform((*H).LiveKitWebhook)) // legacy alias — keep old LiveKit registrations working
	mux.HandleFunc("GET /v1/recordings", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListRecordings)))
	mux.HandleFunc("DELETE /v1/recordings", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteAllRecordings)))
	mux.HandleFunc("GET /v1/recordings/{id}/consent", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListRecordingConsent)))
	mux.HandleFunc("GET /v1/recordings/{id}/download", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DownloadRecording)))
	mux.HandleFunc("DELETE /v1/recordings/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteRecording)))

	// Manage booking (reschedule / cancel via token link)
	mux.HandleFunc("GET /manage/{token}", manageRL(h.Scoped(handler.HostWorkspace, (*H).ManagePage)))
	mux.HandleFunc("POST /manage/{token}/reschedule", manageRL(h.Scoped(handler.HostWorkspace, (*H).RescheduleByToken)))
	mux.HandleFunc("POST /manage/{token}/cancel", manageRL(h.Scoped(handler.HostWorkspace, (*H).CancelByToken)))

	// Webhooks
	mux.HandleFunc("POST /v1/webhooks", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateWebhook)))
	mux.HandleFunc("GET /v1/webhooks", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListWebhooks)))
	mux.HandleFunc("PATCH /v1/webhooks/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PatchWebhook)))
	mux.HandleFunc("DELETE /v1/webhooks/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteWebhook)))
	mux.HandleFunc("GET /v1/webhooks/{id}/deliveries", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListWebhookDeliveries)))

	// Google Calendar — connect/callback/status/disconnect
	mux.HandleFunc("GET /v1/calendar/connect", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ConnectCalendar)))
	mux.HandleFunc("GET /v1/calendar/callback", h.Platform((*H).CalendarCallback))
	mux.HandleFunc("POST /v1/calendar/caldav/connect", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ConnectCalDAV)))
	mux.HandleFunc("GET /v1/calendar/status", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CalendarStatus)))
	mux.HandleFunc("POST /v1/calendar/connections/{id}/destination", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).SetCalendarDestination)))
	mux.HandleFunc("GET /v1/calendar/connections/{id}/calendars", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetConnectionCalendars)))
	mux.HandleFunc("PUT /v1/calendar/connections/{id}/calendars", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).PutConnectionCalendars)))
	mux.HandleFunc("DELETE /v1/calendar/connections/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DisconnectCalendarConnection)))
	mux.HandleFunc("DELETE /v1/calendar", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DisconnectCalendar)))

	// Zoom — per-host OAuth connect (auto-mint meeting links).
	mux.HandleFunc("GET /v1/zoom/connect", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ConnectZoom)))
	mux.HandleFunc("GET /v1/zoom/callback", h.Platform((*H).ZoomCallback))
	mux.HandleFunc("GET /v1/zoom/status", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ZoomStatus)))
	mux.HandleFunc("DELETE /v1/zoom", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DisconnectZoom)))

	// Stripe payment webhook — public, authenticated by the signing secret (no session
	// cookie, so the CSRF check doesn't apply). Must receive the raw body.
	mux.HandleFunc("POST /v1/stripe/webhook", h.Platform((*H).StripeWebhook))

	// API keys
	mux.HandleFunc("GET /v1/api-keys", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListAPIKeys)))
	mux.HandleFunc("POST /v1/api-keys", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).CreateAPIKey)))
	mux.HandleFunc("DELETE /v1/api-keys/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).DeleteAPIKey)))

	// Connected apps — MCP OAuth grants the user can review and revoke.
	mux.HandleFunc("GET /v1/oauth/connections", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).ListOAuthConnections)))
	mux.HandleFunc("DELETE /v1/oauth/connections/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).RevokeOAuthConnection)))

	// Demo instance only — one-click login, on-demand reset, and a disallow-all
	// robots.txt. Never registered on a real deployment.
	if cfg.DemoMode {
		mux.HandleFunc("GET /v1/demo/enter", h.Platform((*H).DemoEnter))
		demoResetRL := RateLimit(2, time.Minute)
		mux.HandleFunc("POST /v1/demo/reset", demoResetRL(h.Platform((*H).DemoReset)))
		mux.HandleFunc("GET /robots.txt", h.Platform((*H).Robots))
	}

	// Favicon at the root, shared by the public server-rendered pages and the
	// browser's default /favicon.ico probe — same embedded source as the admin SPA.
	favicon := frontend.FaviconHandler()
	mux.Handle("GET /favicon.svg", favicon)
	mux.Handle("GET /favicon.ico", favicon)

	// Admin SPA — served at /admin/* with SPA fallback for client-side routing.
	// FrameAncestors is applied here and nowhere else: FRAME_ANCESTORS is about embedding
	// the admin console, and the public pages' own DENY must not be reachable from a
	// config flag.
	adminSPA := FrameAncestors(cfg.FrameAncestors)(frontend.Handler())
	adminIndex := http.Handler(http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	adminTree := http.Handler(http.StripPrefix("/admin", adminSPA))
	// Bare root → admin. The `{$}` anchor matches ONLY the exact path "/", so it
	// stays a no-op for every other unmatched path (those still 404). Public
	// visitors always arrive via a full /book/{slug} link, so this only affects
	// an operator landing on the domain root. 302 (not 301) so it isn't cached
	// permanently if a marketing landing page is ever added here.
	adminRoot := http.Handler(http.RedirectHandler("/admin/", http.StatusFound))

	// ADMIN_SPA=off on a multi-tenant instance: the platform's own console is the
	// admin UI, so these three answer 404 instead. The HANDLERS change and the
	// registrations do not, deliberately —
	//
	//   - the classification gate reads this file, and a route that disappears in one
	//     configuration is a route no gate can classify;
	//   - the 404 goes out through the same middleware chain as every other response,
	//     so the request id, the logging and the security headers are unchanged. A mux
	//     with nothing registered here would answer its own 404 outside all of that;
	//   - /favicon.ico and the whole /v1 tree are registered elsewhere and untouched.
	//     The console is removed, not the instance's public surface.
	//
	// Single-tenant is never switched off (config.AdminSPAEnabled): the operator would
	// have no admin UI at all.
	if !cfg.AdminSPAEnabled() {
		notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
		adminIndex, adminTree, adminRoot = notFound, notFound, notFound
	}

	mux.Handle("GET /admin", adminIndex)
	mux.Handle("/admin/", adminTree)
	mux.Handle("GET /{$}", adminRoot)

	// Trusted-proxy resolution wraps everything, so the per-IP limiters and anything else
	// asking for the client IP see one answer computed once. A bad CIDR is logged and
	// dropped rather than fatal: the consequence is that that hop's headers are not
	// believed, which costs shared rate-limit buckets, never a trusted forgery.
	trustedProxies, err := ParseTrustedProxies(cfg.TrustedProxyCIDRs)
	if err != nil {
		logger.Error("TRUSTED_PROXY_CIDRS: ignoring unparseable entries", "error", err)
	}
	if len(trustedProxies) > 0 {
		logger.Info("trusting forwarded headers from proxies", "cidrs", cfg.TrustedProxyCIDRs)
	}

	return TrustClientIP(trustedProxies)(RequestID(Logging(logger, SameOriginCheck(mux)))), drain
}

// seedSMTPToDB writes env-var SMTP settings into the DB on first boot so they
// appear in the UI. Uses WHERE smtp_host = ” to avoid a check-then-act race
// and to never overwrite settings the user has already saved via the UI.
func seedSMTPToDB(db *db.DB, cfg *config.Config, encKey [32]byte, logger *slog.Logger) {
	var passEnc string
	if cfg.SMTPPass != "" {
		enc, err := secret.Encrypt(encKey, cfg.SMTPPass)
		if err != nil {
			logger.Warn("mailer: seed — could not encrypt password", "error", err)
			return
		}
		passEnc = enc
	}

	boolToInt := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	res, err := db.Exec(`
		UPDATE server_settings SET
		  smtp_host = ?, smtp_port = ?, smtp_user = ?, smtp_pass_enc = ?,
		  smtp_tls = ?, smtp_starttls = ?,
		  email_from = ?, email_from_name = ?,
		  updated_at = ?
		WHERE id = 1 AND smtp_host = ''`,
		cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, passEnc,
		boolToInt(cfg.SMTPTLS), boolToInt(cfg.SMTPStartTLS),
		cfg.EmailFrom, cfg.EmailFromName, dbtime.Now())
	if err != nil {
		logger.Warn("mailer: seed to database failed", "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logger.Info("mailer: seeded SMTP settings from env vars into database (env vars can now be removed)")
	}
}
