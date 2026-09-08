package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/handler"
)

// H1: the instance-credential settings routes, refused to a TENANT credential.
//
// `server_settings` is per workspace, so most of these routes are self-scoped — but what
// each one CONFIGURES is the process's, not the workspace's: the SMTP account every
// tenancy sends through, the Google OAuth client every tenancy's calendar connect and
// login uses, the Zoom app, the LiveKit server, the Stripe account. On a multi-tenant
// instance the platform provisions all five, so a tenant credential gets 403
// `managed_by_platform` and a single-tenant instance is untouched — there the operator
// IS the instance and these pages are the only way to configure it.
//
// The wrapper, not the handler, is what these drive: h.PlatformManaged sits outside
// RequireAuth at registration (see server.go), and routes_platform_managed_test.go in
// internal/server is what pins WHICH paths carry it.

// platformManagedRoute is one guarded registration, wired the way server.go wires it.
type platformManagedRoute struct {
	name   string
	method string
	path   string
	body   string
	// route returns the handler chain for h, so each case builds it against the
	// handler under test rather than against a shared one.
	route func(h *handler.Handler) http.HandlerFunc
}

func platformManagedRoutes() []platformManagedRoute {
	return []platformManagedRoute{
		{"GET /v1/settings/email", http.MethodGet, "/v1/settings/email", "", func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.GetEmailSettings))
		}},
		{"PATCH /v1/settings/email", http.MethodPatch, "/v1/settings/email", `{"smtp_host":"smtp.evil.test"}`, func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.PatchEmailSettings))
		}},
		{"POST /v1/settings/email/test", http.MethodPost, "/v1/settings/email/test", `{}`, func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.TestEmailConnection))
		}},
		{"GET /v1/settings/google", http.MethodGet, "/v1/settings/google", "", func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.GetGoogleSettings))
		}},
		{"PATCH /v1/settings/google", http.MethodPatch, "/v1/settings/google", `{"client_id":"attacker.apps.googleusercontent.com"}`, func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.PatchGoogleSettings))
		}},
		{"GET /v1/settings/zoom", http.MethodGet, "/v1/settings/zoom", "", func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.GetZoomSettings))
		}},
		{"PATCH /v1/settings/zoom", http.MethodPatch, "/v1/settings/zoom", `{"client_id":"x","client_secret":"y"}`, func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.PatchZoomSettings))
		}},
		{"GET /v1/settings/livekit", http.MethodGet, "/v1/settings/livekit", "", func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.GetLiveKitSettings))
		}},
		{"PATCH /v1/settings/livekit", http.MethodPatch, "/v1/settings/livekit", `{"url":"wss://evil.test","api_key":"k","api_secret":"s"}`, func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.PatchLiveKitSettings))
		}},
		{"GET /v1/settings/stripe", http.MethodGet, "/v1/settings/stripe", "", func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.GetStripeSettings))
		}},
		{"PATCH /v1/settings/stripe", http.MethodPatch, "/v1/settings/stripe", `{"secret_key":"sk_test_x"}`, func(h *handler.Handler) http.HandlerFunc {
			return h.PlatformManaged(h.RequireAuth(h.PatchStripeSettings))
		}},
		// ⛔ Blanket rather than by field, and the FALLBACK is why. TestLLMSettings dials
		// whatever `endpoint` the body names, and an empty `api_key` makes it read the
		// STORED key and dial with it — so a tenant could have the instance issue a
		// request to a host of their choosing while holding the platform's credential.
		// Once the credential fields on the PATCH are managed there is nothing here a
		// tenant can legitimately test.
		{"POST /v1/settings/llm/test", http.MethodPost, "/v1/settings/llm/test",
			`{"endpoint":"https://llm.attacker.test/v1","model":"m"}`, func(h *handler.Handler) http.HandlerFunc {
				return h.PlatformManaged(h.RequireAuth(h.TestLLMSettings))
			}},
	}
}

func errorField(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

func TestPlatformManaged_multiTenantRefusesATenantCredential(t *testing.T) {
	for _, tc := range platformManagedRoutes() {
		t.Run(tc.name, func(t *testing.T) {
			h, key, _ := setupWorkspace(t)
			h.SetMultiTenant(true)

			rec := httptest.NewRecorder()
			tc.route(h)(rec, authReq(tc.method, tc.path, tc.body, key))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want 403 — %s", rec.Code, rec.Body.String())
			}
			if got := errorField(t, rec); got != "managed_by_platform" {
				t.Errorf("error = %q; want managed_by_platform", got)
			}
		})
	}
}

