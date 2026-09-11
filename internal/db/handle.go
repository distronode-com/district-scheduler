package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

// DB is a *sql.DB that knows its dialect, rebinds placeholders, and — in
// multi-tenant mode — binds the tenant of every statement it runs.
//
// Every query method takes the portable ? form and rewrites it for the engine in
// use, so the rest of Calnode writes one statement per query no matter which
// database it runs on. Behaviour on SQLite is identical to using *sql.DB
// directly: Rebind is a no-op there.
//
// # Tenant binding
//
// A handle from OpenPair carries a workspace. Before each statement it acquires a
// pooled connection, runs SELECT set_config('app.workspace_id', …), runs the
// statement on that connection, and releases the connection when the statement is
// finished — Exec immediately, Row on Scan/Err, Rows on Close, Tx on
// Commit/Rollback. Nothing is pinned between statements, which is what makes a
// handle safe to copy into a fire-and-forget goroutine: it holds a pool and a
// string, not a session.
//
// Because every statement sets the parameter itself, a value left behind on a
// pooled connection can never leak into a later statement. The unbound handle —
// the platform handle, and the pair's base app handle — binds the empty string,
// which no workspace id can equal, so it matches no row under the policies.
//
// A handle from OpenDB carries no workspace and binds nothing, so single-tenant
// SQLite and single-tenant PostgreSQL run exactly the statements they ran before.
//
// The embedded *sql.DB is exported on purpose — pool tuning, Ping, Close and
// anything that must hand a plain *sql.DB to a library (goose here, Litestream in
// DEPLOY.md) reach it as h.DB. ⛔ Statements issued through that field are NOT
// rebound AND NOT TENANT-BOUND: on a multi-tenant instance they will see nothing
// and write nothing, because an unbound session matches no row. Use the wrapper's
// own methods unless you are deliberately writing engine-specific DDL.
type DB struct {
	*sql.DB
	dialect Dialect

	// multiTenant is set only by OpenPair. It is what turns the binding on; a
	// single handle from OpenDB leaves it false and behaves as it always did.
	multiTenant bool

	// workspace is the tenant this handle is bound to. "" is the unbound handle,
	// which binds '' and therefore matches no row.
	workspace string

	// platform is the paired owner handle. nil on a single handle, where
	// Platform() answers with the handle itself.
	platform *DB

	// err poisons a handle built from a workspace id that failed validation.
	// Every statement method returns it rather than running unbound, because an
	// unbound statement on a multi-tenant database is silently empty.
	err error
}

// ErrInvalidWorkspace is returned by every statement on a handle built from a
// workspace id that is not [a-z0-9_-]{1,64}.
var ErrInvalidWorkspace = errors.New("db: invalid workspace id")

// workspaceIDPattern is the shape of a workspace id. The id is never
// interpolated into SQL — it is a bind parameter to set_config — so this is not
// an injection defence; it is a guard against a caller passing something that is
// not an id at all (an email, a URL, a whole request path) and getting a handle
// that silently matches nothing.
var workspaceIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ValidWorkspaceID reports whether id is a well-formed workspace id.
func ValidWorkspaceID(id string) bool { return workspaceIDPattern.MatchString(id) }

// Dialect reports which engine this handle is talking to, for the few callers
// that must branch on it.
func (h *DB) Dialect() Dialect { return h.dialect }

// MultiTenant reports whether this handle binds a tenant per statement.
func (h *DB) MultiTenant() bool { return h.multiTenant }

// Workspace returns the workspace this handle is bound to, or "" for an unbound
// handle (single-tenant, the platform handle, or a pair's base app handle).
func (h *DB) Workspace() string { return h.workspace }

// Err returns the error that poisoned this handle, or nil.
func (h *DB) Err() error { return h.err }

// ForWorkspace returns a handle that binds id before every statement.
//
// It is a cheap value: the returned handle shares this one's pool and adds a
// string, so it may be copied freely and handed to a goroutine that outlives the
// request that made it. Nothing is pinned.
//
// On a single handle — SQLite, or single-tenant PostgreSQL — it is the identity
// function and does not even validate, because there is exactly one workspace and
// no parameter to bind.
func (h *DB) ForWorkspace(id string) *DB {
	if !h.binds() {
		return h
	}
	scoped := *h
	if !ValidWorkspaceID(id) {
		scoped.err = fmt.Errorf("%w: %q", ErrInvalidWorkspace, id)
		return &scoped
	}
	scoped.workspace = id
	scoped.err = nil
	return &scoped
}

// Platform returns the handle that owns the schema and bypasses row-level
// security: migrations, the worker's cross-tenant claim loop, the reconciler's
// workspace enumeration, and the credential lookups that must resolve a tenant
// before one is known.
//
// On a single handle it returns the handle itself, so a caller needs no branch:
// in single-tenant mode the application role and the platform role are one role.
func (h *DB) Platform() *DB {
	if h.platform != nil {
		return h.platform
	}
	return h
}

