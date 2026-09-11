package handler_test

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// The seeded event type's LOCATION (D12), which is what decides whether the tenant's
// first save from the admin editor succeeds.
//
// ⛔ The failure this pins is invisible at provisioning time and lands on the tenant.
// Seeding wrote location_type 'link' with a NULL location_value — the schema's own column
// default, and a state validateLocation rejects. The editor submits the whole form on
// every save, so the first time the owner changed anything at all, the save came back 400
// "enter a valid meeting URL (https://…)" about a field they had never touched, with no
// way out of it from the UI. Provisioning reported 201 throughout, and the row looked
// ordinary in every read.
//
// ⛔ Postgres-only, for the reason platform_seed_host_test.go states at length:
// multi-tenant IS row-level security, db.OpenPair refuses a non-postgres DSN, and SQLite's
// migration 00060 keeps the single-tenant uniques, so a second workspace cannot be
// provisioned there at all.

// newPlatformSeedAPI returns the create route and the event-type PATCH route as
// server.go registers it (RequireAuth, then Scoped on the credential), plus the pair the
// assertions read through.
//
// The PATCH route is the point: asserting the stored row alone would pin the column
// values without proving the thing that was broken, which is that the editor can save.
func newPlatformSeedAPI(t *testing.T) (create, patch http.HandlerFunc, app, platform *db.DB) {
	t.Helper()
	app, platform = dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)

	return h.Platform((*handler.Handler).CreateWorkspace),
		h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*handler.Handler).PatchEventType)),
		app, platform
}

// seededLocation reads the provisioned event type's location through the platform
// handle, which is the only one that can see across the tenant boundary.
func seededLocation(t *testing.T, platform *db.DB, workspaceID, slug string) (locType string, locValue sql.NullString) {
	t.Helper()
	if err := platform.QueryRow(
		`SELECT location_type, location_value FROM event_types WHERE workspace_id = ? AND slug = ?`,
		workspaceID, slug).Scan(&locType, &locValue); err != nil {
		t.Fatalf("read the seeded event type's location: %v", err)
	}
	return locType, locValue
}

// TestPlatform_seededEventTypeIsSaveableFromTheEditor is the end-to-end proof, and the
// two halves are not interchangeable.
//
// The column assertion says the seed is valid by construction: in_person with no value is
// the one location validateLocation accepts with nothing connected, which is what makes it
// safe to write past the validator (CLAUDE.md: "anything written without validation must
// be valid by construction"). The PATCH says the tenant can actually work — it changes a
// field nowhere near the location, which is exactly the save that used to fail.
func TestPlatform_seededEventTypeIsSaveableFromTheEditor(t *testing.T) {
	create, patch, _, platform := newPlatformSeedAPI(t)

	rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("acme", "book.acme.example"), platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d; want 201 — %s", rec.Code, rec.Body.String())
	}
	var provisioned struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &provisioned); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	locType, locValue := seededLocation(t, platform, "acme", "intro")
	if locType != "in_person" {
		t.Errorf("seeded location_type = %q; want in_person — the default has to be a type "+
			"validateLocation accepts with no value and no connected provider, because this "+
			"INSERT goes straight to the table and nothing validates it", locType)
	}
	if locValue.Valid {
		t.Errorf("seeded location_value = %q; want NULL — every read of this column asks "+
			"COALESCE(location_value, ''), and an empty string is a second spelling of the "+
			"same absence", locValue.String)
	}

	// The tenant's first save, editing a field that has nothing to do with the location.
	//
	// ⚠️ The location is ECHOED BACK from what is stored rather than omitted, because that
	// is what the editor does: it loads the form, the operator changes the duration, and
	// the whole form is submitted — location included, unchanged. A body naming only
	// duration_minutes would be a weaker request than the product ever sends, and would
	// answer 200 against the bug as well as against the fix. location_value is null
	// because the editor sends `.trim() || null`.
	req := authReq(http.MethodPatch, "/v1/event-types/intro",
		`{"duration_minutes": 45, "location_type": "`+locType+`", "location_value": null}`,
		provisioned.APIKey)
	req.SetPathValue("slug", "intro")
	patchRec := httptest.NewRecorder()
	patch(patchRec, req)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("the tenant's first save of the seeded event type: status = %d; want 200 — %s\n"+
			"a provisioned tenancy whose event type cannot be saved is locked out of every "+
			"field by a location the owner never chose", patchRec.Code, patchRec.Body.String())
	}
}