// ⛔ The other half, and the one that decides whether this change is shippable at all:
// a self-hosted single-tenant instance must behave exactly as it did. Asserting "not
// 403" rather than a status per route keeps the case about the guard: several of these
// answer 501/503 or 400 on an unconfigured instance, and pinning those here would make
// this test fail for reasons that have nothing to do with H1.
func TestPlatformManaged_singleTenantIsUntouched(t *testing.T) {
	for _, tc := range platformManagedRoutes() {
		t.Run(tc.name, func(t *testing.T) {
			h, key, _ := setupWorkspace(t)

			rec := httptest.NewRecorder()
			tc.route(h)(rec, authReq(tc.method, tc.path, tc.body, key))

			if rec.Code == http.StatusForbidden {
				t.Fatalf("status = 403 on a single-tenant instance — %s", rec.Body.String())
			}
		})
	}
}

// The tenant-safe settings the platform's own catalog calls. They are NOT wrapped, and
// this is the assertion that says so: a future "tidy-up" that wrapped the whole
// /v1/settings tree would take the dashboard's branding, storage and notetaker pages
// down with it, in multi-tenant mode only, which is the mode nobody runs locally.
func TestPlatformManaged_tenantSafeSettingsStillWorkInMultiTenantMode(t *testing.T) {
	cases := []struct {
		name  string
		route func(h *handler.Handler) http.HandlerFunc
		path  string
	}{
		{"branding", func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.GetBranding) }, "/v1/settings/branding"},
		{"storage", func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.GetStorageSettings) }, "/v1/settings/storage"},
		{"notetaker", func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.GetNotetakerSettings) }, "/v1/settings/notetaker"},
		{"tracking", func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.GetTrackingSettings) }, "/v1/settings/tracking"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, key, _ := setupWorkspace(t)
			h.SetMultiTenant(true)

			rec := httptest.NewRecorder()
			tc.route(h)(rec, authReq(http.MethodGet, tc.path, "", key))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PATCH /v1/settings/llm — guarded by FIELD, not by route
// ---------------------------------------------------------------------------

func llmPatchRoute(h *handler.Handler) http.HandlerFunc {
	return h.PlatformManagedFields("endpoint", "model", "api_key")(h.RequireAuth(h.PatchLLMSettings))
}

func TestPlatformManagedFields_llmCredentialFieldsAreRefusedInMultiTenantMode(t *testing.T) {
	// ⚠️ `{"api_key":""}` is here on purpose: an empty string is how this handler spells
	// "keep the stored one", so a guard that only looked at non-empty values would let
	// the least visible of the three writes through — and the tenant that can send the
	// field at all is a tenant that can CLEAR the instance's key.
	for _, body := range []string{
		`{"endpoint":"https://llm.attacker.test/v1"}`,
		`{"model":"someone-elses-model"}`,
		`{"api_key":"sk-attacker"}`,
		`{"api_key":""}`,
		`{"enabled":true,"endpoint":"https://llm.attacker.test/v1"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, key, _ := setupWorkspace(t)
			h.SetMultiTenant(true)

			rec := httptest.NewRecorder()
			llmPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/llm", body, key))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want 403 — %s", rec.Code, rec.Body.String())
			}
			if got := errorField(t, rec); got != "managed_by_platform" {
				t.Errorf("error = %q; want managed_by_platform", got)
			}
		})
	}
}

// The two fields the platform's catalog actually offers keep working. Without this the
// guard would have taken the AI summariser's on/off switch and its instructions box away
// from every tenancy, which is the whole surface the dashboard exposes.
func TestPlatformManagedFields_llmTenantFieldsStillPatchInMultiTenantMode(t *testing.T) {
	for _, body := range []string{
		`{"enabled":true}`,
		`{"extra_instructions":"Always offer the 30 minute slot first."}`,
		`{"enabled":false,"extra_instructions":"Be brief."}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, key, _ := setupWorkspace(t)
			h.SetMultiTenant(true)

			rec := httptest.NewRecorder()
			llmPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/llm", body, key))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Single-tenant: every field, including the three credential ones, is the operator's own.
func TestPlatformManagedFields_singleTenantAcceptsTheCredentialFields(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetEncKey(testGCalKeyHex)

	rec := httptest.NewRecorder()
	llmPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/llm",
		`{"endpoint":"https://api.example.test/v1","model":"m","api_key":"sk-operator"}`, key))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["endpoint"] != "https://api.example.test/v1" {
		t.Errorf("endpoint = %v; want the value just written", body["endpoint"])
	}
}