// binds reports whether this handle sets app.workspace_id per statement.
func (h *DB) binds() bool { return h.multiTenant && h.dialect == DialectPostgres }

// bindConn takes a connection out of the pool and sets app.workspace_id on it.
// It returns (nil, nil) for a handle that does not bind, which every caller reads
// as "use the pool directly, as before".
//
// The statement is written in PostgreSQL's own $n form because a *sql.Conn does
// not go through the wrapper and is therefore not rebound. This path is
// PostgreSQL-only by construction.
func (h *DB) bindConn(ctx context.Context) (*sql.Conn, error) {
	if !h.binds() {
		return nil, nil
	}
	conn, err := h.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx,
		`SELECT set_config('app.workspace_id', $1, false)`, h.workspace); err != nil {
		conn.Close() // #nosec G104 -- releasing the connection on the error path; the bind error returned below is the useful one
		return nil, fmt.Errorf("bind workspace %q: %w", h.workspace, err)
	}
	return conn, nil
}

// Rebind converts a ?-placeholder statement for this handle's dialect. Useful
// when a caller builds SQL dynamically and hands it to something other than the
// methods below.
func (h *DB) Rebind(query string) string { return h.dialect.Rebind(query) }

func (h *DB) Query(query string, args ...any) (*Rows, error) {
	return h.QueryContext(context.Background(), query, args...)
}

func (h *DB) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	if h.err != nil {
		return nil, h.err
	}
	conn, err := h.bindConn(ctx)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		rows, err := h.DB.QueryContext(ctx, h.dialect.Rebind(query), args...)
		if err != nil {
			return nil, err
		}
		return &Rows{Rows: rows}, nil
	}
	rows, err := conn.QueryContext(ctx, h.dialect.Rebind(query), args...)
	if err != nil {
		conn.Close() // #nosec G104 -- releasing the connection on the error path; the query error returned below is the useful one
		return nil, err
	}
	return &Rows{Rows: rows, conn: conn}, nil
}

func (h *DB) QueryRow(query string, args ...any) *Row {
	return h.QueryRowContext(context.Background(), query, args...)
}

func (h *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if h.err != nil {
		return &Row{err: h.err}
	}
	conn, err := h.bindConn(ctx)
	if err != nil {
		return &Row{err: err}
	}
	if conn == nil {
		return &Row{Row: h.DB.QueryRowContext(ctx, h.dialect.Rebind(query), args...)}
	}
	return &Row{Row: conn.QueryRowContext(ctx, h.dialect.Rebind(query), args...), conn: conn}
}

func (h *DB) Exec(query string, args ...any) (sql.Result, error) {
	return h.ExecContext(context.Background(), query, args...)
}

func (h *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if h.err != nil {
		return nil, h.err
	}
	conn, err := h.bindConn(ctx)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return h.DB.ExecContext(ctx, h.dialect.Rebind(query), args...)
	}
	defer conn.Close() //nolint:errcheck // the statement's error is the useful one
	return conn.ExecContext(ctx, h.dialect.Rebind(query), args...)
}

// Prepare and PrepareContext rebind at prepare time, so the returned *sql.Stmt
// needs no wrapper of its own.
//
// ⛔ They are refused on a handle that binds a tenant. A *sql.Stmt is re-prepared
// on whatever connection the pool hands it, and there is no hook to set
// app.workspace_id on that connection first — so a prepared statement on a
// multi-tenant handle would run unbound and silently see nothing. Nothing in the
// tree prepares a statement; if something needs to, it should take a transaction,
// where the binding is a property of the connection for the whole tx.
func (h *DB) Prepare(query string) (*sql.Stmt, error) {
	return h.PrepareContext(context.Background(), query)
}

func (h *DB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	if h.err != nil {
		return nil, h.err
	}
	if h.binds() {
		return nil, errors.New("db: Prepare is not available on a tenant-bound handle — " +
			"a *sql.Stmt is re-prepared on an arbitrary pooled connection, which would run unbound; use a transaction")
	}
	return h.DB.PrepareContext(ctx, h.dialect.Rebind(query))
}

// Begin and BeginTx return a *Tx rather than a *sql.Tx: a transaction that
// silently stopped rebinding would be the easiest way to reintroduce ? into a
// Postgres statement.
func (h *DB) Begin() (*Tx, error) {
	return h.BeginTx(context.Background(), nil)
}

