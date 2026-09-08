# Multi-tenant mode

One process, many isolated workspaces. `MULTI_TENANT` unset behaves exactly as it always has:
SQLite and single-tenant PostgreSQL are unchanged, and every existing test passes without
modification. That is a gate, not an aspiration.

## What it is

A `workspaces` table is the tenant root. Every application table carries a `workspace_id`, and
**PostgreSQL row-level security — not the query author — is what keeps one workspace out of
another's rows.** A query that forgets its predicate returns nothing rather than everything.

The tenant of a request is resolved from the request `Host` (each workspace has its own public
hostname) or from the credential it carries (API key, session, OAuth token, manage token, invite,
magic link), and every route says which of the two it uses.

## What it requires

- **PostgreSQL.** SQLite has no row-level security, so there is nothing to express the isolation
  with. Startup refuses `MULTI_TENANT` without a `postgres://` DSN.
- **Two roles, two DSNs.**

  | variable | role | used for |
  |---|---|---|
  | `DATABASE_URL` | application: `NOBYPASSRLS`, owns no table in the schema | every request |
  | `DATABASE_ADMIN_URL` | platform: schema owner, `BYPASSRLS` | migrations, RLS setup, cross-tenant reads, the worker's claim loop, the platform API |

  ⛔ **The two must not be the same role.** One role means the application owns the tables and
  every policy is inert against it — and nothing breaks: every request works, and it can also read
  every other workspace. That is the misconfiguration hardest to notice, so startup refuses it
  outright, along with an application role that is a superuser, has `BYPASSRLS`, or owns any table
  (`VerifyRoles`). The platform role is checked to genuinely bypass, because a platform role that
  does not silently does no background work at all.
- **Demo mode off.** Demo mode periodically wipes the database, which here is every tenant's data.
  The two are mutually exclusive and startup refuses both.

## Environment

| key | meaning |
|---|---|
| `MULTI_TENANT` | any non-empty value turns the mode on |
| `DATABASE_URL` / `DATABASE_ADMIN_URL` | the pair above |
| `CALNODE_PLATFORM_TOKEN` | bearer for `/v1/platform/*`. Unset ⇒ those routes 404 |
| `CALNODE_SSO_SHARED_SECRET` | HMAC key for the session hand-off. **Required** if Google or Microsoft login is configured: the callbacks hand off through it |
| `TRUSTED_PROXY_CIDRS` | networks whose forwarded headers are believed. Unset behind a proxy makes each workspace a single rate-limit bucket |
| `CALNODE_ENCRYPTION_KEY` | as before, and it must travel with any workspace moved between instances |
| `BASE_URL` | the identity host (below) |
| `PUBLIC_BASE_URL` | **ignored**; each workspace's `public_host` replaces it |
| `DATA_DIR` | where uploads (avatars, branding) are written; defaults to the relative `data`. A read-only image sets it to its mounted volume |

## The isolation model

Three layers, and each catches what the others cannot.

1. **Row-level security.** Every tenant table has `ENABLE ROW LEVEL SECURITY` and one policy
   comparing `workspace_id` to `current_setting('app.workspace_id', true)`. An unset or empty
   setting matches no row, so an unbound statement is silently empty rather than silently global.
   ⚠️ `FORCE ROW LEVEL SECURITY` is deliberately **not** used: it would apply the policy to the
   table owner too, and in single-tenant mode the ordinary DSN *is* the owner — every existing
   deployment whose DSN is not a superuser would go blind. `ENABLE` alone already isolates a
   non-owner role, which is what the application role is.
2. **Per-statement binding.** A handle from `OpenPair` carries a workspace. Before each statement
   it takes a pooled connection, runs `SELECT set_config('app.workspace_id', $1, false)`, runs the
   statement there, and releases the connection when the statement finishes. Nothing is pinned
   between statements, so a handle is safe to copy into a goroutine that outlives its request.
   ⛔ `Prepare` is refused on a bound handle: a prepared statement is re-prepared on whatever
   connection the pool hands it, which would run unbound — silently empty rather than an error.
3. **Explicit predicates where there is no policy.** Four tables are exempt because they are not
   per tenant: `workspaces`, `crypto_keystore`, `goose_db_version`, `oauth_clients`, plus
   `sso_nonces` (a token id is global — the question "has this token been spent" must not depend on
   which workspace it names). Anything reading those, and everything on the platform handle, names
   its own `workspace_id` in every statement, because there is no policy behind it.

