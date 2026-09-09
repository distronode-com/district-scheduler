-- +goose Up
-- At most one live owner per workspace. See the PostgreSQL file of the same number for
-- why this is an index rather than a constraint, and which race it closes.
--
-- On this engine every row carries workspace_id = 'default', so the rule reads as "one
-- live owner in the database", which is what single-tenant mode has always meant. The
-- index is created here anyway rather than skipped, so the two schemas stay in lockstep
-- and a rule cannot hold on one engine and not the other.
--
-- ⚠️ Fails on a database that already holds two live owners, deliberately. See the
-- PostgreSQL file.
CREATE UNIQUE INDEX idx_users_one_owner_per_workspace
    ON users (workspace_id)
    WHERE is_owner = 1 AND archived_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_users_one_owner_per_workspace;
