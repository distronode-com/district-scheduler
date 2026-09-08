package handler_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// The platform member API (F1): upsert a user, mint and revoke its keys, mark a webhook
// managed, archive.
//
// ⛔ Postgres, through a real OpenPair, for the same reason platform_test.go is: the
// assertions read across the tenant boundary on the platform handle, the routes write on
// it, and the credential half (RequireAuth resolving a minted key) has to run against a
// NOBYPASSRLS application role or it proves nothing about isolation.

// memberAPI returns the F1 routes so far plus workspace create, the handler itself (for
// RequireAuth), and both handles.
func memberAPI(t *testing.T) (routes map[string]http.HandlerFunc, h *handler.Handler, app, platform *db.DB) {
	t.Helper()
	app, platform = dbtest.RequireTenantPair(t)

	h = handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)

	return map[string]http.HandlerFunc{
		"create":     h.Platform((*handler.Handler).CreateWorkspace),
		"upsertUser": h.Platform((*handler.Handler).UpsertWorkspaceUser),
	}, h, app, platform
}

// memberReq drives a route directly, setting the mux path values httptest.NewRequest does
// not populate. Without {id}/{uid}/{keyId} every route reads an empty id and answers 404,
// which looks exactly like a missing workspace.
func memberReq(t *testing.T, route http.HandlerFunc, method, target string, body any, token string, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	route(rec, req)
	return rec
}

// provisionTenancy creates a workspace through the real platform route and returns its
// owner's id.
func provisionTenancy(t *testing.T, routes map[string]http.HandlerFunc, platform *db.DB, id string) (ownerID string) {
	t.Helper()
	rec := memberReq(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody(id, "book."+id+".example"), platformToken, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("provision %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	if err := platform.QueryRow(`SELECT id FROM users WHERE workspace_id = ?`, id).Scan(&ownerID); err != nil {
		t.Fatalf("read owner of %s: %v", id, err)
	}
	return ownerID
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

// ---------------------------------------------------------------------------
// Upsert
// ---------------------------------------------------------------------------

func TestPostgres_PlatformMembers_upsertCreatesThenUpdates(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	provisionTenancy(t, routes, platform, "up1")

	// Create. The address arrives mixed-case and padded; it is stored lower-cased and
	// trimmed, because users is unique on (workspace_id, email) and a second upsert of
	// "Ada@…" must find the row the first one wrote.
	rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/up1/users",
		map[string]any{"email": "  Ada@Up1.Example  ", "name": "Ada L", "role": "member", "timezone": "Europe/Tallinn"},
		platformToken, map[string]string{"id": "up1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Role    string `json:"role"`
		Created bool   `json:"created"`
	}
	decodeJSON(t, rec, &created)
	if !created.Created {
		t.Errorf("created = false on the first upsert")
	}
	if created.Email != "ada@up1.example" {
		t.Errorf("email = %q; want it lower-cased and trimmed", created.Email)
	}
	if created.Role != "member" {
		t.Errorf("role = %q; want member", created.Role)
	}

	// Update: name overwritten, role raised, timezone OMITTED and therefore kept.
	rec = memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/up1/users",
		map[string]any{"email": "ADA@up1.example", "name": "Ada Lovelace", "role": "admin"},
		platformToken, map[string]string{"id": "up1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d, body %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	decodeJSON(t, rec, &updated)
	if updated.Created {
		t.Errorf("created = true on the second upsert of the same address")
	}
	if updated.ID != created.ID {
		t.Errorf("the second upsert made a new row (%s vs %s) — the email match is not working",
			updated.ID, created.ID)
	}

	var name, tz string
	var isAdmin, isOwner int
	if err := platform.QueryRow(
		`SELECT name, iana_timezone, is_admin, is_owner FROM users WHERE workspace_id = ? AND id = ?`,
		"up1", created.ID).Scan(&name, &tz, &isAdmin, &isOwner); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Ada Lovelace" {
		t.Errorf("name = %q; want it overwritten", name)
	}
	if tz != "Europe/Tallinn" {
		t.Errorf("iana_timezone = %q; want it KEPT when the upsert omitted timezone", tz)
	}
	if isAdmin != 1 || isOwner != 0 {
		t.Errorf("is_admin/is_owner = %d/%d; want 1/0 for admin", isAdmin, isOwner)
	}
}

func TestPostgres_PlatformMembers_upsertRejectsBadInput(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	provisionTenancy(t, routes, platform, "up2")

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"empty email", map[string]any{"email": "  ", "name": "A", "role": "member"}},
		{"malformed email", map[string]any{"email": "nope", "name": "A", "role": "member"}},
		{"empty name", map[string]any{"email": "a@up2.example", "name": " ", "role": "member"}},
		{"unknown role", map[string]any{"email": "a@up2.example", "name": "A", "role": "superuser"}},
		{"bad timezone", map[string]any{"email": "a@up2.example", "name": "A", "role": "member", "timezone": "Mars/Olympus"}},
	} {
		rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/up2/users",
			tc.body, platformToken, map[string]string{"id": "up2"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d; want 400 (%s)", tc.name, rec.Code, rec.Body.String())
		}
	}

	// An unknown workspace is 404, not a foreign-key 500.
	rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/nope/users",
		map[string]any{"email": "a@b.example", "name": "A", "role": "member"},
		platformToken, map[string]string{"id": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown workspace: status %d; want 404 (%s)", rec.Code, rec.Body.String())
	}
}

