package db

import (
	"context"
	"fmt"
)

// DefaultWorkspaceID is the tenant every row belongs to when MULTI_TENANT is
// unset. It is a literal rather than a generated id so that a single-tenant
// database migrated by 00060 reads the same on every machine, and so the SQLite
// column default can be a constant.
const DefaultWorkspaceID = "default"

// WorkspaceSetting is the PostgreSQL session parameter a tenant handle binds
// before every statement. The row-level-security policies compare
// workspace_id against it, and its two-argument current_setting form returns
// NULL when it has never been set — so an unbound session matches no row.
const WorkspaceSetting = "app.workspace_id"

// TenantTables lists every table that carries a workspace_id and a
// row-level-security policy. It is the Go copy of the list in the header of
// migration 00060, and TestTenancy_tableListsCoverTheSchema fails if either side
// drifts: a table added by a later migration must be classified here or in
// ExemptTables, so "nobody remembered to protect it" is not a possible outcome.
//
// Sorted, because the migration's ALTER and CREATE POLICY blocks are sorted and
// a reader diffing the two should not have to reorder one of them first.
var TenantTables = []string{
	"api_keys",
	"availability_overrides",
	"availability_rules",
	"booking_answers",
	"booking_attendees",
	"booking_hosts",
	"booking_manage_tokens",
	"bookings",
	"calendar_connections",
	"connection_calendars",
	"event_type_hosts",
	"event_type_questions",
	"event_type_reminders",
	"event_types",
	"idempotency_keys",
	"invite_tokens",
	"jobs",
	"magic_link_tokens",
	"meeting_consents",
	"notes",
	"oauth_access_tokens",
	"oauth_auth_codes",
	"recordings",
	"server_settings",
	"sessions",
	"team_members",
	"teams",
	"transcripts",
	"users",
	"webhook_deliveries",
	"webhooks",
	"zoom_connections",
}

// ReadOnlyTables lists the tables that have no workspace_id and are nonetheless
// under row-level security, because "every role may read this, only the platform
// role may write it" is itself a policy and something has to enforce it.
//
// Today that is workspaces, the tenant root. The host resolver, the suspended check
// and publicURL all read it on the application handle, and every write to it is on
// a Platform-wrapped route or at boot, both of which hold the platform handle.
//
// ⛔ ENABLE, never FORCE. FORCE applies a table's policies to its OWNER as well, and
// the owner here is the platform role — the one role that must be able to write the
// table. Forcing it would leave nothing able to create a workspace. The application
// role is not the owner, so ENABLE alone is what constrains it.
//
// ⚠️ The policy for these lives in migration 00060 and is SELECT-only, so a table
// listed here and given no write policy is readable by everyone and writable only by
// the owner and by BYPASSRLS roles. Adding a table here without checking its policies
// would silently make it read-only for the application.
var ReadOnlyTables = []string{
	"workspaces",
}

// ExemptTables lists the tables that deliberately have no workspace_id and no
// row-level security at all.
//
//   - crypto_keystore holds the wrapped DEK, and there is one DEK per process
//     (ARCHITECTURE §5). Per-tenant DEKs are a later hardening.
//   - goose_db_version is migration bookkeeping.
//   - sso_nonces holds the replay guard for /v1/auth/sso. A nonce is a jti: it is
//     meaningful GLOBALLY, because the question it answers is "has this exact token
//     been spent", and the answer must be the same regardless of which workspace the
//     token names. Per workspace it would be weaker for no benefit — the same jti
//     could be replayed once per tenant. It also has no natural owner: the row exists
//     before the token's `wid` claim has been trusted.
//   - oauth_clients is dynamic client registration, which is per client
//     APPLICATION rather than per tenant: one connector registration serves
//     every workspace it is later authorised against, and the grant that IS
//     per-tenant lives in oauth_access_tokens, which is a tenant table.
var ExemptTables = []string{
	"crypto_keystore",
	"goose_db_version",
	"oauth_clients",
	"sso_nonces",
}

