-- +goose Up
-- At most one live owner per workspace, enforced by the database rather than by the
-- code remembering to check.
--
-- TransferOwnership already maintains this inside a transaction, demoting the current
-- owner before promoting the target. The SSO hand-off did not: both its create path and
-- its existing-user path read "does this workspace have an owner" and then wrote
-- is_owner = 1, with nothing between the read and the write. Two concurrent hand-offs
-- carrying role=owner on an unowned workspace could each see no owner and each become
-- one, which breaks the invariant every other owner-only check is written against.
--
-- A partial index rather than a constraint, because the rule is conditional twice over:
-- only rows with is_owner = 1 participate, and only while they are not archived. An
-- archived owner is not the workspace's owner for any purpose (ssoResolveUser refuses to
-- sign one in at all), so it must not block a live one from being appointed.
--
-- ⚠️ This will FAIL on a database that already holds two live owners for one workspace,
-- and failing is the point: that is the state this index exists to make impossible, and
-- picking one of them automatically would be guessing at which. Resolve it by hand, then
-- migrate.
--
-- The index also gives the code something to catch. With it in place, ownership can be
-- claimed by writing and handling the unique violation, which is decided by the database
-- under contention rather than by whichever request read first.
CREATE UNIQUE INDEX idx_users_one_owner_per_workspace
    ON users (workspace_id)
    WHERE is_owner = 1 AND archived_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_users_one_owner_per_workspace;