func (h *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	if h.err != nil {
		return nil, h.err
	}
	if !h.binds() {
		tx, err := h.DB.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &Tx{Tx: tx, dialect: h.dialect}, nil
	}

	// A transaction owns its connection for its whole life, so the binding is set
	// once, inside the transaction, with SET LOCAL semantics: it reverts when the
	// transaction ends, which means the connection goes back to the pool carrying
	// nothing. Every statement on the pool sets the parameter itself anyway, so
	// this is belt and braces rather than the guarantee.
	conn, err := h.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		conn.Close() // #nosec G104 -- releasing the connection on the error path; the BeginTx error returned below is the useful one
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('app.workspace_id', $1, true)`, h.workspace); err != nil {
		tx.Rollback() // #nosec G104 -- unwinding a transaction that never got its binding; the bind error returned below is the useful one
		conn.Close()  // #nosec G104 -- releasing the connection on the same error path
		return nil, fmt.Errorf("bind workspace %q on transaction: %w", h.workspace, err)
	}
	return &Tx{Tx: tx, dialect: h.dialect, conn: conn, workspace: h.workspace}, nil
}

// Rows is a *sql.Rows that releases the connection its statement was bound on.
//
// Close does both, and is what every caller already defers. A caller that never
// closes leaks a connection out of the pool on a multi-tenant instance, which is
// the same bug as never closing a *sql.Rows, only with a pool of ten rather than
// a cursor.
type Rows struct {
	*sql.Rows
	conn *sql.Conn
}

// Close closes the cursor and releases the bound connection. It returns the
// cursor's error in preference to the release's, because the cursor's is the one
// that says something about the query.
func (r *Rows) Close() error {
	err := r.Rows.Close()
	if r.conn != nil {
		cerr := r.conn.Close()
		r.conn = nil
		if err == nil {
			err = cerr
		}
	}
	return err
}

// Row is a *sql.Row that releases the connection its statement was bound on.
//
// Scan and Err both release, because those are the only two things a caller can
// do with a Row and exactly one of them always happens. errors.Is against
// sql.ErrNoRows works unchanged: Scan returns the underlying error verbatim.
type Row struct {
	*sql.Row
	conn *sql.Conn
	err  error
}

func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	err := r.Row.Scan(dest...)
	r.release()
	return err
}

func (r *Row) Err() error {
	if r.err != nil {
		return r.err
	}
	err := r.Row.Err()
	r.release()
	return err
}

func (r *Row) release() {
	if r.conn != nil {
		// #nosec G104 -- best-effort release back to the pool. Unlike Rows.Close, whose whole
		// job is releasing, release() is reached from Scan and Err, and surfacing a release
		// error there would report a row that was read correctly as a failed read.
		// database/sql discards the connection either way, so there is nothing to retry.
		r.conn.Close()
		r.conn = nil
	}
}

// Tx is a *sql.Tx with the same rebinding behaviour as DB, and — when it came
// from a tenant-bound handle — ownership of the connection the tenant was bound
// on. Commit and Rollback release it.
type Tx struct {
	*sql.Tx
	dialect   Dialect
	conn      *sql.Conn
	workspace string
}

// Dialect reports which engine this transaction is running against.
func (t *Tx) Dialect() Dialect { return t.dialect }

// Workspace returns the workspace this transaction is bound to, or "".
func (t *Tx) Workspace() string { return t.workspace }

// Rebind converts a ?-placeholder statement for this transaction's dialect.
func (t *Tx) Rebind(query string) string { return t.dialect.Rebind(query) }

// Commit and Rollback release the pinned connection.
//
// Both are idempotent about the release, because `defer tx.Rollback()` after a
// successful Commit is the standard pattern in this tree: the second call gets
// sql.ErrTxDone from database/sql, and must not double-close the connection.
func (t *Tx) Commit() error {
	err := t.Tx.Commit()
	t.release()
	return err
}

func (t *Tx) Rollback() error {
	err := t.Tx.Rollback()
	t.release()
	return err
}

func (t *Tx) release() {
	if t.conn != nil {
		// #nosec G104 -- best-effort release back to the pool. release() is reached from
		// Commit and Rollback, which return the transaction's own error; surfacing a release
		// error from Commit would report a transaction that DID commit as having failed.
		// database/sql discards the connection either way, so there is nothing to retry.
		t.conn.Close()
		t.conn = nil
	}
}

func (t *Tx) Query(query string, args ...any) (*Rows, error) {
	rows, err := t.Tx.Query(t.dialect.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	return &Rows{Rows: rows}, nil
}

func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	rows, err := t.Tx.QueryContext(ctx, t.dialect.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	return &Rows{Rows: rows}, nil
}

func (t *Tx) QueryRow(query string, args ...any) *Row {
	return &Row{Row: t.Tx.QueryRow(t.dialect.Rebind(query), args...)}
}

func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	return &Row{Row: t.Tx.QueryRowContext(ctx, t.dialect.Rebind(query), args...)}
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(t.dialect.Rebind(query), args...)
}

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) Prepare(query string) (*sql.Stmt, error) {
	return t.Tx.Prepare(t.dialect.Rebind(query))
}

func (t *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return t.Tx.PrepareContext(ctx, t.dialect.Rebind(query))
}