// A caller that knows how the tenant meets says so, and the seed stores it verbatim —
// but only after the same validateLocation the editor answers to has accepted it, so
// there is still no path that writes a row the first save rejects.
func TestPlatform_seedsTheRequestedLocation(t *testing.T) {
	create, _, _, platform := newPlatformSeedAPI(t)

	body := platformCreateBody("acme", "book.acme.example")
	et := body["defaults"].(map[string]any)["event_type"].(map[string]any)
	et["location_type"] = "phone"
	et["location_value"] = "+14165550123"

	rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces", body, platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d; want 201 — %s", rec.Code, rec.Body.String())
	}

	locType, locValue := seededLocation(t, platform, "acme", "intro")
	if locType != "phone" || locValue.String != "+14165550123" {
		t.Errorf("seeded location = %q/%q; want phone/+14165550123", locType, locValue.String)
	}
}

// A location the seed refuses is the CALLER's mistake, so it gets 400 and the validator's
// own sentence — the same answer the editor gives for the same value — rather than 500
// "internal error", which names nothing and invites a retry that cannot succeed.
//
// ⛔ And nothing is left behind. The seed runs inside the provisioning transaction, after
// the workspace, the owner and the owner's key are already inserted, so a refusal that did
// not roll back would leave a tenancy with no event type answering requests — the
// half-provisioned state CreateWorkspace's one transaction exists to make impossible. The
// absent workspace row is the assertion that proves it, and it is the half a status-code
// check alone would miss.
func TestPlatform_refusesASeedLocationTheEditorWouldReject(t *testing.T) {
	cases := []struct {
		name     string
		locType  string
		locValue string
		wantMsg  string
	}{
		{
			name:     "phone with something that is not a number",
			locType:  "phone",
			locValue: "call the front desk",
			wantMsg:  "enter a valid phone number",
		},
		{
			// validateLocation's default branch accepts any unknown type (in_person and
			// friends impose no value requirement), so without an explicit membership
			// test this would reach the INSERT and come back as a CHECK violation.
			name:    "a type the schema does not admit",
			locType: "bogus",
			wantMsg: `location_type "bogus" is not a supported location`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			create, _, _, platform := newPlatformSeedAPI(t)

			body := platformCreateBody("acme", "book.acme.example")
			et := body["defaults"].(map[string]any)["event_type"].(map[string]any)
			et["location_type"] = tc.locType
			if tc.locValue != "" {
				et["location_value"] = tc.locValue
			}

			rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces", body, platformToken)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create: status = %d; want 400 — %s", rec.Code, rec.Body.String())
			}
			var out struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode error body %s: %v", rec.Body.String(), err)
			}
			if out.Error != tc.wantMsg {
				t.Errorf("error = %q; want %q — the caller needs the sentence that names what "+
					"was wrong, not a constraint violation or a generic internal error",
					out.Error, tc.wantMsg)
			}

			var workspaces int
			if err := platform.QueryRow(
				`SELECT COUNT(*) FROM workspaces WHERE id = 'acme'`).Scan(&workspaces); err != nil {
				t.Fatalf("count workspaces: %v", err)
			}
			if workspaces != 0 {
				t.Errorf("the refused provisioning left %d workspace rows behind; want 0 — a "+
					"tenancy is provisioned whole or not at all, and one with no event type "+
					"would answer requests with a booking page that does not exist", workspaces)
			}
		})
	}
}
