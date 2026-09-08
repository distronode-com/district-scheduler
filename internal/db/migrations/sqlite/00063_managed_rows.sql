-- +goose Up
-- See the Postgres half for what `managed` means and why the webhook has no backfill.
-- Identical here apart from the type: INTEGER is SQLite's spelling of the SMALLINT flags
-- the Postgres schema uses, and it is what every other boolean column in this directory is.
--
-- SQLite cannot run multi-tenant (config.Validate refuses MULTI_TENANT without a
-- postgres:// DSN) and only the platform API ever sets this flag, so on this engine the
-- column is always 0. It exists because the two migration directories keep one file per
-- version, because the cross-engine column comparison asserts the schemas agree, and
-- because no Go call site should have to ask which engine it is on before naming the
-- column.
--
-- Plain ADD COLUMN with a constant default, so no table is rebuilt and every existing row
-- keeps behaving as it did.
ALTER TABLE api_keys ADD COLUMN managed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE webhooks ADD COLUMN managed INTEGER NOT NULL DEFAULT 0;

-- A no-op on this engine: 'platform-provisioned' is written only by the platform API,
-- which cannot run here. Kept so the two files apply the same change.
UPDATE api_keys SET managed = 1 WHERE name = 'platform-provisioned';

-- +goose Down
ALTER TABLE webhooks DROP COLUMN managed;
ALTER TABLE api_keys DROP COLUMN managed;
