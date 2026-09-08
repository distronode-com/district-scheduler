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

// memberAPI returns the F1 routes plus the four provisioning ones, the handler itself (for
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
		"mintKey":    h.Platform((*handler.Handler).MintWorkspaceUserAPIKey),
		"deleteKey":  h.Platform((*handler.Handler).DeleteWorkspaceUserAPIKey),
		"markHooks":  h.Platform((*handler.Handler).MarkWorkspaceWebhooksManaged),
		"archive":    h.Platform((*handler.Handler).ArchiveWorkspaceUser),
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
// Gating: 401 without the token, 404 on a single-tenant instance
// ---------------------------------------------------------------------------

// The five routes are gated by platformAuthorized exactly as the existing four are: a
// wrong token is 401, and an unset token or a single-tenant instance is 404 — the same
// answer for both, so a prober cannot tell a control plane from an instance that has none.
func TestPostgres_PlatformMembers_routesRefuseWithoutTheToken(t *testing.T) {
	routes, _, _, _ := memberAPI(t)

	cases := []struct {
		name   string
		route  string
		method string
		target string
		path   map[string]string
		body   any
	}{
		{"upsert user", "upsertUser", http.MethodPost, "/v1/platform/workspaces/w/users",
			map[string]string{"id": "w"}, map[string]any{"email": "a@b.example", "name": "A", "role": "member"}},
		{"mint key", "mintKey", http.MethodPost, "/v1/platform/workspaces/w/users/u/api-keys",
			map[string]string{"id": "w", "uid": "u"}, map[string]any{"name": "district"}},
		{"delete key", "deleteKey", http.MethodDelete, "/v1/platform/workspaces/w/users/u/api-keys/k",
			map[string]string{"id": "w", "uid": "u", "keyId": "k"}, nil},
		{"mark webhooks", "markHooks", http.MethodPatch, "/v1/platform/workspaces/w/webhooks",
			map[string]string{"id": "w"}, map[string]any{"url": "https://x.example", "managed": true}},
		{"archive", "archive", http.MethodPost, "/v1/platform/workspaces/w/users/u/archive",
			map[string]string{"id": "w", "uid": "u"}, nil},
	}

	for _, tc := range cases {
		rec := memberReq(t, routes[tc.route], tc.method, tc.target, tc.body, "wrong-token", tc.path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token: status %d; want 401", tc.name, rec.Code)
		}
		rec = memberReq(t, routes[tc.route], tc.method, tc.target, tc.body, "", tc.path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no token: status %d; want 401", tc.name, rec.Code)
		}
	}
}

// The same five on an instance that is not multi-tenant: 404, before any body is read.
func TestPostgres_PlatformMembers_routes404OnASingleTenantInstance(t *testing.T) {
	app, _ := dbtest.RequireTenantPair(t)
	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetPlatformToken(platformToken) // configured, but multi-tenant is OFF
	h.SetEncKey(platformTestEncKey)

	routes := map[string]http.HandlerFunc{
		"upsertUser": h.Platform((*handler.Handler).UpsertWorkspaceUser),
		"mintKey":    h.Platform((*handler.Handler).MintWorkspaceUserAPIKey),
		"deleteKey":  h.Platform((*handler.Handler).DeleteWorkspaceUserAPIKey),
		"markHooks":  h.Platform((*handler.Handler).MarkWorkspaceWebhooksManaged),
		"archive":    h.Platform((*handler.Handler).ArchiveWorkspaceUser),
	}
	for name, route := range routes {
		rec := memberReq(t, route, http.MethodPost, "/v1/platform/workspaces/w/anything",
			map[string]any{"email": "a@b.example", "name": "A", "role": "member", "url": "https://x", "managed": true},
			platformToken, map[string]string{"id": "w", "uid": "u", "keyId": "k"})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on a single-tenant instance: status %d; want 404", name, rec.Code)
		}
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

// An upsert un-archives, because a role sync from the directory is the statement "this
// person is in this workspace" and that is the whole point of the route.
func TestPostgres_PlatformMembers_upsertUnarchives(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	provisionTenancy(t, routes, platform, "unarch")

	rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/unarch/users",
		map[string]any{"email": "gone@unarch.example", "name": "Gone", "role": "member"},
		platformToken, map[string]string{"id": "unarch"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body.String())
	}
	var user struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &user)

	rec = memberReq(t, routes["archive"], http.MethodPost,
		"/v1/platform/workspaces/unarch/users/"+user.ID+"/archive", nil, platformToken,
		map[string]string{"id": "unarch", "uid": user.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/unarch/users",
		map[string]any{"email": "gone@unarch.example", "name": "Back", "role": "admin"},
		platformToken, map[string]string{"id": "unarch"})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-upsert: status %d, body %s", rec.Code, rec.Body.String())
	}
	var archived *string
	if err := platform.QueryRow(`SELECT archived_at FROM users WHERE id = ?`, user.ID).Scan(&archived); err != nil {
		t.Fatalf("read archived_at: %v", err)
	}
	if archived != nil {
		t.Errorf("archived_at = %q after a re-upsert; want NULL", *archived)
	}
}

