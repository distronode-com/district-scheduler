package handler_test

import (
	"net/http"
	"testing"
)

// Managed rows (F1, migration 00063).
//
// The two rows POST /v1/platform/workspaces creates on the owner user — the
// 'platform-provisioned' API key an integration spends, and the webhook the platform
// receives bookings on — belong to the platform, not to the workspace. They are marked
// `managed = 1` at provisioning time, which is what makes the credential routes hide them
// and refuse to change them.
//
// ⛔ Postgres only, through a real OpenPair: the assertions read across the tenant boundary
// on the platform handle, and the routes they are about run on the application handle. One
// handle for both would prove neither.

// TestPostgres_ManagedRows_provisioningMarksTheKeyAndTheWebhook is also the migration's
// backfill test in the only form that is worth writing: the backfill exists to bring
// tenancies created BEFORE 00063 to the state a tenancy created after it is already in, so
// the state is what gets asserted. `platform-provisioned` is the marker the backfill keys
// on, and CreateWorkspace is the one statement in the tree that writes that name.
func TestPostgres_ManagedRows_provisioningMarksTheKeyAndTheWebhook(t *testing.T) {
	routes, _, platform := newPlatformAPI(t)

	rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("managed1", "book.managed1.example"), platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body.String())
	}

	var keyName string
	var keyManaged int
	if err := platform.QueryRow(
		`SELECT name, managed FROM api_keys WHERE workspace_id = ?`, "managed1").
		Scan(&keyName, &keyManaged); err != nil {
		t.Fatalf("read provisioned key: %v", err)
	}
	if keyName != "platform-provisioned" {
		t.Errorf("provisioned key name = %q; want platform-provisioned (the backfill's marker)", keyName)
	}
	if keyManaged != 1 {
		t.Errorf("provisioned key managed = %d; want 1 — the owner could delete it through "+
			"DELETE /v1/api-keys and silently break the integration", keyManaged)
	}

	var whManaged int
	if err := platform.QueryRow(
		`SELECT managed FROM webhooks WHERE workspace_id = ?`, "managed1").Scan(&whManaged); err != nil {
		t.Fatalf("read provisioned webhook: %v", err)
	}
	if whManaged != 1 {
		t.Errorf("provisioned webhook managed = %d; want 1", whManaged)
	}
}

// A key a member mints for itself is NOT managed. The flag has to distinguish the two, or
// hiding managed rows would hide every key in the workspace.
func TestPostgres_ManagedRows_aWorkspacesOwnKeyIsNotManaged(t *testing.T) {
	routes, app, platform := newPlatformAPI(t)

	rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("managed2", "book.managed2.example"), platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body.String())
	}

	var ownerID string
	if err := platform.QueryRow(
		`SELECT id FROM users WHERE workspace_id = ?`, "managed2").Scan(&ownerID); err != nil {
		t.Fatalf("read owner: %v", err)
	}

	// Written through the APPLICATION handle bound to the tenant, which is the path
	// POST /v1/api-keys takes.
	bound := app.ForWorkspace("managed2")
	if _, err := bound.Exec(`
		INSERT INTO api_keys (id, workspace_id, user_id, name, key_hash, created_at)
		VALUES (?, ?, ?, 'mine', 'deadbeef', '2026-01-01T00:00:00.000Z')`,
		"key-own-managed2", "managed2", ownerID); err != nil {
		t.Fatalf("insert own key: %v", err)
	}

	var managed int
	if err := platform.QueryRow(
		`SELECT managed FROM api_keys WHERE id = ?`, "key-own-managed2").Scan(&managed); err != nil {
		t.Fatalf("read own key: %v", err)
	}
	if managed != 0 {
		t.Errorf("a key the workspace created has managed = %d; want 0 — the column default "+
			"is what keeps every pre-existing row unmanaged", managed)
	}
}