// The wrapper reads the body to decide, so it has to hand an equivalent one back — a
// consumed body is an empty body, and the handler two frames down would have decoded
// nothing and written nothing while answering 200.
func TestPlatformManagedFields_bodyStillReachesTheHandler(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetMultiTenant(true)

	rec := httptest.NewRecorder()
	llmPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/llm",
		`{"extra_instructions":"Mention parking."}`, key))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d — %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetLLMSettings)(rec, authReq(http.MethodGet, "/v1/settings/llm", "", key))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["extra_instructions"] != "Mention parking." {
		t.Errorf("extra_instructions = %v; the guard swallowed the body", body["extra_instructions"])
	}
}

// ---------------------------------------------------------------------------
// GET /v1/settings/llm — the endpoint, model and key-set flag are the platform's
// ---------------------------------------------------------------------------

func TestGetLLMSettings_multiTenantOmitsTheProviderFields(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetEncKey(testGCalKeyHex)

	// Configure it the way the platform provisions a tenancy, then read it back as a
	// tenant would.
	rec := httptest.NewRecorder()
	llmPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/llm",
		`{"endpoint":"https://api.example.test/v1","model":"m","api_key":"sk-platform"}`, key))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d — %s", rec.Code, rec.Body.String())
	}

	h.SetMultiTenant(true)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetLLMSettings)(rec, authReq(http.MethodGet, "/v1/settings/llm", "", key))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d — %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"endpoint", "model", "api_key_set"} {
		if _, present := body[k]; present {
			t.Errorf("%q is present (%v); it names the model provider the platform pays for", k, body[k])
		}
	}
	// What a tenant admin legitimately needs — whether the summariser is on and will
	// run — is still answered.
	if _, present := body["enabled"]; !present {
		t.Error("enabled is absent; a tenant may still turn the summariser on and off")
	}
	if configured, _ := body["configured"].(bool); !configured {
		t.Error("configured = false; the boolean must still say the summariser can run")
	}
	if _, present := body["extra_instructions"]; !present {
		t.Error("extra_instructions is absent; it is the tenant's own copy")
	}
}

func TestGetLLMSettings_singleTenantStillReturnsTheProviderFields(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetEncKey(testGCalKeyHex)

	rec := httptest.NewRecorder()
	llmPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/llm",
		`{"endpoint":"https://api.example.test/v1","model":"m","api_key":"sk-operator"}`, key))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d — %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetLLMSettings)(rec, authReq(http.MethodGet, "/v1/settings/llm", "", key))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["endpoint"] != "https://api.example.test/v1" {
		t.Errorf("endpoint = %v; a self-hoster configures their own", body["endpoint"])
	}
	if body["model"] != "m" {
		t.Errorf("model = %v; want m", body["model"])
	}
	if set, _ := body["api_key_set"].(bool); !set {
		t.Error("api_key_set = false; want true")
	}
}

// ---------------------------------------------------------------------------
// PATCH /v1/settings/notetaker — guarded by FIELD, like the LLM PATCH
// ---------------------------------------------------------------------------
//
// ⛔ `stt_api_key` is written to the INSTANCE's server_settings row, so a tenant-supplied
// speech-to-text credential is what every OTHER tenancy on this deployment would then
// transcribe through — one workspace paying for, and able to read the usage of, everyone
// else's call audio. The `enabled` toggle beside it is genuinely the workspace's, which
// is why the guard is by field: refusing the route would take the notetaker switch off
// the platform's own console.

func notetakerPatchRoute(h *handler.Handler) http.HandlerFunc {
	return h.PlatformManagedFields("stt_api_key")(h.RequireAuth(h.PatchNotetakerSettings))
}