// Promoting somebody to owner is a TRANSFER: the workspace has exactly one owner before
// and after, and the previous owner keeps admin.
func TestPostgres_PlatformMembers_upsertOwnerTransfersOwnership(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	firstOwner := provisionTenancy(t, routes, platform, "own1")

	rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/own1/users",
		map[string]any{"email": "new@own1.example", "name": "New Owner", "role": "owner"},
		platformToken, map[string]string{"id": "own1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("promote: status %d, body %s", rec.Code, rec.Body.String())
	}
	var promoted struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &promoted)

	var owners int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM users WHERE workspace_id = ? AND is_owner = 1`, "own1").Scan(&owners); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if owners != 1 {
		t.Fatalf("owner count = %d after a transfer; want exactly 1", owners)
	}

	var newIsOwner, newIsAdmin int
	platform.QueryRow(`SELECT is_owner, is_admin FROM users WHERE id = ?`, promoted.ID).Scan(&newIsOwner, &newIsAdmin) //nolint:errcheck
	if newIsOwner != 1 || newIsAdmin != 1 {
		t.Errorf("the new owner is is_owner=%d is_admin=%d; want 1/1 (owner implies admin)", newIsOwner, newIsAdmin)
	}
	var oldIsOwner, oldIsAdmin int
	platform.QueryRow(`SELECT is_owner, is_admin FROM users WHERE id = ?`, firstOwner).Scan(&oldIsOwner, &oldIsAdmin) //nolint:errcheck
	if oldIsOwner != 0 {
		t.Errorf("the previous owner is still is_owner=1")
	}
	if oldIsAdmin != 1 {
		t.Errorf("the previous owner lost admin (is_admin=%d); a transfer demotes to admin, not to member", oldIsAdmin)
	}
}

// ⛔ Demoting the only owner is 409 and changes nothing. Obeying it would leave a workspace
// nothing in the fork can give an owner back to.
func TestPostgres_PlatformMembers_upsertRefusesZeroOwnerDemotion(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	provisionTenancy(t, routes, platform, "own2")

	var ownerEmail string
	if err := platform.QueryRow(
		`SELECT email FROM users WHERE workspace_id = ? AND is_owner = 1`, "own2").Scan(&ownerEmail); err != nil {
		t.Fatalf("read owner email: %v", err)
	}

	for _, role := range []string{"admin", "member"} {
		rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/own2/users",
			map[string]any{"email": ownerEmail, "name": "Owner own2", "role": role},
			platformToken, map[string]string{"id": "own2"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("demote the only owner to %s: status %d; want 409 (%s)", role, rec.Code, rec.Body.String())
		}
		var body struct {
			Error string `json:"error"`
		}
		decodeJSON(t, rec, &body)
		if body.Error != "owner_demotion_requires_transfer" {
			t.Errorf("error = %q; want owner_demotion_requires_transfer", body.Error)
		}
	}

	var owners int
	platform.QueryRow(`SELECT COUNT(*) FROM users WHERE workspace_id = ? AND is_owner = 1`, "own2").Scan(&owners) //nolint:errcheck
	if owners != 1 {
		t.Errorf("owner count = %d after two refused demotions; want 1 — the refusal changed something", owners)
	}

	// The documented way through: upsert the NEW owner (which demotes this one), then the
	// demotion is no longer a zero-owner move.
	rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/own2/users",
		map[string]any{"email": "next@own2.example", "name": "Next", "role": "owner"},
		platformToken, map[string]string{"id": "own2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("promote the replacement: status %d, body %s", rec.Code, rec.Body.String())
	}
	rec = memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/own2/users",
		map[string]any{"email": ownerEmail, "name": "Owner own2", "role": "member"},
		platformToken, map[string]string{"id": "own2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("demote after the transfer: status %d, body %s", rec.Code, rec.Body.String())
	}
}