// ---------------------------------------------------------------------------
// Minting and rotation
// ---------------------------------------------------------------------------

func TestPostgres_PlatformMembers_mintKeyAuthenticatesAsThatUser(t *testing.T) {
	routes, h, _, platform := memberAPI(t)
	provisionTenancy(t, routes, platform, "mint1")

	rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/mint1/users",
		map[string]any{"email": "adm@mint1.example", "name": "Adm", "role": "admin"},
		platformToken, map[string]string{"id": "mint1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: status %d, body %s", rec.Code, rec.Body.String())
	}
	var user struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &user)

	rec = memberReq(t, routes["mintKey"], http.MethodPost,
		"/v1/platform/workspaces/mint1/users/"+user.ID+"/api-keys",
		map[string]any{"name": "district"}, platformToken,
		map[string]string{"id": "mint1", "uid": user.ID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: status %d, body %s", rec.Code, rec.Body.String())
	}
	var minted struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		APIKey string `json:"api_key"`
	}
	decodeJSON(t, rec, &minted)
	if len(minted.APIKey) < 5 || minted.APIKey[:4] != "cno_" {
		t.Fatalf("api_key = %q; want a cno_ key", minted.APIKey)
	}
	if minted.Name != "district" {
		t.Errorf("name = %q; want district", minted.Name)
	}

	var managed int
	platform.QueryRow(`SELECT managed FROM api_keys WHERE id = ?`, minted.ID).Scan(&managed) //nolint:errcheck
	if managed != 1 {
		t.Errorf("a platform-minted key has managed = %d; want 1", managed)
	}

	// RequireAuth resolves it to that user with that user's flags. GetMe reads only the
	// request context, so this asserts the credential lookup and nothing else.
	whoami := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		req.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.GetMe)(rec, req)
		return rec
	}
	rec = whoami(minted.APIKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("RequireAuth refused a freshly minted key: %d — %s", rec.Code, rec.Body.String())
	}
	var me struct {
		ID      string `json:"id"`
		Role    string `json:"role"`
		IsAdmin bool   `json:"is_admin"`
		IsOwner bool   `json:"is_owner"`
	}
	decodeJSON(t, rec, &me)
	if me.ID != user.ID {
		t.Errorf("the key resolved to %q; want the user it was minted for, %q", me.ID, user.ID)
	}
	if me.Role != "admin" || !me.IsAdmin || me.IsOwner {
		t.Errorf("role=%q is_admin=%v is_owner=%v; want admin/true/false", me.Role, me.IsAdmin, me.IsOwner)
	}

	// ⛔ ROTATION. A second mint of the same name replaces the first: the old key stops
	// working at the instant the new one starts, and exactly one managed key of that name
	// survives.
	rec = memberReq(t, routes["mintKey"], http.MethodPost,
		"/v1/platform/workspaces/mint1/users/"+user.ID+"/api-keys",
		map[string]any{"name": "district"}, platformToken,
		map[string]string{"id": "mint1", "uid": user.ID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-mint: status %d, body %s", rec.Code, rec.Body.String())
	}
	var rotated struct {
		APIKey string `json:"api_key"`
	}
	decodeJSON(t, rec, &rotated)

	if got := whoami(minted.APIKey); got.Code != http.StatusUnauthorized {
		t.Errorf("the rotated-out key still authenticates: %d — %s", got.Code, got.Body.String())
	}
	if got := whoami(rotated.APIKey); got.Code != http.StatusOK {
		t.Errorf("the new key does not authenticate: %d — %s", got.Code, got.Body.String())
	}
	var count int
	platform.QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE workspace_id = ? AND user_id = ? AND name = 'district'`,
		"mint1", user.ID).Scan(&count) //nolint:errcheck
	if count != 1 {
		t.Errorf("%d keys named district after a re-mint; want 1 — a caller that lost a key "+
			"must not accumulate credentials", count)
	}
}

func TestPostgres_PlatformMembers_mintRejectsBadNameAndUnknownUser(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	ownerID := provisionTenancy(t, routes, platform, "mint2")

	long := ""
	for i := 0; i < 65; i++ {
		long += "x"
	}
	for _, name := range []any{"", "   ", long} {
		rec := memberReq(t, routes["mintKey"], http.MethodPost,
			"/v1/platform/workspaces/mint2/users/"+ownerID+"/api-keys",
			map[string]any{"name": name}, platformToken,
			map[string]string{"id": "mint2", "uid": ownerID})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("mint with name %q: status %d; want 400", name, rec.Code)
		}
	}

	rec := memberReq(t, routes["mintKey"], http.MethodPost,
		"/v1/platform/workspaces/mint2/users/nobody/api-keys",
		map[string]any{"name": "district"}, platformToken,
		map[string]string{"id": "mint2", "uid": "nobody"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("mint for an unknown user: status %d; want 404", rec.Code)
	}
}

func TestPostgres_PlatformMembers_deleteKey(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	ownerID := provisionTenancy(t, routes, platform, "del1")

	rec := memberReq(t, routes["mintKey"], http.MethodPost,
		"/v1/platform/workspaces/del1/users/"+ownerID+"/api-keys",
		map[string]any{"name": "district"}, platformToken,
		map[string]string{"id": "del1", "uid": ownerID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: status %d, body %s", rec.Code, rec.Body.String())
	}
	var minted struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &minted)

	rec = memberReq(t, routes["deleteKey"], http.MethodDelete,
		"/v1/platform/workspaces/del1/users/"+ownerID+"/api-keys/"+minted.ID, nil, platformToken,
		map[string]string{"id": "del1", "uid": ownerID, "keyId": minted.ID})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %s", rec.Code, rec.Body.String())
	}
	var count int
	platform.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = ?`, minted.ID).Scan(&count) //nolint:errcheck
	if count != 0 {
		t.Errorf("the key survived the delete")
	}

	// A second delete, and a key that is not that user's, are both 404.
	rec = memberReq(t, routes["deleteKey"], http.MethodDelete,
		"/v1/platform/workspaces/del1/users/"+ownerID+"/api-keys/"+minted.ID, nil, platformToken,
		map[string]string{"id": "del1", "uid": ownerID, "keyId": minted.ID})
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete twice: status %d; want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// PATCH …/webhooks — marking by url
// ---------------------------------------------------------------------------