### The platform handle, and the one rule about it

The platform handle bypasses the policies. Two consequences that have each caused a real bug here:

- ⛔ **Every INSERT through it must name `workspace_id`.** It binds the empty string, so an unnamed
  column resolves to `''` and the row fails its foreign key. A route that writes on the platform
  handle and omits the column does not silently misfile the row — it fails — but it fails at the
  database, far from the omission.
- ⛔ **A tenant-scoped handle must be derived from the APPLICATION handle, never from the platform
  one.** Binding a workspace onto a bypassing role produces a handle that *names* a tenant without
  being *confined* to it: `WHERE id = 1` then matches every workspace's row and returns an
  arbitrary one. Reads are the failure mode, and they are silent.

### Reads that must not be bound

**A read whose job is to discover the tenant cannot be bound to it.** Credential lookups —
`api_keys`, `sessions`, OAuth bearer tokens — run on the platform handle and select the user's
`workspace_id` alongside. This is why those uniques stay global while `users(workspace_id, email)`,
`event_types(workspace_id, slug)` and `teams(workspace_id, slug)` become composite.

## Route classification

Every registration declares its class, and a source-scanning test fails on one that does not:

| class | tenant from | examples |
|---|---|---|
| host-scoped | `Host` → `workspaces.public_host` | the booking pages, public event-type reads, `POST /v1/bookings`, `/manage/{token}`, `/admin/*` |
| credential-scoped | the verified caller | the whole authenticated API |
| platform | nothing, on purpose | `/healthz`, `/readyz`, `/version`, `/metrics`, `/.well-known/*`, `/oauth/*`, `/mcp`, `/v1/platform/*`, the OAuth login callbacks, the vendor webhooks |

An unrecognised host is a **404**, never a fallback to a default tenant: falling back would serve
one tenant's booking page on any domain pointed at the instance. A credential that resolves
workspace A on workspace B's host is **403 `{"error":"workspace mismatch"}`**. A suspended
workspace answers **503** with `Retry-After` on its public and admin surfaces.

## The platform API

Identity host, `Authorization: Bearer $CALNODE_PLATFORM_TOKEN`, constant-time compare. With the
token unset — or on a single-tenant instance — every route **404s**, so a prober cannot tell a
control plane from an instance that has none. A wrong token is 401.

### `POST /v1/platform/workspaces` → 201

```json
{
  "id": "acme", "slug": "acme", "public_host": "book.acme.example", "region": "us",
  "owner_email": "owner@acme.example", "owner_name": "Owner", "owner_timezone": "America/Toronto",
  "defaults": {
    "embed_allowed_origins": ["https://acme.example"],
    "webhook": { "url": "https://hooks.acme.example/in", "secret": "<optional hex>",
                 "fields": ["booking_id", "start_at"] },
    "event_type": { "slug": "intro", "name": "Intro call", "duration_minutes": 30,
                    "min_notice_minutes": 60, "max_future_days": 60,
                    "availability": [{ "day_of_week": 1, "start_time": "09:00", "end_time": "17:00" }] },
    "livekit_url": "...", "livekit_api_key": "...", "livekit_api_secret": "...",
    "stt_base_url": "...",
    "smtp": { "host": "...", "port": "587", "user": "...", "pass": "...",
              "tls": true, "starttls": false, "from": "...", "from_name": "..." },
    "llm": { "endpoint": "...", "model": "...", "api_key": "...", "enabled": true,
             "extra_instructions": "..." }
  }
}
```

Response: `{"api_key": "cno_…", "webhook_secret": "…"}` — **shown once**. One transaction creates
the workspace row, its `server_settings` row (`id = 1` per workspace, so existing `WHERE id = 1`
reads need no change), the owner (with `iana_timezone = owner_timezone`, because availability is
local `HH:MM` and defaulting the zone would move the workspace's hours), the first API key, the
webhook subscribed to **every** event the codebase emits, and the default event type with its
availability. Either the tenant exists complete or it does not exist.

`defaults.embed_allowed_origins` and `defaults.stt_base_url` are per workspace and are READ: the public
booking endpoints' CORS allowlist is the one stored for the workspace whose host the request names (an
empty list means any origin, and a host no workspace owns gets no `Access-Control-Allow-Origin` at all,
never `*`), and the notetaker sends that workspace's recordings to its own speech-to-text host, falling
through to `STT_BASE_URL` and then the provider default when the column is empty. In this mode
`EMBED_ALLOWED_ORIGINS` is not consulted.