// EnableRLS turns row-level security on, and forces it, for every tenant table.
// It is idempotent, so it can run on every boot, and it is a no-op on SQLite,
// which has no row-level security to enable.
//
// This is deliberately NOT part of migration 00060. FORCE ROW LEVEL SECURITY
// makes a table's policy apply to its OWNER as well, and in single-tenant mode
// DATABASE_URL is the owner: a schema migrated with FORCE and no
// app.workspace_id binding returns zero rows to its owner for every SELECT.
// Measured against PostgreSQL 17.11 with a NOBYPASSRLS owner role — and hidden
// entirely by a superuser DSN, since superusers bypass RLS, which is the shape
// of green that proves nothing. Gating the two ALTERs on MULTI_TENANT keeps the
// promise that a single-tenant database behaves exactly as it did before.
//
// The policies themselves live in the migration, where they are reviewable SQL.
// A policy on a table whose row-level security is not enabled is inert, so
// creating them unconditionally costs a single-tenant instance nothing.
//
// Table names are interpolated into the DDL because PostgreSQL takes no
// placeholder for an identifier. They come from TenantTables, a committed
// constant list, never from input.
// SuspendDefaultWorkspace marks the seeded `default` workspace suspended. It runs at
// multi-tenant boot only, and is idempotent.
//
// Migration 00060 seeds `default` because it is the workspace every single-tenant row
// belongs to and the SQLite column default names it. On a MULTI_TENANT instance that row
// is a tenant nobody owns: it has no public_host (deliberately, so no Host can reach it),
// no users, and no settings. Left `active` it is still enumerated by every background
// sweep — the reconciler takes a pass over it on each cycle, and any future periodic loop
// would too — so the instance does work on behalf of a tenant that cannot receive it.
//
// ⛔ Suspending it at BOOT rather than in the migration is the whole point. The migration
// cannot see MULTI_TENANT, and in single-tenant mode `default` IS the workspace: a
// suspended row there would make Scoped answer 503 to every request on the instance. So
// the mode decides, and the mode is only known here.
//
// Suspension is the right shape rather than deletion: the row is referenced by
// server_settings and by any single-tenant data a converted instance still holds, and
// activeWorkspaceIDs already filters on status = 'active', so one status flip removes it
// from every sweep at once (D12's suspended semantics, reused).
func (h *DB) SuspendDefaultWorkspace(ctx context.Context) error {
	if _, err := h.Platform().ExecContext(ctx,
		`UPDATE workspaces SET status = 'suspended' WHERE id = ? AND status <> 'suspended'`,
		DefaultWorkspaceID); err != nil {
		return fmt.Errorf("suspend the default workspace: %w", err)
	}
	return nil
}

func (h *DB) EnableRLS(ctx context.Context) error {
	if h.dialect != DialectPostgres {
		return nil
	}
	for _, table := range TenantTables {
		if _, err := h.DB.ExecContext(ctx, `ALTER TABLE `+table+` ENABLE ROW LEVEL SECURITY`); err != nil {
			return fmt.Errorf("enable row level security on %s: %w", table, err)
		}
		if _, err := h.DB.ExecContext(ctx, `ALTER TABLE `+table+` FORCE ROW LEVEL SECURITY`); err != nil {
			return fmt.Errorf("force row level security on %s: %w", table, err)
		}
	}
	// ENABLE without FORCE — see ReadOnlyTables. Without this the SELECT-only policy
	// migration 00060 creates on workspaces is inert, because a policy on a table whose
	// row-level security is off does nothing at all, and the application role's DML
	// grant on the tenant root would be unopposed.
	for _, table := range ReadOnlyTables {
		if _, err := h.DB.ExecContext(ctx, `ALTER TABLE `+table+` ENABLE ROW LEVEL SECURITY`); err != nil {
			return fmt.Errorf("enable row level security on %s: %w", table, err)
		}
	}
	return nil
}
