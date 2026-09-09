package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
)

const (
	appDSN   = "postgres://calnode_app:pw@127.0.0.1:5432/calnode?sslmode=disable"
	adminDSN = "postgres://calnode_platform:pw@127.0.0.1:5432/calnode?sslmode=disable"
)

func TestLoad_multiTenantDefaultsOff(t *testing.T) {
	os.Unsetenv("MULTI_TENANT")
	os.Unsetenv("DATABASE_ADMIN_URL")
	os.Unsetenv("CALNODE_PLATFORM_TOKEN")

	cfg := config.Load()

	if cfg.MultiTenant {
		t.Error("MultiTenant should default to false")
	}
	if cfg.DatabaseAdminURL != "" {
		t.Errorf("DatabaseAdminURL = %q; want empty", cfg.DatabaseAdminURL)
	}
	if cfg.PlatformToken != "" {
		t.Errorf("PlatformToken = %q; want empty", cfg.PlatformToken)
	}
	// The default configuration is the one every existing deployment has, so it
	// has to validate.
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() on the default configuration: %v", err)
	}
}

func TestLoad_multiTenantEnv(t *testing.T) {
	t.Setenv("MULTI_TENANT", "1")
	t.Setenv("DATABASE_URL", appDSN)
	t.Setenv("DATABASE_ADMIN_URL", adminDSN)
	t.Setenv("CALNODE_PLATFORM_TOKEN", "tok")

	cfg := config.Load()

	if !cfg.MultiTenant {
		t.Error("MultiTenant = false; want true")
	}
	if cfg.DatabaseAdminURL != adminDSN {
		t.Errorf("DatabaseAdminURL = %q; want %q", cfg.DatabaseAdminURL, adminDSN)
	}
	if cfg.PlatformToken != "tok" {
		t.Errorf("PlatformToken = %q; want tok", cfg.PlatformToken)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() on a well-formed multi-tenant configuration: %v", err)
	}
}