`day_of_week` is 0 = Sunday. A duplicate `id`, `slug` or `public_host` is 409, and the losing
attempt leaves nothing behind.

### The rest

| route | notes |
|---|---|
| `GET /v1/platform/workspaces/{id}` | the row: `id, slug, public_host, region, status, created_at, updated_at` |
| `PATCH …/{id}` | `public_host`, `status` (`active`\|`suspended`), `slug`. Nothing else: the id is referenced by every tenant row and the region is where the data physically is |
| `DELETE …/{id}` | cascades every tenant table; responds `{"recording_object_keys": [...]}` because objects in storage cannot cascade and deleting them is the caller's job |
| `POST …/{id}/export` | one JSON document, tables in replay order |
| `POST …/{id}/import` | 409 unless the workspace is empty |
| `DELETE …/{id}/attendees?email=` | erasure, counts per table |

### The member API

The identity provider — not this instance's credential routes — owns who is in a workspace
and what they may do there. Each of its people acts here as their own user, holding a
platform-minted key, so a booking, a log line and an API call all name a person rather than
a shared service account.

#### `POST …/{id}/users` → 200

```json
{ "email": "ada@acme.example", "name": "Ada Lovelace", "role": "admin", "timezone": "America/Toronto" }
```

Response: `{"id": "...", "email": "ada@acme.example", "name": "Ada Lovelace", "role": "admin", "created": true}`.

An upsert on `(workspace_id, email)`. The address is lower-cased and trimmed; an empty or
malformed address, an empty name, a role outside `owner|admin|member`, or a `timezone` that
is not an IANA name is **400**. `timezone` is optional.

On an existing user: `name` is overwritten, the role is applied, and `archived_at` is
cleared — an upsert is the statement "this person is in this workspace", so a returning
member comes back. `iana_timezone` is written **only when `timezone` was sent**, because a
zone is something a person sets in their own profile and a sync that omits it must not
reset it. `email_login` is never touched: whether somebody may sign in with a password is a
fact about this instance's login, not about the directory.

⛔ **`role: "owner"` is a TRANSFER.** The same transaction sets `is_owner = 0` on every
other user of the workspace, so exactly one owner exists before and after — the invariant
`POST …/users/{id}/transfer-ownership` maintains holds here too.

⛔ **Demoting the only owner is refused: 409 `{"error":"owner_demotion_requires_transfer"}`,
and nothing changes.** Obeying it would leave a workspace with no owner, which nothing on
the instance can repair, because every route that grants ownership requires an owner to
call it. Upsert the new owner with `role: "owner"` first — that demotes the incumbent — and
re-send the demotion if it is still wanted.

#### `POST …/{id}/users/{uid}/api-keys` → 201

Body `{"name": "district"}` (1–64 characters). Response
`{"id": "...", "name": "district", "api_key": "cno_…"}` — the plaintext is **shown once**.
The key is **managed** (below). **404** when the user is not that workspace's, or is
archived.

⛔ **A mint with a name the user already has ROTATES.** The earlier *managed* key of that
name is deleted in the same transaction, so a caller that lost a key and re-mints never
accumulates credentials, and the old key stops working at the instant the new one starts.
Two live keys therefore need two names. A key the person minted for themselves under the
same name is theirs and is left alone.

#### `DELETE …/{id}/users/{uid}/api-keys/{keyId}` → 204

Managed or not. **404** when the key is not that user's in that workspace, so the route
cannot be used to learn which key ids exist elsewhere.

#### `POST …/{id}/users/{uid}/archive` → 200 `{"archived": true}`

The offboarding path, with the platform as actor: it is not a member, so the "not yourself"
and "only the owner may archive an admin" guards do not apply to it. What does apply is
carried over exactly — **409 `{"error":"upcoming_bookings","count": n}`** while the person
still hosts a booking that has not happened (reassign or cancel first), **409
`{"error":"owner_cannot_be_archived"}`** for the owner, and their event types are
deactivated so the public pages stop taking bookings.

