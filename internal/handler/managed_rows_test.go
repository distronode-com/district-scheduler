package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// Managed rows through the credential-scoped routes (F1, migration 00063).
//
// These run on whichever engine dbtest selects, because nothing here is about tenancy —
// it is about what a caller holding a workspace's own credential can see and change. The
// tenancy half (a platform call for workspace A cannot reach workspace B) is in
// platform_members_test.go, on Postgres, where RLS makes it meaningful.

// markManagedKey / markManagedWebhook flip the flag the way the platform API does. There
// is deliberately no credential route that can set it.
func markManagedKey(t *testing.T, database *db.DB, keyID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE api_keys SET managed = 1 WHERE id = ?`, keyID); err != nil {
		t.Fatalf("mark key managed: %v", err)
	}
}

func markManagedWebhook(t *testing.T, database *db.DB, id string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE webhooks SET managed = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("mark webhook managed: %v", err)
	}
}

// createKeyViaHTTP mints a key through POST /v1/api-keys and returns its id and plaintext.
func createKeyViaHTTP(t *testing.T, h interface {
	RequireAuth(http.HandlerFunc) http.HandlerFunc
	CreateAPIKey(http.ResponseWriter, *http.Request)
}, callerKey, name string) (id, plain string) {
	t.Helper()
	req := authReq(http.MethodPost, "/v1/api-keys", `{"name":"`+name+`"}`, callerKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateAPIKey)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create api key: got %d — %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create api key: decode: %v", err)
	}
	return resp.ID, resp.Key
}

func TestManagedKey_isHiddenFromListAndRefusedOnDelete(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)

	managedID, _ := createKeyViaHTTP(t, h, ownerKey, "platform-provisioned")
	markManagedKey(t, database, managedID)
	ownID, _ := createKeyViaHTTP(t, h, ownerKey, "mine")

	// GET /v1/api-keys omits the managed row.
	req := authReq(http.MethodGet, "/v1/api-keys", "", ownerKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListAPIKeys)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d — %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list: decode: %v", err)
	}
	for _, item := range list.Items {
		if item.ID == managedID {
			t.Errorf("GET /v1/api-keys listed the managed key %q (%s)", item.ID, item.Name)
		}
	}
	// The caller's own key and the bootstrap key are both still there.
	var sawOwn bool
	for _, item := range list.Items {
		if item.ID == ownID {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Errorf("GET /v1/api-keys did not list the caller's own key %q; the filter is too wide", ownID)
	}

	// DELETE on the managed row is 403 with the one shared message, and changes nothing.
	req = authReq(http.MethodDelete, "/v1/api-keys/"+managedID, "", ownerKey)
	req.SetPathValue("id", managedID)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteAPIKey)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete managed key: got %d — %s; want 403", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &errBody) //nolint:errcheck // asserted on the next line
	if errBody.Error != "managed by your platform" {
		t.Errorf("delete managed key: error = %q; want %q", errBody.Error, "managed by your platform")
	}
	var stillThere int
	database.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = ?`, managedID).Scan(&stillThere) //nolint:errcheck
	if stillThere != 1 {
		t.Errorf("the managed key was deleted anyway")
	}

	// The caller's own key still deletes.
	req = authReq(http.MethodDelete, "/v1/api-keys/"+ownID, "", ownerKey)
	req.SetPathValue("id", ownID)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteAPIKey)(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete own key: got %d — %s; want 204", rec.Code, rec.Body.String())
	}
}

