# Contributing to District Scheduler

Thanks for considering a contribution! This guide covers getting the app building,
running, and tested locally. For *how the system fits together*, read
[docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md) first — it's the map.

District Scheduler is Distronode Corporation's Apache-2.0 fork of
[Calnode](https://github.com/Calnode/calnode). Most of the code is shared with
upstream, so the first question on any change is which of the two it belongs to.
There is a short section on that at the bottom, and
[SUPPORT.md](SUPPORT.md) covers the same question for bug reports.

## Prerequisites

- **Go 1.26+** (the SQLite build is pure-Go — no CGO, no C toolchain needed)
- **Node 20+** and **pnpm** (the admin UI is SvelteKit; we use `pnpm`, not npm)
- **PostgreSQL 16+**, only if you are touching SQL or the tenancy model. See Tests.

## Layout

- `cmd/calnode/` — entry point + CLI tools (key recovery/rotation, admin reset)
- `internal/` — the backend (handlers, booking engine, calendar providers, slots,
  mailer, worker, crypto, db). This is where most changes live.
- `frontend/` — the SvelteKit admin SPA, embedded into the Go binary at compile time
- `internal/handler/templates/*.html` — the public, server-rendered booking pages
  (separate from the Svelte app — see ARCHITECTURE §2)
- `internal/db/migrations/` — goose SQL migrations, run automatically on startup

## Build

The frontend is embedded into the binary via `go:embed`, so a build is two steps —
the `Makefile` chains them:

```bash
make build      # pnpm build in frontend/, then go build -o calnode ./cmd/calnode
```

> **Gotcha:** restarting the Go server alone won't pick up frontend changes — the
> assets are baked in at compile time. After editing anything under `frontend/`,
> re-run `make build` (or `pnpm build` then rebuild Go).

## Run locally

Minimum env to boot (a dev box can omit the encryption key — see the caveat below):

```bash
BASE_URL=http://localhost:3000 \
DATABASE_URL=sqlite://./data/calnode.db \
./calnode
```

Open `http://localhost:3000` → it redirects to `/admin/` and walks first-run setup
(create the owner, optionally connect a calendar, add an event type). Full env-var
reference and integration setup (Google/Microsoft OAuth, SMTP, Litestream) live in
[DEPLOY.md](../DEPLOY.md).

> Without `CALNODE_ENCRYPTION_KEY` on a non-https `BASE_URL`, the vault uses an
> ephemeral per-process key, so encrypted data (OAuth tokens, SMTP password) won't
> survive a restart — fine for local dev. Production (https) hard-fails without it.

Multi-tenant mode needs PostgreSQL and two DSNs (`DATABASE_URL` and
`DATABASE_ADMIN_URL`) alongside `MULTI_TENANT`; it refuses to boot with either
missing. See [docs/MULTI_TENANT.md](../docs/MULTI_TENANT.md).

## Tests

```bash
go test ./...                       # backend — keep these green
cd frontend && pnpm test:visual     # real-browser computed-style checks
```

**This fork runs on two database engines, and a test run only ever exercises one.**
`internal/dbtest` returns in-memory SQLite unless `CALNODE_TEST_POSTGRES_DSN` is
set, in which case the same tests run against PostgreSQL and the `TestPostgres_*`
cases stop skipping. If your change touches SQL, a migration, a constraint error
path or anything tenancy-related, run both:

```bash
go test ./...                                    # SQLite
CALNODE_TEST_POSTGRES_DSN='postgres://postgres:pw@127.0.0.1:5432/calnode?sslmode=disable' \
  go test ./...                                  # PostgreSQL
```

A PostgreSQL run that reports no `TestPostgres_*` cases has not tested PostgreSQL:
the DSN was ignored or unreachable and the suite quietly fell back to skipping. CI
runs both as two jobs, `check` and `postgres`.

**Run `pnpm test:visual` after touching** `frontend/src/lib/components/ui/**`,
`frontend/src/app.css`, or the theme. shadcn-svelte styles state via Tailwind
`data-*` variants that need `@custom-variant` remaps in `app.css`; miss one and the
component renders **silently unstyled** (logic works, visuals don't). Unit tests
don't catch this class of bug — only the browser assertions do. See
`frontend/TESTING.md`.

## Migrations

Add a goose SQL file in `internal/db/migrations/` with the next number
(`000NN_short_name.sql`, `-- +goose Up` / `-- +goose Down`). They run on startup.
There are two dialect directories and the pair has to stay in step, which a parity
test enforces. SQLite can't easily drop columns, so `ADD COLUMN` is
reversible-by-convention only — prefer additive, nullable/defaulted columns.

## Translations

District Scheduler ships 9 locales (`en es fr fr-CA de it pt nl sv`) across the
booker-facing surfaces: booking page, manage/reschedule page, embed widget, the four
emails, and the calendar invite. The admin UI and the built-in video room are
English-only.

**Translation PRs are very welcome** - both new languages and corrections to existing
ones. **Every non-English locale is currently a machine draft with no native review**, so
if you speak one of them, fixes are genuinely valuable and will be merged readily. Small,
single-language PRs are easier to review than sweeping ones. Locale files are shared
with upstream unchanged, so a translation fix is the clearest example of a change that
should go upstream as well.

**Adding a language is adding one file:** `internal/i18n/locales/<code>.json`. No Go,
template, or frontend change is needed - `init()` globs the directory, and the language
switcher, the fallback-language setting and the public API payload all read
`SupportedLocales()`.

1. Copy `internal/i18n/locales/en.json` and translate the values. Keep every key, keep
   the key order (it keeps diffs readable), and keep printf verbs (`%s`, `%d`, `%q`)
   intact - reordering them is fine with indexed verbs (`%[2]s`), dropping them is not.
2. Name the file **BCP-47 canonical**: `pt-BR.json`, never `pt-br.json`.
3. `dow_short_*`, `month_short_*`, `date_format` and `clock_format` are data, not code -
   Go has no locale date tables. Match CLDR; the test below checks you against `Intl`.
4. Run `go test ./internal/i18n/`. Three guards will tell you exactly what is wrong:
   key parity, printf-verb parity (with `fmt` as the oracle), and a CLDR cross-check of
   the date tables (needs `node` on PATH; skipped if absent).
5. Eyeball the result: `go run ./cmd/calnode`, then
   `http://localhost:3000/book/<slug>?lang=<code>` and the manage page. Long words
   overflowing buttons is the usual surprise; check mobile width too.

Background and the full list of limitations (2-form plurals, no RTL) are in
[ARCHITECTURE.md §23](../docs/ARCHITECTURE.md).

## Conventions

- **`gofmt`** all Go; standard library style. The codebase favours small, clear
  functions with comments that explain *why*, not *what*.
- **`pnpm` only**, and `pnpm exec <tool>` for local binaries (`pnpm dlx` has a
  Windows manifest bug).
- **Read [ARCHITECTURE.md §17](../docs/ARCHITECTURE.md)** (cross-cutting gotchas) before
  editing — especially the SQLite single-connection rule (never query inside an open
  cursor), all-times-UTC, and "calendar side effects are best-effort."
- **Keep the docs honest.** If your change alters behaviour described in
  ARCHITECTURE.md, update the matching section in the same PR.
- **`calnode`-prefixed identifiers are compatibility surfaces, not oversights.** The
  module path, the binary name, the `CALNODE_*` variables, the `X-Calnode-*` headers,
  the `calnode_*` metric names and cookies, and the `<calnode-booking>` element are all
  wire contracts shared with upstream and with already-deployed instances. Renaming one
  breaks a customer's embed or an operator's dashboard. Don't.

## Branches and images

- **`district`** is the default branch and what you branch from. A push to it publishes
  `:edge` and `:sha-<short>`.
- **`dev`** is the pre-release lane. A push to it publishes `:dev` and `:sha-<short>`
  and deliberately moves neither `:edge` nor `:latest`, so a branch can be deployed to
  a real instance and exercised before it reaches the default branch.
- **`main`** tracks upstream and is fast-forwarded on each sync. Don't open PRs against it.
- Pin a deployment to `:sha-<short>`, never to `:dev`, `:edge` or `:latest`. Those move
  under you and then nothing records which commit an instance is running. This fork
  itself ships by digest and cuts no release lines.

## Pull requests

1. Branch off `district`.
2. `go test ./...` green (both engines if you touched SQL); `make build` succeeds; run
   `pnpm test:visual` if you touched UI/theme.
3. Keep the change focused; explain the *why* in the PR description.
4. **There is no CLA on this fork.** Your contribution is accepted under the terms of
   the [Apache License 2.0](../LICENSE), §5 of which already covers it: unless you say
   otherwise in writing, anything you deliberately submit for inclusion is submitted
   under that licence. You keep the copyright to your work. No bot will comment on your
   PR and there is nothing to sign.

## Should this go upstream instead?

Most of this code is Calnode's, and a fix in shared code is worth more upstream than
here, because every deployment gets it rather than only ours. Rough test: if the change
would make sense on an instance that runs SQLite, single-tenant and no platform API,
it is upstream's.

- **Say so in the PR.** The template has a checkbox for it. That is all that is needed;
  you don't have to open the upstream PR yourself.
- **We do forward changes**, under our own name and crediting you, and we ask you first.
  Note that upstream **does** require a CLA, so if you would rather not sign one we
  simply keep the change here and say so.
- **Fork-only code is ours to fix**: multi-tenant mode, the platform API, row-level
  security, the PostgreSQL build and the `Dockerfile.district` image do not exist
  upstream. Send those here.
- If you would rather just open it upstream in the first place, please do. We track
  their releases and it will reach us.

The project's distributed code remains [Apache-2.0](../LICENSE). "Calnode" is upstream's
project name and mark (see [TRADEMARK.md](../TRADEMARK.md)), which is why this fork is
named and branded differently.
