package handler_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/caldav"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// M1, the save-time half: `POST /v1/calendar/caldav/connect` accepts only https in
// multi-tenant mode.
//
// ⛔ CalDAV authenticates with HTTP Basic, so the person's app-specific password is on
// the wire in every single request. Over `http://` that is a credential disclosure — and
// it stays allowed on a single-tenant instance anyway, because there it is the operator's
// own password going to their own server on their own network, which is a call they are
// entitled to make and have always been able to. On a multi-tenant instance the password
// is a TENANT's and the hop leaves the operator's network.
//
// The dial-time guard (internal/caldav) is a different question and does not cover this:
// it decides which ADDRESSES may be reached, not whether the credential travels in clear
// to a permitted one.

// newCalDAVHandler builds a handler with the CalDAV provider registered, which
// ConnectCalDAV requires before it looks at anything in the body.
func newCalDAVHandler(t *testing.T, multiTenant bool) (*handler.Handler, string) {
	t.Helper()
	database := dbtest.Open(t)
	t.Cleanup(func() { database.Close() })

	h := handler.New(database, slog.New(slog.DiscardHandler))
	cdav, err := caldav.New(database, testGCalKeyHex, caldav.WithStrictSSRFGuard(multiTenant))
	if err != nil {
		t.Fatalf("caldav.New: %v", err)
	}
	svc := calendar.NewService(database)
	svc.Register(cdav)
	h.SetCalendar(svc)

	body := `{"name":"CalDAV User","email":"caldav@example.com","timezone":"UTC"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Setup(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d — %s", rec.Code, rec.Body.String())
	}
	var setup struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &setup); err != nil {
		t.Fatalf("setup decode: %v", err)
	}

	// After Setup, so the flag cannot change how the bootstrap wrote its rows.
	h.SetMultiTenant(multiTenant)
	return h, setup.APIKey
}

func connectCalDAV(t *testing.T, h *handler.Handler, serverURL, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"server_url":"` + serverURL + `","username":"person@example.test","app_password":"app-specific"}`
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ConnectCalDAV)(rec, authReq(http.MethodPost, "/v1/calendar/caldav/connect", body, apiKey))
	return rec
}

func TestConnectCalDAV_multiTenantRefusesPlainHTTP(t *testing.T) {
	h, key := newCalDAVHandler(t, true)

	rec := connectCalDAV(t, h, "http://caldav.customer.example/dav/", key)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 — %s", rec.Code, rec.Body.String())
	}

	// The existing one-sentence refusal shape: validateBYOServerURL names the field and
	// the schemes it will take, and says nothing else. No address, no reason beyond the
	// scheme, and the same wording every BYO-server field uses.
	got := errorField(t, rec)
	if !strings.Contains(got, "server URL") || !strings.Contains(got, "https") {
		t.Errorf("error = %q; want the field and the accepted scheme named", got)
	}
	if strings.Contains(got, "http,") || strings.Contains(got, ", http") {
		t.Errorf("error = %q; it still offers http as an option in this mode", got)
	}
}

// ⛔ The refusal has to land BEFORE the connection is attempted, or the guard is only a
// nicer error message: the password would already have been sent. A 400 whose body is
// the scheme complaint (rather than a connect failure) is what proves the order.
func TestConnectCalDAV_multiTenantRefusesBeforeDialing(t *testing.T) {
	h, key := newCalDAVHandler(t, true)

	// An address the dial-time guard would also refuse. If the scheme check ran second,
	// the body would be the connect-failure sentence instead.
	rec := connectCalDAV(t, h, "http://10.43.0.1/dav/", key)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 — %s", rec.Code, rec.Body.String())
	}
	if got := errorField(t, rec); !strings.Contains(got, "server URL") {
		t.Errorf("error = %q; want the scheme refusal, which means it ran before the dial", got)
	}
}

// Single-tenant is unchanged: a self-hoster's own Radicale on plain http is the intended
// configuration, and it gets past validation to the connect attempt, which then fails for
// the ordinary reason (nothing is listening) rather than for the scheme.
func TestConnectCalDAV_singleTenantStillAcceptsPlainHTTP(t *testing.T) {
	h, key := newCalDAVHandler(t, false)

	rec := connectCalDAV(t, h, "http://127.0.0.1:1/dav/", key)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 from the connect attempt — %s", rec.Code, rec.Body.String())
	}
	got := errorField(t, rec)
	if strings.Contains(got, "must be a valid URL using one of") {
		t.Errorf("error = %q; a self-hoster's http:// URL was refused on the scheme", got)
	}
	// It got as far as issuing a PROPFIND at the URL, which is what "validation passed"
	// means here. The connect failure itself is the ordinary one for a port with nothing
	// on it.
	if !strings.Contains(got, "127.0.0.1:1") {
		t.Errorf("error = %q; want a connect-time failure at the URL, which means validation passed", got)
	}
}

// https is accepted in both modes, or "everything is refused" would satisfy the cases
// above. It reaches the connect attempt and fails there, at a port with nothing on it.
func TestConnectCalDAV_httpsIsAcceptedInBothModes(t *testing.T) {
	for _, multiTenant := range []bool{false, true} {
		name := "single-tenant"
		if multiTenant {
			name = "multi-tenant"
		}
		t.Run(name, func(t *testing.T) {
			h, key := newCalDAVHandler(t, multiTenant)

			rec := connectCalDAV(t, h, "https://caldav.customer.example/dav/", key)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 from the connect attempt — %s", rec.Code, rec.Body.String())
			}
			if got := errorField(t, rec); strings.Contains(got, "must be a valid URL using one of") {
				t.Errorf("error = %q; https must pass validation in this mode", got)
			}
		})
	}
}