func TestPlatformManagedFields_notetakerKeyIsRefusedInMultiTenantMode(t *testing.T) {
	// The empty case is here for the same reason it is on the LLM PATCH: this handler
	// treats `""` as "keep the stored one", so a guard that looked at values rather than
	// at presence would let the field through unnoticed.
	for _, body := range []string{
		`{"stt_api_key":"sk-attacker"}`,
		`{"stt_api_key":""}`,
		`{"enabled":true,"stt_api_key":"sk-attacker"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, key, _ := setupWorkspace(t)
			h.SetMultiTenant(true)

			rec := httptest.NewRecorder()
			notetakerPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/notetaker", body, key))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want 403 — %s", rec.Code, rec.Body.String())
			}
			if got := errorField(t, rec); got != "managed_by_platform" {
				t.Errorf("error = %q; want managed_by_platform", got)
			}
		})
	}
}

// The toggle is the whole surface the platform's console offers on this page, so it has
// to keep working — a guard that took it away would turn the notetaker off for every
// tenancy that had not already enabled it.
func TestPlatformManagedFields_notetakerToggleStillPatchesInMultiTenantMode(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetMultiTenant(true)

	for _, body := range []string{`{"enabled":true}`, `{"enabled":false}`} {
		rec := httptest.NewRecorder()
		notetakerPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/notetaker", body, key))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; want 200 — %s", body, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	notetakerPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/notetaker", `{"enabled":true}`, key))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Errorf("enabled = %v; the toggle must round-trip", body["enabled"])
	}
}

func TestPlatformManagedFields_notetakerSingleTenantAcceptsTheKey(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetEncKey(testGCalKeyHex)

	rec := httptest.NewRecorder()
	notetakerPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/notetaker",
		`{"enabled":true,"stt_api_key":"sk-operator"}`, key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if set, _ := body["stt_api_key_set"].(bool); !set {
		t.Error("stt_api_key_set = false; a self-hoster's own key must still store")
	}
}

// ---------------------------------------------------------------------------
// GET /v1/settings/notetaker — the provider fields are the instance's
// ---------------------------------------------------------------------------

func TestGetNotetakerSettings_multiTenantOmitsTheProviderFields(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetEncKey(testGCalKeyHex)
	h.SetSTTBaseURL("https://stt.the-operators-vendor.test")

	// Configured the way the platform provisions a tenancy, then read back as a tenant.
	rec := httptest.NewRecorder()
	notetakerPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/notetaker",
		`{"enabled":true,"stt_api_key":"sk-platform"}`, key))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d — %s", rec.Code, rec.Body.String())
	}

	h.SetMultiTenant(true)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetNotetakerSettings)(rec, authReq(http.MethodGet, "/v1/settings/notetaker", "", key))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d — %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// ⚠️ stt_base_url is omitted as well as the key flag, and it is the less obvious of
	// the two: h.sttBaseURL() answers the WORKSPACE's value when it has one and the
	// process value otherwise, and the response cannot say which — so a workspace that
	// never set one would read the instance's vendor host back as if it were its own.
	for _, k := range []string{"stt_api_key_set", "stt_base_url"} {
		if _, present := body[k]; present {
			t.Errorf("%q is present (%v); it names the instance's speech-to-text provider", k, body[k])
		}
	}
	if raw := rec.Body.String(); strings.Contains(raw, "the-operators-vendor.test") {
		t.Errorf("the response names the instance's STT host: %s", raw)
	}
	if _, present := body["enabled"]; !present {
		t.Error("enabled is absent; a tenant may still turn the notetaker on and off")
	}
}

func TestGetNotetakerSettings_singleTenantStillReturnsTheProviderFields(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	h.SetEncKey(testGCalKeyHex)
	h.SetSTTBaseURL("https://stt.example.test")

	rec := httptest.NewRecorder()
	notetakerPatchRoute(h)(rec, authReq(http.MethodPatch, "/v1/settings/notetaker",
		`{"enabled":true,"stt_api_key":"sk-operator"}`, key))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d — %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetNotetakerSettings)(rec, authReq(http.MethodGet, "/v1/settings/notetaker", "", key))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if set, _ := body["stt_api_key_set"].(bool); !set {
		t.Error("stt_api_key_set = false; want true")
	}
	if body["stt_base_url"] != "https://stt.example.test" {
		t.Errorf("stt_base_url = %v; a self-hoster reads their own endpoint back", body["stt_base_url"])
	}
}
