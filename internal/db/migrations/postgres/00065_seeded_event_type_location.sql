-- +goose Up
-- Repair the event types provisioning seeded into a state the editor refuses to save.
--
-- seedWorkspaceEventType (internal/handler/platform.go) inserted every tenant's default
-- event type with location_type 'link' and a NULL location_value. That is the schema's own
-- column default, and it looks like what the admin UI produces; it is not. The UI's create
-- path runs the location through validateLocation, which rejects link-with-no-URL — so the
-- row was born in a state no save could reproduce. The editor submits the whole form on
-- every save, so the tenant's FIRST edit of ANY field came back 400 "enter a valid meeting
-- URL (https://…)" about a location they had never touched, and no other field could be
-- saved either.
--
-- The seed now writes 'in_person' with no value, which is the one location validateLocation
-- accepts with no value and no connected provider. That fixes the rows written from here
-- on. This fixes the ones already written: the production row for the QA tenancy read
-- phone-consultation | link | NULL.
--
-- ⚠️ Deliberately CROSS-TENANT: no workspace_id predicate, because every workspace
-- provisioned before the fix holds one of these and there is nothing tenant-specific about
-- the repair. Migrations run on the PLATFORM handle (cmd/calnode/main.go calls
-- platform.Migrate()), which per internal/db/pair.go owns the schema and carries BYPASSRLS
-- — and on a fresh database EnableRLS has not run yet at that point, because main.go calls
-- it after Migrate. Migration 00063 already does a cross-tenant UPDATE on api_keys the same
-- way.
--
-- ⛔ Narrow on purpose. Only 'link' with NO value is repaired: a 'link' row that carries a
-- URL is a location somebody chose and it is valid, so it is left exactly as it is. The
-- COALESCE also catches an empty string, which is the same absence spelled differently and
-- which validateLocation rejects identically. Nothing here can reach a row that was ever
-- saveable.
UPDATE event_types
SET location_type = 'in_person', location_value = NULL
WHERE location_type = 'link' AND COALESCE(location_value, '') = '';

-- +goose Down
-- Deliberately empty.
--
-- The state this migration left is 'in_person' with no value; the state it came from is
-- 'link' with no value, which validateLocation rejects and which locks the owner out of
-- the editor. Restoring it would re-break every tenancy this repaired, and there is no
-- record of which rows were changed anyway — a row now reading 'in_person'/NULL may always
-- have read that way. A down migration that cannot tell the two apart would have to guess,
-- and the only answer it could guess is the invalid one.
--
-- Rolling back past this migration is therefore safe and does nothing: the column values
-- are valid under every schema version involved.
SELECT 1;
