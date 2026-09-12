# District Scheduler

**A fork of [Calnode](https://github.com/Calnode/calnode)**, the Apache-2.0 scheduling engine,
run by Distronode Corporation as the booking, meetings and notetaker engine behind
[District AI](https://distronode.com/district-ai).

A lean, self-hostable scheduling engine that lives in your AI stack: a Calendly-style
booking app, with **first-party video meetings, recording, and AI notetaking** built in,
shipped as one Go binary that serves its own public booking pages and its own admin
console. It is API-first, webhook-native, and built for a world where agents do the
booking. Nothing is paywalled.

What this fork adds:

- **`MULTI_TENANT`** — one PostgreSQL-backed process serving many isolated workspaces,
  with row-level security as the isolation mechanism, a platform API for provisioning, a
  signed session hand-off, per-workspace vendor credentials, export/import and erasure.
  Everything about it is in [docs/MULTI_TENANT.md](docs/MULTI_TENANT.md). With the variable
  unset the software is upstream Calnode, byte for byte in behaviour.
- **A PostgreSQL-only image** — `ghcr.io/distronode-corporation/district-scheduler`, built from
  [Dockerfile.district](Dockerfile.district): distroless, non-root, no SQLite, no Litestream.
  The upstream single-binary SQLite image is still built from the upstream `Dockerfile`.

Both were offered upstream and declined on architectural grounds, so they are this fork's
to carry. See [Relationship to upstream Calnode](#relationship-to-upstream-calnode).

Branches: `district` is what the fleet runs; `feat/multi-tenant` is the upstream-facing
branch the pull requests are cut from. The Go module path is kept as upstream's so the fork
rebases cleanly. See [NOTICE](NOTICE) for attribution.

`Apache-2.0` · `Go 1.26` · `PostgreSQL / SQLite`

[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/distronode-corporation/district-scheduler/badge)](https://securityscorecards.dev/viewer/?uri=github.com/distronode-corporation/district-scheduler) · [Audit it yourself](AUDIT.md)

---

## Quick start

**This fork's image.** `MULTI_TENANT` is PostgreSQL-only, and it needs two DSNs for two
database roles: the platform role that owns the schema, and the application role every
request runs as. Boot refuses anything less, because a single role would make the
row-level security policies inert against the very connection they exist to confine.

```bash
docker run -p 3000:3000 \
  -e MULTI_TENANT=1 \
  -e BASE_URL=https://scheduling.example.com \
  -e DATABASE_URL=postgres://app:PASS@db:5432/scheduler \
  -e DATABASE_ADMIN_URL=postgres://platform:PASS@db:5432/scheduler \
  -e CALNODE_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  ghcr.io/distronode-corporation/district-scheduler:edge
```

Read [docs/MULTI_TENANT.md](docs/MULTI_TENANT.md) before running that anywhere real: the
two roles are the whole of the isolation guarantee, startup verifies both, and a workspace
is provisioned through the platform API rather than through the console. The fork publishes
no release tags, so `:edge` is the tip of `district` and `:sha-<short>` is the tag to pin.
See [Releases and versioning](#releases-and-versioning).

**Upstream's image, for a look at the engine itself.** It is built from this repository's
unmodified `Dockerfile` (SQLite, Litestream, single binary), needs no database, and with
`MULTI_TENANT` unset this fork's code behaves exactly as it does:

```bash
docker run -p 3000:3000 -v ./data:/data ghcr.io/calnode/calnode:latest
# → open http://localhost:3000
```

The whole app — booking pages, admin UI, SQLite — in one container, with data in
`./data`. With no encryption key set it runs on an ephemeral one (fine for a look;
stored credentials won't survive a restart). **Deploying for real** — HTTPS, a
persistent encryption key, backups — see **[Deploy for real](#deploy-for-real)** below,
or the full **[DEPLOY.md](DEPLOY.md)**.

---

## Why District Scheduler

- **One binary.** Pure-Go SQLite (no CGO) compiles to a fully static
  binary. `docker run` it, or drop it on a VPS. Single-tenant, there are no external
  services to orchestrate at all
  (built-in video, if you turn it on, is the one add-on — it needs a LiveKit server).
- **Meetings built in.** Optional first-party video rooms (LiveKit) as a booking
  location — guests join in-browser, no app or account. Recording lands in your own
  Litestream backup bucket (no extra storage to provision); an AI notetaker turns each
  call into a transcript + notes, exposed as MCP tools and webhooks. The one add-on:
  video needs a LiveKit endpoint (Cloud or self-hosted).
- **API-first, agent-ready.** A full REST API with API keys and
  **HMAC-signed webhooks configured *via API*** — script every booking action from
  Claude, ChatGPT, n8n, or curl. Plus a native **MCP server** built into the binary
  (official Go SDK; stdio + Streamable HTTP) so agents get first-class booking tools.
- **Modern, no bloat.** Go backend + a SvelteKit 5 admin app; public booking pages
  are server-rendered Go templates for instant first paint and a tiny payload.
- **Correct by construction.** DST-safe time handling (UTC instant + IANA name),
  a transactional double-booking guard, and native-API calendar free/busy (never
  stale `.ics` feeds).
- **Yours.** Your data, your calendar credentials, your infrastructure. Run one
  instance as one workspace, or turn on `MULTI_TENANT` and serve many from one process
  with the database enforcing the boundary. Both shapes are this repository.
- **Easy to extend.** A clean Go codebase with `sqlc`-generated queries — not a
  100-package monorepo. Add an endpoint without spelunking.

---

## District Scheduler vs cal.com

The default open-source scheduler is a SaaS monolith. This one is the opposite.

| | cal.com | District Scheduler |
|---|---|---|
| **Codebase** | 500k+ LOC TS across ~100 packages | Lean Go + one SvelteKit app |
| **Runtime deps** | 4 GB+ image; Redis **+** Postgres **+** API server | One static binary + PostgreSQL (or SQLite) |
| **Database** | Postgres (+ Redis) | PostgreSQL 17, or SQLite + Litestream in single-tenant mode |
| **Webhooks** | UI-only | **API-first**, HMAC-signed, per-webhook payloads |
| **AI / agents** | None | REST API + webhooks **+ a native MCP server** (stdio + HTTP) |
| **Video & recording** | Third-party links (Zoom / Meet) | **Built-in in-browser rooms + recording + AI notes** (self-hosted LiveKit) |
| **Deploy** | Orchestrate several services | `docker run` one container |
| **Isolation** | Application-level `org_id` filtering | **PostgreSQL row-level security**, enforced by the database ([docs/MULTI_TENANT.md](docs/MULTI_TENANT.md)) |
| **Licence** | AGPL-3.0 | **Apache-2.0**, nothing paywalled for self-host |

*(cal.com figures reflect its public footprint; see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full rationale.)*

If you're weighing self-hosted schedulers and you want **small, fast, scriptable,
and AI-ready** over feature-maximal, this one is built for you.

---

## AI-native

Everything a human can do, an agent can do — over the API today:

```bash
# Find slots and book, with an API key
curl -s "$BASE/v1/event-types/intro-call/slots?from=2026-06-16&to=2026-06-20&tz=Pacific/Auckland" \
  -H "Authorization: Bearer $API_KEY"

curl -s -X POST "$BASE/v1/bookings" -H "Authorization: Bearer $API_KEY" \
  -H 'Idempotency-Key: 9f3c…' -H 'Content-Type: application/json' \
  -d '{"event_type_slug":"intro-call","start_at":"2026-06-17T21:00:00Z","name":"Alex","email":"alex@example.com","timezone":"Pacific/Auckland"}'
```

Wire booking lifecycle events (`booking.created` / `.rescheduled` / `.cancelled`)
to n8n / Make / your own service with HMAC-signed webhooks — all configured through
the API, not buried in a UI.

**Native MCP server.** A Model Context Protocol server is compiled *into* the binary
(official Go SDK), exposing eight first-class tools — `list_event_types`,
`get_event_type`, `get_available_slots`, `create_booking`, `get_booking`,
`reschedule_booking`, `cancel_booking`, `list_bookings`. The MCP tools call the same internal services as
the REST API (no parallel code path), so booking side effects — calendar events,
confirmation emails, webhooks, reminders — fire identically.

Two transports:
- **stdio** for local agents — run `calnode mcp` (logs to stderr, JSON-RPC on stdout).
- **Streamable HTTP** at `POST /mcp` for remote agents. The binary is its own **OAuth 2.1
  authorization server** (dynamic client registration + PKCE), so an agent adds the
  server by URL and clicks **Connect** → signs in with the workspace's Google/Microsoft
  login → approves a consent screen — no pre-shared key. A `cno_` API key also works
  (`Authorization: Bearer <key>`) for scripts. *(The Connect UX needs HTTPS — it shines
  on a deployed instance.)*

```
User:  "Book a 30-min call with Wynne next week — I'm in Auckland."
Agent: get_available_slots("intro-call", "2026-06-16", "2026-06-20", "Pacific/Auckland")
       → presents options → create_booking(…) → returns confirmation + meeting link
```

**Conversational booking — in the booking page itself.** Beyond agents, the booking page
(and the embed widget) ships an optional **"Book by chat"** assistant: a visitor types
"free Tuesday afternoon or next week" and it resolves real availability and books — the
deterministic engine still computes the slots (the model never invents times), and the
assistant only ever sees free/busy windows, never your calendar contents. **Bring your own
model** — any OpenAI-compatible endpoint (a hosted model or one you run yourself); off by
default, with the standard calendar always there as the fallback.

**Connecting Claude (remote / HTTP).** In Claude (claude.ai or Desktop) →
**Settings → Connectors → Add custom connector** → enter `https://<your-instance>/mcp`
→ **Connect** → sign in → **Allow**. Custom connectors need a paid Claude plan; the
server must be on HTTPS. *Local stdio alternative (any plan):* point an MCP client at
the `calnode mcp` subcommand via its config file — no OAuth, runs against the local DB.

**Permissions.** MCP tools are **role-scoped**, mirroring the rest of the app: an
**owner/admin** acts across the whole workspace, a **member** sees and manages only
bookings they host. (The stdio subcommand is the local operator → full access.) Booking
*creation* and availability are the public booking surface, open to all. Roles are fixed
(owner / admin / member); configurable RBAC is intentionally out of the lean core.

---

## Multi-tenant mode

`MULTI_TENANT` turns one process into a host for many isolated workspaces. A `workspaces`
table is the tenant root, every application table carries a `workspace_id`, and
**PostgreSQL row-level security, not the query author, is what keeps one workspace out of
another's rows**: a statement that forgets its predicate returns nothing rather than
everything. The tenant of a request is resolved from its `Host` (each workspace has its
own public hostname) or from the credential it carries, and every route declares which,
with a source-scanning test that fails on a registration that declares neither.

It requires PostgreSQL and two roles, because SQLite has no row-level security to express
the isolation with. On top of that it adds a platform API for provisioning, export, import
and erasure, a signed session hand-off so an external console can seat a session on a
tenant's own domain, and per-workspace vendor credentials. Unset, none of it is reachable
and nothing about single-tenant behaviour changes.

**→ [docs/MULTI_TENANT.md](docs/MULTI_TENANT.md)** is the full contract: the isolation
model, the environment, the platform API, and the operator checklist.

---

## Deploy for real

This fork, multi-tenant, which is the shape District AI runs:

```bash
docker run -d -p 3000:3000 \
  -e MULTI_TENANT=1 \
  -e BASE_URL=https://scheduling.example.com \
  -e DATABASE_URL=postgres://app:PASS@db:5432/scheduler \
  -e DATABASE_ADMIN_URL=postgres://platform:PASS@db:5432/scheduler \
  -e CALNODE_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  -e CALNODE_RECOVERY_SECRET="$(openssl rand -hex 32)" \
  -e CALNODE_PLATFORM_TOKEN="$(openssl rand -hex 32)" \
  -e CALNODE_SSO_SHARED_SECRET="$(openssl rand -hex 32)" \
  -e ADMIN_SPA=off \
  ghcr.io/distronode-corporation/district-scheduler:sha-abc1234
```

Single-tenant, on SQLite, from upstream's image:

```bash
docker run -d -p 3000:3000 \
  -e BASE_URL=https://booking.example.com \
  -e CALNODE_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  -e CALNODE_RECOVERY_SECRET="$(openssl rand -hex 32)" \
  -e DATABASE_URL=sqlite:///data/calnode.db \
  -v calnode-data:/data \
  ghcr.io/calnode/calnode:latest
```

### Releases and versioning

**This fork has no release lines and no backports.** It ships by digest: a push to
`district` publishes `:edge` and `:sha-<short>`, a push to `dev` publishes `:dev` and
`:sha-<short>`, and nothing moves `:latest`. Pin the **digest** you tested, or at least
`:sha-<short>`, which names the commit; `:edge` and `:dev` move under you, and then
nothing records which commit an instance is running. Upstream's own tags and releases are
upstream's, and this repository inherits them without publishing releases of its own. See
[this fork's releases](https://github.com/distronode-corporation/district-scheduler/releases),
which is deliberately empty, and [CHANGELOG.md](CHANGELOG.md), which is the record.

Open `/` → it redirects to `/admin/` and walks you through first-run setup (create
the owner account, connect a calendar, add an event type). Put a TLS-terminating
proxy in front that forwards the original `Host` header. In multi-tenant mode with
`ADMIN_SPA=off` there is no console to redirect to, and `/` serves a neutral index of
that workspace's public event types instead.

**Full guide → [DEPLOY.md](DEPLOY.md)** (env vars, both images, Railway step-by-step, custom
domains, Resend email, Google & Microsoft OAuth, Litestream backups, troubleshooting).

---

## Features

**Shipped**
- Event types with per-type duration, location, custom questions, custom email copy
- DST-correct availability (working hours, day-of-week rules, date overrides)
- Team routing: **fixed · round-robin · collective · priority**
- **Google Calendar & Microsoft 365 / Outlook** — native free/busy conflict checks
  behind one provider abstraction; auto **Google Meet / Teams** links, minted only
  when the host's connected calendar matches the platform (else a manual link is used)
- **Sign in with Google or Microsoft** (OAuth), email + password, or **passwordless magic-link**
- **CalDAV calendars** — iCloud / Fastmail / Nextcloud via app-password (free/busy + event write-back)
- Public booking + self-serve **reschedule/cancel** via signed manage links
- HTML branded email (logo, banner, business name, size/opacity) with add-to-calendar links
- REST API + API keys; **HMAC webhooks** with per-webhook payloads + delivery log
- **Native MCP server** (10 tools incl. meeting notes + transcript; stdio via `calnode mcp` + Streamable HTTP at `/mcp`)
- **Conversational booking** ("Book by chat" on the booking page + embed widget; BYO-LLM, off by default)
- **Paid bookings** — Stripe Checkout (pay-then-book: the slot is held, confirmed on the payment webhook, auto-refunded on cancel)
- **Zoom** — per-host OAuth; a Zoom-located booking mints a meeting under the assigned host's account
- **Built-in video meetings (LiveKit)** — in-browser rooms as a booking location (no app or account for guests); host controls (end-for-all, hand-off **and reclaim** host, attendee screen-share toggle), **meeting recording** straight to your own **Litestream backup bucket** (the same one
  you already use for DB backup — no extra storage to provision) with in-app downloads, **recording consent** (notice + consent-or-leave), and an **AI notetaker** (Deepgram transcript → LLM notes). Headless-consumable: MCP `get_meeting_notes`/`get_transcript` + `recording.completed`/`transcript.ready`/`notes.ready` webhooks. BYO LiveKit endpoint (Cloud or self-hosted); configured in Settings → Video — see [docs/VIDEO.md](docs/VIDEO.md)
- **9 languages** on every booker-facing surface - booking page, manage/reschedule page,
  embed widget, all four emails, and the calendar invite: **English · Spanish · French ·
  Canadian French · German · Italian · Portuguese · Dutch · Swedish**. Picked from `Accept-Language` with a
  footer switcher and an operator-set fallback language; the booker's choice is stored on
  the booking, so reminders arrive in the language they booked in. Adding a language is
  adding one JSON file - no code change. *(The admin UI and the built-in video room are
  English-only. Non-English translations are machine drafts without native review -
  corrections by PR are very welcome.)*
- Embeddable booking widget (Shadow-DOM web component; inline + popup)
- Members, roles (owner/admin/member), email-token invitations
- `Idempotency-Key` on booking creation; transactional double-booking guard
- Envelope encryption at rest (secrets sealed with a KEK; recovery escrow)
- Optional analytics: `<head>` code injection + `window.dataLayer` events (GTM/GA4)
- `GET /metrics` in Prometheus text exposition, bearer-gated on `METRICS_TOKEN` and a 404 without it
- Multi-domain: one instance, many hostnames, each workspace on its own public host in `MULTI_TENANT` mode
- **`MULTI_TENANT`**: many isolated workspaces in one process, PostgreSQL row-level security,
  a platform API, a signed session hand-off, export, import and per-attendee erasure

**On the roadmap**
- OpenAPI spec

---

## Stack

- **Backend:** Go 1.26 · pure-Go SQLite (`modernc.org/sqlite`, CGO-free → static binary) · `goose` migrations
- **Admin UI:** SvelteKit 2 / Svelte 5 · Vite 8 · Tailwind 4 · shadcn-svelte (embedded at compile time via `go:embed`)
- **Public pages:** server-rendered Go `html/template` + vanilla JS (no framework runtime)
- **Durability:** SQLite WAL + optional Litestream replication (S3/R2) for backup & point-in-time restore
- **Background work:** in-process job queue on the same DB — reminders, webhook delivery, calendar reconciliation. No broker.

---

## Design principles

A few load-bearing decisions (full detail in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)):

- **The DB is the source of truth; external calendars are a projection.** A booking
  exists once it's committed locally; syncing to Google is a retryable side effect.
- **Time = UTC instant + IANA timezone name**, never a fixed offset — availability
  resolves to UTC per-date so DST shifts never corrupt a slot.
- **Single process, no external services.** Durability comes from Litestream, not a
  second datastore. (The one exception is optional built-in video: it talks to a
  LiveKit server — Cloud or self-hosted — only when you enable video.)
- **One process can serve many workspaces, and the database enforces the boundary.**
  Isolation is PostgreSQL row-level security rather than a predicate a query author has
  to remember, so a statement that forgets returns nothing instead of everything. The
  single-tenant codepath is unchanged when `MULTI_TENANT` is unset, which is a gate
  rather than an aspiration: every pre-existing test passes without modification.

---

## Audit it yourself in 10 minutes

This backend is small enough to fit entirely in one LLM's context window —
most scheduling software (cal.com included) can't say that. **[AUDIT.md](AUDIT.md)**
turns that into a self-serve check: a copy-paste scanner block (govulncheck, gosec,
gitleaks across full history, SBOM, semgrep — all neutral, standard tooling you run
yourself), an adversarial LLM prompt-pack for your own coding agent, and
**[a claims → verification manifest](audit/claims.yaml)** mapping every security
claim we make to exactly how to check it in the source. Not a certification — a
due-diligence accelerator.

---

## Documentation

| Doc | What it covers |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | **Start here.** How the pieces fit and why: persistence, auth, slot generation, the booking lifecycle, calendars, webhooks, MCP, video, i18n |
| [docs/MULTI_TENANT.md](docs/MULTI_TENANT.md) | This fork's `MULTI_TENANT` mode: the isolation model, the two roles, route classification, the platform API, the SSO hand-off, the operator checklist |
| [DEPLOY.md](DEPLOY.md) | Deploying either image: environment variables, reverse proxy requirements, email, OAuth, Litestream backups, troubleshooting |
| [docs/DOMAINS.md](docs/DOMAINS.md) | `BASE_URL` against `PUBLIC_BASE_URL`, and custom domains |
| [docs/VIDEO.md](docs/VIDEO.md) | Built-in video meetings: LiveKit setup, recording, consent, the AI notetaker |
| [AUDIT.md](AUDIT.md) + [audit/claims.yaml](audit/claims.yaml) | Audit it yourself: the scanner block, the adversarial prompt-pack, and every claim mapped to how to falsify it |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Building, running and testing locally, and how a change gets forwarded upstream |
| [SECURITY.md](SECURITY.md) | How to report a vulnerability |
| [CHANGELOG.md](CHANGELOG.md) | What changed, and which entries are this fork's |

There are no screenshots here on purpose. To see the booking surface, run either image
above and open a booking page; to see the embed widget, open
[docs/embed-local.html](docs/embed-local.html) in a browser against a running instance.

---

## Relationship to upstream Calnode

This is a fork, not a rewrite, and it is kept close enough to rebase. `main` tracks
[Calnode/calnode](https://github.com/Calnode/calnode) and is fast-forwarded on each sync;
`district` is this fork's default branch and what [District AI](https://distronode.com)
runs; `feat/multi-tenant` is the branch upstream-facing pull requests are cut from. The Go
module path, the binary name and every wire identifier are upstream's (see the licence note
below), so a sync is a merge rather than a conflict-resolution exercise.

Work sent upstream, as of September 2026:

- **Nine merged**: #22, #23, #24, #25, #26, #27, #32, #37, #38. Duplicate event types,
  booking-page clarity, calendar reconnection, locale handling and constraint-code
  handling all landed in upstream Calnode from here.
- **Seven open**: #39, #40, #41, #43, #44, #45, #46.
- **Two declined**: #29 (PostgreSQL support) and #31 (`MULTI_TENANT`) were declined in
  September 2026 for architectural reasons. Upstream's product is the single binary with
  an embedded SQLite file, and neither change fits it. They are this fork's to carry, and
  they are the reason this fork exists.

Bugs in the scheduling engine itself are usually worth reporting upstream as well as here.
Anything about `MULTI_TENANT`, the PostgreSQL paths or the `Dockerfile.district` image
belongs here: upstream does not ship them.

---

## License

[Apache-2.0](LICENSE). The full scheduler is self-hostable, and nothing previously
free is ever paywalled.

The **code** is Apache-2.0; the **"Calnode" name and logo** are not — see
[TRADEMARK.md](TRADEMARK.md). This repository is the renamed fork that policy asks for:
it is called District Scheduler, it is run by
[Distronode Corporation](https://distronode.com), and it is not the official Calnode.
[NOTICE](NOTICE) carries the attribution and the statement that upstream does not endorse
it.

**The `calnode`-prefixed identifiers are kept on purpose.** They are compatibility
surfaces, not branding left behind: the Go module path `github.com/calnode/calnode`, the
binary name `calnode`, `CALNODE_*` environment variables, `X-Calnode-*` headers,
`calnode_*` metric names and cookies, the `cno_` API-key prefix, the
`<calnode-booking>` embed element and `window.Calnode`. Renaming any of them would break
an existing deployment's configuration, an operator's dashboards or a customer's embed
for no gain, and would turn every upstream sync into a rename conflict.

**Contributions are accepted under Apache-2.0 §5**, which is to say under the licence the
file you are editing already carries. There is no fork CLA: upstream's CLA assigns
relicensing rights to the Calnode project, which this fork cannot accept on anyone's
behalf. If a change is one we forward upstream, upstream's CLA applies to it there, and
[CONTRIBUTING.md](CONTRIBUTING.md) says how that works.
