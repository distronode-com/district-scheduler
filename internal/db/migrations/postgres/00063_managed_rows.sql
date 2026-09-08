-- +goose Up
-- Managed rows: an api_keys or webhooks row the PLATFORM owns, not the workspace.
--
-- Two rows created by POST /v1/platform/workspaces sit on the owner user and are
-- reachable from the tenant's own credential routes: the 'platform-provisioned' API key an
-- integration spends, and the provisioning webhook the platform receives bookings on.
-- Deleting either through GET/DELETE /v1/api-keys or /v1/webhooks breaks the integration
-- silently — the workspace keeps working, the platform stops hearing about it. `managed`
-- marks those rows so the credential routes hide them and refuse to change them (403),
-- while everything on the DELIVERY side keeps reading them exactly as before.
--
-- SMALLINT, not BOOLEAN: every flag in this schema is SMALLINT on Postgres and INTEGER on
-- SQLite (users.is_admin, event_types.is_active, webhooks.is_active), and the Go scan
-- targets are ints. A BOOLEAN here would be the only one, and would fail those scans.
--
-- The default is 0, so every existing row — and every row a tenant creates for itself —
-- is unmanaged, which is what it was before this migration.
ALTER TABLE api_keys ADD COLUMN managed SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE webhooks ADD COLUMN managed SMALLINT NOT NULL DEFAULT 0;

-- Backfill for tenancies provisioned before this migration.
--
-- 'platform-provisioned' is written by exactly one statement in the tree (CreateWorkspace
-- in internal/handler/platform.go), so it is a precise marker rather than a guess. A
-- single-tenant database has no such row and this UPDATE is a no-op there.
--
-- ⚠️ There is NO equivalent backfill for webhooks: the provisioning webhook carries no
-- marker of its own — its url, events and fields are all values the caller chose, and a
-- tenant could legitimately have created the same subscription itself. Guessing by url
-- shape would mark rows the platform does not own. PATCH /v1/platform/workspaces/{id}/
-- webhooks is how the platform marks its own, by exact url, once it knows which it is.
UPDATE api_keys SET managed = 1 WHERE name = 'platform-provisioned';

-- +goose Down
ALTER TABLE webhooks DROP COLUMN managed;
ALTER TABLE api_keys DROP COLUMN managed;
