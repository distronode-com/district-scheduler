# PROGRESS — Postgres call sites (`feat/postgres-callsites`)

Converting Calnode's call sites onto the dialect-aware `*db.DB` / `*db.Tx` wrapper
(`internal/db`, built on the branch's base commit) so the app runs on PostgreSQL as
well as SQLite. SQLite behaviour must not change.

## Boundary 1 — thread the type through — DONE

Every struct field, constructor, package-level helper and test helper that held
`*sql.DB` now holds `*db.DB`; `*sql.Tx` became `*db.Tx`. Call sites needed no edit:
the wrapper's method names match `database/sql`'s.

- `db.Open` → `db.OpenDB` and `db.Migrate(x)` → `x.Migrate()` in `cmd/calnode/*` and
  every test helper.
- `internal/connstore`: `Execer` is an interface, so it needed no change; its doc
  comment now names `*db.DB` / `*db.Tx`. `destination_test.go` deliberately opens a
  bare `sql.Open("sqlite", ":memory:")` against a bespoke fragment schema and stays
  `*sql.DB` — `Execer` accepts it.
- `internal/handler/health.go` passes `h.db.DB` to `db.SchemaReady`, which takes a
  bare `*sql.DB`. Legitimate: its one statement carries no placeholders.

Gates: `go build ./...`, `go vet ./...`, `gofmt -l`, `go test ./...` all clean on SQLite.

## Boundary 2 — non-portable SQL — DONE

New package `internal/dbtime`: `Now()` for `datetime('now')`, `NowMilli()` for
`strftime('%Y-%m-%dT%H:%M:%fZ','now')`. Every one of the 38 engine-side timestamps
became a bound parameter. The two layouts are kept distinct on purpose — the schema
already stores both shapes, they are compared lexicographically and served to
clients verbatim, so folding them into one would change SQLite's stored bytes.
`dbtime_test.go` asserts both layouts against SQLite's own output.

- `INSERT OR IGNORE` → `INSERT … ON CONFLICT DO NOTHING`, no conflict target, which
  both engines accept and which matches OR IGNORE's "any unique constraint" scope
  (3 sites: demo seed, and the two reminder enqueues).
- `COLLATE NOCASE` → `LOWER(col) = LOWER(?)` at both sites. No `d.SQL` pair: nothing
  indexes `booking_attendees.email`, so there is no collation for an index to depend
  on and no plan to preserve.
- `PRAGMA foreign_keys` in `demo.Reset` is now SQLite-only. Postgres has no
  equivalent short of superuser rights, so that branch wipes with one
  `TRUNCATE … CASCADE` naming every table, which needs no FK ordering.
- `sqlite_master` in `demo.listTables` is the one `d.SQL` pair: `pg_tables` scoped to
  `current_schema()` on Postgres, which also keeps it correct inside an isolated
  test schema.
- `RETURNING position` (`question_handler.go`) is unchanged. Both engines support it;
  verified on SQLite by the existing question tests, unverified on Postgres.
- No `LIMIT` inside any `UPDATE`/`DELETE`. Checked by scanning every backquoted
  string in the tree, not by a line grep.

Gates: `go build ./...`, `go vet ./...`, `gofmt -l`, `go test ./...` clean on SQLite.

## Boundary 3 — advisory lock replacing the single-writer guarantee — DONE

`internal/booking/hostlock.go`. `lockHosts` takes `pg_advisory_xact_lock` on each
host whose availability the transaction is about to decide, at the start of the
transaction and before the first `hostBusy` read. On SQLite it returns immediately
and the `SetMaxOpenConns(1)` guarantee stands untouched.

- **Key derivation**: `int64(binary.BigEndian.Uint64(sha256.Sum256("calnode:booking:host:" + hostID)[:8]))`.
  Domain-separated so a future advisory lock on another entity cannot collide by
  hashing the same raw id. A collision between two hosts would cost an unnecessary
  serialisation, never a wrong answer, so stability and spread are what matter.
  `hostlock_internal_test.go` pins two values computed independently of the code.
- **Deadlock**: ids are sorted and deduplicated on a copy before locking, so two
  transactions needing the same pair cannot take them in opposite orders. The copy
  matters because `Create`'s `HostIDs` arrive in round-robin priority order.
- **Three call sites, not two.** `Create` and `Reschedule` as specified, plus
  `ReassignHost`, which has the identical check-then-write shape (`hostBusy`'s own
  doc names all three). Leaving it out would have left a known hole.
- `Reschedule` locks the primary host *before* reading `booking_hosts`, not after: a
  concurrent `ReassignHost` holds that same key, and that ordering is what stops a
  reassignment committing between the host-list read and the `UPDATE`.
- `isUniqueViolation` now recognises Postgres by SQLSTATE 23505 (`pgconn.PgError`)
  as well as SQLite by message. Without it the index's backstop returned a 500
  instead of `ErrDoubleBooked` on Postgres.
- The materialise-into-a-slice patterns in `Reschedule` and the calendar reconciler
  are untouched; they are still required on SQLite.

**Measured, against the live PostgreSQL 17 on 127.0.0.1:55432**, with a temporary
hand-written minimal schema because `migrations/postgres` does not exist yet. Two
goroutines race overlapping-but-not-identical slots (10:00–10:30 against
10:15–10:45, so `idx_bookings_no_double` cannot catch it), 40 rounds:

| | created | conflicts | overlapping pairs in the DB |
|---|---|---|---|
| lock in place | 40 | 40 | **0** |
| lock disabled | 79 | 1 | **39** |

The negative control is the point: unlocked, the partial unique index caught 1 of
40. The committed test (`internal/booking/concurrency_test.go`) is the same race
through `dbtest.RequirePostgres`, and skips on SQLite saying the single-connection
pool makes the race impossible.

## Boundary 4 — Postgres test harness, docs, CI — DONE

`internal/dbtest` landed with Boundary 3 because the concurrency test needs it.
`Open(t)` returns in-memory SQLite unless `CALNODE_TEST_POSTGRES_DSN` is set, in
which case each test gets its own `calnode_test_<random>` schema, created before
the migrations and dropped after. Isolation rides on `search_path` in the DSN,
which `dbtest_test.go` verifies against a live server rather than assuming
(`TestSearchPathIsolation` passes; it also asserts the schema is invisible from the
default path).

- **22 test helpers across 18 files** repointed from `db.OpenDB("sqlite://:memory:")`
  + `Migrate()` onto `dbtest.Open(t)`, so the whole suite follows the environment.
  Deliberately left on SQLite: `dbtime_test.go` (it pins SQLite's own output),
  `hostlock_internal_test.go` (it asserts the lock is a no-op there),
  `connstore/destination_test.go` (bare driver, bespoke fragment schema), and the
  `health_test.go` cases that want an *unmigrated* database.
  `keyvault_test.go` keeps `sqlite://file::memory:?cache=shared&_fk=1` — a different
  DSN for a reason, not a candidate for a blanket rewrite.
- `docs/ARCHITECTURE.md` §4 amended in place, not appended to: the heading is now
  both engines, the pool difference and the rebinding trap are stated, the cursor
  gotcha is scoped to SQLite with the reason the pattern must stay anyway, and a new
  "double-booking guarantee, per engine" subsection carries the single connection on
  SQLite and the advisory lock on Postgres, with the index's real scope. §17's
  gotcha 1 and one now-conditional aside in §8 follow it.
- `.github/workflows/ci.yml` gains a `postgres` job: `postgres:17` service with a
  `pg_isready` health check, the same steps as `check` minus `svelte-check`, and
  `CALNODE_TEST_POSTGRES_DSN` set for `go test ./...`. Additive — with the variable
  unset nothing about an existing run changes.

## No longer blocked

`migrations/postgres` landed with `feat/postgres-core`, so the `postgres` CI job now
has something to migrate and passes.

## Boundary 5 — the suite actually green on both engines — DONE

Rebased onto the finished `feat/postgres-core`, so `migrations/postgres` exists and
the suite could finally run against PostgreSQL. **15 failures**, four distinct
causes — not the one the first triage suggested.

1. **Constraint classification (7 failures).** Thirteen sites decided 409/400/404
   vs 500 by substring-matching SQLite's English error text. `internal/db` now
   exports `IsUniqueViolation` / `IsCheckViolation` / `IsForeignKeyViolation`,
   matching SQLSTATE 23505 / 23514 / 23503 on Postgres and the message on SQLite.
   Pinned by `constraint_test.go`, which provokes a real violation of each class
   against the real schema on whichever engine is configured, asserts exactly one
   of three predicates matches, and asserts none match an unrelated error.
2. **Engine-dependent boolean expressions (2 failures).** `(user_id = ?) AS owned`
   and `(archived_at IS NOT NULL) AS archived` yield 0/1 on SQLite and a boolean on
   Postgres, which does not scan into the `int` the columns beside them use. Both
   are now `CASE WHEN … THEN 1 ELSE 0 END`, portable and matching the schema's own
   0/1 convention.
3. **`json_extract` in production (1 failure).** `replaceReminderJobs` filtered on
   `json_extract(payload, '$.booking_id')`. No portable spelling exists, so this is
   a second `d.SQL` pair: `payload::json ->> 'booking_id'` on Postgres (the cast is
   needed because the column is TEXT). The reschedule test carried its own copy of
   the same expression, and discarded the `Scan` error, so the real failure showed
   up two seconds later as a time-parsing error.
4. **`ORDER BY rowid` (3 failures).** `webhook_deliveries` had no timestamp of its
   own, so recency was `rowid DESC` — unportable, and not strictly correct on
   SQLite either, since VACUUM may renumber rowids. Migration **00058** adds
   `created_at TEXT NOT NULL DEFAULT ''` to both dirs, the writer binds
   `dbtime.NowMilli()`, and the query is `ORDER BY created_at DESC, id DESC`.
5. **`randomblob` in a gcal test helper (2 failures).** SQLite-only id generator,
   replaced with `uid.New()` like every other row in those tests.

Both suites green in one commit: 28/28 packages on SQLite, 28/28 on PostgreSQL.

## Boundary 6 — constraint codes on BOTH engines — DONE

The first version of `constraint.go` claimed modernc.org/sqlite exposes no error
codes and matched SQLite on message text. That was false: it defines `type Error`
with `Code()`, populated for every constraint class. Measured, not read:

| violation | SQLite `Code()` | PostgreSQL SQLSTATE |
|---|---|---|
| UNIQUE | 2067 | 23505 |
| PRIMARY KEY | **1555** | 23505 |
| CHECK | 275 | 23514 |
| FOREIGN KEY | 787 | 23503 |
| NOT NULL | 1299 | 23502 |

⛔ A PRIMARY KEY collision is **1555, not 2067**, while its message still reads
"UNIQUE constraint failed". The text match caught both by accident;
`Code() == 2067` alone would not. `idempotency_keys.idempotency_key` is a bare
PRIMARY KEY, so the whole idempotent-replay path depends on 1555. Proved by
deleting 1555 and re-running: the new `unique via primary key` subtest fails naming
the code and the table, and `TestCreateBooking_idempotentReplay` returns
`replay: 500`. `IsUniqueViolation` matches both codes; PostgreSQL needed no change.

The text comparison is now a fallback only, for an error arriving without its
driver type attached, and `TestConstraintTextFallback` executes that branch so it
is not dead code. A driver error whose code does not match is a definite no and
does not fall through — falling through would readmit a 1555 by its message after
excluding it by code.

Also fixed: the two intermittent handler failures under `go test ./...` were a flake
in **my** `dbtest` harness, not the application. Handler goroutines are
fire-and-forget (notify hosts, enqueue webhook, enqueue reminders) and outlive the
test body; closing the pool does not stop an in-flight statement; `DROP SCHEMA
CASCADE` needs an exclusive lock on every object and deadlocks against them
(SQLSTATE 40P01). The drop now runs on a pinned connection with `lock_timeout` and
retries within a bounded budget. Three consecutive full PostgreSQL runs clean.

1. **Booleans — resolved.** The Postgres migrations declare them `SMALLINT`, so the
   tree's `= 1` comparisons and `boolToInt(...)` work unchanged. Only *computed*
   boolean expressions in SELECT lists needed a fix (Boundary 5, cause 2).
2. **Text timestamp collation.** Timestamps are `TEXT` and compared
   lexicographically on purpose (the recordings consent window, the job queue's
   `run_at <= ?`). That is byte ordering under SQLite. Under a non-`C` Postgres
   collation, ordering of mixed shapes (a space-separated `run_at` against a
   `T`-separated one) is not guaranteed to match. Worth either `COLLATE "C"` on
   those columns or a deliberate decision that it is safe. **Closed by Boundary 7.**

## Boundary 7 — the last four items — DONE

Open item 2 above, the `RETURNING` clause Boundary 2 left unverified, the bare
`Open` the port had been dragging along, and the hard-coded pool size.

### 1. TEXT timestamps are `COLLATE "C"` — migration 00059

**54 columns across 27 tables**, enumerated from the migrated schema rather than
from memory: every `*_at` (49 of them, including `locked_until`), plus
`availability_rules.start_time`/`end_time` and `availability_overrides.date`/
`start_time`/`end_time`, which hold `HH:MM` and `YYYY-MM-DD` and are ordered as
times too (`ORDER BY day_of_week, start_time`, `ORDER BY date`). One grouped
`ALTER TABLE` per table, so each is rewritten once. The SQLite half is a no-op
file: BINARY *is* memcmp, so there is nothing to pin, and the file exists because
the directories keep one file per version.

Fixed in the schema rather than in the predicates. There are ~20 distinct
lexicographic time comparisons in the tree (`run_at <= ?`, `expires_at > ?`,
`locked_until < ?`, `decided_at BETWEEN`, `start_at`/`end_at` overlap,
`created_at < ?`, several `ORDER BY created_at`); a `COLLATE "C"` clause on each
is 20 chances to forget one, and forgetting is silent.

⚠️ **The load-bearing site is `internal/handler/notetaker.go`**, which writes
`datetime('now')`'s space-separated shape *because* it sorts before any
`T`-separated stamp, which is what makes a notetaker job due immediately. That is
a dependency on byte ordering in production code, not a theoretical one.

**Measured, and it corrects the framing of the open item.** The server this
branch develops against is PostgreSQL 17.11 with `datcollate = en_US.utf8`
(libc provider) — read from `pg_database`, because ⛔ `SHOW lc_collate` no longer
exists (it stopped being a GUC in PostgreSQL 16 and errors with "unrecognized
configuration parameter", so the obvious way to ask is a dead end). The two
shapes the schema actually stores do **not** flip under it:

| pair | en_US.utf8 | memcmp |
|---|---|---|
| `2026-01-01 20:00:00` vs `2026-01-01T10:00:00Z` | space first | space first |

Nor under **any of the other 878 collations installed on the server** — scanned,
not assumed. glibc ignores the space at the primary level but still sorts a digit
before `T`, which happens to agree with memcmp. So a control built only on those
two values would pass with or without the migration, which is exactly the
vacuous green the packet warned about. The control therefore also carries RFC
3339's lower-case `t`/`z` spelling (§5.6 permits it, so an importer or a
third-party API can hand it to us), where the two orders really do differ:

```
plain: 2026-01-01 10:00:00 | 2026-01-01T10:00:00.000Z | 2026-01-01t10:00:00z | 2026-01-01T10:00:00Z
C    : 2026-01-01 10:00:00 | 2026-01-01T10:00:00.000Z | 2026-01-01T10:00:00Z | 2026-01-01t10:00:00z
```

`internal/db/collation_test.go` holds it three ways. (a) An audit over
`information_schema.columns` matching by NAME, so a timestamp column added by a
later migration is caught without anyone remembering — `collation_name` is NULL
for a default-collated column and `'C'` after an explicit `COLLATE`, so the
assertion is exact. (b) The control above, which **skips naming the server's
collation** if the default already orders byte-wise, rather than passing
vacuously. (c) An ordering test on `jobs.run_at` through the worker's real claim
predicate.

Proved failable by dropping `jobs.run_at` from the migration:

```
collation_test.go:79: 1 of 54 timestamp columns are not COLLATE "C":
    jobs.run_at = <database default>
collation_test.go:242: ORDER BY run_at =
    [… 2026-01-01t10:00:00z 2026-01-01T10:00:00Z …]
    want byte order
    [… 2026-01-01T10:00:00Z 2026-01-01t10:00:00z …]
```

### 2. `RETURNING position` — works unchanged on Postgres

No `d.SQL` pair, no rewrite. The clause is the auto-position `INSERT` in
`question_handler.go`, which computes the position inside the statement
(`VALUES (…, (SELECT COALESCE(MAX(position)+1, 0) …)) RETURNING position`) so two
concurrent creates cannot land on the same one.

The new test goes through the HTTP handler on `dbtest.Open(t)`, so it follows the
environment like everything else, and it asserts the value the handler **scanned
from RETURNING** as well as the row left behind: a `RETURNING` that quietly
produced a zero would still leave a correct row, so checking the table alone —
which `TestCreateQuestion_autoPosition` already did — cannot see it. The
explicit-position case is in the same test because the next auto position after a
pinned 9 must be 10, which is what shows the subselect is evaluated by the engine
rather than the sequence being an artifact of insertion order.

Measured: **0, 1, 2, then 9 explicit, then 10 — identical on both engines.**

### 3. `db.Open` deleted

It returned a bare `*sql.DB` "for callers that have not moved to OpenDB yet".
Nothing outside the package's own tests used it, and it could only ever do harm:
statements through that handle are not rebound, so every `?` is a Postgres syntax
error found at runtime, far from the call. It was also the shape most likely to
be copied by whoever added the next call site.

`/readyz` was the one production caller reaching into the exported embedded field
(`db.SchemaReady(ctx, h.db.DB)`). `*db.DB` now carries `SchemaReady` and
`AppliedVersion`, so the handler asks the handle. The package-level functions stay
for the cases that genuinely hold a bare pool — goose's bookkeeping, the tests
that open an unmigrated one — and `db_test.go` passes `database.DB` explicitly,
which says at the call site that the bare pool is deliberate. `TestOpen_inMemory`
became `TestOpenDB_inMemory` rather than staying named after something that no
longer exists. No test asserts the symbol's absence: the build is the proof.

### 4. Pool size is configurable

`DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS`, defaults unchanged at 10/5, validated
in `config.PoolFromEnv`: unset, unparsable or non-positive falls back to the
default with a warning (matching `getBool`/`getDuration`, which also refuse to
fail a boot over a typo in an optional knob), and idle above open is clamped —
`database/sql` reduces it silently anyway, so the clamped pair is the honest
description of what the pool will do. ⚠️ The clamp has to apply to the **default**
idle as well: `DB_MAX_OPEN_CONNS=2` alone must give 2/2, not 2/5. A test pins it.

`OpenDB` reads `PoolFromEnv` itself rather than taking the numbers as an
argument, so all five entry points pick them up without five identical edits and
without one of them silently keeping the defaults; `db.WithPool` is the override
for a caller that must not follow the environment. `db` → `config` is not a
cycle (`config` imports only the standard library).

**SQLite stays pinned at 1/1 and ignores both the environment and `WithPool`.**
That is the correctness guarantee, not a preference: the single connection is
what serialises write transactions (it is why `booking.lockHosts` is a no-op
there) and the pragmas are connection-scoped.
`TestOpenDB_sqlitePoolIsNotConfigurable` asserts it with both knobs at 40/20, for
the `:memory:` and file forms.

The idle limit cannot be read back — `sql.DBStats` exposes the open limit only —
so it is measured: four live transactions force four connections, and after
committing them all the pool keeps **1** with `WithPool(4, 1)`.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, **28/28 packages ok on SQLite and
28/28 on PostgreSQL**. Negative control, the same PostgreSQL command with the
password changed to `wrong`: `internal/db` **fails** rather than skipping, with
`failed SASL auth: FATAL: password authentication failed for user "postgres"
(SQLSTATE 28P01)` — including `TestPostgres_idleLimitApplied`, the one new test
whose assertions could conceivably have held without a server.

---

# Multi-tenant mode (`feat/multi-tenant`)

One process, many isolated workspaces. `workspaces` is the tenant root, every
application table carries a `workspace_id`, and PostgreSQL row-level security —
not the query author — is what keeps one workspace out of another's rows.
`MULTI_TENANT` unset must behave exactly as before, on SQLite and on
single-tenant PostgreSQL. That is a gate, not a wish.

## Boundary 1 — config + schema — DONE

### Migration 00060, not 00061

The packet numbered it 00061 on the assumption that `feat/platform-hooks` had
landed 00060. It has not: that branch is not in this clone (no `/v1/auth/sso`
endpoint, no `TRUSTED_PROXY_CIDRS`, no `/metrics`), and the 59 files in each
directory are contiguous. `migrations_internal_test.go` asserts
`target version == file count` and names the failure "a gap or a duplicate
number", so 00061 over 59 files reds an existing gate. 00060 it is;
`knownMigrationCount` moved 59 → 60 with it.

### ⛔ ENABLE / FORCE ROW LEVEL SECURITY is NOT in the migration, and the reason is measured

D2 puts `ENABLE` + `FORCE` + the policy in the migration. `FORCE` makes a
table's policy apply to the table's **owner** as well, and in single-tenant mode
`DATABASE_URL` *is* the owner. Measured against the PostgreSQL 17.11 this branch
develops on, with a `NOBYPASSRLS` owner role and one row present:

| role | RLS | `app.workspace_id` | rows seen |
|---|---|---|---|
| owner (NOBYPASSRLS) | ENABLE + FORCE | unset | **0** |
| superuser | ENABLE + FORCE | unset | 1 |
| owner (NOBYPASSRLS) | ENABLE only | unset | 1 |
| non-owner app role | ENABLE only | unset | **0** |

So an unconditional `FORCE` silently blinds every existing single-tenant
PostgreSQL deployment whose DSN is not a superuser — and **the suite's own DSN
is a superuser, so the test lane would have gone green anyway.** Row 4 is the
other half: `ENABLE` alone already isolates a non-owner role, which is what the
application role is under D4.

The split that keeps both promises: the **policies** live in the migration,
where they are reviewable SQL, and a policy on a table whose RLS is not enabled
is inert (verified, not assumed). The two `ALTER TABLE` lines live in
`db.EnableRLS`, which runs at boot **only when `MULTI_TENANT` is set**, on the
**platform handle** (`DATABASE_ADMIN_URL` — the application role cannot run DDL
in multi-tenant mode at all), is idempotent, and whose failure is
`os.Exit(1)`: without it every policy is inert and the process would come up
looking multi-tenant while separating nothing.
`TestPostgres_rlsIsOffUntilEnabled` pins both halves — off straight after
migrating, on for all 32 tenant tables and none of the 4 exempt ones after
`EnableRLS`, twice.

### The column default is `COALESCE(current_setting(…, true), 'default')`

D1 spells it `current_setting('app.workspace_id')`. The bare form **raises** on
an unset parameter, so it would fail every INSERT in single-tenant mode. The
`missing_ok` form plus `COALESCE` gives `'default'` when nothing is bound, which
is what single-tenant wants, and is never reached in multi-tenant mode because
the handle binds before every statement. It also fails **closed** if a
multi-tenant statement ever escapes that binding: the row would be written as
`'default'` and the policy's `WITH CHECK` compares it against an unset parameter
(NULL, so not true), which refuses the INSERT with SQLSTATE 42501 rather than
letting it land in the wrong tenant. `rls_proof_test.go` asserts exactly that,
including that nothing arrives in the default workspace.

### The tenancy proof, and its negative control

`internal/db/rls_proof_test.go` creates a `calnode_app_<hex>` role with
`NOBYPASSRLS` per test schema, grants it the schema's tables, and opens a handle
as it. ⛔ It **skips loudly** rather than falling back if the role cannot be
created, or reports `rolsuper`/`rolbypassrls`, or owns any table in the schema —
a superuser handle would satisfy every assertion whether the policies existed or
not. Four subtests, both halves required:

- unbound: `SELECT COUNT(*) FROM users` returns **0** of 2
- unbound: INSERT refused with **42501**, and **nothing lands in `default`**
- bound to `ws-a`: sees 1 of 2, reads `a@example.com`, INSERT naming **no**
  `workspace_id` lands as `ws-a`, `b@example.com` invisible
- bound to `ws-a`, naming `ws-b` explicitly: refused with **42501**

`set_config(…, false)` is session-scoped, so every subtest pins one `*sql.Conn`
and writes `$n` directly — statements through a `*sql.Conn` are not rebound.

Proved failable by making `EnableRLS` return early:

```
--- FAIL: TestPostgres_rlsIsolatesAnUnprivilegedRole/unbound_reads_nothing
    rls_proof_test.go:178: an unbound session sees 2 of 2 users; want 0 — an unset app.workspace_id must match no row
--- FAIL: .../unbound_write_is_refused_and_lands_nowhere
    rls_proof_test.go:187: an unbound INSERT succeeded; the policy's WITH CHECK must refuse it
--- FAIL: .../bound_reads_and_writes_exactly_one_workspace
    rls_proof_test.go:215: a session bound to ws-a sees 3 users; want 1
--- FAIL: .../naming_another_workspace_explicitly_is_refused
    rls_proof_test.go:261: writing into another workspace succeeded; WITH CHECK must refuse it
```

### 32 tenant tables, 4 exempt, and a gate against forgetting

`db.TenantTables` / `db.ExemptTables` are the Go copies of the list in the
migration header, and `TestTenancy_tableListsCoverTheSchema` fails if a base
table is in neither — so a table added by a later migration has to be
classified. Exempt: `workspaces` (the root, with its own SELECT-only policy for
the application role), `crypto_keystore` (one DEK per process, D3),
`goose_db_version`, `oauth_clients` (dynamic client registration is per client
*application*; the per-tenant half is `oauth_access_tokens`, which is a tenant
table).

### SQLite's three forced differences

1. **No `REFERENCES workspaces(id)`.** Measured on modernc.org/sqlite with
   `foreign_keys=ON`: `ALTER TABLE t ADD COLUMN workspace_id TEXT NOT NULL
   DEFAULT 'default' REFERENCES ws(id)` is **rejected** — "Cannot add a
   REFERENCES column with non-NULL default value (1)". Rebuilding all 32 tables
   to get a constraint whose only payoff is cascade-on-workspace-delete, in an
   engine that cannot run multi-tenant, is not worth it. The two tables rebuilt
   below omit it too, so the engine is consistent with itself.
2. **No RLS, no policies.** Nothing to express them with, which is why
   `config.Validate` refuses `MULTI_TENANT` without a `postgres://` DSN.
3. **Only the uniqueness that has to MOVE moves.** `idempotency_keys` and
   `meeting_consents` are rebuilt (their PRIMARY KEY changes and SQLite cannot
   ALTER a table constraint); `ux_jobs_type_payload` and `idx_notes_booking` are
   dropped and recreated (plain indexes need no rebuild). `users(email)`,
   `event_types(slug)`, `teams(slug)` and `server_settings`' `id = 1` singleton
   stay exactly as they are: with one workspace, a global unique and a
   `(workspace_id, x)` unique admit precisely the same rows.

### ⚠️ `jobs` keeps a second, workspace-free copy of each partial index

The packet says every partial index on `bookings` and `jobs` gets `workspace_id`
prepended. Correct for `bookings` — every query that uses those indexes now
carries a workspace predicate. **Not** for `jobs`: it is the one table worked
*across* tenants, and the worker's claim and its crash-recovery reaper (B5) run
on the platform handle with no workspace predicate, ordered by `run_at` /
`locked_until` globally. A workspace-leading index cannot serve an ordered global
scan. So both are prepended as instructed **and** `idx_jobs_pending_global`
`(run_at)` and `idx_jobs_running_expired_global` `(locked_until)` are added
alongside. Four small indexes on a table that holds pending work, not history.

### `demo.Reset` had to learn about the tenant root

The Postgres path is one `TRUNCATE … CASCADE` naming every table from
`pg_tables`, which took `workspaces` with it, and the re-seed's first
`server_settings` INSERT then failed with SQLSTATE 23503. `workspaces` is now
held back alongside `goose_db_version`: it is the tenant root, not visitor data,
and in demo mode it holds exactly one constant row. This was the only pre-existing
test the migration broke, on either engine.

### Config

`MULTI_TENANT`, `DATABASE_ADMIN_URL`, `CALNODE_PLATFORM_TOKEN`, and a new
`(*Config).Validate` called from `main` before the database is opened. It is
separate from `Load` because every optional knob in `Load` deliberately falls
back on a typo rather than refusing to boot (`PoolFromEnv`); these are not typos.
Five refusals, each one a combination whose only other outcome is silent:
a non-`postgres://` `DATABASE_URL`, a missing or non-Postgres
`DATABASE_ADMIN_URL`, **the two DSNs being equal** (one role means the
application role owns the tables and every policy is inert against it — the
misconfiguration hardest to notice, because everything works, including reading
other tenants' rows), and `DEMO_MODE` (D13: demo mode periodically wipes the
whole database, which here is every tenant's data).

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test ./...` **rc=0 on SQLite (28/28 packages)** and **rc=0 on PostgreSQL
(28/28)**.

### Not yet wired (later boundaries, stated so it is not mistaken for done)

`cmd/calnode/main.go` opens the platform handle with a second `db.OpenDB` and
uses it for `Migrate` + `EnableRLS`; Boundary 2 replaces that pair with
`db.OpenPair`. The other entry points (`mcp`, `reset-admin`, `rotate-key`,
`recover-key`) still open one handle from `DATABASE_URL` and are untouched —
they are single-tenant operator tools today, and B3/B6 decide what they become.

## Boundary 2 — the two handles, and the per-statement binding — DONE

`db.OpenPair(appURL, adminURL)` returns `(app, platform)`, `app.Platform()`
answers with `platform`, and `ForWorkspace(id)` returns a **value** carrying the
pool plus a string. `OpenDB` is untouched, and a handle from it binds nothing —
so single-tenant SQLite and single-tenant PostgreSQL run exactly the statements
they ran before. `TestSingleHandle_forWorkspaceIsIdentity` pins that:
`ForWorkspace` returns the identical pointer, does not even validate, `Platform()`
is the handle itself, and `Prepare` still works.

### Binding is per statement, and the release is the part that needed proving

Each `Query`/`QueryRow`/`Exec`/`Begin` on a bound handle takes a pooled
`*sql.Conn`, runs `SELECT set_config('app.workspace_id', $1, false)`, runs the
statement there, and gives the connection back: `Exec` immediately, `Row` on
`Scan`/`Err`, `Rows` on `Close`, `Tx` on `Commit`/`Rollback` (idempotently — a
`defer tx.Rollback()` after a successful `Commit` is the standard pattern here and
must not double-close). `Begin` uses `set_config(…, true)`, i.e. `SET LOCAL`, so
the connection goes back carrying nothing.

Nothing is pinned between statements, which is the point:
`TestOpenPair_handleSurvivesItsRequest` builds a handle inside a function, lets
that function return, and then reads through it from four concurrent goroutines.
Calnode's handlers are full of fire-and-forget goroutines (notify hosts, enqueue
the webhook, enqueue reminders) that outlive their request, and a handle that
pinned a session would be unusable in them.

Release is asserted by `handle.Stats().InUse` returning to **0** after every
shape, with a positive control on the same number: while a cursor or a
transaction is open it must read **1**, so the zero afterwards means something.
Proved failable by making `Row.release` and `Rows.Close` skip the release:

```
--- FAIL: TestOpenPair_workspaceHandleSeesOnlyItsOwn/QueryRow
    pair_test.go:103: pool reports InUse=2 after two QueryRow/Scan pairs; the connection was not released
--- FAIL: .../Query
    pair_test.go:114: with a cursor open the pool reports InUse=3; want 1
    pair_test.go:133: pool reports InUse=3 after Rows.Close; the connection was not released
--- FAIL: .../Exec
    pair_test.go:156: pool reports InUse=3 after Exec; the connection was not released
--- FAIL: .../Tx
    pair_test.go:167: with a transaction open the pool reports InUse=4; want 1
```

### ⛔ `Prepare` is refused on a bound handle

A `*sql.Stmt` is re-prepared on whatever connection the pool hands it, and there
is no hook to set `app.workspace_id` on that connection first — so a prepared
statement on a multi-tenant handle would run **unbound**, which is silently
empty rather than an error. Nothing in the tree prepares a statement (checked, not
assumed), and a caller that needs one should take a transaction, where the binding
is a property of the connection for the whole tx.

### `VerifyRoles` — because D4 was otherwise documentation only

Called on the application handle at boot, right after `EnableRLS`, and the boot
fails if it does. Both halves fail **silently** otherwise:

- The application role being a superuser, having `BYPASSRLS`, or **owning any
  table in the schema** means it is not constrained by the policies. Nothing
  breaks. Every request works. It can also read every other workspace. That is a
  security hole with no symptom, which is exactly the kind of thing that needs a
  boot-time refusal rather than a doc line.
- The platform role *not* bypassing means its `''` binding matches no row, so the
  worker claims nothing and the reconciler enumerates nothing. Also no error, just
  an instance that quietly does no background work. This is what makes binding
  `''` on the platform handle (D5) safe rather than a gamble.

`openTenantPair` in the tests runs it, so every case in the file is asserted
against a configuration the guard accepts.

### ⚠️ `connstore.Execer` had to change, and `destination_test.go` with it

`Execer` was `QueryRowContext(…) *sql.Row`, and it is called with a `*db.DB` in
three provider packages (`gcal`, `calendar/microsoft`, `caldav`) as well as with a
`*db.Tx`. Go has no covariant return types, so once `QueryRowContext` returns
`*db.Row` the interface has to say `*db.Row`. The consequence is that
`connstore/destination_test.go`'s deliberately-bare `sql.Open("sqlite",
":memory:")` no longer satisfies it and is now `db.OpenDB("sqlite://:memory:")`
— which is a correction, not a concession: a bare `*sql.DB` does not rebind
placeholders either, so holding it up as "Execer accepts this too" was already
describing a handle that cannot run the tree's SQL on PostgreSQL. The bespoke
fragment schema is unchanged; `OpenDB` does not migrate.

The compiler found the rest, and there were only three: `*sql.Rows` declarations
in `handler/availability.go` and `handler/livekit_recording.go`, and the
`scanStrings` helper in `internal/db/postgres_test.go`.

### Boot

`cmd/calnode/main.go` opens the pair when `MULTI_TENANT` is set and one handle
otherwise, runs migrations and `EnableRLS` on **`platform`**, and `VerifyRoles` on
`database`. Both refusals are `os.Exit(1)`.

⚠️ Still single-handle, from `DATABASE_URL`, and untouched: the `mcp`,
`reset-admin`, `rotate-key` and `recover-key` subcommands. They are single-tenant
operator tools today; B3 and B6 decide what they become.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test ./...` **rc=0 on SQLite (28/28)** and **rc=0 on PostgreSQL (28/28)**.
The 11 new cases all run rather than skip on the PostgreSQL lane, and the
tenant-binding ones skip with a stated reason on SQLite.

## Boundary 3 — handler scoping and tenant resolution — PART ONE of two

⚠️ **B3 is not finished.** This commit lands the foundation: the `shared` split,
the `Workspace` type, the two resolvers, `Scoped`, `Platform`, the refusal
mapping, `AuthUser.WorkspaceID`, and the credential lookups moved onto the
platform handle. What remains is the part that touches `internal/server/server.go`:
**163 `mux.HandleFunc` registrations** have to be rewritten through
`h.Scoped(resolve, (*Handler).Method)`, and with them the route-classification
test, the five end-to-end request tests, and the `/v1/bookings/{id}` tenant check.
Split because the registration rewrite is a single mechanical edit across 163
lines that has to land with its own gate, not because either half is optional.

### The split, and why the mutexes had to move

`Handler` was 30 fields including **six `sync.RWMutex`**. A tenant-scoped request
needs a handler whose `db` is bound to its workspace, and the only way to give
**314 methods** a bound `h.db` without editing all of them is to hand them a
receiver that differs in that one field — so `Handler` is copied per request. A
struct containing a mutex cannot be copied (`go vet` copylocks, and rightly:
copying a mutex copies its state).

So: `shared` holds everything the process has one of — logger, mailer, the
hot-swappable integration clients and their mutexes, the configured hosts, demo
bookkeeping — and `Handler` is `{ *shared; db; ws; bookingSvc; webhookSvc }`.
Embedding as `*shared` rather than naming it is what keeps `h.logger`,
`h.livekitMu`, `h.mailer = m` and the rest compiling **untouched across all 61
files**. Measured: the split needed zero edits to any handler method, and
`go vet ./...` is clean.

`bookingSvc` and `webhookSvc` stay on `Handler` because each wraps a `*db.DB`;
`forWorkspace` rebuilds them from the scoped handle through new
`(*Service).ForDB` methods on `internal/booking` and `internal/webhook`. Both are
structs over a pool, so a rebuild is one allocation and pins nothing.

### ⛔ Credential lookups had to move to the platform handle

`RequireAuth`'s two reads — `api_keys` and `sessions` — ran on `h.db`. On a
multi-tenant instance the application handle is bound to the workspace **of the
request**, and the workspace of the request is what those reads exist to
DISCOVER. Bound, they find nothing, and a perfectly good API key is reported
`invalid API key`. Both now use `h.platformDB()` (`h.db.Platform()`, which is the
same handle in single-tenant mode) and both select `u.workspace_id` into the new
`AuthUser.WorkspaceID`. This is the reason D9 keeps `api_keys.key_hash` and
`sessions.id` globally unique.

### Four refusals, and none of them is "carry on unscoped"

`errUnknownHost` → **404** (HTML: the host-resolved surfaces are pages a person is
looking at). `errWorkspaceSuspended` → **503** + `Retry-After`.
`errWorkspaceMismatch` → **403 `{"error":"workspace mismatch"}`**, which is the
body D10 specifies and a test pins. `errNoWorkspace` → **500**.

⛔ There is deliberately **no fallback to the default workspace** on an
unrecognised host. Falling back would serve one tenant's booking page on any
domain pointed at the instance. Migration 00060 seeds `workspaces.public_host`
empty for `default` for the same reason: no HTTP request carries an empty Host, so
the default workspace is unreachable by host resolution. Both are asserted.

Proved failable by making `HostWorkspace` return `DefaultWorkspace` on a failed
lookup — the exact bug:

```
--- FAIL: TestHostWorkspace_multiTenant/unknown_host_does_not_fall_back_to_default
    workspace_internal_test.go:102: resolved &{ID:default Slug:default PublicHost: Region: Status:active}; want an error
--- FAIL: TestHostWorkspace_multiTenant/the_default_workspace_is_unreachable_by_host
    workspace_internal_test.go:117: an empty Host resolved; err = <nil>
--- FAIL: TestScoped_refusalsNeverReachTheMethod/unknown_host_is_404
    workspace_internal_test.go:230: status = 200; want 404 (body "{\"workspace\":\"default\"}\n")
    workspace_internal_test.go:233: the method ran 1 times; wantReach=false
```

### `Scoped` takes a method EXPRESSION, and the compiler enforces it

`h.Scoped(HostWorkspace, (*Handler).BookPage)`, not `h.Scoped(…, h.BookPage)`. A
bound method value would capture the **unscoped** receiver, which is exactly the
bug the wrapper exists to prevent, and it would compile silently. A method
expression cannot: its first parameter is the receiver, which `Scoped` supplies
from `forWorkspace`.

`Platform(method)` is the sibling for routes that belong to no workspace — the
identity host's OAuth endpoints, `/.well-known/*`, `/healthz`, `/readyz`,
`/version`, `/metrics`, the platform API. It exists so "unscoped, on purpose" is
something the registration says out loud rather than by omission, which is what
the route-classification test in part two will hold.

### `publicURL()` is per workspace

In multi-tenant mode it returns `https://<ws.public_host>` and `PUBLIC_BASE_URL`
is ignored entirely (D11); `BASE_URL` stays the identity host of the process.
Single-tenant is unchanged: `PUBLIC_BASE_URL` if set, else `BASE_URL`. Four cases
pinned.

### Scope of the tests here, stated so it is not overread

`workspace_internal_test.go` runs against ONE handle with `multiTenant` set. That
is enough for everything it asserts, because resolution reads `workspaces` through
`platformDB()` and on a single handle that is the handle itself. It does **not**
re-prove the row-level-security binding — that needs a NOBYPASSRLS role and is
already proven in `internal/db` (`rls_proof_test.go`, `pair_test.go`). The
end-to-end request tests in part two are where a real pair gets exercised through
HTTP.

### TODO(integration) — blocked on `feat/platform-hooks`

Neither is implemented, and neither is faked:

- **D11, the OAuth login hand-off.** After a Google/Microsoft callback on the
  identity host, the callback cannot set a cookie for the workspace's public host,
  so it must mint an SSO token for the workspace carried in the OAuth `state` and
  redirect to `https://<public_host>/v1/auth/sso?token=…`. That endpoint arrives
  with `feat/platform-hooks`, which is **not in this clone**. Expected shape at
  integration: the branch's SSO issue/verify pair, extended per the packet with a
  required `wid` claim and an `aud` equal to the workspace's public host.
  `finishOAuthLogin` in `internal/handler/auth_oauth.go` is where the redirect
  replaces the cookie set.
- **D14, rate-limit keys.** They should become `(workspace_id, client_ip)` using
  that branch's `TRUSTED_PROXY_CIDRS`-aware client-IP helper. Expected shape:
  `handler.clientIP(r) string`. Today's limiters key on the TCP remote address
  (`ARCHITECTURE` §16 says so deliberately), and prefixing the workspace without
  the helper would key a whole tenant behind one proxy as a single client.
- **The platform `/metrics` endpoint**, also on that branch, reads the `jobs`
  table. At integration it **must** read through `Platform()`: `jobs` is a tenant
  table and an application-handle read would report one workspace's queue, or on
  the unbound handle, zero.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test ./...` **rc=0 on SQLite (28/28)** and **rc=0 on PostgreSQL (28/28)**.
The 21 new resolver cases all run on both lanes.

## Boundary 3, part two — the registrations, and the gate that keeps them honest

⚠️ **The end-to-end request tests through a real `OpenPair` are NOT in this
commit.** Everything else part two owed is: the 160 rewritten registrations, the
MCP per-workspace scoping, the classification gate, and `/v1/bookings/{id}`. The
e2e file is the next thing, before B4.

### 169 routes, every one of them classified

```
169 routes: 31 host-scoped, 106 credential-scoped, 24 platform, 8 allowlisted
```

`type H = handler.Handler` in `internal/server` so a registration reads
`(*H).ListBookings`. The brevity is incidental; the **method expression** is the
point. `Scoped` takes `func(*Handler, http.ResponseWriter, *http.Request)`, so
passing `h.ListBookings` — a bound method value on the *unscoped* handler, which
is precisely the bug `Scoped` exists to prevent — **does not compile**.

Credential-scoped routes are `h.RequireAuth(h.Scoped(handler.CredentialWorkspace,
(*H).Method))`, in that order: `CredentialWorkspace` reads the caller out of the
request context, so the auth middleware has to have run first.
`TestCredentialScopedRoutesAuthenticateFirst` checks both the presence and the
nesting order across all 106.

### The gate, and why it reads the file

`internal/server/routes_classified_test.go` scans `server.go` and fails on a
registration that is neither `Scoped(HostWorkspace, …)`,
`Scoped(CredentialWorkspace, …)`, `Platform(…)`, nor on an 8-entry allowlist whose
every member is a handler that is **not a `*handler.Handler` method at all** (two
empty CORS preflights, the embedded favicon/SPA/redirects, the MCP mount).

⛔ A source scan rather than a runtime walk because `http.ServeMux` exposes no way
to enumerate its patterns, and the thing to catch is a registration written
without a wrapper — a property of the text. Three guards against a vacuous pass: a
floor of 150 registrations, a floor of 3 per bucket (so a refactor that classified
everything one way fails), and a check that every allowlist entry is still a live
route.

`TestPlatformRoutesAreTheIdentityHostSet` pins the 24-member platform set exactly
against D11, with a one-line reason per group. A route joining or leaving the
identity host is a decision about which host serves it, and this makes it
impossible to make silently.

⚠️ **The rewrite missed one route and the gate caught it.** `POST
/v1/livekit/egress-webhook` carries a trailing `// legacy alias` comment, so the
scripted edit's line pattern did not match it and it stayed unwrapped. That is the
gate doing exactly the job it was written for, on its first run.

### ⛔ The MCP tools close over their handler, so one cached server was wrong

`/mcp` was `mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return
mcpSrv }, nil)` over a single instance built at boot. The eight tools capture `h`,
so **every workspace's tool calls would have run on whichever handler built that
instance**. The mount is on the identity host and carries no tenant Host, so the
credential is the only source (D10).

`MCPCallerMiddleware` now also reads `users.workspace_id` — through
`platformDB()`, same reasoning as `RequireAuth` — into `mcpCaller.WorkspaceID`,
and the factory becomes `h.MCPServerForRequest`, which returns a server per
workspace from a cache on `shared` (`map[string]*mcp.Server` behind an RWMutex).
One entry keyed `""` in single-tenant mode, so that path is the old behaviour with
a map in front of it. Building one allocates eight tool registrations and their
JSON schemas, which is not something to do per request.

### `/v1/bookings/{id}` — the tenant check is the scoping, and the missing auth is still missing

`GET /v1/bookings/{id}` is now `h.Scoped(handler.HostWorkspace, (*H).GetBooking)`,
so the handle it runs on is bound to the workspace of the request Host and a
booking id belonging to another workspace is simply not visible — the 404 comes
from the row not existing, enforced by the policy rather than by a predicate the
handler has to remember.

⚠️ **It still has no auth middleware at all**, alone among `/v1/bookings/{id}/*`
(`server.go`, and every sibling is wrapped in `h.RequireAuth`). Reported in the
first packet turn and deliberately not changed: adding auth to a route the booking
page may depend on is a product decision, not a tenancy one. What multi-tenancy
changes is the blast radius — it is now bounded to one workspace instead of the
whole instance.

✅ **CLOSED by F4** — the product decision was taken and the route is now
`h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetBooking))`. The
premise the paragraph above deferred on ("a route the booking page may depend on")
was checked rather than assumed and is false: nothing public reads it. See the F4
section at the end of this file.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.

## Boundary 3, part three — the end-to-end tenancy proof

Two workspaces on distinct public hosts, one process, the **real mux** via
`httptest`, and a real `db.OpenPair` whose application handle is a `NOBYPASSRLS`
role that owns nothing. `internal/server/tenancy_e2e_test.go`, and the harness it
uses is now reusable: `dbtest.RequireTenantPair(t)`.

Eight assertions, each in both directions:

| surface | A sees its own | A cannot reach B's |
|---|---|---|
| `GET /v1/event-types` (API key) | ✓ | ✓ |
| `GET /v1/bookings?scope=all` (API key) | ✓ | ✓ |
| `GET /v1/event-types/{slug}/slots` (public) | ✓ | ✓ |
| `GET /book/{slug}` (public page) | — | ✓ |
| `GET /v1/bookings/{id}` | 200 | **404** |
| `POST /v1/bookings` (public write) | lands in `acme` | B's count unmoved |
| MCP `list_bookings` over the HTTP transport | ✓ | ✓ (both directions) |
| A's API key on B's host | — | **403 `workspace mismatch`** |

⚠️ **The `GET /v1/bookings/{id}` row is as written at D-time and its left column has
since changed meaning.** That 200 was an ANONYMOUS request on A's public host — the
route carried no auth middleware. Since F4 it is 401 without a credential and 200 with
A's own key; the 404 in the right column is unaffected, because it was always the
scoping rather than the auth that produced it.

The MCP call is a real `tools/call` through `mcp.StreamableClientTransport`
against an `httptest.Server`, with the `cno_` key as a bearer token. It is
asserted in **both** directions — A then B — because that is what catches a
cached server built for whichever workspace called first. The mismatch test also
checks the same key on the **identity** host still works: that host names no
workspace, so there is nothing to disagree with, and it is where API and MCP
callers legitimately arrive.

### ⛔ `VerifyMCPBearer` was reading credentials on the tenant handle

Found by the MCP test failing `connect to /mcp: Unauthorized`. All four of its
reads and writes — `oauth_access_tokens`, `api_keys`, and the two `last_used_at`
updates — ran on `h.db`. `/mcp` is on the identity host, so **no workspace is
bound at all**, and every valid bearer token would have been reported
Unauthorized on a multi-tenant instance. Now on `platformDB()`, which is the same
reasoning as `RequireAuth` and `MCPCallerMiddleware`, and the third place in the
tree where a global-unique credential lookup had to move. That is the pattern:
**any read whose purpose is to discover the tenant cannot be bound to it.**

### Two controls, and the second is the one that matters

**Control 1 — the binding stubbed off** (`DB.binds()` forced false). Every test
fails, but at the FIXTURE, because the seeding writes are refused:

```
tenancy_e2e_test.go:192: create user for acme: ERROR: new row violates row-level security policy for table "users" (SQLSTATE 42501)
```

That proves the binding is load-bearing, and nothing more — the read assertions
never run.

**Control 2 — the application handle given the bypassing owner role**, with
`VerifyRoles`' refusal disabled. This is the "a superuser DSN proves nothing"
scenario made concrete, and it is what shows the assertions themselves working:

```
--- FAIL: TestTenancy_readSurfaces/bookings_list
    A's booking list contains B's "globex-booking"
--- FAIL: TestTenancy_readSurfaces/slots_for_B's_event_type_on_A's_host
    B's event type produced slots on A's host: {…"host_ids":["globex-user"]…}
--- FAIL: TestTenancy_readSurfaces/public_event_type_page_for_B's_slug_on_A's_host
    B's booking page rendered on A's host: status 200
--- FAIL: TestTenancy_bookingByIDIsNotFoundAcrossWorkspaces
    B's booking id on A's host: status = 200, want 404: {"id":"globex-booking",…}
--- FAIL: TestTenancy_mcpToolCallOverHTTP
    A's MCP list_bookings returned B's booking "globex-booking"
    B's MCP list_bookings returned A's booking "acme-booking"
```

⚠️ Worth stating plainly: **that is what this code does without the NOBYPASSRLS
role.** It is why `RequireTenantPair` skips loudly rather than falling back, and
why `VerifyRoles` refuses the boot.

### ⚠️ A trap the fixture paid for

`server.New`'s `drain` blocks until the worker finishes its poll cycle, and the
worker only stops when its context is done. Passing `context.Background()` gives a
**green test body and a hang in `Worker.Wait`** at cleanup, which reads as a
deadlock in the code under test. `main.go` cancels then drains; the fixture now
does the same.

---

## Carried into later boundaries

⛔ **Vendor webhooks (B6).** `POST /v1/livekit/webhook`, its
`/v1/livekit/egress-webhook` alias, and `POST /v1/stripe/webhook` are classified
`Platform` today, which gets them far enough to find their row and no further.
They arrive at whatever host the vendor was given and carry no tenant Host, so
each must **resolve its workspace from the row it names** — the recording's `room`,
the booking's `stripe_session_id` — on the platform handle, and then hand off to
`h.forWorkspace(ws)` for **all** processing. A test per webhook that a room or
session belonging to B, handled on the platform path, writes only into B.

⛔ **Every INSERT through a `Platform`-wrapped route or the platform API must name
`workspace_id` explicitly.** The platform handle binds `''`, and the column default
is `COALESCE(current_setting('app.workspace_id', true), 'default')` — so a write
that omits the column lands in **`default`**, silently, rather than failing. The
e2e fixture already has to do this for `workspaces` and `server_settings`. B6 owes
a test on the platform API's create path that the row carries the **requested**
workspace id, not `default`.

⚠️ **The platform `/metrics` endpoint** (on `feat/platform-hooks`) reads the `jobs`
table, which is a tenant table. At integration it must read through `Platform()`:
on the application handle it would report one workspace's queue, and on the unbound
handle, zero.

## The identity-resolution sweep

Every read whose PURPOSE is to resolve an identity or a tenant **before one is
bound**. Three had already been fixed one at a time (`RequireAuth`,
`MCPCallerMiddleware`, `VerifyMCPBearer`); this is the enumeration, so the fourth
is not found by another failing test.

The rule the table applies: **a read whose job is to discover the tenant cannot be
bound to it.** The corollary, which is what the (c) rows are: **a write on a
Platform-wrapped route must NAME `workspace_id`**, because the platform handle
binds `''` and the column default is
`COALESCE(current_setting('app.workspace_id', true), 'default')` — so an omitted
column does not fail, it lands the row in the default workspace.

| # | site | credential / lookup | route class | verdict |
|---|---|---|---|---|
| 1 | `auth.go:85,97` | `api_keys.key_hash` → user | any (middleware) | **(a)** `platformDB()` |
| 2 | `auth.go:113` | `sessions.id` → user | any (middleware) | **(a)** `platformDB()` |
| 3 | `mcp_oauth.go:86` | `users` role + workspace | `/mcp` | **(a)** `platformDB()` |
| 4 | `mcp_oauth.go:133,141` | `oauth_access_tokens.token_hash` | `/mcp` | **(a)** `platformDB()` |
| 5 | `mcp_oauth.go:148,151` | `api_keys.key_hash` (bearer) | `/mcp` | **(a)** `platformDB()` |
| 6 | `mcp_oauth_authorize.go:409` | `sessions.id` → consenting user | Platform | **(a)** `h.db` **is** the platform handle inside `Platform()` |
| 7 | `mcp_oauth_authorize.go:216,223` | `oauth_auth_codes.code_hash` | Platform | **(a)** same |
| 8 | `mcp_oauth_authorize.go:250` | `oauth_access_tokens.refresh_hash` | Platform | **(a)** same |
| 9 | `mcp_oauth.go:284`, `:220`, `mcp_oauth_authorize.go:344` | `oauth_clients.client_id` | Platform | **(a)** global table, no `workspace_id`, exempt from RLS by design (D2) |
| 10 | `magic_link.go:68,100,115` | `magic_link_tokens.token_hash` | HostWorkspace | **(b)** the link was mailed from that workspace's own public host |
| 11 | `invites.go:228,278` | `invite_tokens.token_hash` | HostWorkspace | **(b)** same |
| 12 | `booking/service.go:386` | `booking_manage_tokens.token_hash` | HostWorkspace | **(b)** and this is how D10's "the token's booking belongs to that workspace" is enforced — by the policy, not a predicate |
| 13 | `idempotency.go:37,51,63,73` | `idempotency_keys` PK | HostWorkspace | **(b)** PK is now `(workspace_id, key)`; two tenants may reuse a key |
| 14 | `auth_google.go:105` | `sessions.id` (logout) | HostWorkspace | **(b)** a session of B on A's host matches nothing, which is the right answer |
| 15 | `apikey.go`, `invites.go` admin, `mcp_oauth_admin.go` | by `user_id` / `id` | CredentialWorkspace | **(b)** bound, and that is what scopes them |
| 16 | **`mcp_oauth_authorize.go:163`** | `INSERT oauth_auth_codes` | Platform | ⛔ **(c)** named no `workspace_id` |
| 17 | **`mcp_oauth_authorize.go:279`** | `INSERT oauth_access_tokens` | Platform | ⛔ **(c)** named no `workspace_id` |
| 18 | **`setup.go:75,84`** | `INSERT users` + `api_keys` | Platform | ⛔ **(c)** would seat the first owner and a live key in `default` |

### (c) 16 and 17 — the OAuth grant landed in the wrong tenant

⛔ **And it still worked, which is why nothing caught it.** `VerifyMCPBearer` reads
on the platform handle, which bypasses the policies, so an agent could complete
the Connect flow and call tools normally. What broke was **ownership**: the
workspace's Connected-apps page (`GET /v1/oauth/connections`, a
CredentialWorkspace route on the bound handle) could neither list nor revoke the
grant, and deleting the workspace would not have cascaded it. A revocation UI that
silently cannot revoke is worse than one that is absent.

Fixed by naming `workspace_id`, resolved through a new
`(*Handler).workspaceOfUser` on the platform handle.

`TestSweep_oauthGrantLandsInTheOwnersWorkspace` drives the real flow — `POST
/oauth/register`, the consent page and its CSRF cookie, the decision POST, then
`POST /oauth/token` with PKCE S256 — and asserts the issued row's `workspace_id`
through the platform handle, plus the observable consequence: A's Connected-apps
page lists the grant and B's does not.

### (c) 18 — `POST /v1/setup` in multi-tenant mode

Platform-wrapped, and it creates the first user **plus a live API key**. Both would
land in `default`: a working credential in a tenant nobody owns and no host
reaches. It now 404s when `MULTI_TENANT` is set — 404 rather than 403 because on a
multi-tenant instance the endpoint does not exist; workspaces and their owners come
from the platform API.

⚠️ **Honest scope: this is reachable only before any user exists in any
workspace.** Setup's own "workspace already configured" guard (409) fires once
there is one, which the negative control below shows. So it is a narrow window —
a freshly provisioned multi-tenant database between migration and the first
platform-API call — and defence in depth after that.

### Controls

Positive: all three sweep tests pass on the PostgreSQL lane.

Negative, both fixes reverted:

```
--- FAIL: TestSweep_oauthGrantLandsInTheOwnersWorkspace
    tenancy_sweep_test.go:81: no code in the redirect
    "http://127.0.0.1:9999/cb?error=server_error&error_description=could+not+issue+code&…"
--- FAIL: TestSweep_setupIsRefusedInMultiTenantMode
    tenancy_sweep_test.go:149: POST /v1/setup: status = 409, want 404: {"error":"workspace already configured"}
```

⚠️ Both controls fail, but neither fails at the assertion I predicted, and that is
worth recording rather than smoothing over. The OAuth one fails **earlier**: with
`workspace_id` unnamed the auth-code write does not complete at all, surfacing as
`could not issue code`. I did not have time to establish whether that is the policy
refusing the write or an artifact of the reverted statement, so **the mechanism is
unconfirmed** — what is confirmed is that the test distinguishes fixed from
unfixed. The setup one fails on the status code only, because the fixture has
users and the 409 guard fires first; it does **not** demonstrate a row being
written, for the reason in the scope note above.

### ⚠️ One assertion I had to correct, and it is a trap worth knowing

The manage-page test first asserted `status != 200` for B's token on A's host. It
failed: `ManagePage` renders **200 with `TokenInvalid: true`** for an unknown
token, by design — it is a booker-facing page, not an API. The isolation was
working; the assertion was wrong. It now asserts on content (none of B's booking
id, host name or attendee address appears). A status-code assertion against a
surface that renders its own errors proves nothing.

## Boundary 4 — per-tenant runtime state — DONE (with one documented gap)

Five Set/get singleton pairs on `shared`, each behind its own `sync.RWMutex`,
became five `*tenantCache[T]`: **mailer, LLM, Zoom, Stripe, LiveKit**. Each value
is built lazily from **that workspace's** `server_settings` row through the
**bound** handle, and replaced when that workspace saves. The settings-save
handlers needed **no edit**: they already call `SetX(...)` after writing, and
`SetX` now writes at `h.cacheKey()`, so a save on a credential-scoped handler
primes its own key and nobody else's.

`internal/handler/tenantcache.go` is 90 lines and generic. Three things in it are
deliberate:

- **The key is `""` in single-tenant mode, not `"default"`.** One entry, built on
  first ask, replaced by `SetX` exactly as before — so the map cannot grow past
  one and nothing that calls `ForWorkspace` can make it.
- **`present` is separate from `entries`, because nil is a MEANINGFUL value.** A
  workspace with no Stripe credentials caches a nil `*stripe.Client`; if absence
  and nil were the same thing, every request would rebuild it. A test pins that a
  read after `SetStripe(nil)` does not grow the cache.
- **The builder runs OUTSIDE the lock**, because every builder reads
  `server_settings` — holding the write lock across a round trip would serialise
  every tenant behind the slowest. The cost is that two concurrent
  first-requests may both build; the first store wins and the loser is discarded.
  Clients are stateless value wrappers, so that is waste, not incorrectness, and
  `TestTenantCache_getBuildsOnceUnderContention` asserts all 32 callers get **one
  value** (measured: 32 gets, 1 builder).

### `mailer.From()` is new, and it is what makes the assertion possible

The sender address is the only per-workspace value a built mailer exposes from
outside, so `*SMTP`, `*Resend` and `*Live` (delegating) gained a `From()`.
Without it the test could only assert that two mailers are different pointers,
which a shared cache would also satisfy.

### The negative control lives in the tree

`TestTenantCache_keyIsWhatSeparatesThem` builds the same two mailers twice: once
under the real keys, once under a key stubbed to `""`. Both halves are asserted,
so the test fails if the real keys collide **and** if the stubbed key does not:

```
tenantcache_test.go:143: key stubbed to "": B would send as "bookings@acme.example" instead of "hello@globex.example"
```

That is the bug in one line — B's booking confirmations going out as A.

### ⛔ A single handle cannot tell two workspaces apart, and the first draft of these tests did not

`db.ForWorkspace` is the **identity function** on a handle that did not come from
`OpenPair`, so every "scoped" copy read the same rows and the first version of
this file compared a value with itself — it failed as
`mailer *mailer.Noop exposes no From address`, which reads like a builder bug
rather than a harness one. The four workspace-distinguishing cases now take a real
pair through `dbtest.RequireTenantPair` and skip loudly on SQLite; the two
pure-cache cases (`singleTenantKeepsOneEntry`, `getBuildsOnceUnderContention`) do
not need one and run on both lanes.

### ⚠️ The gap: the calendar Service is still a process-wide singleton

`getCal` is unchanged. This is a scope decision, stated rather than hidden:

- The `calendar.Service` is a registry of **providers** keyed by the instance's
  Google/Microsoft OAuth **app** credentials, and D7 keeps `googleAuth` /
  `microsoftAuth` platform-level. So the thing being cached is not obviously
  per-tenant in the way an SMTP transport is.
- Each provider (`gcal.New`, `microsoft.New`, `caldav.New`) **captures a `*db.DB`
  at construction**. Making the Service per-tenant needs a `ForDB` on the Service
  and on all three providers, plus the config plumbing to rebuild them per
  workspace — more than this boundary.
- **What that means today, measured against the design rather than guessed:** on a
  multi-tenant instance the captured handle is the **unbound** application handle,
  which under the policies matches no row. So calendar reads return nothing and
  the integration is **inert**, not cross-tenant. That is the safe failure, but it
  is a failure: a multi-tenant deployment has no working calendar sync until this
  is finished. It belongs with B5, which touches the reconciler anyway.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
All seven cases pass **under `-race`** on the PostgreSQL lane:

```
--- PASS: TestTenantCache_mailerIsPerWorkspace (1.11s)
--- PASS: TestTenantCache_keyIsWhatSeparatesThem (1.14s)
--- PASS: TestTenantCache_saveInvalidatesOnlyThatWorkspace (1.14s)
--- PASS: TestTenantCache_singleTenantKeepsOneEntry (0.78s)
--- PASS: TestTenantCache_concurrentWorkspaces (1.33s)
--- PASS: TestTenantCache_getBuildsOnceUnderContention (0.00s)
--- PASS: TestTenantCache_llmAndStripeBuildFromTheirOwnRow (1.11s)
```

## Boundary 5, part one — the calendar gap closed, and boot priming gated

⚠️ **B5 deliverables 2, 3 and 5 are NOT in this commit** — the worker's claim loop,
the reconciler, and the CLI subcommands. This lands 1 and 4, which separate
cleanly from the rest and close the gap B4 left open.

### The calendar gap (deliverable 1) — `ForDB` through the Provider interface

B4 left `getCal` as a process-wide singleton and called it "inert rather than
cross-tenant". Inert is not shippable: calendar connections are the product.

`calendar.Provider` gains **`ForDB(handle *db.DB) Provider`**, implemented by all
three backends as a shallow copy with the handle replaced. No import cycle: all
three already import `internal/calendar` for `CalendarInfo`, and `calendar` imports
none of them. `calendar.Service.ForDB` rebuilds its provider map through each
provider's own `ForDB` and carries `primary` over, so which backend claims a new
connection does not change per workspace.

⛔ **The OAuth APP configuration stays platform-level, per D7** — client id,
secret, redirect, encryption key identify the *Calnode instance* to Google and
Microsoft, not the tenant. So `ForDB` is deliberately a shallow copy: same config,
different handle. `shared` now holds `calBase` (the registry boot installs) and
`calCache` (the per-workspace bound copies), and **`getCal` is the only reader** —
returning `calBase` directly is the bug this splits apart.

Single-tenant returns `calBase` itself rather than a rebound copy: one workspace,
one handle, so rebinding would allocate a second Service and every provider in it
for no behaviour change. `SetCalendar` invalidates the cache, so `SetCalendar(nil)`
is visible immediately instead of being shadowed.

**Why CalDAV carries the test:** it needs no instance-level OAuth app, so the test
is about the handle and not about credentials. Two workspaces, one
`calendar_connections` row each, then per workspace: `Connected` and
`HasDestination` are true for its own user and **false for the other's**, with a
positive control through the platform handle that both rows exist (so a `false` is
the policy, not an empty table). `Disconnect` is the write half — A asking to
disconnect B's user must leave B's row intact, whether or not it errors.

Negative control, same shape as the mailer's and in the tree:

```
calendar_tenancy_test.go:229: key stubbed to "": B's service sees A's connection = true, its own = false
```

That is one tenant's free/busy deciding another's availability — the failure mode
that matters here, and it is worse than the mailer's, because it is silent.

### Boot priming (deliverable 4) — made single-tenant-only, not removed

Every `LoadXSettingsFromDB(db, encKey)` in `server.New` reads `server_settings`
through the **unbound** application handle. In multi-tenant mode that matches no
row, so priming installed nothing and logged "not configured" for an instance whose
tenants are all configured — a misleading boot log and six pointless round trips.

`primeFromDB := !cfg.MultiTenant` gates all six through a small
`loadIfSingleTenant` generic that returns `(nil, nil)` when priming is off, which
every call site already treats as "not configured in the database". Multi-tenant
boot now logs once, plainly:

```
multi-tenant mode: per-workspace settings are built on first use, not primed at boot
```

**Gated rather than removed**, and the reason is specific: it is the only path that
seeds env-var SMTP into the database on first boot (`seedSMTPToDB`), and on a
single-tenant instance it is what makes the first request fast instead of lazy.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean. The four calendar
cases pass **under `-race`**:

```
--- PASS: TestCalendar_providersSeeOnlyTheirWorkspace (1.27s)
    --- PASS: TestCalendar_providersSeeOnlyTheirWorkspace/acme
    --- PASS: TestCalendar_providersSeeOnlyTheirWorkspace/globex
--- PASS: TestCalendar_disconnectCannotReachAnotherWorkspace (1.39s)
--- PASS: TestCalendar_keyIsWhatSeparatesThem (1.35s)
--- PASS: TestCalendar_singleTenantReusesTheRegistry (0.89s)
```

### Still owed by B5

2. **Worker.** The claim loop must run on the platform handle across tenants and
   process each job on `ForWorkspace(job.workspace_id)` with the per-tenant caches.
   Today it holds one handle from `worker.New(db, …)` and one `*mailer.Live`, so on
   a multi-tenant instance it claims nothing (`jobs` is a tenant table, the handle
   is unbound). Two-workspace claim-loop test owed.
3. **Reconciler.** `StartCalendarReconciler` iterates state on one handle; it needs
   to enumerate `workspaces` on the platform handle and then run each pass bound.
   ⚠️ It is started from `server.New` only when `calSvc.Any()`, which after
   deliverable 4 is still true (providers are registered from env/CalDAV, not from
   `server_settings`) — so it does run, and it currently runs unbound.
4. **CLI.** `mcp` (stdio), `reset-admin`, `rotate-key`, `recover-key` still open one
   handle from `DATABASE_URL`. `rotate-key` and `recover-key` are genuinely
   platform-wide (`crypto_keystore` is exempt, one DEK per process, D3);
   `reset-admin` and `mcp` stdio are per-workspace and have no way to say which.

## Boundary 5, part two — the worker claims across tenants and works inside one

⚠️ **Deliverables 3 (reconciler) and 5 (CLI) are NOT in this commit.**

### The claim loop and the work are on opposite sides of the boundary

That is the whole shape of it, and getting either side wrong is silent:

| | handle | why |
|---|---|---|
| claim, reaper, housekeeping sweeps | **platform** | one queue ordered by `run_at` across every tenant; a bound handle would serve one workspace and starve the rest |
| processing a claimed job | **`ForWorkspace(job.workspace_id)`** | every read is one tenant's; the platform handle would read across them |

⛔ **The claim query carries no workspace predicate**, deliberately, and now selects
`workspace_id` alongside. This is what migration 00060's workspace-free
`idx_jobs_pending_global` exists for — a workspace-leading index cannot serve an
ordered global scan.

`worker.TenantDeps{DB, Mailer, Webhook}` is what a job is processed with, supplied
by `WithTenantResolver`. Unset — single-tenant — `depsFor` returns the Worker's own
handle and mailer, so nothing changes. `sendReminder` and `deliverWebhook` take
`deps` and use `deps.DB` / `deps.Mailer` / `deps.Webhook` rather than the Worker's
fields.

### Custom handlers took a new signature, not a context value

`RegisterHandler` is now `func(ctx, workspaceID, payload) error`. The packet offered
context smuggling or a signature change and asked for the smaller: the signature is
**two call sites** in `server.New` and two method heads in `notetaker.go`, versus a
context key that every future handler author has to know exists. Each notetaker job
starts with `h = h.workspaceForJob(workspaceID)` — **before any row is read**,
because the receiver the worker calls it on is unbound.

`handler.TenantRuntime(workspaceID)` returns `(*db.DB, mailer.Mailer,
*webhook.Service)` and `server.New` adapts it into `worker.TenantDeps`. The handler
deliberately does **not** import the worker package: the worker owns the queue, the
handler owns the per-tenant state, and one closure joins them.

### The tests

`TestWorker_processesEachJobInItsOwnWorkspace` — one reminder each, both due, one
`Poll`. Both jobs reach `done` (the "one poll delivers both" half), each was sent
with its **own** workspace's mailer, to its own attendee, and a cross-check asserts
neither mailer saw the other's.

`TestWorker_webhookSignsWithItsOwnWorkspacesSecret` — each workspace registers a
webhook at its own path on one `httptest` server, so the signature that arrives can
be attributed. The two signatures must differ, and the two issued secrets must
differ. A shared `webhook.Service` would sign one tenant's payloads with another's
secret and every subscriber's verification would start failing — which is the kind
of break that gets reported as "your webhooks are broken", not as a tenancy bug.

`TestWorker_customHandlersReceiveTheWorkspace` — the signature change, asserted by
job rather than by inspection.

### The negative control is the old shape

```
tenancy_test.go:340: negative control: a worker on the application handle claimed 0 of 1 due jobs,
with no error — reminders and webhook deliveries would silently never fire
```

It runs the same due job twice: once through the platform handle (1 reminder sent),
then requeued and run through the **application** handle (0 sent, job still
`pending`). `jobs` is a tenant table and the application handle is unbound, so it
matches no row — the loop runs, finds nothing, logs nothing. That is the third
failure mode, distinct from leakage and from starvation, and it is what this
deliverable removed.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean. The whole worker
package passes **under `-race`**, pre-existing cases included:

```
--- PASS: TestWorker_processesEachJobInItsOwnWorkspace (1.75s)
--- PASS: TestWorker_webhookSignsWithItsOwnWorkspacesSecret (1.76s)
--- PASS: TestWorker_theOldShapeClaimsNothing (1.69s)
--- PASS: TestWorker_customHandlersReceiveTheWorkspace (1.93s)
--- PASS: TestWorker_sendsReminderForConfirmedBooking (1.39s)
--- PASS: TestWorker_deliversSuccessfully (1.25s)
--- PASS: TestWorker_purgesExpiredManageTokens (1.36s)
    … and the rest of the package
```

### ⚠️ Housekeeping sweeps stay cross-tenant, and that is correct

The `Poll` preamble deletes expired manage tokens, sessions, magic links,
idempotency keys and auth codes, purges old webhook deliveries, and releases stale
Stripe payment holds — all on the platform handle, all without a workspace
predicate. That is deliberate: they are retention rules, not tenant logic, and
running them per workspace would mean N times the statements to express one policy.
The Stripe hold backstop is the one worth naming, because it **writes**
(`bookings.status = 'cancelled'`) across tenants — it is keyed on
`payment_status = 'pending' AND created_at < cutoff`, which is a property of the
row and not of the tenant, so a global sweep is the right shape. If that ever needs
to differ per workspace it has to move.

## Boundary 5, part three — the reconciler sweeps per workspace

`reconcileCalendar` is now a dispatcher: `reconcileTargets()` enumerates active
workspaces on the **platform** handle, and each `reconcileCalendarPass()` runs on a
handler bound to one of them. `activeWorkspaceIDs` lives beside the other workspace
reads and filters `status = 'active'` **there** rather than at each call site, so a
new periodic loop cannot forget.

⛔ Suspended workspaces are skipped, and that is a decision rather than an
optimisation: a suspended tenant answers 503 on its own public and admin surfaces
(D12), so healing its calendar in the background would be doing work on its behalf
that it can neither see nor stop.

Single-tenant short-circuits before the query — one target, the handler itself, no
round trip per sweep.

### The tests, and what each half proves

`TestReconciler_enumeratesActiveWorkspacesOnly` — three workspaces, one suspended.
It asserts the target set **and** that each target's handle is bound to the same
workspace it is labelled with (`target.db.Workspace() == id`), because a target
list that is right while the handles are wrong would pass a weaker check.

`TestReconciler_healsItsOwnWorkspaceAndSkipsSuspended` — a cancelled booking still
carrying a calendar event id, in each of an active and a suspended workspace. The
host has no calendar connection, so `CancelEvent` has nothing to call and the pass
clears the id without a network. Positive control on the fixture first: both rows
start stale. After the sweep the active one is cleared and the suspended one is
byte-identical.

`TestReconciler_theOldShapeHealsNothing` is the negative control — one
`reconcileCalendarPass()` on the unbound application handle, which is what the
reconciler was until this commit:

```
calendar_reconcile_tenancy_test.go:183: negative control: a reconciler pass on the unbound
application handle healed 0 of 1 diverged bookings, with no error — every stale calendar
event would stay stale forever
```

It then runs the fixed sweep over the same fixture and asserts the row IS healed, so
the difference is demonstrably the binding and not the fixture.

Measured sweep target set with a suspended tenant present:
`map[acme:true default:true initech:true]` — the suspended `globex` absent, and the
seeded `default` workspace present because it is active in every migrated database.

## Boundary 5, part four — the CLI subcommands

Which of the four needs what follows from what it touches, so the classification is
data (`platformWideCLI`) and a test asserts it rather than re-deriving it.

| command | touches | under `MULTI_TENANT` |
|---|---|---|
| `rotate-key` | `crypto_keystore` | **unchanged** |
| `recover-key` | `crypto_keystore` | **unchanged** |
| `reset-admin` | `users` (tenant) | **requires `--workspace=<id>`** |
| `mcp` (stdio) | everything | **refused** |

`crypto_keystore` is exempt from tenancy (D2) because there is one DEK per process
(D3) — it has no policies at all, so `DATABASE_URL`'s handle reaches it whether or
not RLS is enabled. Both key commands now carry a comment saying that, and saying
that per-tenant DEKs would make it wrong.

### ⛔ `reset-admin` was a real hole, not a hygiene issue

Since D9 the unique on `users` is **`(workspace_id, email)`**, not `email`. So
`UPDATE users SET password_hash = ? WHERE email = ?` on an unscoped handle matches
**every workspace that has a user with that address** — and a shared address across
an agency's client workspaces is the ordinary case, not a contrivance. The negative
control measures it:

```
tenancy_test.go:222: negative control: one unscoped `WHERE email = "owner@example.com"`
reset 2 workspaces' owners — since D9 the unique is (workspace_id, email), so a shared
address is legal and common
```

A recovery tool that quietly hands an operator access to tenants nobody asked about
is worse than one that refuses. `--workspace` is accepted in both `--workspace=id`
and `--workspace id` forms, anywhere among the positional arguments, and is validated
against the same `[a-z0-9_-]{1,64}` shape `db.ForWorkspace` enforces. In
single-tenant mode it is **accepted and ignored** rather than rejected, so a script
written for a multi-tenant fleet keeps working against one instance.

### `mcp` stdio is refused, and the message names the path that works

The stdio transport carries no credential and no Host, so there is nothing to
resolve a tenant from; the tools would run on the unbound handle, which matches no
row, and the operator would see an empty workspace with no explanation. The refusal
points at `POST /mcp` with a workspace's API key or OAuth token, which resolves the
tenant from the credential (D10). A test asserts the message mentions the HTTP
transport, the path, and the credential — a refusal that does not say what to do
instead is a dead end.

---

## Every remaining cross-tenant read and write, and why each is correct

For B7's proof to assert this list is complete. Anything reaching more than one
workspace's rows is here; everything else is bound.

### Platform handle, by design — the tenant root and the exempt tables

| site | what | why correct |
|---|---|---|
| `workspaceByHost`, `workspaceByID`, `activeWorkspaceIDs` | `workspaces` | resolving *which* tenant; `workspaces` is exempt and carries a SELECT-only policy for the app role (D2) |
| `RequireAuth` (api_keys, sessions) | credential → user | the read that DISCOVERS the tenant cannot be bound to it; those uniques are global for exactly this (D9) |
| `MCPCallerMiddleware`, `VerifyMCPBearer` | bearer → user | same |
| `workspaceOfUser` | `users.workspace_id` | resolves the tenant for a Platform route's write |
| `/oauth/*` handlers | auth codes, tokens, clients | Platform-wrapped: `h.db` **is** the platform handle; the two INSERTs name `workspace_id` (the sweep's finding 16/17) |
| `rotate-key`, `recover-key` | `crypto_keystore` | exempt (D2), one DEK per process (D3) |
| goose migrations, `EnableRLS`, `VerifyRoles` | DDL and role attributes | schema-level, not row-level |

### Worker, on the platform handle — one queue, and retention rules

| site | what | why correct |
|---|---|---|
| the claim query + reaper | `jobs` | ONE queue ordered by `run_at` globally; a bound loop would serve one tenant and starve the rest. This is what 00060's `idx_jobs_pending_global` is for |
| purge expired manage tokens, sessions, magic links, idempotency keys, oauth auth codes | 5 `DELETE`s | retention rules keyed on `expires_at`/`created_at`, properties of the row and not of the tenant; per-workspace would be N statements to express one policy |
| purge finished webhook deliveries | `DELETE` | same, 30-day retention |
| ⚠️ **Stripe payment-hold backstop** | **`UPDATE bookings SET status='cancelled'`** | the only cross-tenant **WRITE** left. Keyed on `payment_status='pending' AND created_at < cutoff`, a property of the row. ⛔ **It has to move if hold timeouts ever become per-workspace**, and it is the one entry on this list a reviewer should push back on |

### Reconciler

Enumerates on the platform handle, then every pass is bound. No cross-tenant read
inside a pass.

### Not yet correct — known, and owed to B6

| site | state |
|---|---|
| `POST /v1/livekit/webhook` + its alias, `POST /v1/stripe/webhook` | `Platform`-wrapped so they can find the row they name; the processing after that is **not** yet bound. Must resolve the workspace from the room / session and hand off to `forWorkspace` |
| ~~`/v1/auth/sso` hand-off (D11), `clientIP` rate-limit keys (D14)~~ | ✅ **the VERIFY halves landed with the merge below.** What remains of D11 is the MINT half: no OAuth callback issues an SSO token yet, so nothing exercises the endpoint in production. Plan at the end of this file |
| ~~`/metrics`~~ | ✅ reads `jobs` through `Platform()` |

---

## Integration — `feat/platform-hooks` merged (commit `8a93fad`)

Merged **into** `feat/multi-tenant` with `--no-ff` before B6, because three of that
branch's four features are things B6 was otherwise going to have to fake: the SSO
endpoint D11 hands off to, the trusted-proxy client IP D14 keys on, and `/metrics`.

Six conflicted files, ten hunks. The commit message carries the per-file resolution; what
belongs here is the part that was a **decision** rather than a merge.

### The three new routes, classified

```
172 routes: 31 host-scoped, 107 credential-scoped, 26 platform, 8 allowlisted
```

| route | class | why |
|---|---|---|
| `GET /metrics` | **Platform** | queue depth is an INSTANCE number. Its `jobs` read goes through `Platform()` inside the handler as well as being registered unscoped — a bound read would report one workspace's backlog as if it were the whole queue, an unbound one zero, and both are wrong in a way a dashboard cannot show you |
| `GET /v1/auth/sso` | **Platform** | no tenant Host and no credential yet. The TOKEN is the credential and the workspace is its `wid` claim |
| `POST /v1/auth/sessions/revoke-all` | **credential-scoped** | whose sessions to cut is a question about one workspace's users. All three of its statements (`users`, `sessions`, `oauth_access_tokens`) are then scoped by the bind, which is what makes a target id from another workspace a 404 rather than a revocation |

⚠️ **The classification gate caught the third one, and the cause is worth knowing:
`routes_classified_test.go` scans `server.go` LINE BY LINE.** The platform branch had
written the registration across two lines, so the scanner saw a pattern with an empty
expression and reported it unclassified. A wrapper on a continuation line is invisible to
the gate. Every registration in that file is therefore one line, however long.

### `sso_nonces` is EXEMPT, not a tenant table

The migration is renumbered `00059` → **`00061`** in both dirs (00060 is tenancy);
`knownMigrationCount` 60 → 61.

A jti is **global**. The question the table answers is "has this exact token been spent",
and the answer must not depend on which workspace the token names — per workspace, the
same jti could be replayed once per tenant, which is strictly weaker for no benefit. It
also has no owner at the moment the row is written: the nonce is claimed *before* the
`wid` claim has been trusted. So it joins `workspaces`, `crypto_keystore`,
`goose_db_version` and `oauth_clients` in `db.ExemptTables`, and
`TestTenancy_tableListsCoverTheSchema` is updated with it.

⛔ **The Postgres file declares `expires_at TEXT COLLATE "C"`, and without it the merge
would have redded a gate nobody was thinking about.** The worker purges the table with a
lexicographic `expires_at < ?` — exactly what migration 00059 pinned the other 54 TEXT
timestamps for — and `collation_test.go`'s audit matches by column NAME, so a new `_at`
column is caught whether or not anyone remembers. `wantTimestampColumns` 56 → 57.

### ⛔ The `wid` claim was documentation, and the merge made it load-bearing

The platform branch parsed `wid` and **deliberately ignored** it ("an instance is a
single workspace today"). In multi-tenant mode it is now required, validated against
`db.ValidWorkspaceID`, and read back through the platform handle, so a token naming a
deleted, suspended or public-host-less workspace is refused instead of seating a user
nobody can reach. Single-tenant is untouched: `wid` is ignored, `aud` is `BASE_URL`.

**The audience is the WORKSPACE's public host, not `BASE_URL` (D11).** The hand-off exists
precisely because the identity host cannot set a cookie on a tenant's domain, so the token
is minted for that domain — and a token for tenant A must not be spendable on tenant B's
host, which would seat a session on B for a person A vouched for.

**Every statement the endpoint runs now names the workspace**, and this is the finding
rather than a tidy-up. `/v1/auth/sso` is Platform-wrapped, so `h.db` is the platform
handle: it bypasses the policies and binds `''`. Since D9 the unique on `users` is
`(workspace_id, email)`, so one address legitimately exists in several workspaces — and
an unqualified `WHERE email = ?` resolves an **arbitrary** one of them. It is the
`reset-admin` hole again, reachable here by anyone holding the shared secret. Same fix
for `ssoOwnerExists`, which counted owners instance-wide: unscoped, the first SSO user of
every workspace after the first would silently never be its owner.

`createSessionIn(…, workspaceID)` is how the session names its tenant. An empty
`workspaceID` keeps the original statement, so the other seven callers — all on handles
already bound to the request's workspace, where the bind fills the column — are untouched.
⚠️ It matters beyond bookkeeping: every later request for that user runs on a **bound**
handle, which could neither read that session nor delete it on logout.

Negative control, the workspace predicate removed from the lookup only:

```
--- FAIL: TestSSOHandoff_multiTenantLandsInTheTokensWorkspace
    sso_tenancy_test.go:100: no user shared@example.test in workspace globex: sql: no rows in result set
```

That is the bug stated exactly: the second workspace's hand-off found the FIRST
workspace's user by email, so no globex user was ever created — and the session it minted
pointed at acme's person. `sessions.user_id` is global, so the foreign key would not have
complained.

### D14 wired, and the two halves pull in opposite directions

`rateLimitKey` is `(workspace host, client IP)` when `SetMultiTenantLimits` is on, the IP
alone otherwise. Both halves are load-bearing and each is useless without the other:

- **without the workspace**, two tenants' bookers behind one address — a shared office, a
  corporate NAT, a CDN — share a bucket, so the busier workspace spends the quieter one's
  allowance. One tenant degrading another's service through no fault of either.
- **without `TRUSTED_PROXY_CIDRS`**, the "IP" behind a proxy IS the proxy, so adding the
  workspace prefix would make things WORSE: a whole tenant becomes ONE bucket and its
  first 20 bookers a minute exhaust it for everyone else in that tenant. The prefix is
  only safe because `remoteIP` resolves the client's own address when a trusted hop
  forwarded it. This is why D14 waited for this branch instead of being approximated.

The workspace comes from the **Host**, not from a resolved `*Workspace`: the limiter runs
before any handler and rejecting a request must not cost a database read. An unrecognised
host keys on the host string itself, which is the safe direction — a Host-rotating
attacker gets a bucket per value and spends no real tenant's allowance.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test ./...` **rc=0 on SQLite (30/30)** and **rc=0 on PostgreSQL (30/30)**.
`go test -race -count=1 ./internal/handler/ ./internal/worker/` rc=0 on PostgreSQL
(585.5s / 31.8s).

The platform branch's own tests, all PASS on both lanes: `sso_test` (11),
`session_test` (9 `RevokeAllSessions` cases), `metrics_test` (3),
`notetaker_settings_test` (2), `trustedproxy_test` (10), `reminder_test` (9 including
`reminderFiresBookingReminderWebhook` and `reminderWebhookCarriesTheFiringOffset`, which
is rule 5: the webhook fires from the worker's **bound** per-workspace path through
`deps.Webhook`, so it is signed with that workspace's secret), `i18n` and `stt`
(fr-CA and `STT_BASE_URL` need no tenancy change and are confirmed only by building and
passing).

The tenancy suites still hold with the platform routes in the mux:

```
--- PASS: TestTenancy_readSurfaces (1.02s)          --- PASS: TestSweep_oauthGrantLandsInTheOwnersWorkspace (0.97s)
--- PASS: TestTenancy_bookingByIDIsNotFoundAcrossWorkspaces (1.02s)
--- PASS: TestTenancy_credentialOnTheWrongHostIs403 (1.07s)
--- PASS: TestTenancy_writeLandsInItsOwnWorkspace (1.17s)
--- PASS: TestTenancy_mcpToolCallOverHTTP (1.00s)   --- PASS: TestSweep_setupIsRefusedInMultiTenantMode (1.07s)
--- PASS: TestEveryRouteIsClassified               --- PASS: TestSweep_tenantLocalTokensDoNotResolveAcrossHosts (1.03s)
--- PASS: TestCredentialScopedRoutesAuthenticateFirst   (107 credential-scoped routes checked)
--- PASS: TestPlatformRoutesAreTheIdentityHostSet
```

New multi-tenant cases, running rather than skipping on the PostgreSQL lane:

```
--- PASS: TestSSOHandoff_multiTenantLandsInTheTokensWorkspace (0.92s)
--- PASS: TestSSOHandoff_multiTenantRefusesABadWID (3.80s)   [missing, empty, unknown, not an id]
--- PASS: TestSSOHandoff_multiTenantAudienceIsThePublicHost (2.84s)   [identity host, another tenant, bare hostname]
--- PASS: TestSSOHandoff_multiTenantRefusesASuspendedWorkspace (1.03s)
--- PASS: TestRateLimit_workspacesBehindOneProxyDoNotShareABucket (0.00s)
--- PASS: TestRateLimit_twoBookersOfOneWorkspaceKeepTheirOwnBuckets (0.00s)
--- PASS: TestRateLimit_singleTenantIgnoresTheHost (0.00s)
--- PASS: TestRateLimitKey_portIsNotPartOfTheBucket (0.00s)
```

Negative control, the same PostgreSQL command with the password changed to `wrong` —
these FAIL rather than skip, which is what says they were really talking to a server:

```
--- FAIL: TestPostgres_timestampColumnsCollateC
    failed SASL auth: FATAL: password authentication failed for user "postgres" (SQLSTATE 28P01)
--- FAIL: TestSSOHandoff_multiTenantLandsInTheTokensWorkspace
    failed SASL auth: FATAL: password authentication failed for user "postgres" (SQLSTATE 28P01)
```

### ⚠️ One correction to this file, found while merging

Several notes above (the B3 carry-over list, the sweep's rule, `workspaceOfUser`'s doc)
say that a write omitting `workspace_id` on a Platform route "lands in `default`,
silently". That is true of a handle that never sets the parameter at all. It is **not**
what the platform handle does: `OpenPair` binds `''` before every statement, so
`current_setting('app.workspace_id', true)` returns the empty string rather than NULL,
`COALESCE` keeps it, and the row fails the foreign key to `workspaces(id)` with 23503.
The sweep's own negative control is consistent with this — it failed at "could not issue
code" rather than by landing a row in `default`, and PROGRESS recorded the mechanism as
unconfirmed at the time. **The remedy is identical either way** (name `workspace_id`), so
nothing built on that reasoning is wrong; but "silently lands in `default`" overstates how
quiet the failure is, and the distinction matters to anyone debugging a 23503 from a
Platform route. Not corrected in place above, because those notes are the record of what
was believed when each boundary was written.

## Boundary 6, part one — the platform API (D12): create, get, patch, delete

`internal/handler/platform.go`. Four routes on the identity host, all `Platform`-wrapped,
bearer `CALNODE_PLATFORM_TOKEN` compared with `subtle.ConstantTimeCompare`.

### Two gates, and they answer differently on purpose

| state | answer | why |
|---|---|---|
| token unset | **404** | the API does not exist here, and a prober should not learn whether it could |
| single-tenant instance | **404** | there are no workspaces to provision; same reasoning, and it is Setup's mirror image (Setup 404s in multi-tenant mode) |
| token set, wrong token | **401** | an operator with a typo needs to tell that apart from a missing feature |

### Provisioning is one transaction, and every INSERT names `workspace_id`

The workspaces row, `server_settings` (id = 1 per workspace, D8), the owner user, that
owner's first `cno_` key, the webhook subscription, the default event type and its
availability rules. Either the tenant exists complete or it does not exist: a
half-provisioned workspace would answer requests with no owner, or serve a booking page
with no availability. `TestPlatform_duplicateIDOrHostIs409` asserts the rollback, not just
the status code.

⛔ **Every INSERT names `workspace_id` explicitly**, which is the rule this file exists to
obey rather than to demonstrate. `h.db` here is the platform handle: it bypasses the
policies and binds `''`, so an unnamed column resolves to `''` and the row fails its
foreign key with 23503. **Reads are equally unscoped and carry their own predicate** —
there is no policy behind this file to catch a forgotten `WHERE`.

⚠️ `owner_timezone` is stored as the owner's `iana_timezone`, **not** UTC, and that is a
correctness requirement rather than a nicety: `availability_rules` holds local `HH:MM` with
no zone of its own, so defaulting the owner's zone would silently move the workspace's
working hours. A test pins the stored value.

### Two settings had no column, so migration 00062 gives them one

`defaults.embed_allowed_origins` and `defaults.stt_base_url` are in the settled contract
and `server_settings` had nowhere to put either — the enumerated column list has neither.
Both are per-TENANT facts that the environment cannot express: one process-wide
`EMBED_ALLOWED_ORIGINS` would let one tenant's allowlist govern another's embed, and the
STT host is a residency knob, which is a property of the tenant.

⚠️ **They are WRITTEN and not yet READ.** The embed CORS check still uses
`config.EmbedAllowedOrigins` and the notetaker still uses `config.STTBaseURL`, so a
multi-tenant deployment currently shares one embed allowlist and one STT host. Storing them
now means a provisioned workspace does not silently lose what the caller sent; wiring the
readers is separate work, because each has to go through the per-workspace settings cache
(D7) rather than a process-wide read. Named here so it is not mistaken for done.
`knownMigrationCount` 61 → 62.

### The webhook secret's encoding has exactly one implementation

`webhook.NewSecret` is new and the platform API calls it: the column holds the AES-GCM of
the **raw** secret bytes, `Sign` uses those bytes as the HMAC key, and the string handed to
the subscriber is their **hex**. A second implementation that encrypted the hex string
would store a key the subscriber cannot reproduce, and every delivery would fail its
signature check with nothing in the logs to point at. A caller-supplied secret must
therefore be hex and at least 16 bytes; absent, one is generated. The INSERT itself stays
in the provisioning transaction, where it can name `workspace_id`.

### ⛔ Decision: the `default` workspace is SUSPENDED at multi-tenant boot

`db.SuspendDefaultWorkspace`, called from `main` right after `EnableRLS`, idempotent, and
non-fatal on error (a sweep over an empty workspace is waste, not damage).

Migration 00060 seeds `default` because it is the workspace every single-tenant row belongs
to and the one SQLite's column default names. On a multi-tenant instance it is a tenant
nobody owns: no `public_host` (deliberately, so no Host reaches it), no users, no settings —
and while it is `active`, every background sweep enumerates it, because
`activeWorkspaceIDs` filters on `status = 'active'`.

**Not in the migration, and that is the whole point:** a migration cannot see
`MULTI_TENANT`, and in single-tenant mode `default` **is** the workspace — a suspended row
there would make `Scoped` answer 503 to every request on the instance. The mode decides,
and the mode is only known at boot. Suspension rather than deletion because the row is
referenced by `server_settings` and by whatever single-tenant data a converted instance
still holds, and because one status flip removes it from every sweep at once instead of
adding a second exclusion rule a new loop could forget.
`TestPostgres_defaultWorkspaceIsSuspendedAtMultiTenantBoot` asserts active-after-migrate
(what single-tenant needs), suspended-after-call, idempotence, and that a real workspace
beside it is untouched.

### ⚠️ Two things the tests paid for

- **`httptest.NewRequest` does not populate mux path values.** They come from the pattern
  `ServeMux` matched, and these tests call the handler directly — so every `{id}` route
  read an empty id and answered **404, which looks exactly like a missing workspace**. The
  helper calls `SetPathValue` now.
- **`location_type` and `routing_mode` have CHECK constraints** (migration 00001) that
  admit no `'none'` or `'single'`. The event type seeds `'link'` / `'fixed'`, the schema's
  own defaults, which is what a single-host event type created in the admin UI gets. The
  first run failed with 23514 as a bare 500, and the reason was only visible with a live
  logger.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test -count=1 ./...` **rc=0 on SQLite (30/30)**.
PostgreSQL lane, per package: `internal/handler` **rc=0** (410.2s), `internal/db`,
`internal/server`, `internal/webhook` **rc=0**.

```
--- PASS: TestPlatform_createProvisionsTheWholeWorkspace (1.04s)
--- PASS: TestPlatform_createdWorkspacesAreIsolated (1.25s)
--- PASS: TestPlatform_duplicateIDOrHostIs409 (1.10s)
--- PASS: TestPlatform_getPatchDelete (1.08s)
--- PASS: TestPlatform_tokenGate (1.08s)          [wrong token, no token]
--- PASS: TestPlatform_404WithoutATokenConfigured (0.73s)
--- PASS: TestPlatform_404OnASingleTenantInstance (0.74s)
--- PASS: TestPlatform_createValidation (11.47s)  [9 cases, each asserting nothing was left behind]
--- PASS: TestPostgres_defaultWorkspaceIsSuspendedAtMultiTenantBoot (0.76s)
```

### Still owed by B6

Export/import/erasure; the SSO + OAuth hand-off on the public host (the plan is at the end
of this file); the vendor webhooks resolving their workspace from the row they name. D13 is
already done (B1's `config.Validate` refuses `MULTI_TENANT` with `DEMO_MODE`) and D14
landed with the platform-hooks merge.

### `embed_allowed_origins` and `stt_base_url` are READ — the D7 reader wiring, done

Migration 00062 added both columns and the platform API filled them; until this commit nothing
read them, so a multi-tenant instance shared one embed allowlist and one STT host across every
tenant whatever its row said. Now `internal/handler/tenant_settings.go` reads both through the
bound handle into a per-workspace `settingsCache` (the same `tenantCache` the vendor clients use):

- **`embed_allowed_origins`**: the CORS wrapper became `server.PublicCORSFor(originsFor)`, with
  `PublicCORS(list)` as the single-tenant special case. In multi-tenant mode `server.New` passes
  `h.EmbedOriginsFor`, which resolves the request host to its workspace on the platform handle
  (one indexed read, the route's own 404 unchanged) and answers that workspace's list. An unknown
  host answers known=false, which the middleware turns into NO allow-origin header — never `*`.
  An empty per-tenant list keeps the single-tenant meaning: any origin. `EMBED_ALLOWED_ORIGINS`
  is not consulted in this mode. The 'move the check inside the handler' alternative was not
  taken: a disallowed origin is still refused before any handler runs, exactly as today.
- **`stt_base_url`**: `sttBaseURL()` gained one rung on top of its existing ladder — the
  workspace's column when multi-tenant and non-empty, then `STT_BASE_URL`, then the provider
  default. The notetaker job already runs bound (`workspaceForJob`, B5), so it needed no change.

Tests: `TestTenantSettings_perWorkspaceReaders` (handler, PG: two workspaces via the platform API;
A's own STT host and origins, B's fall-through to the process value and then the default, an
unknown host resolving to no tenant), `TestPublicCORSFor_*` (middleware unit, both engines) and
`TestTenancy_embedOriginsArePerWorkspace` (end to end through the real mux). Negative control,
the multi-tenant branch in `server.New` disabled: the end-to-end test fails with `200 "*"` on
acme's own origin, on a foreign origin, and on an unknown host — the shared-allowlist bug, seen.

### ⚠️ A pre-existing upstream race, fixed here (upstream-first PR candidate)

Not a tenancy bug and not caused by the merge: it reproduces on a stock single-tenant
instance, and the fix is generic. Worth carrying upstream as its own patch.

**The bug.** Two detached goroutines write the same reminder row. The booking CREATE path
enqueues reminders from one; the RESCHEDULE path replaces them from another. Both marshal
the identical payload `{booking_id, hours_before}`, and `jobs` carries a unique on
`(type, payload)` — so exactly one row can exist per `(booking, hours_before)`. The
replacement deletes the booking's non-running reminder rows and re-inserts them at the new
time, and the losing interleaving is:

```
reschedule: DELETE …            (create's row not yet committed, so invisible to it)
create:     INSERT at the OLD time
reschedule: INSERT at the NEW time  ->  conflict  ->  ON CONFLICT DO NOTHING  ->  dropped
```

The reschedule's own row is the one discarded, so the reminder stays pinned to the
**original** time — permanently, with nothing logged. A booking created and rescheduled
inside the same second is enough. The attendee is then reminded about a time the meeting no
longer has, or not at all if the old time has already passed.

**The fix.** The reschedule's INSERT upserts; the create side keeps `DO NOTHING`, because a
create must never overwrite a reschedule and by the time the two can collide the reschedule
is the later fact. One side upserting makes all three interleavings agree on the
reschedule's time. `WHERE status <> 'running'` preserves the invariant the DELETE already
encodes — a claimed job is executing, and resetting it underneath the worker would either
double-send or be clobbered by the worker's completion write.

**Measured:** 2 failures in 60 runs before, 0 in 30 after, on PostgreSQL. The test forces
the interleaving with an uncommitted row rather than waiting for it, and fails if no backend
ever blocks, so it cannot pass on the easy path.

⚠️ **The test that found it reported it as something else**, which is why it took a bisect:
it polled for "any `run_at` that is not the old one", kept the last value read when its
deadline expired, and asserted on that — so a timeout surfaced as "`run_at` is three days
wrong". A poll loop that falls through to an assertion on stale data will misattribute every
failure it ever catches.

## Boundary 6, part two — export, import and erasure (D12)

`internal/handler/platform_data.go`. Three more `Platform` routes behind the same token
gate: `POST …/export`, `POST …/import`, `DELETE …/attendees?email=`.

### The replay order is data, not convention

`exportTableOrder` is a hand-ordered list (parents before children) and the document carries
its tables as an ordered ARRAY, because import replays them in the order it receives.
Alphabetical — `db.TenantTables`' order — would put `booking_answers` before `bookings` and
`event_type_hosts` before `event_types`, i.e. a foreign-key violation.

⛔ **`exportCoversEveryTenantTable` runs at request time**, not only in a test: it fails the
export if any `db.TenantTables` entry is missing from the order, or if the order names
something that is not a tenant table. `TestTenancy_tableListsCoverTheSchema` already forces a
new table to be classified; this makes the same guarantee reach the backups, so a table added
by a later migration cannot be silently absent from every tenant's export. Being wrong here
produces data that was never backed up, which is exactly the class of failure nobody notices
until they need the backup.

Rows are read with `SELECT *` + `rows.Columns()` rather than 32 hand-written column lists,
for the same reason: a migration that adds a column would otherwise stop exporting it, and
the loss would surface only as data missing after an import. `ORDER BY 1` makes two exports
of one workspace byte-comparable, which is what the round-trip test relies on.

### ⛔ Decision: the DEK does NOT travel; a fingerprint does

The turn's instruction said "the `crypto_keystore` row for the workspace travels with it
because the DEK is per tenant". **There is no such row.** `crypto_keystore` has no
`workspace_id` at all — it is exempt (D2) and holds one wrapped DEK per PROCESS (D3),
labelled `primary` / `recovery`. Verified in the schema rather than assumed.

So carrying it was not an option, and manufacturing one would have been worse than useless:
an export of a SINGLE tenant would then contain the key that decrypts EVERY tenant's secrets
on that instance, which is the opposite of what the isolation exists for.

What travels is `dek_fingerprint`: SHA-256 over the already-encrypted `wrapped_dek`, which
cannot be reversed to the key. Equal fingerprints mean the two instances share a data key, so
the document's `_enc` columns will decrypt there. **Different ones make import refuse with
409.** That refusal is the point: without it the rows import perfectly and then every secret
in them — SMTP password, LLM key, LiveKit secret, calendar tokens — fails at first use, one
integration at a time, long after anyone is watching. Import is the only moment the two keys
can be compared. The message names `CALNODE_ENCRYPTION_KEY`, because moving it with the data
is the operator's actual remedy.

⚠️ A per-tenant DEK would change all of this, plus D3 and the schema. It is a separate
packet, and until then moving a workspace between instances means moving the encryption key
with it.

Secrets and API-key hashes otherwise travel **verbatim**, per the contract: a tenant whose
keys and manage links stopped working on migration has not been migrated. The consequence is
that the document is as sensitive as the database.

### ⛔ Import forces the target workspace

Every row is inserted with `workspace_id` = the id **in the URL**, and the document's own
value is discarded. This endpoint is authorised by the platform token, so trusting the
document would make an export of any workspace a way to write rows into any other.

⚠️ **Ids are GLOBAL primary keys, so import is a MOVE and not a copy.** Replaying a document
into a second workspace while the first still holds its rows collides on `users_pkey`. The
supported operation is export → delete → import, usually into another region's instance where
those ids do not exist; `TestPlatformData_importForcesTheTargetWorkspace` performs exactly
that inside one database, which is the only place the workspace_id question can be asked.
This is worth knowing before anyone tries to use import to clone a tenant for staging.

`UseNumber` on the decoder, and `importValue` converts a `json.Number` to `int64` when it is
one: without it every numeric column round-trips through `float64` and a large id loses its
low bits silently.

### Erasure: exactly one email, in exactly one workspace, cancelling nothing

⚠️ **`booking_answers` carries no attendee** — it is keyed `(booking_id, question_id)` — so
"their answers" has to be derived. They are erased only for bookings where the erased person
was the **only** attendee. With anyone else still on the booking the answers cannot be
attributed to them, and deleting them would erase a third party's data to satisfy someone
else's request. Both halves are tested: two attendees ⇒ 1 attendee row and **0** answers
erased; sole attendee ⇒ 1 and 1.

Bookings are not cancelled (the host's calendar and the other attendees' records are not the
erased person's data), and the workspace boundary holds: the same address in another
workspace is untouched, because one tenant's erasure request is not consent to delete
another's records.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test -count=1 ./...` **rc=0 on SQLite (30/30)**.
PostgreSQL: `internal/handler` **rc=0** (329.5s), `internal/server` **rc=0**, `internal/db`
**rc=0**.

```
--- PASS: TestPlatformData_exportDeleteImportRoundTrip (1.31s)
--- PASS: TestPlatformData_importIntoAPopulatedWorkspaceIs409 (1.29s)
--- PASS: TestPlatformData_importForcesTheTargetWorkspace (1.33s)
--- PASS: TestPlatformData_eraseAttendee (1.31s)
--- PASS: TestPlatformData_eraseTakesAnswersWhenNobodyElseIsOnTheBooking (1.27s)
--- PASS: TestPlatformData_eraseRequiresAnEmail (1.22s)   [missing, empty, not an email]
--- PASS: TestPlatformData_routesRefuseWithoutTheToken (1.27s)   [export, import, erase]
--- PASS: TestPlatformData_importRefusesAForeignDEK (1.28s)
```

Negative control, the workspace-forcing removed so the document's own `workspace_id` is
trusted:

```
--- FAIL: TestPlatformData_importForcesTheTargetWorkspace
    import into acme: 400 — insert or update on table "server_settings" violates foreign key
    constraint "server_settings_workspace_id_fkey" (SQLSTATE 23503)
```

⚠️ Stated precisely, because it is not the failure I predicted: it proves the forcing is
load-bearing (unfixed 400, fixed 200 with the rows in `acme`), but it fails at the foreign key
rather than by writing rows into the wrong tenant — the source workspace has been deleted by
then, so the id the document names no longer exists. The leak shape it guards against is
unreachable on one instance for the same reason ids are global; the guard matters for a
destination where `globex` DOES exist.

### Still owed by B6

The SSO + OAuth hand-off on the public host (plan at the end of this file) and the vendor
webhooks resolving their workspace from the row they name. D13 and D14 are done.

---

## Boundary 6, part three — the SSO hand-off and the OAuth login, both halves (D11)

The verify half arrived with the platform-hooks merge; this is the mint half, plus the
correction that makes the verify half safe on a public host.

### ⛔ The workspace comes from the HOST, and `wid` is CHECKED against it

The endpoint is reached at `https://<public_host>/v1/auth/sso`, because landing the cookie on
the tenant's own domain is the entire reason the hand-off exists. So resolving the workspace
from the token's `wid` — which is what the merge left — is wrong: a token for workspace A
presented on **B's** host has a valid signature, a `wid` that resolves, and an `aud` that
matches A's host, so it would quietly create **A's session on B's domain**.

Now: resolve by `workspaceByHost`, then require `wid == that workspace's id` (403
`{"error":"workspace mismatch"}`, D10's body) and `aud == "https://" + that public host`, no
trailing slash. Unknown host 404, suspended 503. Single-tenant unchanged: `wid` ignored, `aud`
is `BASE_URL`.

Negative control, the resolution put back on `wid`:

```
--- FAIL: TestSSOHandoff_tokenForAnotherWorkspaceIsRefusedOnThisHost
    status = 302; want 403 — <a href="/admin/">Found</a>.
--- FAIL: TestOAuthHandoff_mintedTokenIsRefusedOnAnotherWorkspacesHost
    status = 302; want 403
```

That 302 IS the bug: one tenant's session created on another tenant's domain.

### The state cookie carries the workspace; the URL carries only the nonce

`newOAuthState(w, workspaceID)` writes `<nonce>|<workspace_id>` into the HttpOnly
`calnode_oauth_state` cookie and sends only the nonce to the provider.
`verifyOAuthState` compares the nonce and **reads** the workspace.

⛔ That split is the security property. A visitor can rewrite the `state` query parameter —
doing so produces a failed login. The value that decides which tenant a Google identity is
admitted to never left this server's cookie. Putting the workspace in the URL instead would let
anyone choose the tenant they are let into.

The workspace is resolved at the login START (`GET /v1/auth/login`, from the Host the person
clicked on), because the callback arrives on the identity host and cannot know it. That route
is `Platform`-wrapped — it has to be, since its callback is — so it calls `workspaceByHost`
explicitly and 404s an unrecognised host, doing by hand exactly what `HostWorkspace` would.

### `finishOAuthLogin`: two bugs, one of them live before this

1. ⛔ **The lookup was `WHERE email = ?` on the platform handle.** Since D9 the unique on
   `users` is `(workspace_id, email)`, so the same address in several workspaces is ordinary —
   and that statement resolves an **arbitrary** one of them and starts a session for a
   stranger. Now scoped by `workspace_id`, in both modes (`'default'` in single-tenant, where
   every row carries it), so there is one statement and no branch to drift.
2. **The session is no longer set here in multi-tenant mode.** The callback runs on the
   identity host; a cookie for it is no use to a person whose admin UI is on their own domain.
   It mints a 30 s HS256 token (`iss` = BASE_URL, `aud` = `https://<public_host>`, `wid`,
   `role = member`, `jti`) and redirects to that workspace's `/v1/auth/sso`.

`role = member` always: the callback knows that Google or Microsoft vouched for an address,
which says nothing about what that person may do here. `ssoResolveUser` leaves an existing
user's role alone, so the value only ever applies to a user it creates.

⛔ **No shared secret ⇒ refuse.** With `CALNODE_SSO_SHARED_SECRET` unset the hand-off endpoint
404s, so there is nowhere to land; setting a cookie on the identity host instead would produce
a session the person's own admin UI cannot see. `error=sso`, no session.

⚠️ **The MCP Connect return keeps its identity-host cookie tail**, deliberately:
`/oauth/authorize` is an identity-host endpoint whose consent-step cookies were set there, so
handing it to a tenant's public host would arrive with none of them.

### The calendar callback resolves its workspace from the state's USER

`p.Exchange` writes `calendar_connections` through a provider that captured a handle at
construction. On this `Platform` route that is the **unbound** handle, so the connection
appeared to succeed and no row existed. Now the workspace comes from `workspaceOfUser` on the
state's user id — the state is encrypted, so the id cannot be forged — the exchange runs
through `forWorkspace(ws).getCal()`'s provider (B5's `Provider.ForDB`), and the redirect goes
to that workspace's `publicURL()` rather than `h.baseURL`, which would have sent the person
somewhere their session does not exist.

⚠️ The workspace is deliberately **not** also carried in the calendar state: it would be a
second copy of a fact the user id already settles, and two sources of one truth is how they
come to disagree.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test -count=1 ./...` **rc=0 on SQLite (30/30)**. PostgreSQL: `internal/handler` **rc=0**
(302.5s), `internal/server` **rc=0**.

```
--- PASS: TestSSOHandoff_multiTenantLandsInTheTokensWorkspace (0.96s)
--- PASS: TestSSOHandoff_tokenForAnotherWorkspaceIsRefusedOnThisHost (0.90s)
--- PASS: TestSSOHandoff_multiTenantRefusesABadWID (3.49s)     [missing, empty, unknown, not an id]
--- PASS: TestSSOHandoff_multiTenantAudienceIsThePublicHost (2.40s)  [identity host, bare host, trailing slash]
--- PASS: TestSSOHandoff_multiTenantReplayIsRefused (0.77s)
--- PASS: TestSSOHandoff_unknownHostIs404 (0.73s)
--- PASS: TestSSOHandoff_multiTenantRefusesASuspendedWorkspace (0.71s)
--- PASS: TestOAuthHandoff_callbackMintsATokenTheSSOEndpointAccepts (0.76s)
--- PASS: TestOAuthHandoff_mintedTokenIsRefusedOnAnotherWorkspacesHost (0.72s)
--- PASS: TestOAuthHandoff_refusesWithoutTheSharedSecret (0.76s)
```

⚠️ **A trap the test bridge paid for.** Driving `finishOAuthLogin` on the bare handler reported
`no_account` for a user that exists: the tail's lookup runs on whatever handle the route gives
it, and on the bare handler that is the **unbound application** handle, which matches no row.
The bridge goes through `h.Platform(...)` as `server.New` does. A test that had accepted the
bare handler would have been asserting against the wrong handle.

### Still owed by B6

Only the vendor webhooks: `POST /v1/livekit/webhook` (+ its alias) and `POST /v1/stripe/webhook`
must resolve their workspace from the room or session row they name, then hand off to
`forWorkspace`. D12, D13 and D14 are done.

## Boundary 6, part four — the vendor webhooks, and a hole they exposed

`POST /v1/livekit/webhook` (+ its legacy alias) and `POST /v1/stripe/webhook`. Both stay
`Platform`-classified, because the caller is a vendor: no tenant Host (the URL was registered
once in a dashboard) and no credential of ours (the signature is theirs).

### The workspace comes from OUR row, keyed on an id WE issued

`internal/handler/vendor_webhook.go`. LiveKit resolves in three steps — the egress id on a
`recordings` row, then the room's `recordings` row, then the booking the room name encodes
(`booking-<id>`, which is the only room name this application creates, and is what a
`room_finished` for a never-recorded meeting needs). Stripe resolves from
`bookings.stripe_session_id`, falling back to the metadata booking id — and even then takes
`workspace_id` from the ROW, not the payload.

⛔ **No resolver consults a `workspace_id` in a vendor payload.** Such a field would be a
tenant selector supplied by whoever can forge a body, and every read here names `workspace_id`
in its own SELECT list instead.

An event no row owns is **2xx and ignored**. A 4xx would make LiveKit and Stripe retry for days,
and no retry can make a row exist that never did.

### ⚠️ Decision: in multi-tenant mode the RESOLVE precedes the VERIFY

The turn asked for verify-then-resolve. That is impossible here, and the reason is worth
recording: **both vendors' credentials live in `server_settings`, i.e. per workspace (D7).** On
a Platform-wrapped route the handle bypasses the policies, so `LoadLiveKitSettingsFromDB`'s
`WHERE id = 1` matches every tenant's row and returns an arbitrary one — verifying a signature
against a randomly chosen tenant's secret is not verification. There is no process-wide
credential to fall back on either, because boot priming is single-tenant-only (B5, deliverable 4).

So: resolve (one keyed SELECT), then verify with **that workspace's** secret, then act. Two
properties make it safe and both are load-bearing: **nothing is written before the signature
verifies**, and **nothing is disclosed** (the body is empty either way).

⚠️ The residual is an existence oracle — an unsigned request naming a real egress id gets 403
while an unknown one gets 200. The ids are opaque (`booking-<uuid>`, LiveKit egress ids) and a
caller holding a valid signature already knows the id it sent. Closing it would mean 403 for an
unknown row, i.e. the retry storm this design exists to avoid.

**Single-tenant keeps the original order exactly** (verify, then act): one workspace, one set of
credentials, nothing to resolve, and no reason to change what a working deployment does.

### ⛔ THE HOLE: `forWorkspace` was binding onto the PLATFORM pool

Found by the vendor-webhook test, and it reached further than this group.

`Platform()` replaces `h.db` with the platform handle, whose role **BYPASSES** row-level
security. `forWorkspace` then built its scoped copy with `h.db.ForWorkspace(ws.ID)` — so the
result **named** a tenant and was not **confined** to it. A `WHERE id = 1` read through such a
handle matches every workspace's `server_settings` row and returns an arbitrary one; a write
lands wherever the statement says. Both silent.

The symptom was oblique: the hand-off from the LiveKit webhook to
`forWorkspace(ws).getLiveKit()` read the **default** workspace's empty settings row instead of
the resolved tenant's, so the client was nil and the handler answered 200 having verified
nothing. No error, no log line.

Fixed by holding the application handle on `shared` (`appDB`, set once in `handler.New`) and
binding every scoped copy from **that**: `h.appBase().ForWorkspace(ws.ID)`. A tenant-scoped
handler now always rides the role the policies constrain, whichever route it came from.

⚠️ **Why nothing else caught it:** every other Platform-route hand-off either names
`workspace_id` explicitly on its writes (the platform API, the SSO endpoint — which is why
those tests pass either way) or runs on the worker's path, where the base handler's `db` IS the
application handle. The vendor webhooks are the first code to depend on a scoped handle's
**reads** being filtered.

Negative control, the bind put back on `h.db`:

```
--- FAIL: TestVendorWebhook_livekitBadSignatureWritesNothing
    status = 200; want 403
```

200 means it never reached verification, because it loaded the wrong tenant's (absent)
credentials.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test -count=1 ./...` **rc=0 on SQLite (30/30)**. PostgreSQL: `internal/handler` **rc=0**
(321.2s), `internal/server` **rc=0** — including the whole B3/B4/B5 tenancy suite, which is
what says the `appDB` change did not move anything else.

```
--- PASS: TestVendorWebhook_livekitResolvesTheOwningWorkspace      [7 cases]
--- PASS: TestVendorWebhook_livekitEventWritesOnlyItsOwnWorkspace
--- PASS: TestVendorWebhook_livekitUnknownRoomIs2xxWithNoWrite
--- PASS: TestVendorWebhook_livekitBadSignatureWritesNothing
--- PASS: TestVendorWebhook_stripeResolvesTheOwningWorkspace       [6 cases]
--- PASS: TestVendorWebhook_stripeUnknownSessionIs2xxWithNoWrite
--- PASS: TestVendorWebhook_stripeBadSignatureWritesNothing
```

---

# Boundaries 1–7 — COMPLETE. Phase 1 of the fork is gated

B1 config + schema, B2 the two handles, B3 handler scoping and resolution, B4 per-tenant state,
B5 the background loops, B6 the platform API + hand-off + vendor webhooks, **B7 the proof, the
cost measurement and `docs/MULTI_TENANT.md`**.

The B7 gate lines, in one place:

```
gofmt -l .                          empty
go vet ./...                        clean
go build ./...                      clean
go test -count=1 ./...              rc=0 on SQLite, 30/30 packages
PostgreSQL, per package:            internal/server rc=0, internal/handler rc=0,
                                    internal/db rc=0, internal/worker rc=0, internal/webhook rc=0
go test -race ./internal/server/ -run TestProof     rc=0, no data race

--- PASS: TestProof_noRouteLeaksAnotherWorkspace     138 tenant routes as A, 61 answered 2xx
--- PASS: TestProof_everyMCPToolIsScoped             10 tools, derived from tools/list
--- PASS: TestProof_everyWorkerJobTypeIsCoveredAndScoped   4 job types, derived from two sources
--- PASS: TestProof_unscopedQueryReturnsNothing      6 tables, both halves
--- PASS: TestRSS_200Workspaces                      34.6 KB per tenant

negative control (wrong password): internal/db FAILS with SQLSTATE 28P01 rather than skipping
```

# Boundary 6 — done. D11, D12, D13, D14 all closed

| decision | state |
|---|---|
| **D11** hosts, per-workspace `publicURL`, the OAuth hand-off through SSO, the calendar callback's workspace | **done** (B3 for the hosts, B6 part three for both hand-off halves) |
| **D12** platform API: create, get, patch, delete, export, import, attendee erasure; suspended ⇒ 503 | **done** (B6 parts one and two; 503 was already in `Scoped`) |
| **D13** demo mode and multi-tenant mutually exclusive | **done** in B1 (`config.Validate`) |
| **D14** rate limits keyed `(workspace, client IP)` via `TRUSTED_PROXY_CIDRS` | **done** with the platform-hooks merge |

## Packet F1 — the platform member API, and managed rows

The dashboard replacing the `/admin/` SPA needs each of its people to act here as their own
user with their own key, and it needs roles to be the identity provider's answer rather than
this instance's. Two rows that provisioning already creates were also a live hazard: the
`platform-provisioned` API key an integration spends and the webhook the platform receives
bookings on both sat on the owner user, listed with a delete button on the workspace's own
settings pages. One click, no warning, integration broken, and nothing on either side to say
so.

### Migration 00063, and what `managed` does and does not mean

`api_keys.managed` and `webhooks.managed`, `SMALLINT` on Postgres and `INTEGER` on SQLite
(the schema's spelling for every flag it has; `BOOLEAN` would be the only one and the int
scan targets would reject it). Default 0, so every existing row is unmanaged.

The backfill is `UPDATE api_keys SET managed = 1 WHERE name = 'platform-provisioned'`. That
name is written by exactly one statement in the tree, so it is a precise marker rather than
a guess, and a single-tenant database has no such row.

⚠️ **There is deliberately NO webhook backfill.** A webhook row carries nothing that
distinguishes the platform's from one the workspace created — url, events and fields are all
values the caller chose, and a tenant could legitimately have made the same subscription.
Guessing by url shape would mark rows the platform does not own. `PATCH
/v1/platform/workspaces/{id}/webhooks` is how the platform marks its own, by exact url, once
it knows which it is: the platform knows the url it gave, the database does not.

| surface | a managed row |
|---|---|
| `GET /v1/api-keys`, `GET /v1/webhooks` | omitted |
| `DELETE /v1/api-keys/{id}`, `PATCH`/`DELETE /v1/webhooks/{id}` | 403 `{"error":"managed by your platform"}` |
| `RequireAuth` | accepted, unchanged |
| `webhook.Service.Enqueue` (delivery) | delivered, unchanged |

403 rather than 404 because the row exists and the caller owns the user it hangs off: "not
found" is a lie they can disprove, and the actionable answer is who to ask. Both refusals
read the row before writing, since `WHERE ... AND managed = 0` alone affects zero rows for a
missing row and a managed one alike and cannot tell the two apart.

⛔ **`managed` governs administration, never delivery, and the two are one grep apart.** A
`managed = 0` predicate added to `Enqueue` for symmetry would stop the provisioning webhook
delivering the moment 00063 marked it — silently, with the row still present and still
`is_active = 1`. `TestManagedWebhookStillDelivers` pins it and the comment at the read says
why. `RequireAuth` is the same shape: hiding a row from a settings page and refusing to
authenticate it are different things, and only the first is intended.

### The five routes, classified

```
184 routes: 31 host-scoped, 107 credential-scoped, 38 platform, 8 allowlisted
```

All five are `h.Platform(...)`, on one line each, and in `routes_classified_test.go`'s
pinned set. The tenant is named in the URL rather than resolved from a Host or a credential,
authorised by `CALNODE_PLATFORM_TOKEN`, so the class is the same one the four provisioning
routes have.

| route | contract |
|---|---|
| `POST …/{id}/users` | `{"email","name","role","timezone"?}` → 200 `{"id","email","name","role","created"}` |
| `POST …/{id}/users/{uid}/api-keys` | `{"name"}` → 201 `{"id","name","api_key"}`, once |
| `DELETE …/{id}/users/{uid}/api-keys/{keyId}` | 204 |
| `POST …/{id}/users/{uid}/archive` | 200 `{"archived":true}` |
| `PATCH …/{id}/webhooks` | `{"url","managed":true}` → 200 `{"updated":n}` |

### The three refusals, each of which is a decision

⛔ **Demoting the sole owner is 409 `owner_demotion_requires_transfer`**, and nothing
changes. Obeying it leaves a workspace with no owner, which nothing here can repair: every
route that grants ownership requires an owner to call it. `role: "owner"` is therefore a
TRANSFER — the same transaction sets `is_owner = 0` on everyone else, so the invariant
`TransferOwnership` maintains holds after every call to the new route as well.

⛔ **A re-mint of the same name ROTATES rather than adding.** The earlier managed key of that
name is deleted in the same transaction, so a caller that lost a key never accumulates
credentials and the old one stops working exactly when the new one starts. The trade is
accepted: two live keys need two names. Only managed keys rotate — a key the person minted
for themselves is theirs.

⛔ **`{"managed": false}` is 400.** One-way on purpose: the flag's value is that a credential
caller cannot reach the row, and an un-manage route is a way to reach it. Delete and
re-create is the reversal, and it is one an operator has to mean.

### What the archive route inherited, and what it added

The fork REFUSES to archive somebody who still hosts an upcoming booking (`ArchiveUser`
answers 409), so this route does too — `{"error":"upcoming_bookings","count":n}`, machine
readable because the platform has to act on the count. The owner is
`{"error":"owner_cannot_be_archived"}`. Event types are deactivated. Dropped are the guards
that are about an ADMIN actor rather than about the workspace ("not yourself", "only the
owner may archive an admin"): the platform is not a member.

Added, and the reason it is a transaction: the person's **managed** keys, sessions and MCP
tokens go with the same commit. Archiving already blocks a key through `RequireAuth`'s
`archived_at IS NULL`, but leaving the rows means an un-archive through the upsert route
silently brings a live credential back on whatever host still holds it. `archived_by` is
left NULL because the archiver has no `users` row, and an id naming nobody would make
`RestoreUser`'s "members you archived yourself" check compare against a phantom.

### Two things worth not relearning

⛔ **`ssoResolveUser` is NOT reused, deliberately.** It never rewrites an existing user's
role and it refuses an archived user, and both are the opposite of what an upsert from the
identity provider is for: a role change made there has to land here, and a person who comes
back has to be able to come back.

⚠️ **One upsert statement cannot be written for both engines.** `users` is unique on
`(workspace_id, email)` on Postgres (00060) and still globally unique on `email` on SQLite,
so `ON CONFLICT (workspace_id, email)` names a constraint one engine does not have and is a
hard error there. Read-then-write is the same statement on both, and it is what the
`created` flag and the zero-owner guard read anyway.

⚠️ **`mintAPIKey` is not yet the only mint.** It replaced the two in `apikey.go` and
`platform.go`, but `Setup` (`/v1/setup`) still writes its bootstrap key inline — it runs
before any workspace exists, names no `workspace_id`, and cannot mint a managed key, so
folding it in is a separate change. `grep -rn '"cno_"' internal/` is the enumeration and
returns two sites.

## What a bring-up needs from this side

Environment, on every instance:

| key | value |
|---|---|
| `MULTI_TENANT` | any non-empty value turns the mode on. ⛔ Requires a `postgres://` DSN and refuses `DEMO_MODE` (D13) |
| `DATABASE_URL` | the **application** role: `NOBYPASSRLS`, owns no table in the schema. `VerifyRoles` refuses the boot otherwise |
| `DATABASE_ADMIN_URL` | the **platform** role: owner, `BYPASSRLS`. ⛔ Must not equal `DATABASE_URL` — one role means every policy is inert against it, and everything appears to work |
| `CALNODE_PLATFORM_TOKEN` | bearer for `/v1/platform/*`. Unset ⇒ those routes 404 |
| `CALNODE_SSO_SHARED_SECRET` | HMAC key for `/v1/auth/sso`. ⛔ **Required** in multi-tenant mode if Google or Microsoft login is configured: the callbacks hand off through it, and unset means every social login ends at `error=sso` |
| `TRUSTED_PROXY_CIDRS` | the networks whose forwarded headers are believed. ⚠️ Unset behind a proxy makes every tenant one rate-limit bucket (D14's IP half) |
| `CALNODE_ENCRYPTION_KEY` | as before, and ⛔ it must travel with any workspace moved between instances — import refuses a document whose `dek_fingerprint` differs |
| `BASE_URL` | the identity host: OAuth callbacks, `/oauth/*`, `/mcp`, `/v1/platform/*`, `/healthz`, `/readyz`, `/version`, `/metrics` |
| `PUBLIC_BASE_URL` | **ignored** in multi-tenant mode; each workspace's `public_host` replaces it |

Per workspace, through `POST /v1/platform/workspaces`: `public_host` (its own hostname, DNS
pointed at the instance) and the `defaults` block. The response's `api_key` and
`webhook_secret` are shown once.

At boot, in this order: migrate on the platform handle → `EnableRLS` → suspend the `default`
workspace → `VerifyRoles` on the application handle. The first, second and fourth are fatal on
failure; the third is logged.

## The cost of a tenant, measured (B7)

`internal/server/rss_proof_test.go`, opt-in behind `CALNODE_RSS_PROOF=1`. It provisions 200
workspaces through the real platform API in one process, serves one request to each on its own
public host, and reads `/proc/self/status`. A test rather than a script because the number wanted
is the RSS of a process that has the handler, the caches and the pool in it.

| | VmRSS |
|---|---|
| baseline (handler + worker, no tenants) | **28 936 KB** |
| after provisioning 200 workspaces | **33 676 KB** |
| after one request to each | **35 860 KB** |

**Growth: 6 924 KB for 200 tenants — 34.6 KB each.** `runtime.MemStats.Sys` 39 354 → 43 722 KB.
Pool after all of it: application `open=1 idle=1`, platform `open=1 idle=1`.

⛔ **This CORRECTS the plan's claim, which said "a few MB per tenant".** It is a few MB for
*hundreds*: ~7 MB bought 200 workspaces including their settings rows, owners, keys, webhooks,
event types and availability. The per-tenant cost is the cache entries plus the workspace row, and
`ForWorkspace` returning a value over a shared pool is why the connection count does not move —
200 tenants, one connection each side. The test asserts both: a ceiling of 1 MB per tenant (a
per-workspace allocation that never came back would breach it) and a ceiling of 20 pool
connections.

## The two known follow-ups

1. ⛔ **Per-tenant DEKs.** `crypto_keystore` is exempt and holds one wrapped DEK per process
   (D3), so every tenant's secrets are encrypted under one key: an operator who can read the
   database can decrypt every workspace, and a workspace cannot be moved between instances
   without moving the key. Import's `dek_fingerprint` check makes the coupling loud rather than
   silent, which is as far as this packet goes. Making it per tenant changes D3, the schema
   (`crypto_keystore` gains `workspace_id`), and the export format.
2. ✅ **The D7 reader wiring for `embed_allowed_origins` and `stt_base_url`** — done after
   B7, see the section above. One known follow-up remains in this list.

⚠️ This file carried a plan for the SSO mint half between the merge and part three above.
It is now BUILT, and part three is the record; the plan is deleted rather than annotated,
because a plan left beside its implementation is the next reader's wrong answer. Two of its
three points survived contact unchanged (the callback to change, and the `<nonce>|<wid>`
state cookie). The third did not: the plan had the SSO endpoint resolving its workspace from
`wid`, and doing that on a public host is what part three had to fix.

---

## The D11 plan this file used to carry

⚠️ Deleted rather than annotated once it was built: a plan left beside its implementation is the
next reader's wrong answer. Two of its three points survived contact unchanged (the callback to
change, and the `<nonce>|<wid>` state cookie). The third did not — it had the SSO endpoint
resolving its workspace from `wid`, and doing that on a public host is exactly what part three
had to fix.

---

## `return_to` on the calendar OAuth round trip (`feat/calendar-return-to`)

A platform that has replaced the `/admin/` SPA with its own pages still sends the person
through this instance to connect a calendar: the OAuth redirect needs the session cookie
that lives here. The callback then finished on `scoped.publicURL()+"/admin/calendar"`, a
page that platform does not show, and answered every failure as JSON at a URL the browser
is looking at. `GET /v1/calendar/connect?provider=…&return_to=…` now names where the round
trip should end instead.

**No new routes.** `179 routes: 31 host-scoped, 107 credential-scoped, 33 platform, 8
allowlisted`, unchanged — this is two existing handlers and one env var, and
`routes_classified_test.go` is green without an edit.

### The contract

| | |
|---|---|
| connect | `GET /v1/calendar/connect?provider=<name>&return_to=<absolute URL>` |
| refused | `400 {"error":"return_to origin not allowed"}` |
| success | `302 <return_to>?calendar=connected` |
| failure | `302 <return_to>?calendar=error&reason=<code>` |
| codes | `provider_denied`, `missing_code`, `exchange_failed`, `invalid_state` |
| config | `PLATFORM_RETURN_ORIGINS`, comma-separated `scheme://host[:port]` |

With `PLATFORM_RETURN_ORIGINS` unset the parameter is refused and everything else is
byte-for-byte what it was: the JSON errors, the `/admin/calendar?connected=true` redirect,
and the two-field state.

### Three decisions, and why each is the one that was taken

⛔ **Off means REFUSED, not ignored.** A `return_to` against an instance nobody configured
is a 400 at the first attempt. Ignoring it would land the person on this instance's
`/admin/calendar` and present as a bug on the platform's side, in the one place where
nobody is watching a log.

⛔ **The destination only ever comes out of the CIPHERTEXT.** The origin is checked against
the allowlist at CONNECT time, on an authenticated request, and the accepted value travels
inside the encrypted state. The callback is a public route, so a `return_to` on its own
query is attacker-controlled and is ignored — and a state that does not decrypt has no
trustworthy destination at all, so it answers today's JSON rather than the query's
suggestion. Both have their own test. The match is the whole origin, byte for byte: a
prefix comparison accepts `https://console.example.com.evil.test`, and an open redirect out
of an OAuth callback is a better prize than most bugs in a scheduler.

⚠️ **The state's third field is written only when it is non-empty**, rather than always
emitting `provider\x1fuserID\x1f`. The parse side takes at most two separators and treats
one, two and three fields alike, so a round trip in flight when the deploy lands still
completes — but the OTHER direction of a rolling deploy is the reason: an older binary
parses with a single `strings.Index` and would read a trailing separator as part of the
user id, exchanging against a user that does not exist. A `return_to` containing the
separator is refused before it is encoded (`url.Parse` rejects ASCII control characters
anyway, so the explicit guard has its own test rather than resting on that accident).

### The ordering that is easy to get wrong

The `?error=` branch had to move after the decryption — a denied consent with a
`return_to` has to go home rather than answer JSON — but it is handled **before** the
state is judged invalid. A denial arrives with no state at all when the user clicks
Cancel, and today's answer to that is `OAuth error: access_denied`, not `invalid or
missing state`. Moving the branch to the end of the decrypt block changes it silently;
both directions are pinned.

### What the tests run against

A **stub provider**, not `gcal`: the failure this feature is mostly about is a failed token
exchange, and `gcal.Exchange` is an HTTP POST to Google. It keeps the real AES-GCM
`EncryptState`/`DecryptState` (a `caldav.Client` needs no OAuth app to construct), so every
state assertion goes through the real crypto rather than the encoder alone.

⛔ **The multi-tenant half asserts the ROW, not the redirect** (`TestPostgres_*` in
`calendar_tenancy_test.go`, through `h.Platform(…)` because the wrapper is the shape under
test). A 302 looks identical whether the exchange ran on the workspace's bound handle or on
the unbound platform one and wrote nothing. It caught two real failures during development
— and the second is worth writing down: `calendar_connections.provider` carries
`CHECK (provider IN ('google','microsoft','caldav'))`, so a stub under an invented name is
refused by the database and the callback reports `exchange_failed`, which reads exactly
like the bug the test exists to find.

Gates: `gofmt -l ./internal ./cmd` clean, `go vet ./...`, `go build ./...`,
`go test ./...` on SQLite, and the handler/config/server packages on PostgreSQL.

## F4 — `GET /v1/bookings/{id}` requires a credential

The last route under `/v1/bookings/` that anyone could call. It was
`h.Scoped(handler.HostWorkspace, (*H).GetBooking)` with no `RequireAuth`, so reaching a
tenant's public host and holding a booking id was the whole capability, and the body it
returns is the attendee's name, email and intake answers. D-era work bounded the blast
radius to one workspace (the section above) and deferred closing it, on the premise that
the booking page might depend on it.

### The premise was checked, not assumed

`grep -rn "/v1/bookings/"` over `frontend/src`, `internal/handler/templates` and
`internal/handler/*.go`. Every hit, and why none is public:

- `frontend/src/routes/{bookings,members,recordings}/+page.svelte` — nine hits, all
  **sub-paths** (`/answers`, `/cancel`, `/reschedule`, `/reassign`, `/notes`,
  `/notes/regenerate`, `/transcript`) and all from the admin SPA, which sends a
  credential on every call. None is the bare `{id}` read.
- `internal/handler/templates/book.html:665` and `internal/handler/embed.js:533` — both
  are `POST /v1/bookings`, the public create, untouched here. **Neither follows it with
  a read**: the confirmation view is rendered from the POST response body, which is
  exactly why closing the GET is invisible to the booker.
- `internal/handler/templates/manage.html` — no hit at all. The cancel/reschedule links
  in the four emails land on `/manage/{token}`, and the page calls
  `/manage/{token}/{reschedule,cancel}` plus the public slots endpoint. A separate
  token-bearing surface, as documented.
- `internal/handler/embed.js`'s `api()` helper is the only other way the widget can
  fetch, and its three call sites are `/public`, `/questions` and `/slots`.
- The paid path does not read it either: `stripe_booking.go` sends Checkout back to
  `/book/{slug}?paid=1`, not to a booking id.
- Everything else is a `_test.go`, a doc, or the route's own registration.

### The change

```go
mux.HandleFunc("GET /v1/bookings/{id}", h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*H).GetBooking)))
```

Host-scoped routes 31 → 30, credential-scoped 107 → 108. ⚠️ **Those totals are logged,
not pinned.** `routes_classified_test.go` asserts a floor (`len(routes) >= 150`,
`checked >= 90` credential routes) and a non-triviality check (each class ≥ 3), then
`t.Logf`s the counts; the only exact-set pin is `TestPlatformRoutesAreTheIdentityHostSet`,
and this route is not platform. So nothing in that file needed editing, and nothing there
would have caught the move either — the gate that does is
`TestCredentialScopedRoutesAuthenticateFirst`, which now covers this line and insists
`RequireAuth` wraps `Scoped` rather than the reverse.

### Two refusals, and they are not the same one

- **No credential → 401** (`authentication required`, from `RequireAuth`).
- **A credential from another workspace → 404.** `CredentialWorkspace` binds the handle
  to the caller's own workspace, so another workspace's booking is a row that does not
  exist. Not a 403: a 403 would confirm the id.

Both are asserted. `TestGetBooking_requiresAuth` (SQLite, `internal/handler`) covers
401 / bad key 401 / own key 200 / unknown id 404, and checks the 401 **body** as well as
its status, since a 401 that still serialised the row would pass a status-only test.
⚠️ It was `TestGetBooking_public` and asserted the opposite; it also called the bare
`h.GetBooking`, so it would have gone on passing untouched after the route was closed —
it now drives `h.RequireAuth(h.GetBooking)`.

The cross-workspace half needs two workspaces and a `NOBYPASSRLS` role, so it stays in
the Postgres suite: `TestTenancy_bookingByIDRequiresACredentialOfItsOwnWorkspace`
(formerly `TestTenancy_bookingByIDIsNotFoundAcrossWorkspaces`, whose first assertion was
that the anonymous read answered **200**). It adds the mirror case — B's own key on A's
host must not read B's booking — which is the one that would still pass if the route had
gained `RequireAuth` while staying `HostWorkspace`.

### What F4 deliberately did not change

`GetBooking` carries no per-user check, so any authenticated member of a workspace can
read any booking in it by id. Its siblings do not agree on that point either
(`GetBookingAnswers` and `RescheduleBooking` are host-only, `CancelBooking` is
admin-or-host, `GetBookingNotes` is deliberately admin-wide and has an
`audit/claims.yaml` claim saying so), so narrowing it is an access-model decision that
would want the MCP `get_booking` tool moved in the same commit. Recorded in the handler's
doc comment rather than fixed here.

## F3b — the booking page's three accessibility findings, on all three surfaces

A browser audit of the live `/book/phone-consultation` pages returned three axe
violations. All three live in the booking UI, so all three had to be answered on the
booking page, the manage page **and** the embed widget — the rule in CLAUDE.md.

### 1. `aria-required-children` (critical) — the calendar was not a grid

`_shared.html`'s `calendarGrid` partial put `role="grid"` on `#cal`, and `#cal` holds a
**flat** list: seven `.ch` header cells, blank spacers for the month's start offset, then
one `<button class="cd">` per day. The weeks are produced by
`grid-template-columns: repeat(7, …)` and by nothing else — there is no row in the DOM on
any surface. `grid` promises `role="row"` children holding gridcells, so a screen reader
was told to expect rows, found none, and announced an empty grid.

⛔ **Real rows were considered and rejected twice over.** A per-week wrapper becomes the
CSS grid item and collapses the layout unless it takes `display: contents`, which has a
history of dropping the element's role from the accessibility tree — the same class of bug
this fix exists to remove. And `grid` carries a keyboard contract (arrow-key roving
tabindex across cells) that none of the three surfaces implements; declaring it while
shipping plain Tab order is a worse lie than declaring nothing.

`role="group"` + `aria-labelledby="month-label"` describes what the markup honestly is: a
labelled set of day buttons. `#month-label` is already `aria-live="polite"`, so the visible
month is both the group's name and announced on change, and the enclosing `<section>`
carries the static "date picker" label — the pair reads "Date picker → September 2026".

⚠️ **The widget had the opposite problem and it would have been missed by fixing only what
the audit named.** `embed.js` set no role at all, so its calendar announced nothing. It now
takes the same group role with the month string as `aria-label` (it rebuilds the pane on
every month change, so there is no id to point at), its month span gained the `aria-live`
the pages had, and its `.cal-col` gained the `date_picker_aria` label the pages had.

### 2. Colour contrast (serious) — and the finding was mis-attributed

⛔ **The primary button's hover fill does NOT fail, and the packet brief said it did.**
Measured from the shipped hex rather than accepted: `--bk-on-primary` on
`--bk-primary-hover` is **14.68:1** on booking.css's neutral defaults (`#ffffff` on
`#1f2937`) and **9.41:1** on the pages' Distronode palette (`#ffffff` on `#2c24cc`). There
is also no dark scheme on these surfaces to check it in — no `prefers-color-scheme` block
exists in `booking.css` or either page.

What does fail is `--bk-subtle`, and it is the worst contrast on the surface. It is not
decoration: it is the text colour of `.ch` (the day-of-week headers), `.tz-label`, `.hint`,
the timezone `<select>`, and manage's `.scheduled-label` / `.row-label` — all real 11px
copy on the card's `#fff`.

| palette | token | before | after |
|---|---|---|---|
| booking.css defaults (widget) | `--bk-subtle` | `#9ca3af` — **2.54:1** | `#697382` — **4.80:1** |
| book.html / manage.html (Distronode) | `--bk-subtle` | `#8b8b9c` — **3.35:1** | `#6e6e7c` — **5.02:1** |

Against `--bk-elevated` the new values are 4.59:1 and 4.89:1; on the pages' `--bk-page-bg`,
4.77:1. ⚠️ On the neutral default palette the subtle tier now sits very close to
`--bk-muted` (4.83:1). That is the palette's doing rather than the fix's — that set has no
room for a third grey above 4.5:1 — and the Distronode palette, which does have room, keeps
a visible gap. Re-spacing means darkening `--bk-muted` first.

⛔ **The widget's `.powered` line held the `#9ca3af` LITERAL rather than the token**, so
correcting the shared sheet alone would have left exactly one string on one surface still
at 2.54:1. It now reads `var(--bk-subtle)`.

Not touched, deliberately: `.cd:disabled` (1.69:1) and `.slot-btn.taken` (3.27:1) are
disabled controls, which WCAG 1.4.3 exempts, and both have to keep reading as unavailable.

### 3. `landmark-one-main` (moderate) — no `<main>` on either page

`<main class="card">`, not `<main><div class="card">`. The card is a flex item of the body's
column layout and carries `width:100%` / `max-width:860px` plus the mobile step-flow classes;
an extra wrapper would have become the flex item and changed the box. Putting the landmark on
the card is layout-neutral — every `.card` rule still applies and `document.querySelector('.card')`
still finds it.

⚠️ manage.html has **two** cards, the arms of one `{{if .TokenInvalid}}`. Both are `<main>`,
which yields exactly one per rendered document; making them siblings would trade one violation
for another, and the test renders all four states (valid / expired / cancelled / book) and
counts.

⛔ The embed adds **none**. It renders into a customer's own document, which has its own
`<main>`; a second one there would be this same violation on someone else's page, where nobody
reading this repo would ever find it.

### Tests, and the one that nearly lied

Three new tests in `booking_surfaces_contract_test.go`, next to the three-surface contract
they extend. `TestBookingSubtleTextMeetsContrast` **computes** WCAG relative luminance in Go
from the token values parsed out of the shipped bytes — not from a table copied into the test,
which would keep passing after the palette changed — and it also pins the button hover the
brief named, so the "it does not fail" claim above stays true rather than remaining an
observation. Each was verified to fail when its fix is reverted: role restored → 4 errors,
tokens restored → the exact 2.54 / 3.35 above (a second implementation agreeing with the
first), `<main>` removed → 2 errors.

⚠️ **`TestBookingCalendarIsAGroupNotAGrid` is a SOURCE scan, and its first run redded on a
COMMENT.** An explanatory comment in `embed.js` quoted the forbidden attribute in prose, and
the scan cannot tell prose from markup. The comment was reworded and now says why. Same shape
as the monolith's `main-landmark.test.ts`, which blanks block comments and not `//` ones: near
a source-scanning gate, never spell out the token the gate forbids.

### Not done

No `next dev`-style visual verification: this worktree has no browser and the packet is
template-local. The layout claims above rest on the CSS being unchanged and the landmark
being placed on an existing element rather than around it, plus the full SQLite suite; a
desktop and mobile pass on each of the three surfaces is still worth doing before release,
which is what CLAUDE.md asks for after any calendar change.

## F3 — `ADMIN_SPA=off`, so the platform's console can be the only admin UI

A multi-tenant deployment whose own dashboard already carries every admin surface does not
want a second, parallel one on each tenant host. `ADMIN_SPA=off` removes the embedded
SvelteKit console: `GET /admin`, `GET /admin/` and every path under it, plus the bare-root
redirect `GET /{$}` that leads there, answer 404. Nothing is deleted — not the SPA, not its
embed, not the routes — and with the variable unset the binary behaves exactly as it did
before it existed.

### The registrations do not move, only the handlers

`server.go` still calls `mux.Handle` three times with the same three patterns; what changes
is which handler each one gets. Three reasons, and the first is the one that decided it:

- `routes_classified_test.go` is a SOURCE scan of `server.go`, and a route that exists in
  one configuration and not another is a route no gate can classify. The totals are
  unchanged and asserted: 184 routes, 30 host / 108 credential / 38 platform / 8
  allowlisted.
- A 404 from a handler goes out through the whole middleware chain — request id, logging,
  `SameOriginCheck`, the security headers. A mux with nothing registered on the pattern
  would answer its own 404 outside all of it, so the switch would quietly change what a
  404 on this instance looks like.
- `/favicon.ico` is registered separately from the same embedded source, and the `/v1` tree
  is registered elsewhere entirely. Both keep their handlers; the console is removed, not
  the instance's public surface. The test asserts `/favicon.ico` still answers 200 and
  `/v1/event-types` still answers 401 rather than 404.

### Single-tenant ignores it, and the rule lives in exactly one expression

A self-hoster has no other admin UI, so `ADMIN_SPA=off` on a single-tenant instance would
lock the operator out of their own installation. `config.AdminSPAEnabled()` is
`!MultiTenant || AdminSPA`, and it is what both the route registration and the SSO hand-off
consult; `cfg.AdminSPA` keeps the value the environment asked for so boot can WARN that it
did nothing rather than pretend it was never set. Two callers each remembering the rule is
how the two halves would come to disagree, and the disagreement that matters is the one
that locks somebody out.

⚠️ `TestAdminSPA_offIsIgnoredInSingleTenantMode` was verified to fail when the registration
reads `!cfg.AdminSPA` instead, which is the shape this is guarding against.

### `on`/`off`, and why `true` is refused

The value is a switch, so it is spelled like one. ⛔ The brief said "parse like the other
booleans", and the other booleans go through `strconv.ParseBool`, which accepts
`true/false/1/0/t/f` and **not** `on`/`off` — the two halves of that instruction cannot both
be honoured. The explicit contract won: `on` and `off` (case-insensitive, trimmed) are the
only accepted values and everything else, `true` and `false` included, is refused by
`Validate` at boot.

That refusal is load-bearing rather than pedantic. `Load` has no error return and the
fallback for this one is **on**, so a value nobody can read exactly would serve the console
the operator wrote the variable to remove, with nothing anywhere saying why. It is the same
family as `FRAME_ANCESTORS` and `PLATFORM_RETURN_ORIGINS` — a malformed value that fails
OPEN — which is why `Config` keeps the raw string: without it `Validate` has nothing left to
object to. The check runs before the `!MultiTenant` early return, because a typo is a typo
in either mode.

### SSO: refuse before the session, not after

`ssoDefaultNext` is `/admin/`. With the console off, a hand-off carrying no `?next=` would
mint a session, spend the token's `jti` and redirect the person into a 404 on a host whose
console lives somewhere else — and because the token is single-use, they could not retry.
So the handler answers 404 **before** the nonce is claimed, in the same position as the
existing `?next=` validation and for the same stated reason. The test asserts the negative
directly: no `calnode_session` cookie, `sessions` empty, and `sso_nonces` empty, so the same
token would still work with an explicit destination.

An explicit `next` is untouched, including one naming `/admin/`. The platform's calendar
connect round trip (F2) is exactly that case, and this endpoint is not the place to
second-guess a path that has already been origin-checked.

⛔ **The handler's flag is stored NEGATED (`adminSPAOff`), and that is the whole of its
safety.** The zero value has to mean "the console is served", because every handler built
without `SetAdminSPA` — every other test in the package, and any future entry point that
forgets the call — must keep behaving as it always did. Stored in the positive sense, one
forgotten setter would 404 the hand-off on an ordinary single-tenant instance, and the
symptom would appear nowhere near the omission. `TestSSOHandoff_defaultsToServingTheConsole`
pins it.

### Tests

Nine new cases across three packages, each verified to fail with its own fix reverted:

- `internal/server/adminspa_test.go` — `TestAdminSPA_defaultConfigServesTheConsole` (the
  three routes as they are today: 301, SPA bytes, 302),
  `TestAdminSPA_offInMultiTenantModeIs404` (all three plus two sub-paths, with
  `/favicon.ico` and `/v1/event-types` proving the blast radius),
  `TestAdminSPA_offIsIgnoredInSingleTenantMode`. They drive the REAL mux rather than
  `frontend.Handler()` directly, which is what `frameancestors_test.go` does — a test on the
  handler alone cannot tell a 404 that came through the middleware chain from a pattern the
  mux never had.
- `internal/config/tenancy_test.go` — `TestLoad_adminSPADefaultsOn`,
  `TestLoad_adminSPAOffAndOn`, `TestValidate_rejectsAnUnreadableAdminSPA`,
  `TestAdminSPAEnabled`.
- `internal/handler/sso_test.go` — `TestSSOHandoff_withoutAdminSPAAHandoffWithNoNextIs404`,
  `TestSSOHandoff_withoutAdminSPAAnExplicitNextIsUnchanged`,
  `TestSSOHandoff_defaultsToServingTheConsole`.

⚠️ **A `config.Config` literal skips `Load`, so its zero value is `ADMIN_SPA=off`** — which
on a multi-tenant literal now means a 404 where the fixture meant an ordinary instance. The
two multi-tenant literals in this package (`newTenancyFixture`, the RSS proof) restate
`AdminSPA: true` so the Postgres lane keeps exercising the shipped default. Nothing in
either suite asserts on `/admin`, so this changed no result; it is there so the next
addition to those fixtures is not debugging a 404 it never asked for.

### Not done

`docs/MULTI_TENANT.md` gained the row and a section; `DEPLOY.md` did not, following
`PLATFORM_RETURN_ORIGINS`, which is likewise multi-tenant-only and documented in one place.
No browser verification: the switch is a status code and every surface it touches is
asserted through the real mux.

## F6 — the hardening release: what a tenant credential cannot do

Six findings from the platform-side audit, all of them "this is correct for a self-hoster
and wrong for a multi-tenant instance". Every one is gated on `MultiTenant` and
single-tenant behaviour is unchanged throughout, which is the gate this fork has always
held itself to.

### H1 — the instance-credential settings routes

`server_settings` is per workspace, so `GET|PATCH /v1/settings/{email,google,zoom,livekit,
stripe}` looked self-scoped. What each one CONFIGURES is the process's: the SMTP account
every tenancy sends through, the OAuth client every tenancy's calendar connect and login
uses, the Zoom app, the LiveKit server, the Stripe account.

`PatchGoogleSettings` made it concrete. It hot-reloaded `calendar.Service` and the Google
OAuth config on `*shared`, which every per-request copy of the Handler points at — so one
workspace's PATCH re-pointed every OTHER workspace's calendar connect and OAuth login
until the next restart, and `client_id: ""` switched Google calendar off instance-wide.

Two guards, deliberately. `h.PlatformManaged` at the REGISTRATION answers 403
`managed_by_platform` before `RequireAuth` spends a database round trip on a request that
cannot succeed; and the hot-reload block itself is skipped in that mode, because the first
guard is one line in another package and the blast radius of losing it is every tenancy on
the process.

⛔ **The platform's own op catalog was never the boundary.** It excludes these routes, and
its Developer tab mints a raw `cno_` key that `RequireAuth` accepts on every credential
route — so the catalog protects the console and not the instance.

`PATCH /v1/settings/llm` is guarded by FIELD rather than by route, because the page carries
both: `enabled` and `extra_instructions` are the workspace's and are the only two the
catalog offers, while `endpoint`, `model` and `api_key` name the model provider the
platform pays for. `h.PlatformManagedFields` reads the body, decides on key PRESENCE, and
hands an equivalent reader back. `{"api_key":""}` is refused too — an empty string is how
the handler spells "keep the stored one", so a guard that looked at values would let the
least visible of the three writes through. `GET /v1/settings/llm` stops returning the same
three; `configured` and `active` stay, because "will the summariser run" is a question a
tenant admin legitimately has.

⛔ **Two more routes were found during review of this packet and are in the release, not
deferred.**

`POST /v1/settings/llm/test` is blanket-guarded, and it is the sharpest route in the set:
it dials whatever `endpoint` the body names, and an empty `api_key` makes it read the
STORED key and dial with it — a request to a tenant-chosen host carrying the platform's
credential, with only the metadata-only guard in front of it rather than `ResolveSafe`.
Blanket rather than split, because once the PATCH's credential fields are managed every
value this route takes is the platform's.

`PATCH /v1/settings/notetaker` splits like the LLM PATCH. `stt_api_key` is written to the
singleton `server_settings` row, so a tenant-supplied speech-to-text credential is what
every OTHER tenancy on the deployment would then transcribe through — one workspace paying
for, and able to read the usage of, everyone else's call audio — while the `enabled` toggle
beside it is the workspace's own and is the only thing the console offers on that page,
which is why refusing the route was not an option. `GET` drops `stt_api_key_set` and
`stt_base_url`. ⚠️ The URL is the less obvious of the two: `sttBaseURL()` answers the
workspace's value when it has one and the process value otherwise, and the response cannot
say which — so a workspace that never set one read the instance's vendor host back as if it
were its own.

### H3 — the console off-switch, and the bypass it shipped with

The `ADMIN_SPA=off` guard refused only a hand-off with NO `?next=`, on the reasoning that
an explicit destination is the caller saying where to land. That read as deference and was
a bypass: both native clients send `next=/admin/` on every hand-off, so after a flip the
guard would have fired for nobody.

What the person got instead was the exact failure it exists to prevent, one step later —
token verified, single-use `jti` spent, session cookie set, sheet landing on a bare 404 —
and neither app could report it, because only a non-3xx opens their error path. A probe
asserting the hand-off answers 3xx stays green straight through the flip.

A `next` whose normalised path is `/admin` or under it now answers 404 before the nonce is
claimed. `nextIsAdminConsole` normalises the way the mux and `http.Redirect` do — `url.Parse`
to strip the query and percent-decode, `path.Clean` to resolve `..` — so `/%61dmin/` is
refused and `/admin/../book/intro` is allowed, because it genuinely lands outside the
console. Case-SENSITIVE, because `ServeMux` matches bytes and `/ADMIN/` is not the console.

### M1 — the CalDAV client's SSRF tier is now a property of the instance

`server_url` is a bring-your-own-server field, and the narrow metadata-only guard is
correct for a self-hoster: a Nextcloud, Radicale or Baïkal on their own LAN is the intended
configuration. Every term of that inverts here — the string is a TENANT's, the private
network it reaches is the OPERATOR's (the k3s service range, node-exporter,
postgres_exporter, Alloy, the website pod, the media plane), and `calendar.caldav.connect`
is a viewer-level op — so `caldav.WithStrictSSRFGuard(cfg.MultiTenant)` routes every dial,
and every redirect hop, through `netutil.ResolveSafe`.

⛔ **And a second, separate check answering a different question: does the CREDENTIAL
travel in clear?** CalDAV authenticates with HTTP Basic, so the app-specific password is on
the wire in every request; over `http://` to a permitted PUBLIC host that is a disclosure
the address guard has no opinion about. `ConnectCalDAV` therefore hands
`validateBYOServerURL` only `https` in this mode — single-tenant keeps `http`, where it is
the operator's own password on their own network — and it runs BEFORE the dial, because
after it would be a nicer error message on a password already sent. Both presets are https,
so the picker path is untouched.

⛔ **The error text is half the fix, not a detail.** `ConnectCalDAV` writes `err.Error()`
straight into a 400 for the connect form, so a refusal naming the blocked address would
hand the caller the oracle the guard closes — and with a hostname resolving several ways,
which address it picked. A blocked dial now produces the sentence discovery has always
produced for an unreachable server, verbatim. `netutil.ErrBlockedAddress` is a sentinel so
the mapping is one `errors.Is` through `*url.Error` rather than string matching.

### M2 — limiter buckets key on the credential, not the address

D14 made a bucket `(workspace, client IP)` so two tenants' bookers behind one address could
not share an allowance. The same failure was live one dimension over: a platform reaches
every tenant host from one egress address per region, so all authenticated traffic to a
given tenant shared one 20-per-minute budget and the busiest caller spent it on everyone
else. The workspace prefix cannot help, because it is the traffic to ONE tenant host that
shares the address.

The caller half is now a SHA-256 prefix of the credential the request presents —
`X-API-Key`, an `Authorization` bearer, or the session cookie, in `RequireAuth`'s own
precedence — and the resolved client IP when there is none. Hashed and read straight off
the request, because the limiter runs before `RequireAuth` and must not do a database read
to reject. It keys on what was PRESENTED rather than the user it resolves to, so an invalid
key gets its own bucket instead of falling in with the anonymous ones. The two halves are
namespaced `c:` and `ip:` so a hash cannot collide with an address.

### L1 — https-only webhooks

A booking payload carries the attendee's name, email address and intake answers. Plaintext
delivery stays legal single-tenant (their data, their network) and is 400 here. Only create
is affected: `PATCH /v1/webhooks/{id}` has no `url` parameter to guard.

### L6 — `head_html` refused, and ignored

The field injects raw HTML into the `<head>` of the workspace's booking pages and RELAXES
that page's CSP to fit it — on the operator's domain, on a page that collects card details.
A non-empty value is 400 `managed_by_platform`; empty passes, because clearing it is what
this mode wants. Refused rather than stripped: a tenant who believes their tag is installed
and sees no traffic has the worse problem.

⛔ **The write check alone would have been half a fix.** A row can already hold the value
— provisioned earlier, restored by an import, carried over from an instance later switched
over — and nothing would stop it rendering until someone saved the page. `loadTrackingSettings`
blanks it at READ time, the one chokepoint every reader goes through, which is also what
keeps the CSP strict: `publicCSP` decides on the same struct, so markup and policy have no
second place to disagree.

### Tests

No route was added or removed: the classification gate still reads **184 routes — 30
host-scoped, 108 credential-scoped, 38 platform, 8 allowlisted**.

- `internal/server/routes_platform_managed_test.go` — the new classification-style gate.
  `TestInstanceCredentialRoutesAreGuardedByPlatformManaged` asserts in BOTH directions (a
  guarded path that loses its wrapper, and a tenant-safe path that gains one),
  `TestEverySettingsRouteHasATenancyDecision` fails on a `/v1/settings` route in neither
  table (26 routes: 14 platform-managed, 12 tenant-safe),
  `TestNoPlatformManagedGuardOutsideItsTable` catches a stray guard.
- `internal/handler/platform_managed_settings_test.go` — the twelve blanket-guarded routes
  in both modes, the tenant-safe four still answering 200 under `MultiTenant`, the LLM and
  notetaker field splits (each including the empty-string case), the body surviving the
  wrapper, and both GET redactions. ⚠️ Named `…_settings_test.go` because
  `platform_managed_test.go` already exists and is the F1 managed-ROWS suite; the two are
  about different things that share a word.
- `internal/handler/caldav_connect_tenancy_test.go` — the connect form's scheme in both
  modes, https accepted in both, and the ordering case that proves the scheme check runs
  before the dial (it uses an address the dial-time guard would also refuse and asserts the
  SCHEME complaint comes back).
- `internal/handler/google_settings_tenancy_internal_test.go` — the hot reload, asserted by
  calling the handler DIRECTLY past the wrapper, which is what a refactor that dropped it
  would produce. Internal because `calBase`/`googleAuth` are unexported on purpose.
- `internal/handler/sso_test.go` — `…AnExplicitAdminNextIs404` (seven shapes, each
  asserting the `jti` is unspent), `…AnExplicitNextIsUnchanged` (four, including the
  climbing case), `…withTheConsoleOnAnAdminNextStillLands`.
- `internal/caldav/ssrf_test.go` — seven IP literals through the REAL resolver, a name
  through a stub (the rebinding case a literal cannot cover), a public address still
  connecting in the same mode, and the narrow default still allowing loopback.
  `assertRefusedWithoutDisclosing` pins that the sentence names no address and no reason.
- `internal/server/ratelimit_credential_test.go` — two bearers from one address, two
  sessions, anonymous still keyed on the address, credential and anonymous not sharing,
  the workspace dimension surviving, single-tenant unchanged, and the key's shape.
- `internal/handler/webhook_test.go`, `tracking_tenancy_test.go`,
  `tracking_settings_internal_test.go` — L1 and L6 in both modes, including the stored-row
  read and the CSP that must stay strict.

### Not done

- No migration, no schema change, no frontend change, no `go.mod` change.
- `DEPLOY.md` is unchanged, following `PLATFORM_RETURN_ORIGINS` and `ADMIN_SPA`: all of
  this is multi-tenant-only and documented in one place.

---

## F3a — vhost hardening, a neutral root, and metrics a collector can reach

Four findings from the same platform-side review, three of them about what a browser is
told and one about what a scraper is refused. None is gated on `MultiTenant` except the
neutral root, which is reached only through a multi-tenant-only switch.

### M9 (1) — the security headers existed only where a handler remembered them

The finding is not that a page was missing a header. `book.html`, `manage.html` and the
SPA each set a subset, and **everything else set none**: `/embed.js` and `/booking.css`,
which third-party sites load, every JSON error on the `/v1` tree, and every 404. A
per-handler header is a header that is absent from whatever nobody edited.

`server.SecurityHeaders` sits at the mux root, inside `Logging` and outside
`SameOriginCheck` — the only position that covers the mux's own 404s AND the CSRF check's
403. Four headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy:
strict-origin-when-cross-origin`, `Permissions-Policy: camera=(), microphone=(),
geolocation=()`, and `Strict-Transport-Security: max-age=31536000; includeSubDomains`.

⚠️ **Set BEFORE calling through, which is what makes it set-if-absent.** A handler that
`Set`s one of these keys overwrites it, so the LiveKit room keeps its own
`Referrer-Policy: no-referrer`. The alternative — a wrapping `ResponseWriter` filling gaps
at `WriteHeader` time — buys nothing and costs the `Flush` transparency the logging
wrapper already had to restore by hand.

⛔ **The room is the `Permissions-Policy` exception** (`camera=(self), microphone=(self),
geolocation=()`, chosen by path prefix). It is a video meeting, and a Permissions-Policy
denial is not recoverable from JavaScript — `getUserMedia` rejects and the page has no way
to ask again.

⛔ **HSTS is conditional and the condition is not cosmetic.** Only on a request that
arrived over TLS: a real connection, or `X-Forwarded-Proto: https`. Browsers ignore HSTS
on a plain-http response anyway, but this binary is also run by self-hosters on a LAN, and
an accidental `includeSubDomains` pinned against a hostname reachable only over http locks
that operator out of their own installation for a year with nothing to retract it with.

⚠️ **The forwarded header is believed WITHOUT consulting `TRUSTED_PROXY_CIDRS`**, unlike
the rate limiter's client IP, and the asymmetry is deliberate: forging it buys an HSTS
header browsers honour only over https, i.e. only where it was true anyway, while
requiring the allowlist would leave HSTS off on every deployment that has not set one.

### M9 (2) — `'self'` was missing from the strict `script-src`

`strictPublicCSP` read `script-src 'unsafe-inline'`. A CSP source list is an allowlist and
`'unsafe-inline'` permits inline code and nothing else, so **every same-origin `<script
src>` on a booking page was refused**. Behind Cloudflare that is the console error on every
booking page: the edge injects `/cdn-cgi/challenge-platform/scripts/jsd/main.js` as a
same-origin script, it was blocked, and the bot signal it feeds was silently absent. The
relaxed `publicCSP` has always carried `'self'`, so this also removes an asymmetry where
turning a tracking tag ON made the policy accept MORE of our own origin than the strict
default did.

⛔ **The suite could not have caught it.** Every test compared `publicCSP(...)` to
`strictPublicCSP`, so both sides moved together and nothing said what the policy actually
was. It is now pinned as a literal, plus a second assertion in the terms of the bug (both
policies must allow same-origin script), because a literal alone would still pass if
someone "tidied" the pair and updated the literal to match.

### M9 (3) — the neutral root

With `ADMIN_SPA=off`, `GET /{$}` was a bare 404 on a tenant's own public host. It now
lists the workspace's public, active event types as links to `/book/<slug>`, on
`booking.css` and the workspace's branding, translated, `noindex`, under the strict CSP.

⛔ **No route was added or removed**: the registration in `server.New` is the same line
with a different handler behind it, and `routes_classified_test.go` still reads **184
routes — 30 host-scoped, 108 credential-scoped, 38 platform, 8 allowlisted**. The
allowlist entry for `GET /{$}` gained the second handler in its reason string.

⚠️ **What it deliberately is not** is a marketing page or a workspace profile: no host
names, no descriptions, no counts. It publishes nothing the booking pages do not already
publish to the same audience, and `noindex` keeps the list from becoming a directory of an
operator's tenants assembled by a search engine (the booking pages it links to stay
indexable, which is what a workspace hands out a link for).

A workspace with nothing public renders the same page with one sentence. An empty
workspace is not a missing one, and the unknown-host 404 — still raised by `Scoped`, before
the handler runs — already carries that other answer.

⛔ **It reads through the workspace-bound handle with no workspace predicate in the SQL**,
which is D1, and it is the one public surface where getting that wrong shows up as someone
else's data ON the page rather than as a 404: every other host-scoped page names its
subject in the URL, so an unbound read there returns the wrong single row. Proven rather
than asserted — with the query swapped to `Platform()`,
`TestTenancy_theIndexListsOnlyItsOwnWorkspace` lists both tenants.

**The Distronode token block moved into a `districtTheme` partial** rather than being
copied a third time. It was byte-identical in `book.html` and `manage.html`; three copies
of a token table is how one surface drifts a shade, and the contrast reasoning on
`--bk-subtle` is worth having in one place. It stays a page partial and not a
`booking.css` rule for the reason `page_theme_test.go` asserts in both directions: the
embed widget loads that sheet on customers' sites and must not inherit our brand.

The page adds body chrome, a link reset so `.slot-btn` works as an `<a>`, and its own list
rule — `booking.css`'s `.slots-list` caps at 340px and scrolls, which is right inside a
card column and wrong for the only content on a page. Everything with a look comes from
the shared sheet. Checked in a real browser at **1280×900 and 390×844**: the card, the
rows and the footer match the booking surfaces, and at the phone width the card goes
full-bleed and the duration/location line stacks under the event name.

### I6 — `METRICS_ALLOW_UNAUTHENTICATED_FROM`

`GET /metrics` is bearer-gated and 404s without the token, which is right for a publicly
reachable endpoint and wrong for the one caller that has to read it. **A Prometheus
collector cannot hold this kind of secret**: Grafana Alloy's annotation autodiscovery sends
ONE bearer token file to every target it scrapes, so pointing it at `METRICS_TOKEN` would
present this instance's token to every other annotation-scraped pod on the cluster.
Measured: the fleet's scrape 404'd about **5,755 times a day** and the fork published no
metrics in any region.

The opt-in is the network position. Empty by default; set to the collector's networks
(the cluster's pod CIDR on a Kubernetes origin) and those requests are served with no
bearer. Nothing else moves — the bearer still works, and outside those networks the answer
is the 404 it always was.

⛔ **Matched against the TCP PEER, never a forwarded header**, and not the
trusted-proxy-resolved client IP the rate limiter uses. Here the address IS the credential,
so it has to be the one value in a request a client cannot choose; reading a header would
let anyone who can reach the endpoint claim to be the collector by asserting it. The
limiter can afford the opposite trade because a forged value there costs a shared bucket.
⚠️ The consequence to know before deploying it: the endpoint must not be reachable THROUGH
a proxy inside the allowed range, or every request arrives wearing that proxy's address.

### The locale strings, and the review they have not had

Three keys — `index_title`, `index_intro`, `index_none` — added to **all nine** locale
files. No printf verbs, so the format-parity guard is satisfied trivially; the same-keys
guard is what makes the nine mandatory.

⚠️ **The eight non-English translations are LLM drafts with no native review**, exactly as
CLAUDE.md says of every other non-English string in this repo. Structure is verified,
wording is not. Register was matched to each file's existing copy rather than invented —
`de` keeps Sie, `es`/`sv`/`nl` stay informal, `pt` stays European (`marcação`, not
`agendamento`) — but that is a draft's judgement about a draft. Say so before anyone
markets a language, and treat `fr-CA`'s divergence from `fr` (`offerte à la réservation`)
as the guess it is.

### Tests

- `internal/server/security_headers_test.go` —
  `TestSecurityHeaders_onEverySurfaceInBothModes` (a public asset, a `/v1` JSON 401 and a
  mux 404, in single- and multi-tenant; multi-tenant matters because its unknown-host 404
  comes out of the resolver rather than a handler, which is exactly what a per-handler
  header misses), `…HSTSFollowsTheRequestScheme` (six cases including a proxy chain in both
  directions), `…HSTSOnADirectTLSConnection`, `…theRoomPageKeepsCameraAndMicrophone` (and
  that the exception is scoped to the prefix), `…aHandlerKeepsItsOwnValue`. Verified by
  unwiring the middleware: every case fails, and the two that do NOT are informative —
  `nosniff` is already present on a `ServeMux` 404 and on the unknown-host HTML 404.
- `internal/handler/tracking_csp_test.go` — `TestStrictPublicCSP_isTheStringWeThinkItIs`
  (the literal) and `…allowsSameOriginScripts` (both policies).
- `internal/server/tenant_index_test.go` — the list and its two exclusions asserted by
  name AND slug, the ordering, the empty state, the unknown host still 404, the headers and
  the `noindex`, `?lang=fr` (page strings translated, event-type name left verbatim), and
  the console-ON redirect **in both modes** plus single-tenant-off.
- `internal/server/tenancy_index_test.go` — the cross-tenant claim, on a real NOBYPASSRLS
  role, both directions. LOUD SKIPs without `CALNODE_TEST_POSTGRES_DSN`.
- `internal/handler/metrics_test.go` — inside the CIDR anonymously, outside it in six
  combinations (including the bearer still working and an address one bit past the mask),
  four proxy headers asserted **individually** so a refactor reaching for the resolved
  client IP cannot pass by handling only `X-Forwarded-For`, and the empty setting from
  three addresses.

### Not done

- No migration, no schema change, no `frontend/` change, no `go.mod` change.
- No Caddy `header` block anywhere: everything M9 asked for is expressible in Go, which is
  also the only place a self-hoster who does not run our Caddy gets it.
- The k8s manifest that sets `METRICS_ALLOW_UNAUTHENTICATED_FROM` is the platform side's,
  not this repo's. `docs/MULTI_TENANT.md` names the variable and the value it expects.
- `DEPLOY.md` is unchanged, following `ADMIN_SPA` and `PLATFORM_RETURN_ORIGINS`: the root
  page and the metrics opt-in are documented in one place.