func TestPostgres_PlatformMembers_markWebhooksManagedByURL(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	ownerID := provisionTenancy(t, routes, platform, "hook1")

	// Simulate a tenancy provisioned before 00063: unmark the provisioning webhook, and
	// add a second row on the same url plus one the workspace made for itself.
	url := "https://hooks.book.hook1.example/calnode"
	if _, err := platform.Exec(`UPDATE webhooks SET managed = 0 WHERE workspace_id = ?`, "hook1"); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	for i, u := range []string{url, "https://mine.hook1.example/in"} {
		if _, err := platform.Exec(`
			INSERT INTO webhooks (id, workspace_id, user_id, url, events, secret_enc)
			VALUES (?, ?, ?, ?, '["booking.created"]', '')`,
			"extra-hook1-"+string(rune('a'+i)), "hook1", ownerID, u); err != nil {
			t.Fatalf("insert extra webhook: %v", err)
		}
	}

	mark := func() int64 {
		rec := memberReq(t, routes["markHooks"], http.MethodPatch, "/v1/platform/workspaces/hook1/webhooks",
			map[string]any{"url": url, "managed": true}, platformToken, map[string]string{"id": "hook1"})
		if rec.Code != http.StatusOK {
			t.Fatalf("mark: status %d, body %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Updated int64 `json:"updated"`
		}
		decodeJSON(t, rec, &body)
		return body.Updated
	}
	if n := mark(); n != 2 {
		t.Errorf("updated = %d; want 2 (both rows carrying that exact url)", n)
	}
	// Idempotent: the same call reports the same count, because it is a SET not a toggle.
	if n := mark(); n != 2 {
		t.Errorf("second mark: updated = %d; want 2", n)
	}

	var otherManaged int
	platform.QueryRow(`SELECT managed FROM webhooks WHERE url = ?`, "https://mine.hook1.example/in").
		Scan(&otherManaged) //nolint:errcheck
	if otherManaged != 0 {
		t.Errorf("a webhook on a different url was marked; the match must be exact")
	}

	// ⛔ managed:false is 400. Nothing un-manages a row.
	rec := memberReq(t, routes["markHooks"], http.MethodPatch, "/v1/platform/workspaces/hook1/webhooks",
		map[string]any{"url": url, "managed": false}, platformToken, map[string]string{"id": "hook1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("managed:false: status %d; want 400", rec.Code)
	}
	// So is an omitted `managed`, and an empty url.
	rec = memberReq(t, routes["markHooks"], http.MethodPatch, "/v1/platform/workspaces/hook1/webhooks",
		map[string]any{"url": url}, platformToken, map[string]string{"id": "hook1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("managed omitted: status %d; want 400", rec.Code)
	}
	rec = memberReq(t, routes["markHooks"], http.MethodPatch, "/v1/platform/workspaces/hook1/webhooks",
		map[string]any{"url": "  ", "managed": true}, platformToken, map[string]string{"id": "hook1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty url: status %d; want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Archive
// ---------------------------------------------------------------------------

func TestPostgres_PlatformMembers_archiveRevokesManagedKeysAndSessions(t *testing.T) {
	routes, h, _, platform := memberAPI(t)
	provisionTenancy(t, routes, platform, "arch1")

	rec := memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/arch1/users",
		map[string]any{"email": "leaver@arch1.example", "name": "Leaver", "role": "member"},
		platformToken, map[string]string{"id": "arch1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: status %d, body %s", rec.Code, rec.Body.String())
	}
	var user struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &user)

	rec = memberReq(t, routes["mintKey"], http.MethodPost,
		"/v1/platform/workspaces/arch1/users/"+user.ID+"/api-keys",
		map[string]any{"name": "district"}, platformToken,
		map[string]string{"id": "arch1", "uid": user.ID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: status %d, body %s", rec.Code, rec.Body.String())
	}
	var minted struct {
		APIKey string `json:"api_key"`
	}
	decodeJSON(t, rec, &minted)

	// A session and an unmanaged key of their own, so the blast radius is measurable.
	if _, err := platform.Exec(`
		INSERT INTO sessions (id, workspace_id, user_id, expires_at)
		VALUES ('sess-arch1', ?, ?, '2099-01-01T00:00:00.000Z')`, "arch1", user.ID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := platform.Exec(`
		INSERT INTO api_keys (id, workspace_id, user_id, name, key_hash, created_at)
		VALUES ('own-arch1', ?, ?, 'mine', 'deadbeef01', '2026-01-01T00:00:00.000Z')`,
		"arch1", user.ID); err != nil {
		t.Fatalf("insert own key: %v", err)
	}

	rec = memberReq(t, routes["archive"], http.MethodPost,
		"/v1/platform/workspaces/arch1/users/"+user.ID+"/archive", nil, platformToken,
		map[string]string{"id": "arch1", "uid": user.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Archived bool `json:"archived"`
	}
	decodeJSON(t, rec, &body)
	if !body.Archived {
		t.Errorf(`archive response = %s; want {"archived":true}`, rec.Body.String())
	}

	var managedKeys, sessions, ownKeys int
	platform.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id = ? AND managed = 1`, user.ID).Scan(&managedKeys) //nolint:errcheck
	platform.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, user.ID).Scan(&sessions)                    //nolint:errcheck
	platform.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id = ? AND managed = 0`, user.ID).Scan(&ownKeys)     //nolint:errcheck
	if managedKeys != 0 {
		t.Errorf("%d managed keys survived the archive; an un-archive would resurrect a live credential", managedKeys)
	}
	if sessions != 0 {
		t.Errorf("%d sessions survived the archive", sessions)
	}
	if ownKeys != 1 {
		t.Errorf("own (unmanaged) key count = %d; want 1 — the platform revokes its own credentials, not the person's", ownKeys)
	}

	// The revoked key no longer authenticates, and would not even if the row had survived
	// (RequireAuth checks archived_at IS NULL).
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("X-API-Key", minted.APIKey)
	whoami := httptest.NewRecorder()
	h.RequireAuth(h.GetMe)(whoami, req)
	if whoami.Code != http.StatusUnauthorized {
		t.Errorf("an archived user's key still authenticates: %d", whoami.Code)
	}

	// Archiving twice is 404: platformUserInWorkspace treats an archived user as absent.
	rec = memberReq(t, routes["archive"], http.MethodPost,
		"/v1/platform/workspaces/arch1/users/"+user.ID+"/archive", nil, platformToken,
		map[string]string{"id": "arch1", "uid": user.ID})
	if rec.Code != http.StatusNotFound {
		t.Errorf("archive twice: status %d; want 404", rec.Code)
	}
}

// ⛔ The fork REFUSES to archive a member who still hosts an upcoming booking (ArchiveUser
// answers 409), and this route mirrors it — with a machine-readable body, because the
// platform has to be able to act on the count.
func TestPostgres_PlatformMembers_archiveRefusesUpcomingBookingsAndTheOwner(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	ownerID := provisionTenancy(t, routes, platform, "arch2")

	rec := memberReq(t, routes["archive"], http.MethodPost,
		"/v1/platform/workspaces/arch2/users/"+ownerID+"/archive", nil, platformToken,
		map[string]string{"id": "arch2", "uid": ownerID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("archive the owner: status %d; want 409 (%s)", rec.Code, rec.Body.String())
	}
	var ownerErr struct {
		Error string `json:"error"`
	}
	decodeJSON(t, rec, &ownerErr)
	if ownerErr.Error != "owner_cannot_be_archived" {
		t.Errorf("error = %q; want owner_cannot_be_archived", ownerErr.Error)
	}

	rec = memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/arch2/users",
		map[string]any{"email": "busy@arch2.example", "name": "Busy", "role": "member"},
		platformToken, map[string]string{"id": "arch2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: status %d, body %s", rec.Code, rec.Body.String())
	}
	var user struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &user)

	var etID string
	if err := platform.QueryRow(
		`SELECT id FROM event_types WHERE workspace_id = ?`, "arch2").Scan(&etID); err != nil {
		t.Fatalf("read event type: %v", err)
	}
	if _, err := platform.Exec(`
		INSERT INTO bookings (id, workspace_id, event_type_id, host_id, start_at, end_at, status)
		VALUES ('bk-arch2', ?, ?, ?, '2099-01-01T09:00:00.000Z', '2099-01-01T09:30:00.000Z', 'confirmed')`,
		"arch2", etID, user.ID); err != nil {
		t.Fatalf("insert booking: %v", err)
	}

	rec = memberReq(t, routes["archive"], http.MethodPost,
		"/v1/platform/workspaces/arch2/users/"+user.ID+"/archive", nil, platformToken,
		map[string]string{"id": "arch2", "uid": user.ID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("archive with an upcoming booking: status %d; want 409 (%s)", rec.Code, rec.Body.String())
	}
	var conflict struct {
		Error string `json:"error"`
		Count int    `json:"count"`
	}
	decodeJSON(t, rec, &conflict)
	if conflict.Error != "upcoming_bookings" || conflict.Count != 1 {
		t.Errorf(`got %s; want {"error":"upcoming_bookings","count":1}`, rec.Body.String())
	}
	var archived *string
	platform.QueryRow(`SELECT archived_at FROM users WHERE id = ?`, user.ID).Scan(&archived) //nolint:errcheck
	if archived != nil {
		t.Errorf("the refusal archived the user anyway")
	}
}

// ---------------------------------------------------------------------------
// Cross-tenant
// ---------------------------------------------------------------------------

// ⛔ Every statement in this file names workspace_id, because the platform handle bypasses
// the policies and there is nothing behind it to catch a forgotten predicate. This is the
// test that would fail if one were dropped: a call naming workspace A cannot see, mint for,
// revoke from or archive a user of workspace B, even holding a valid platform token.
func TestPostgres_PlatformMembers_cannotReachAnotherWorkspacesUser(t *testing.T) {
	routes, _, _, platform := memberAPI(t)
	provisionTenancy(t, routes, platform, "xta")
	ownerB := provisionTenancy(t, routes, platform, "xtb")

	// B's own key, so "the key exists" is true and only the workspace is wrong.
	rec := memberReq(t, routes["mintKey"], http.MethodPost,
		"/v1/platform/workspaces/xtb/users/"+ownerB+"/api-keys",
		map[string]any{"name": "district"}, platformToken,
		map[string]string{"id": "xtb", "uid": ownerB})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint for B: status %d, body %s", rec.Code, rec.Body.String())
	}
	var bKey struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &bKey)

	for _, tc := range []struct {
		name   string
		route  string
		method string
		body   any
		path   map[string]string
	}{
		{"mint for B's user under A", "mintKey", http.MethodPost,
			map[string]any{"name": "district"}, map[string]string{"id": "xta", "uid": ownerB}},
		{"delete B's key under A", "deleteKey", http.MethodDelete,
			nil, map[string]string{"id": "xta", "uid": ownerB, "keyId": bKey.ID}},
		{"archive B's user under A", "archive", http.MethodPost,
			nil, map[string]string{"id": "xta", "uid": ownerB}},
	} {
		rec := memberReq(t, routes[tc.route], tc.method, "/v1/platform/workspaces/xta/x", tc.body,
			platformToken, tc.path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d; want 404 (%s)", tc.name, rec.Code, rec.Body.String())
		}
	}

	// Nothing was changed in B.
	var keys, archived int
	platform.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = ?`, bKey.ID).Scan(&keys)                             //nolint:errcheck
	platform.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ? AND archived_at IS NOT NULL`, ownerB).Scan(&archived) //nolint:errcheck
	if keys != 1 {
		t.Errorf("B's key was deleted by a call naming A")
	}
	if archived != 0 {
		t.Errorf("B's owner was archived by a call naming A")
	}

	// An upsert under A with B's owner's address creates a SECOND user, in A — the same
	// address in two workspaces is two people, which is what (workspace_id, email) means.
	var bEmail string
	platform.QueryRow(`SELECT email FROM users WHERE id = ?`, ownerB).Scan(&bEmail) //nolint:errcheck
	rec = memberReq(t, routes["upsertUser"], http.MethodPost, "/v1/platform/workspaces/xta/users",
		map[string]any{"email": bEmail, "name": "Impostor", "role": "member"},
		platformToken, map[string]string{"id": "xta"})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert B's address into A: status %d, body %s", rec.Code, rec.Body.String())
	}
	var made struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	decodeJSON(t, rec, &made)
	if !made.Created || made.ID == ownerB {
		t.Errorf("an upsert into A resolved B's user (%+v); the email match is not scoped by workspace", made)
	}
	var madeWorkspace string
	platform.QueryRow(`SELECT workspace_id FROM users WHERE id = ?`, made.ID).Scan(&madeWorkspace) //nolint:errcheck
	if madeWorkspace != "xta" {
		t.Errorf("the new user landed in workspace %q; want xta", madeWorkspace)
	}
	var bName string
	platform.QueryRow(`SELECT name FROM users WHERE id = ?`, ownerB).Scan(&bName) //nolint:errcheck
	if bName == "Impostor" {
		t.Errorf("the upsert overwrote B's owner")
	}
}
