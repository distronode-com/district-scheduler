package db

import (
	"context"
	"errors"
	"fmt"
)

// OpenPair opens the two handles multi-tenant mode needs (D4).
//
//	app      DATABASE_URL       — the application role. NOBYPASSRLS, and it must
//	                              not own the tables: a role that owns a table is
//	                              exempt from that table's policy unless FORCE is
//	                              set, and BYPASSRLS is exempt regardless. This is
//	                              the handle every request runs on, through
//	                              ForWorkspace.
//	platform DATABASE_ADMIN_URL — the platform role. Owner of the schema, with
//	                              BYPASSRLS. Runs migrations and EnableRLS, the
//	                              worker's cross-tenant claim loop, the
//	                              reconciler's workspace enumeration, the platform
//	                              API, and the credential lookups that have to
//	                              resolve a tenant before one is known.
//
// app.Platform() returns platform, so nothing downstream has to carry both.
// Both are returned so the caller closes both; Close on one closes one pool.
//
// Both DSNs must be PostgreSQL. SQLite has no row-level security, so there is
// nothing to enforce isolation with and a "multi-tenant" SQLite instance would
// separate nothing — config.Validate refuses that combination before this is
// reached, and this refuses it again because a library that only works when its
// caller validated is a library that will one day be called by something else.
func OpenPair(appURL, adminURL string, opts ...Option) (app, platform *DB, err error) {
	if dialectFromURL(appURL) != DialectPostgres {
		return nil, nil, errors.New("db: multi-tenant mode needs a postgres:// application DSN — " +
			"tenant isolation is PostgreSQL row-level security")
	}
	if dialectFromURL(adminURL) != DialectPostgres {
		return nil, nil, errors.New("db: multi-tenant mode needs a postgres:// platform DSN")
	}

	platform, err = OpenDB(adminURL, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("open platform handle: %w", err)
	}
	// The platform handle binds too, so that "every statement on a paired handle
	// sets app.workspace_id" has no exceptions to remember. It binds '', which no
	// workspace id can equal, so it matches no row under the policies — and it is
	// inert in practice because the platform role bypasses them. VerifyRoles is
	// what makes that "in practice" a checked fact rather than an assumption.
	platform.multiTenant = true

	app, err = OpenDB(appURL, opts...)
	if err != nil {
		platform.Close() // #nosec G104 -- unwinding the handle opened a few lines above; the application-handle error returned below is the one the caller acts on
		return nil, nil, fmt.Errorf("open application handle: %w", err)
	}
	app.multiTenant = true
	app.platform = platform

	return app, platform, nil
}

// VerifyRoles checks the two role attributes the isolation guarantee rests on,
// and is meant to run at boot, right after EnableRLS, with the boot failing if it
// does. It is called on the application handle.
//
// ⛔ Both halves are load-bearing and both fail SILENTLY if they are wrong.
//
//   - If the application role is a superuser, has BYPASSRLS, or owns any tenant
//     table, it is not constrained by the policies. Nothing breaks. Every request
//     works. It can also read every other workspace's rows. That is the
//     misconfiguration this exists for: the failure mode is a security hole with
//     no symptom.
//   - If the platform role does NOT bypass — it is the owner, and FORCE ROW LEVEL
//     SECURITY covers owners — then the platform handle's ” binding matches no
//     row and the worker claims nothing, the reconciler enumerates nothing, and
//     the platform API returns empty. Also no error, just an instance that quietly
//     does no background work.
//
// It is a no-op outside multi-tenant mode, where there is one role and no policy.
func (h *DB) VerifyRoles(ctx context.Context) error {
	if !h.binds() {
		return nil
	}

	var super, bypass bool
	if err := h.DB.QueryRowContext(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		return fmt.Errorf("read the application role's attributes: %w", err)
	}
	if super || bypass {
		return fmt.Errorf("the DATABASE_URL role bypasses row-level security (superuser=%v bypassrls=%v), "+
			"so every workspace can read every other workspace's rows; it must be a NOBYPASSRLS, non-superuser role",
			super, bypass)
	}

	var owned int
	if err := h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_tables
		 WHERE schemaname = current_schema() AND tableowner = current_user`).Scan(&owned); err != nil {
		return fmt.Errorf("count tables owned by the application role: %w", err)
	}
	if owned > 0 {
		return fmt.Errorf("the DATABASE_URL role owns %d tables in this schema; "+
			"it must not own them, or its row-level-security policies do not apply to it "+
			"(FORCE covers the owner, but relying on that leaves no margin)", owned)
	}

	platform := h.Platform()
	if platform == h {
		return errors.New("multi-tenant mode has no platform handle; open the pair with db.OpenPair")
	}
	var pSuper, pBypass bool
	if err := platform.DB.QueryRowContext(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&pSuper, &pBypass); err != nil {
		return fmt.Errorf("read the platform role's attributes: %w", err)
	}
	if !pSuper && !pBypass {
		return errors.New("the DATABASE_ADMIN_URL role neither is a superuser nor has BYPASSRLS, " +
			"so it cannot read across workspaces: the worker would claim no jobs and the reconciler would " +
			"enumerate nothing, silently")
	}

	return nil
}
