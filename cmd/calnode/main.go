package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed IANA timezone database so scratch/distroless images work

	"github.com/joho/godotenv"

	"github.com/calnode/calnode/internal/buildinfo"
	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/keyvault"
	"github.com/calnode/calnode/internal/server"
)

func main() {
	// Subcommand dispatch.
	if len(os.Args) > 1 {
		_ = godotenv.Load()
		switch os.Args[1] {
		case "reset-admin":
			runResetAdmin(os.Args[2:])
			return
		case "rotate-key":
			runRotateKey(os.Args[2:])
			return
		case "recover-key":
			runRecoverKey(os.Args[2:])
			return
		case "mcp":
			runMCPStdio(os.Args[2:])
			return
		}
	}

	// Load .env if present (dev convenience). Real env vars always win.
	_ = godotenv.Load()

	cfg := config.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	// Before the database is opened: every combination Validate refuses would
	// otherwise present as a bug somewhere much later — a tenant reading another
	// tenant's rows, or a demo reset wiping a fleet.
	if err := cfg.Validate(); err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	bi := buildinfo.Get()
	logger.Info("starting calnode", "version", bi.Version, "commit", bi.Commit, "build_time", bi.BuildTime, "dirty", bi.Dirty)

	if cfg.GoogleClientID != "" {
		slog.Info("Google OAuth configured", "client_id_prefix", cfg.GoogleClientID[:20])
	} else {
		slog.Warn("Google OAuth NOT configured — GOOGLE_CLIENT_ID is empty")
	}

	// One handle in single-tenant mode; two in multi-tenant mode, because
	// migrations, EnableRLS and every cross-tenant read are the PLATFORM role's
	// work. DATABASE_URL is then a NOBYPASSRLS role that does not own the schema
	// and cannot run DDL at all. database.Platform() answers with the right one
	// either way, so nothing downstream has to know which mode this is.
	var (
		database, platform *db.DB
		err                error
	)
	if cfg.MultiTenant {
		database, platform, err = db.OpenPair(cfg.DatabaseURL, cfg.DatabaseAdminURL)
		if err != nil {
			logger.Error("failed to open the database pair", "error", err)
			os.Exit(1)
		}
		defer platform.Close()
	} else {
		database, err = db.OpenDB(cfg.DatabaseURL)
		if err != nil {
			logger.Error("failed to open database", "error", err)
			os.Exit(1)
		}
		platform = database
	}
	defer database.Close()

	if err := platform.Migrate(); err != nil {
		logger.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	logger.Info("database migrations applied")

	// ⛔ Refuse to serve if either of these fails. Without EnableRLS every policy
	// created by migration 00060 is inert and the process comes up looking
	// multi-tenant while separating nothing; without VerifyRoles the same is true
	// of a DATABASE_URL that happens to bypass or to own the tables. Both failures
	// are otherwise silent — every request works, and each one can read every
	// workspace. EnableRLS is idempotent, so booting twice is fine, and it is
	// deliberately gated on MULTI_TENANT: FORCE ROW LEVEL SECURITY applies a policy
	// to the table's owner too, and in single-tenant mode DATABASE_URL is that
	// owner (see db.EnableRLS).
	if cfg.MultiTenant {
		if err := platform.EnableRLS(context.Background()); err != nil {
			logger.Error("failed to enable row-level security; refusing to serve", "error", err)
			os.Exit(1)
		}
		// The seeded `default` workspace is nobody's tenant on a multi-tenant instance;
		// suspending it keeps the background sweeps off it. Not fatal: a sweep over an
		// empty workspace is waste, not damage, so a failure here is worth a line in the
		// log and not a refusal to serve.
		if err := platform.SuspendDefaultWorkspace(context.Background()); err != nil {
			logger.Error("could not suspend the default workspace", "error", err)
		}
		if err := database.VerifyRoles(context.Background()); err != nil {
			logger.Error("database roles cannot enforce tenant isolation; refusing to serve", "error", err)
			os.Exit(1)
		}
		logger.Info("row-level security enabled", "tables", len(db.TenantTables))
	}

	// Open the key vault. devMode allows an ephemeral DEK when no secret is set
	// (handy for local development); production deployments must set
	// CALNODE_ENCRYPTION_KEY or the vault will refuse to start.
	devMode := !strings.HasPrefix(cfg.BaseURL, "https://")
	vault, err := keyvault.Open(database, cfg.EncryptionKey, cfg.RecoverySecret, devMode)
	if err != nil {
		logger.Error("keyvault: failed to open", "error", err)
		os.Exit(1)
	}
	// Replace the platform secret in cfg with the real DEK so all downstream
	// components (handler, gcal, webhook) receive the AES key they expect.
	cfg.EncryptionKey = vault.DEKHex()

	workerCtx, workerCancel := context.WithCancel(context.Background())

	srv, drainWorker := server.New(workerCtx, cfg, database, logger)

	httpServer := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      srv,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("server listening", "port", cfg.Port, "base_url", cfg.BaseURL)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			quit <- syscall.SIGTERM // triggers graceful shutdown so deferred db.Close() runs
		}
	}()

	<-quit
	logger.Info("shutting down — draining worker and in-flight requests")

	// Stop the worker's ticker loop and drain the HTTP server concurrently so a
	// slow poll cycle (up to 10 jobs × 10 s each) does not delay in-flight HTTP
	// request draining. Both must complete before the process exits.
	workerCancel()

	workerDone := make(chan struct{})
	go func() {
		drainWorker()
		close(workerDone)
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("forced shutdown", "error", err)
		os.Exit(1)
	}

	<-workerDone // wait for current poll cycle to complete before db.Close()
	logger.Info("server stopped")
}