// TestValidate_multiTenantRefusals covers every combination that cannot work.
// Each one is a refusal rather than a warning because its only other outcome is
// silent: a tenant reading another tenant's rows, or a demo reset wiping a fleet.
func TestValidate_multiTenantRefusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantSub string
	}{
		{
			name:    "sqlite has no row-level security",
			mutate:  func(c *config.Config) { c.DatabaseURL = "sqlite://./data/calnode.db" },
			wantSub: "postgres:// DATABASE_URL",
		},
		{
			name:    "no platform DSN",
			mutate:  func(c *config.Config) { c.DatabaseAdminURL = "" },
			wantSub: "DATABASE_ADMIN_URL",
		},
		{
			name:    "platform DSN is not postgres",
			mutate:  func(c *config.Config) { c.DatabaseAdminURL = "sqlite://./data/calnode.db" },
			wantSub: "must be a postgres:// DSN",
		},
		{
			// The dangerous one: everything works, including reading other
			// tenants' rows, because a role that owns a table is not
			// constrained by that table's policy.
			name:    "one role for both handles",
			mutate:  func(c *config.Config) { c.DatabaseAdminURL = c.DatabaseURL },
			wantSub: "must differ from DATABASE_URL",
		},
		{
			name:    "demo mode wipes every tenant",
			mutate:  func(c *config.Config) { c.DemoMode = true },
			wantSub: "mutually exclusive",
		},
		{
			// A multi-tenant OAuth callback finishes by minting a hand-off token
			// rather than setting a cookie, so without the secret the process boots
			// happily and the failure surfaces on somebody's sign-in.
			name:    "google login without the hand-off secret",
			mutate:  func(c *config.Config) { c.GoogleClientID = "google-client-id" },
			wantSub: "CALNODE_SSO_SHARED_SECRET",
		},
		{
			name:    "microsoft login without the hand-off secret",
			mutate:  func(c *config.Config) { c.MicrosoftClientID = "microsoft-client-id" },
			wantSub: "CALNODE_SSO_SHARED_SECRET",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				MultiTenant:      true,
				DatabaseURL:      appDSN,
				DatabaseAdminURL: adminDSN,
			}
			tc.mutate(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil; want a refusal mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Validate() = %q; want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidate_multiTenantOAuthWithTheSecretIsFine is the other half of the two
// cases above: the rule is "social login needs the secret", not "social login is
// refused", so a configuration that supplies both has to pass. Without this, the
// refusal could be tightened into a ban and nothing would notice.
func TestValidate_multiTenantOAuthWithTheSecretIsFine(t *testing.T) {
	cfg := &config.Config{
		MultiTenant:       true,
		DatabaseURL:       appDSN,
		DatabaseAdminURL:  adminDSN,
		GoogleClientID:    "google-client-id",
		MicrosoftClientID: "microsoft-client-id",
		SSOSharedSecret:   "a-shared-secret",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v; social login with the hand-off secret set is a supported configuration", err)
	}
}

// TestValidate_singleTenantIgnoresTheRest is the byte-identical promise at the
// config layer: with MULTI_TENANT unset, none of the combinations above is an
// error, because none of the machinery they guard is running.
func TestValidate_singleTenantIgnoresTheRest(t *testing.T) {
	cfg := &config.Config{
		DatabaseURL:      "sqlite://./data/calnode.db",
		DatabaseAdminURL: "sqlite://./data/calnode.db",
		DemoMode:         true,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v; single-tenant mode should not police the multi-tenant knobs", err)
	}
}

func TestValidate_postgresqlSchemeAccepted(t *testing.T) {
	cfg := &config.Config{
		MultiTenant:      true,
		DatabaseURL:      "postgresql://app:pw@h/db",
		DatabaseAdminURL: "postgresql://platform:pw@h/db",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v; postgresql:// is the same engine as postgres://", err)
	}
}

// ADMIN_SPA switches the embedded admin console off for a multi-tenant fleet whose
// tenants are administered from the platform's own dashboard.

func TestLoad_adminSPADefaultsOn(t *testing.T) {
	os.Unsetenv("ADMIN_SPA")

	cfg := config.Load()

	if !cfg.AdminSPA {
		t.Error("AdminSPA = false with ADMIN_SPA unset; the console must default to on")
	}
	if !cfg.AdminSPAEnabled() {
		t.Error("AdminSPAEnabled() = false with ADMIN_SPA unset")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with ADMIN_SPA unset: %v", err)
	}
}

func TestLoad_adminSPAOffAndOn(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"off", false},
		{"OFF", false},
		{" off ", false}, // trimmed: an operator's stray space is not a typo
		{"on", true},
		{"ON", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("ADMIN_SPA", tc.value)
			cfg := config.Load()
			if cfg.AdminSPA != tc.want {
				t.Errorf("AdminSPA = %v for ADMIN_SPA=%q; want %v", cfg.AdminSPA, tc.value, tc.want)
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() for ADMIN_SPA=%q: %v", tc.value, err)
			}
		})
	}
}

// ⛔ Anything that is neither on nor off refuses the boot. The fallback is "on", so a
// value nobody can read exactly would otherwise serve the console the operator wrote
// the variable to remove, with nothing anywhere saying why. `true` and `false` are
// refused for the same reason: one setting, one spelling.
func TestValidate_rejectsAnUnreadableAdminSPA(t *testing.T) {
	for _, value := range []string{"maybe", "true", "false", "1", "0", "no", "disabled"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ADMIN_SPA", value)
			cfg := config.Load()
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for ADMIN_SPA=%q; want a refusal", value)
			}
			if !strings.Contains(err.Error(), "ADMIN_SPA") {
				t.Errorf("Validate() = %v; the message must name the variable", err)
			}
		})
	}
}

// ⛔ The single-tenant rule: a self-hoster has no other admin UI, so ADMIN_SPA=off is
// recorded and not acted on. AdminSPAEnabled is where that lives, and it is the only
// expression the route registration and the SSO hand-off consult — quoting
// cfg.AdminSPA directly is how the two halves would come to disagree.
func TestAdminSPAEnabled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		multiTenant bool
		adminSPA    bool
		want        bool
	}{
		{"single-tenant, on", false, true, true},
		{"single-tenant, off is ignored", false, false, true},
		{"multi-tenant, on", true, true, true},
		{"multi-tenant, off", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{MultiTenant: tc.multiTenant, AdminSPA: tc.adminSPA}
			if got := cfg.AdminSPAEnabled(); got != tc.want {
				t.Errorf("AdminSPAEnabled() = %v; want %v", got, tc.want)
			}
		})
	}
}
