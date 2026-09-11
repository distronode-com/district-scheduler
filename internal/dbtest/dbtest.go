// Package dbtest opens a migrated database for a test, on whichever engine the
// environment selects.
//
// Unset CALNODE_TEST_POSTGRES_DSN — the default, and what upstream CI and every
// other contributor sees — means in-memory SQLite, exactly as before. Set it, and
// the same tests run against that PostgreSQL server, each in a schema of its own
// that is created before the migrations and dropped afterwards. Nothing here changes
// what a test asserts; it changes which engine has to satisfy it.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
)

// DSNEnv names the environment variable that switches the suite onto PostgreSQL.
const DSNEnv = "CALNODE_TEST_POSTGRES_DSN"

// PostgresDSN returns the configured PostgreSQL DSN, or "" when the suite should
// run on SQLite.
func PostgresDSN() string { return os.Getenv(DSNEnv) }

// Open returns a migrated handle for t, on Postgres when DSNEnv is set and on
// in-memory SQLite otherwise. The handle is closed when t finishes.
func Open(t *testing.T) *db.DB {
	t.Helper()
	if dsn := PostgresDSN(); dsn != "" {
		return openPostgres(t, dsn)
	}
	return openSQLite(t)
}

// RequirePostgres returns a migrated Postgres handle, skipping t when DSNEnv is
// unset. For the tests that are only meaningful on Postgres — a race the SQLite
// pool makes impossible, say.
func RequirePostgres(t *testing.T) *db.DB {
	t.Helper()
	dsn := PostgresDSN()
	if dsn == "" {
		t.Skipf("%s is not set; nothing to run against PostgreSQL", DSNEnv)
	}
	return openPostgres(t, dsn)
}

func openSQLite(t *testing.T) *db.DB {
	t.Helper()
	h, err := db.OpenDB("sqlite://:memory:")
	if err != nil {
		t.Fatalf("dbtest: open sqlite: %v", err)
	}
	t.Cleanup(func() { h.Close() }) // #nosec G104 -- test-helper cleanup of an in-memory handle the test is finished with; a close error cannot affect the result it already produced
	if err := h.Migrate(); err != nil {
		t.Fatalf("dbtest: migrate sqlite: %v", err)
	}
	return h
}

// openPostgres gives t its own schema on the shared server.
//
// A schema rather than a database: CREATE DATABASE cannot run inside a
// transaction, takes seconds on a busy server, and needs rights a CI service
// container may not grant the test role. A schema is one statement, isolates
// tables and goose's own bookkeeping equally well, and disappears whole with
// DROP ... CASCADE. Everything reaches it through search_path on the connection,
// so no query in the tree needs to name it.
func openPostgres(t *testing.T, dsn string) *db.DB {
	t.Helper()

	schema := "calnode_test_" + randomSuffix(t)

	// A second handle on the default search_path, kept open only to create and drop
	// the schema: the drop cannot run through a connection whose search_path points
	// at the schema being dropped.
	admin, err := db.OpenDB(dsn)
	if err != nil {
		t.Fatalf("dbtest: open postgres (admin): %v", err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + quoteIdent(schema)); err != nil {
		admin.Close() // #nosec G104 -- unwinding the admin handle before the t.Fatalf below, which reports the create-schema error that actually failed the test
		t.Fatalf("dbtest: create schema %s: %v", schema, err)
	}
	// Registered first, so it runs last: the schema is dropped after the handle
	// below has been closed.
	t.Cleanup(func() {
		defer admin.Close()
		if err := dropSchema(admin, schema); err != nil {
			t.Errorf("dbtest: drop schema %s: %v", schema, err)
		}
	})

	h, err := db.OpenDB(withSearchPath(t, dsn, schema))
	if err != nil {
		t.Fatalf("dbtest: open postgres: %v", err)
	}
	t.Cleanup(func() { h.Close() }) // #nosec G104 -- test-helper cleanup; the schema this handle points at is dropped by the cleanup registered above, which does report its error

	if err := h.Migrate(); err != nil {
		t.Fatalf("dbtest: migrate postgres: %v", err)
	}
	return h
}

// dropSchema drops the test's schema, retrying while work the test started is still
// finishing.
//
// Calnode's handlers do several things fire-and-forget: a booking spawns goroutines
// to notify hosts, enqueue the webhook and enqueue reminders. Those outlive the test
// BODY, and closing the pool does not stop them — database/sql closes idle
// connections and lets in-flight statements run to completion. DROP SCHEMA CASCADE
// needs an exclusive lock on every object in the schema, so it meets those
// statements and PostgreSQL reports "deadlock detected" (SQLSTATE 40P01). It showed
// up only under `go test ./...`, where packages run concurrently and everything is
// slower — two handler tests out of several hundred, in a different pair each run.
//
// lock_timeout is what makes retrying viable: without it the DROP either waits
// indefinitely or is chosen as a deadlock victim. With it the attempt fails fast and
// cheaply, and the background work it was contending with has finished by the next
// one. The budget is bounded so a schema that genuinely cannot be dropped is
// reported rather than waited on forever — a leaked schema accumulates on a shared
// server, so it is worth failing the test over.
//
// The statements run on ONE pinned connection because lock_timeout is per-session
// and the pool would otherwise be free to apply the SET to a different connection
// than the DROP. Neither statement has placeholders, so nothing needs rebinding.
func dropSchema(admin *db.DB, schema string) error {
	ctx := context.Background()
	conn, err := admin.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // returning the drop's error is more useful

	if _, err := conn.ExecContext(ctx, `SET lock_timeout = '250ms'`); err != nil {
		return err
	}

	const attempts = 20 // ~5s of wall clock, against background work that takes ms
	var lastErr error
	for i := 0; i < attempts; i++ {
		if _, lastErr = conn.ExecContext(ctx, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`); lastErr == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return lastErr
}

// withSearchPath returns dsn with search_path pointed at schema. pgx passes
// unrecognised query parameters through as PostgreSQL runtime parameters, so this
// is the whole of the isolation: goose writes goose_db_version there, the
// migrations create their tables there, and every unqualified name in the tree
// resolves there.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("dbtest: parse %s: %v", DSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// randomSuffix keeps concurrent runs — two packages under `go test ./...`, or two
// CI jobs against one server — out of each other's schemas.
func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("dbtest: random schema name: %v", err)
	}
	return hex.EncodeToString(b)
}

// quoteIdent quotes a generated schema name. The names are hex from the line
// above, so this is belt and braces rather than a defence — but a schema name
// interpolated into DDL unquoted is a bad habit to leave lying around for the next
// person who passes something else in.
func quoteIdent(name string) string {
	return `"` + name + `"`
}