// ⛔ RequireAuth must keep accepting a managed key. It is the credential the platform
// mints for an integration to spend; hiding the row from a settings page and refusing to
// authenticate it are entirely different things, and only the first is intended.
func TestManagedKey_stillAuthenticates(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)

	managedID, managedKey := createKeyViaHTTP(t, h, ownerKey, "platform-provisioned")
	markManagedKey(t, database, managedID)

	req := authReq(http.MethodGet, "/v1/api-keys", "", managedKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListAPIKeys)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a managed key was refused by RequireAuth: got %d — %s", rec.Code, rec.Body.String())
	}

	// And it resolved to the user it hangs off, with that user's flags — which is what
	// the platform's per-member keys will depend on.
	var lastUsed *string
	database.QueryRow(`SELECT last_used_at FROM api_keys WHERE id = ?`, managedID).Scan(&lastUsed) //nolint:errcheck
	if lastUsed == nil {
		t.Errorf("RequireAuth did not stamp last_used_at on the managed key it accepted")
	}
	var isOwner int
	database.QueryRow(`SELECT is_owner FROM users WHERE id = ?`, ownerID).Scan(&isOwner) //nolint:errcheck
	if isOwner != 1 {
		t.Fatalf("fixture: the bootstrap user is not the owner")
	}
}

func TestManagedWebhook_hiddenFromListAndRefusedOnPatchAndDelete(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)

	// Two subscriptions, inserted directly: POST /v1/webhooks resolves the URL to refuse a
	// private or loopback address, so a fixture host has to exist in DNS to go through it.
	// What is under test here is the managed flag, not the SSRF guard.
	const managedID, ownID = "wh-managed", "wh-own"
	for _, w := range []struct{ id, url string }{
		{managedID, "https://platform.example/in"},
		{ownID, "https://mine.example/in"},
	} {
		if _, err := database.Exec(`
			INSERT INTO webhooks (id, user_id, url, events, secret_enc)
			VALUES (?, ?, ?, '["booking.created"]', '')`, w.id, ownerID, w.url); err != nil {
			t.Fatalf("insert webhook %s: %v", w.id, err)
		}
	}
	markManagedWebhook(t, database, managedID)

	req := authReq(http.MethodGet, "/v1/webhooks", "", ownerKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListWebhooks)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list webhooks: got %d — %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list) //nolint:errcheck
	if len(list.Items) != 1 || list.Items[0].ID != ownID {
		t.Errorf("GET /v1/webhooks returned %+v; want exactly the caller's own %q", list.Items, ownID)
	}

	for _, tc := range []struct {
		name   string
		method string
		body   string
		route  func(http.ResponseWriter, *http.Request)
	}{
		{"patch", http.MethodPatch, `{"events":["booking.cancelled"]}`, h.PatchWebhook},
		{"delete", http.MethodDelete, "", h.DeleteWebhook},
	} {
		req := authReq(tc.method, "/v1/webhooks/"+managedID, tc.body, ownerKey)
		req.SetPathValue("id", managedID)
		rec := httptest.NewRecorder()
		h.RequireAuth(tc.route)(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s managed webhook: got %d — %s; want 403", tc.name, rec.Code, rec.Body.String())
			continue
		}
		var errBody struct {
			Error string `json:"error"`
		}
		json.Unmarshal(rec.Body.Bytes(), &errBody) //nolint:errcheck
		if errBody.Error != "managed by your platform" {
			t.Errorf("%s managed webhook: error = %q; want %q", tc.name, errBody.Error, "managed by your platform")
		}
	}

	var stillThere int
	database.QueryRow(`SELECT COUNT(*) FROM webhooks WHERE id = ?`, managedID).Scan(&stillThere) //nolint:errcheck
	if stillThere != 1 {
		t.Errorf("the managed webhook was deleted anyway")
	}

	// The caller's own webhook still patches and deletes.
	req = authReq(http.MethodPatch, "/v1/webhooks/"+ownID, `{"events":["booking.cancelled"]}`, ownerKey)
	req.SetPathValue("id", ownID)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.PatchWebhook)(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("patch own webhook: got %d — %s; want 204", rec.Code, rec.Body.String())
	}
	req = authReq(http.MethodDelete, "/v1/webhooks/"+ownID, "", ownerKey)
	req.SetPathValue("id", ownID)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteWebhook)(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("delete own webhook: got %d — %s; want 204", rec.Code, rec.Body.String())
	}
}