The same transaction deletes their **managed** API keys, their sessions and their MCP
access tokens. Archiving already blocks a key (`RequireAuth` requires `archived_at IS
NULL`), but leaving the rows would mean an un-archive through the upsert route silently
brings a live credential back on whatever host still holds it. Their own unmanaged keys are
left alone: those are the person's, not the platform's.

#### `PATCH …/{id}/webhooks` → 200 `{"updated": n}`

Body `{"url": "https://hooks.acme.example/in", "managed": true}`. Sets `managed = 1` on
every webhook in that workspace whose `url` matches exactly. Idempotent.

This is how a workspace provisioned before the `managed` column existed gets its
provisioning webhook marked. The migration could not do it: unlike the API key, whose name
(`platform-provisioned`) is written by exactly one statement in the codebase, a webhook row
carries nothing that distinguishes the platform's from one the workspace created — url,
events and fields are all values the caller chose. The platform knows which url it gave;
the database does not.

⛔ **`{"managed": false}` is 400.** Managed is one-way on purpose: the flag's value is that
a credential caller cannot reach the row, and an un-manage route is a way to reach it.
Deleting the row and re-creating it is the reversal.

### Managed rows

`api_keys.managed` and `webhooks.managed` mark a row the **platform** owns rather than the
workspace. Two rows created by `POST /v1/platform/workspaces` are managed from the moment
they are written: the `platform-provisioned` API key an integration spends, and the webhook
the platform receives bookings on. Both used to sit on the owner user, listed with a delete
button on the workspace's own settings pages, where one click broke the integration with
nothing on either side to say so.

| surface | managed row |
|---|---|
| `GET /v1/api-keys`, `GET /v1/webhooks` | omitted |
| `DELETE /v1/api-keys/{id}`, `PATCH`/`DELETE /v1/webhooks/{id}` | **403** `{"error":"managed by your platform"}` |
| authentication (`RequireAuth`) | accepted, unchanged |
| webhook **delivery** | delivered, unchanged |

403 rather than 404: the row exists and the caller owns the user it hangs off, so "not
found" is a lie they can disprove, and the actionable answer is that the platform minted it.

⛔ **`managed` governs administration, never delivery.** The dispatcher's read
(`WHERE user_id = ? AND is_active = 1`) deliberately does not filter on it. A `managed = 0`
predicate added there for symmetry would stop the provisioning webhook delivering the
moment the row was marked — silently, with the row still present and still active.

Managed rows exist only in multi-tenant mode: nothing a credential caller can reach sets
the flag, and the only writers are the platform routes, which 404 on a single-tenant
instance.

## The SSO hand-off

The identity host cannot set a cookie for a tenant's domain, so after a Google or Microsoft login
the callback mints a short-lived token and redirects to the workspace's own host.

`GET https://<public_host>/v1/auth/sso?token=<compact HS256 JWS>[&next=/path]`

Claims: `iss`, `aud`, `sub` (email), `name`, `role` (`owner`\|`admin`\|`member`), `iat`, `exp`,
`jti`, `wid`. Verified with the shared secret; only HS256 is accepted, checked before the signature
is looked at.

- `exp - iat` ≤ 60 s, 30 s of clock skew allowed either way. The mint side uses 30 s.
- `jti` is claimed in `sso_nonces` **before** the session is created, so a replay inside the
  validity window loses on the primary key.
- ⛔ **The workspace comes from the HOST, and `wid` is checked against it.** Resolving from `wid`
  alone would let a token for workspace A, presented on B's host, create A's session on B's domain
  — a good signature, a resolvable `wid` and a matching audience, and the wrong outcome. A mismatch
  is 403.
- `aud` must equal `https://<that workspace's public_host>`, with no trailing slash.
- The user is created or resolved **in that workspace**, with `workspace_id` named, and the session
  row carries it too — every later request runs on a bound handle that could otherwise neither read
  nor delete it.
- `role` from the token applies only to a user it creates; an existing user's role is never
  rewritten by a sign-in.

The login start carries the workspace in the state **cookie** (`<nonce>|<workspace_id>`) and sends
only the nonce to the provider. The nonce is compared, the workspace is read: a visitor can rewrite
the query parameter and achieve a failed login, and the value that selects the tenant never left
the server.

## Export, import, erasure

**Export** is one JSON document: `format_version`, `exported_at`, the workspace row, a
`dek_fingerprint`, and `tables` as an **ordered array** — parents before children, because import
replays it in the order it receives. Secrets and API-key hashes travel **verbatim**, because a
tenant whose keys and manage links stopped working on migration has not been migrated. The document
is therefore as sensitive as the database.

