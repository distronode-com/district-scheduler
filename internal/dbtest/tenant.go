package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// RequireTenantPair returns a live multi-tenant handle pair against the test's own
// schema: app connected as a freshly created NOBYPASSRLS role that owns nothing,
// and platform as the suite's own role, which owns the schema and bypasses.
// Row-level security is enabled before it returns.
//
// ⛔ THE NON-SUPERUSER ROLE IS THE WHOLE POINT. Superusers bypass row-level
// security unconditionally, so does any role with BYPASSRLS, and so does a table's
// owner unless FORCE is set. The suite's DSN is normally the superuser that owns
// the test schema, so a tenancy assertion made through it passes whether the
// policies exist or not. This SKIPS LOUDLY rather than falling back if the role
// cannot be created, reports rolsuper/rolbypassrls, or owns a table.
//
// It lives here rather than in an internal/db test file because more than one
// package needs it: internal/db proves the handle, internal/server proves the
// routes, and Boundary 7 proves the whole surface.
func RequireTenantPair(t *testing.T) (app, platform *db.DB) {
	t.Helper()

	dsn := PostgresDSN()
	if dsn == "" {
		t.Skipf("LOUD SKIP: %s is not set. A multi-tenant test needs a real PostgreSQL server: "+
			"the isolation guarantee is row-level security, and there is nothing to assert without it.", DSNEnv)
	}

	owner := openPostgres(t, dsn) // creates the schema, migrates it, drops it after

	var schema string
	if err := owner.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("dbtest: read current_schema: %v", err)
	}

	// Random hex, so a name interpolated into DDL cannot carry a quote or a
	// keyword. PostgreSQL takes no placeholder for a role name.
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("dbtest: rand: %v", err)
	}
	role := "calnode_app_" + hex.EncodeToString(buf)
	const password = "tenant_pair_pw" // #nosec G101 -- test-only role password for the local tenant-pair harness, never a real credential

	if _, err := owner.Exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + password + `' NOBYPASSRLS`); err != nil {
		t.Skipf("LOUD SKIP: cannot CREATE ROLE on this server (%v). The multi-tenant tests REQUIRE a "+
			"NOBYPASSRLS role — asserting isolation through the suite's own superuser DSN would pass "+
			"with or without the policies. Point %s at a server where the test role may create roles.",
			err, DSNEnv)
	}
	t.Cleanup(func() {
		// DROP OWNED also revokes the grants below, which DROP ROLE would refuse over.
		if _, err := owner.Exec(`DROP OWNED BY ` + role); err != nil {
			t.Errorf("dbtest: drop owned by %s: %v", role, err)
		}
		if _, err := owner.Exec(`DROP ROLE ` + role); err != nil {
			t.Errorf("dbtest: drop role %s: %v", role, err)
		}
	})

	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA ` + quoteIdent(schema) + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + quoteIdent(schema) + ` TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + quoteIdent(schema) + ` TO ` + role,
	} {
		if _, err := owner.Exec(stmt); err != nil {
			t.Fatalf("dbtest: %s: %v", stmt, err)
		}
	}

	if err := owner.EnableRLS(context.Background()); err != nil {
		t.Fatalf("dbtest: EnableRLS: %v", err)
	}

	app, platform, err := db.OpenPair(
		rewriteUser(t, dsn, role, password, schema),
		withSearchPath(t, dsn, schema),
	)
	if err != nil {
		t.Fatalf("dbtest: OpenPair: %v", err)
	}
	t.Cleanup(func() {
		// #nosec G104 -- test-helper cleanup of the two handles the test is finished with;
		// the role and schema they used are dropped by the cleanups registered above, and
		// those DO report their errors.
		app.Close()
		platform.Close() // #nosec G104 -- as above
	})

	// The guard that turns "the application role cannot bypass" from an assumption
	// into a checked fact, and that would catch a role able to read everything.
	if err := app.VerifyRoles(context.Background()); err != nil {
		t.Skipf("LOUD SKIP: VerifyRoles rejected this pair, so nothing below would prove isolation: %v", err)
	}

	return app, platform
}

// rewriteUser points dsn at a different role and pins search_path. pgx forwards
// unrecognised query parameters as PostgreSQL runtime parameters, which is how
// search_path reaches the session — the same mechanism openPostgres uses.
func rewriteUser(t *testing.T, dsn, user, password, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("dbtest: parse %s: %v", DSNEnv, err)
	}
	u.User = url.UserPassword(user, password)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