The table list is checked against the schema's own tenant-table list at request time, so a table
added by a later migration cannot be silently absent from every backup.

**Import** refuses (409) unless the workspace is empty, runs in one transaction, and **forces
`workspace_id` to the id in the URL** — the document's own value is discarded, or an export of any
workspace would be a way to write into any other.

⚠️ Row ids are global primary keys, so **import is a move, not a copy**: replaying a document into a
second workspace while the first still holds its rows collides on the primary key. The supported
sequence is export → delete → import, normally into another instance.

⛔ **The DEK fingerprint rule.** `crypto_keystore` holds one wrapped data key per **process**, not
per workspace, so the key itself does not travel — an export of one tenant containing the key that
decrypts every tenant would be the opposite of isolation. Instead the document carries a SHA-256 of
the *already-encrypted* wrapped key, and **import refuses 409 when it differs**. Without that
check the rows import perfectly and then every secret in them fails at first use, one integration
at a time, long after anyone is watching. Moving a workspace means moving
`CALNODE_ENCRYPTION_KEY` with it.

**Erasure** (`DELETE …/attendees?email=`) removes the attendee rows for that address in that
workspace and returns counts. It cancels nothing: the bookings, the host's calendar and the other
attendees' records are not the erased person's data. Answers are keyed `(booking_id, question_id)`
and carry no attendee, so they are erased only for bookings where that person was the **only**
attendee — with anyone else on the booking, deleting them would erase a third party's data.

## Vendor webhooks

LiveKit and Stripe call in with their own signature and no tenant Host, so both routes are platform
routes that resolve the workspace from **our** row: the egress id or room on a recordings row, the
booking a room name encodes, or `bookings.stripe_session_id`. No resolver consults a `workspace_id`
in a vendor payload. An event no row owns is **2xx and ignored** — a 4xx would make the vendor retry
for days and no retry can make the row exist.

⚠️ In multi-tenant mode the resolve necessarily precedes the signature check, because the signing
credentials are per workspace: there is no instance-wide secret to verify against, and verifying
against an arbitrary tenant's is not verification. Nothing is written before the signature verifies
and nothing is disclosed either way. Single-tenant keeps the original verify-then-act order.

## What is NOT per tenant

| thing | why, and what it costs |
|---|---|
| **the data encryption key** | one wrapped DEK per process. An operator who can read the database can decrypt every workspace, and a workspace cannot move between instances without its key. The import fingerprint check makes the coupling loud rather than silent |
| **the OAuth app credentials** | Google/Microsoft client id and secret identify the *instance* to the provider, not the tenant |
| **rate-limit windows** | keyed `(workspace, client IP)`, but the counters live in one process |
| **retention sweeps** | expired sessions, tokens and deliveries are purged globally: they are retention rules, not tenant logic |

## Operator checklist

1. PostgreSQL 16+ with two roles: an owner (`BYPASSRLS`) and an application role (`NOBYPASSRLS`,
   owning nothing, granted DML on the schema's tables).
2. Set `MULTI_TENANT`, both DSNs, `CALNODE_PLATFORM_TOKEN`, `CALNODE_SSO_SHARED_SECRET` (if social
   login is configured), `TRUSTED_PROXY_CIDRS` (if behind a proxy), and `BASE_URL`.
3. Start. Boot order is: migrate on the platform handle → enable RLS → suspend the seeded `default`
   workspace → verify both roles. The first, second and fourth are fatal; the third is logged.
4. Point DNS for each tenant's `public_host` at the instance and terminate TLS for it.
5. Provision each workspace through `POST /v1/platform/workspaces`; store the `api_key` and
   `webhook_secret` from the response, which are shown once.
6. Verify with a request to each tenant's own host, and confirm an unknown host 404s.
7. Before moving a workspace between instances, move `CALNODE_ENCRYPTION_KEY` too, or import will
   refuse the document.

## Cost

Measured with 200 workspaces provisioned through the API in one process: RSS 28.9 MB → 35.9 MB,
i.e. **~35 KB per tenant**, with the connection pool unchanged at one connection per role.
`ForWorkspace` returns a value over a shared pool, so tenants cost cache entries and rows, not
connections.
