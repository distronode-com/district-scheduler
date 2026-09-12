# Deploying District Scheduler

One binary, two images, and which one you want depends on whether you are running
one workspace or many.

## This fork's image: PostgreSQL, multi-tenant

`ghcr.io/distronode-corporation/district-scheduler`, built by CI from
`Dockerfile.district` (multi-stage: build the SvelteKit admin → compile Go →
distroless static runtime, non-root, uid 10001). No SQLite file, no Litestream, no
shell entrypoint. This is what District AI runs.

| Tag | Points at | Use for |
|---|---|---|
| `…:sha-<short>` | one commit, immutably | **every real deploy.** Pin this, or the digest |
| `…:edge` | the tip of `district` | tracking the default branch |
| `…:dev` | the tip of `dev` | a pre-release branch on a real instance |

⛔ **There is no `:latest` on this package and there will not be.** The fork
publishes no release lines and no backports, so `:latest` (which the workflow emits
only from a `vX.Y.Z` tag) is never written. `:edge` and `:dev` move under you, and
then nothing records which commit an instance is running. Pin the digest.

## Upstream's image: SQLite, single-tenant

`ghcr.io/calnode/calnode`, built from the repo `Dockerfile`, which this fork keeps
byte-identical to upstream's. A **single Go binary** that embeds the admin SPA and
serves the public booking pages, with **SQLite** on a persistent volume. No separate
database, cache, or web server.

| Tag | Points at | Use for |
|---|---|---|
| `ghcr.io/calnode/calnode:latest` | the newest tagged **release** | most deploys |
| `…:0.1.0` (exact) / `…:0.1` (tracks patches) | a pinned **release** | reproducible / version-pinned deploys (Watchtower, Dockge) |
| `…:edge` | the tip of upstream's `main` | trying unreleased changes |

Or build it yourself with `docker build -t calnode .`.

Everything in this guide applies to both images unless a section says otherwise:
the binary is the same binary, and the image only decides which engine it is
configured to reach. §1 has the variables, §10 has the multi-tenant ones, and
[docs/MULTI_TENANT.md](docs/MULTI_TENANT.md) is the contract for that mode.

This guide covers a generic Docker deploy and a step-by-step **Railway** deploy
(the reference host for the single-tenant shape).

---

## 1. Configuration (environment variables)

| Variable | Required | Default | Notes |
|---|---|---|---|
| `CALNODE_ENCRYPTION_KEY` | **prod: yes** | — | KEK input (Argon2id). **Required when `BASE_URL` is https** — the app refuses to start without it. Use a long random string: `openssl rand -hex 32`. **Losing it makes encrypted data unrecoverable** unless you set the recovery secret below. |
| `CALNODE_RECOVERY_SECRET` | recommended | — | Escrow secret so the data key can be recovered if the encryption key is rotated/lost. Store it somewhere separate. |
| `CALNODE_SSO_SHARED_SECRET` | no | — | HMAC key for the signed session hand-off (`GET /v1/auth/sso`). Unset ⇒ that endpoint **404s**. Anything holding this secret can mint a session and create a user, so treat it like the encryption key: `openssl rand -hex 32`, env only, never in the admin UI. |
| `METRICS_TOKEN` | no | — | Bearer token for `GET /metrics` (Prometheus text exposition). Unset ⇒ that endpoint **404s**, so an instance never publishes its request volume, booking rate or queue depth by accident. Scrape with `Authorization: Bearer $METRICS_TOKEN`; a wrong token gets the same 404 as an unconfigured one. |
| `METRICS_ALLOW_UNAUTHENTICATED_FROM` | no | — | Comma-separated CIDRs whose requests may scrape `GET /metrics` with **no** bearer, for a collector that cannot hold a per-target secret. ⛔ Matched against the **TCP peer**, never a forwarded header, because here the address *is* the credential. So the endpoint must not be reachable through a proxy inside the allowed range. Empty ⇒ off, and the bearer is the only way in. |
| `ADMIN_SPA` | no | `on` | `off` answers **404** on `GET /admin`, `GET /admin/` and everything under it, so a platform whose own console already has every admin surface does not ship a second one on each tenant host. ⛔ **Honoured only in multi-tenant mode**: a self-hoster has no other admin UI, so `off` on a single-tenant instance is ignored and said so at boot rather than locking the operator out. Values are `on` and `off` only; `true`/`false` are refused at boot, because the fallback is `on` and a value nobody can read exactly would serve the console the operator wrote the variable to remove. See §10 for what the bare root serves with it off. |
| `PLATFORM_RETURN_ORIGINS` | no | — | Comma-separated origins that `GET /v1/calendar/connect?provider=…&return_to=…` may finish the OAuth round trip on, so a platform that replaced the admin SPA with its own pages can land the person back on its own console. Each entry is `scheme://host[:port]` with no path, query or fragment, `https` unless the host is `localhost`/`127.0.0.1`, and **a malformed one is fatal at boot** rather than a silently dead allowlist. Empty ⇒ a `return_to` is a **400**, not a no-op, so a platform pointed at an instance nobody configured for it finds out on the first attempt. The match is the whole origin byte for byte: a prefix match would accept `https://console.example.com.evil.test`, and an open redirect out of an OAuth callback is a better prize than most bugs in a scheduler. |
| `CALNODE_PLATFORM_TOKEN` | no | — | Bearer for `/v1/platform/*`, compared in constant time. Unset, or on a single-tenant instance, every one of those routes **404s** rather than 401ing, so a prober cannot tell a control plane from an instance that has none. A wrong token is 401. |
| `BASE_URL` | **yes (prod)** | `http://localhost:3000` | Identity host — admin UI, OAuth callbacks, invite links. **Must include the scheme** (`https://booking.example.com`). The `https://` prefix flips the app into production mode (secure cookies, encryption-key enforcement). |
| `PUBLIC_BASE_URL` | no | = `BASE_URL` | Booker-facing host for booking links/emails, if different from the identity host. |
| `DATABASE_URL` | no | `sqlite://./data/calnode.db` | Point at the persistent volume, e.g. `sqlite:///data/calnode.db`. A `postgres://user:pass@host:5432/dbname` URL selects PostgreSQL instead; anything else is SQLite. In multi-tenant mode this is the **application** role. |
| `MULTI_TENANT` | no | off | Turns on multi-tenant mode: one process, many isolated workspaces, PostgreSQL row-level security as the boundary. ⚠️ Parsed as a Go boolean, so use `1` or `true`: a value `strconv.ParseBool` cannot read falls back to **off**, silently. Everything this mode adds is in §10. |
| `DATABASE_ADMIN_URL` | **multi-tenant: yes** | — | The **platform** role's DSN: schema owner, `BYPASSRLS`, used for migrations, RLS setup, the worker's cross-tenant claim loop and the platform API. ⛔ Must be a different role from `DATABASE_URL`, and startup refuses them being the same: one role means the policies are inert against the application and nothing appears broken. |
| `DB_MAX_OPEN_CONNS` | no | `10` | **PostgreSQL only.** Size of the connection pool. It has to fit inside the server's own `max_connections`, shared with every other client — raise it for a busy instance on a well-sized server, lower it behind PgBouncer or on a shared one. Must be a positive integer; anything else is ignored (with a warning) and the default stands. **Ignored on SQLite, which is always 1**: the single connection is what serialises write transactions, not a tuning choice. |
| `DB_MAX_IDLE_CONNS` | no | `5` | **PostgreSQL only.** How many idle connections the pool keeps rather than closing. Positive integer, and capped at `DB_MAX_OPEN_CONNS` (a larger value is clamped, since `database/sql` would silently do the same). |
| `PORT` | no | `3000` | The app listens on `$PORT`. Many platforms inject their own (Railway injects `8080`) — let them. |
| `EMAIL_SMTP_HOST` / `_PORT` / `_USER` / `_PASS` | no¹ | — / `587` | SMTP. Can also be set later in Settings → Email (DB-stored, encrypted). |
| `EMAIL_SMTP_TLS` / `_STARTTLS` | no | `false` | `STARTTLS` for 587, implicit `TLS` for 465. |
| `EMAIL_FROM_ADDRESS` / `EMAIL_FROM_NAME` | no | `bookings@localhost` / `District AI Scheduling` | The From identity. Set `EMAIL_FROM_NAME` to your own business name: it is what a booker reads in their inbox. |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | no | — | Google sign-in + calendar. Can also be set in Settings → Google OAuth. |
| `LITESTREAM_REPLICA_URL` | recommended | — | Enables continuous SQLite backup (see §6). |
| `COOKIE_SECURE` | no | https→true | Override cookie Secure flag; defaults from `BASE_URL` scheme. |
| `TRUSTED_PROXY_CIDRS` | no | — | Comma-separated CIDRs (a bare address = one host) whose `X-Forwarded-For` is believed when keying per-IP rate limits, e.g. `10.0.0.0/8`. Include a fronting CDN's own ranges so the walk steps over its edge and lands on the visitor. Unset ⇒ the header is ignored and the limit keys on the TCP peer, so behind a CDN every visitor shares one bucket. **Only list networks you control**: anything in the list can name any client IP it likes. Single-value vendor headers (`CF-Connecting-IP`, `X-Real-IP`) are never read, from any peer. |
| `FRAME_ANCESTORS` | no | — | **Space**-separated origins allowed to embed the **admin UI** in a frame, e.g. `https://console.example.com 'self'`. Each entry must be `https://host[:port]` or `'self'` — anything else and **the app refuses to start**, because browsers drop a policy they cannot parse. Does not affect the public booking pages, which always deny framing. |
| `LOG_LEVEL` | no | `info` | `debug`/`info`/`warn`/`error`. |
| `STT_BASE_URL` | no | `https://api.deepgram.com` | Speech-to-text endpoint **host** for meeting transcription, e.g. a regional endpoint so recording audio stays in one jurisdiction. Host only — the path, model and options are fixed. Shown read-only in Settings → Notetaker as `stt_base_url`. |

¹ Email is optional to boot, but bookings won't send confirmations until SMTP is configured (env **or** the admin UI). Precedence is **env var > DB setting > default**.

---

## 2. Generic Docker

This fork's image, multi-tenant (see §10 for the variables):

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
  ghcr.io/distronode-corporation/district-scheduler:sha-abc1234
```

There is no volume: this image writes nothing it cannot lose, and `DATA_DIR`
(uploads: avatars, branding) should point at a mounted path if you accept uploads.

Upstream's image, single-tenant on SQLite:

```bash
docker run -d -p 3000:3000 \
  -e BASE_URL=https://booking.example.com \
  -e CALNODE_ENCRYPTION_KEY="$(openssl rand -hex 32)" \
  -e CALNODE_RECOVERY_SECRET="$(openssl rand -hex 32)" \
  -e DATABASE_URL=sqlite:///data/calnode.db \
  -v calnode-data:/data \
  ghcr.io/calnode/calnode:latest
```

(Or swap in your own build: `docker build -t calnode .` from the repo root, then
use `calnode` in place of the image name above.)

Put a TLS-terminating reverse proxy in front (Caddy/nginx/Traefik). **The proxy
must forward the original `Host` header** — the CSRF check compares
`Origin`/`Referer` against `Host`, so a rewritten Host causes 403s on admin writes.

---

## 3. Railway (reference deploy)

Railway auto-detects the `Dockerfile` and builds it. Steps:

1. **Create a project + service** from the repo (or `railway up` from the repo dir).
2. **Add a volume** mounted at **`/data`** (Service → Settings → Volumes).
3. **Set variables** (Service → Variables):
   - `BASE_URL=https://<your-domain>` (with scheme)
   - `CALNODE_ENCRYPTION_KEY`, `CALNODE_RECOVERY_SECRET` (generate, keep safe)
   - `DATABASE_URL=sqlite:///data/calnode.db`
   - (email / Google / Litestream as needed)
   - **Don't** set `PORT` — Railway injects `8080`, and the app honours it.
4. **Deploy:** `railway up` (or push-to-deploy). Migrations run automatically on boot.
5. **Custom domain** (Service → Settings → Networking → Custom Domain):
   - **Target port = the port the app is *actually* listening on.** On Railway that's
     **`8080`** (the injected `PORT`), **not** 3000. A wrong target port → 502 /
     "Application failed to respond". Check the `server listening port=…` startup log.
   - Add the CNAME (+ TXT verify) record Railway shows at your DNS provider. On
     **Cloudflare, set it to "DNS only" (grey cloud)** — proxying (orange cloud)
     blocks Railway's cert issuance and can cause redirect loops. You may re-enable
     the proxy after the cert issues, with SSL mode = Full (strict).

### Railway-specific build notes (already handled in this repo)
- **No `VOLUME` directive in the Dockerfile** — Railway's builder rejects it; storage
  comes from the managed volume.
- **pnpm under CI:** the build pins `pnpm` and lists `pnpm.onlyBuiltDependencies`, so
  `pnpm install --frozen-lockfile` doesn't hard-fail under `CI=true`.

---

## 4. Email (Resend recommended for production)

Gmail/Workspace SMTP **rewrites the From** to the authenticated account unless you
use a verified "Send mail as" alias — so for a branded From, use a transactional
provider. With **Resend**:

1. Verify your domain in Resend (add the SPF/DKIM/MX records it shows; **DNS-only** on Cloudflare).
2. Create an API key.
3. Settings → Email. Pick **one** of:
   - **Resend API key** (recommended): paste the key into the "Resend API key" field and
     set From `bookings@yourdomain`. Delivery goes over HTTPS. Leave the SMTP fields empty.
   - **SMTP**: host `smtp.resend.com`, port `587`, username `resend`, password = the API
     key, **STARTTLS on**, From `bookings@yourdomain`.
4. Send a test email.

### If SMTP does not work, it may not be your fault

**Many hosting platforms block outbound SMTP on their cheaper plans.** Railway disables it
below Pro; other constrained tiers do the same. They block it by *dropping* the packets
rather than refusing the connection, so from inside the container it looks exactly like a
hang, and then exactly like a wrong password. No SMTP setting can work around it: ports 25,
465, 587 and 2525 all behave identically, and it is not specific to Resend.

The fix is the **Resend API key** field above, which sends over HTTPS on port 443. That is
never blocked, and it is what Resend itself recommends regardless of platform.

Which path is actually in use is shown as a badge at the top of Settings → Email, so
"SMTP fields are filled in" is never mistaken for "mail is going out over SMTP". Setting an
API key takes precedence over the SMTP fields; clearing it ("Remove key") switches back.

> Email settings are stored **per instance** in that instance's DB — staging/prod/local each need their own.

---

## 5. Google OAuth (sign-in + calendar)

In Google Cloud Console → Credentials → your OAuth client, add **Authorized
redirect URIs** (derived from `BASE_URL`):

```
https://<your-domain>/v1/auth/callback
https://<your-domain>/v1/calendar/callback
```

Set `GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET` (env or Settings → Google OAuth — the
page shows the exact redirect URIs for the running instance). Calendar is a
sensitive scope, so submit the app for verification before wide public use
(unverified = warning screen + 100-user cap).

---

## 6. Backups (Litestream)

[Litestream](https://litestream.io) streams the SQLite DB continuously to
**S3-compatible** object storage (≈1-second RPO) and **restores it automatically on
boot** if the volume comes up empty (lost/replaced). It's built into the image and
supervises the app (`entrypoint.sh` runs `litestream replicate -exec /calnode`).

**Backups are OFF until you set the env vars below** — until then the container
just runs the app directly. A single volume with no replica is the biggest
data-loss risk, so enable this before real bookings exist.

### Universal config (one set of vars, any provider)

Because R2, B2, MinIO, Spaces, Wasabi, and AWS all speak the S3 API, the same five
variables cover every provider — only the endpoint/region change:

| Variable | Purpose |
|---|---|
| `LITESTREAM_REPLICA_URL` | `s3://<bucket>/calnode` — the bucket + path. Setting this turns backups ON. (For native Google Cloud Storage use `gcs://` — see below.) |
| `LITESTREAM_ENDPOINT` | Provider S3 endpoint. **Leave unset for AWS.** |
| `LITESTREAM_REGION` | Bucket region (`auto` for R2). |
| `LITESTREAM_ACCESS_KEY_ID` | Access key (use a bucket-scoped key, not a root credential). |
| `LITESTREAM_SECRET_ACCESS_KEY` | Secret key. |

Per-provider values (everything else is identical):

| Provider | `LITESTREAM_ENDPOINT` | `LITESTREAM_REGION` |
|---|---|---|
| **Cloudflare R2** | `https://<account-id>.r2.cloudflarestorage.com` | `auto` |
| **Backblaze B2** | `https://s3.<region>.backblazeb2.com` | `<region>` (e.g. `us-west-004`) |
| **AWS S3** | *(unset)* | e.g. `us-east-1` |
| **MinIO / self-host** | `https://minio.example.com` | `us-east-1` |

**Two easy mistakes:**
1. `LITESTREAM_ENDPOINT` is the **account** endpoint — do **not** append the bucket
   name. Litestream takes the bucket from `LITESTREAM_REPLICA_URL`; a bucket in both
   places fails. (R2 shows the S3 API URL *with* the bucket — drop the last path segment.)
2. For R2, `LITESTREAM_REGION` is always **`auto`**, not the bucket's physical
   location (WNAM/ENAM/etc.). A real region string makes R2 reject the signature.

If the endpoint/region are empty or wrong, Litestream silently falls back to **AWS**
and you'll see `InvalidAccessKeyId` (403) in the logs (your R2 key sent to Amazon).

### Google Cloud Storage (native, not via the S3 API)

The bundled Litestream also speaks **GCS natively**, which is the simpler option on GCP:
credentials come from the instance metadata server, so there is no access key to create,
store or rotate. One variable is enough:

| Variable | Value |
|---|---|
| `LITESTREAM_REPLICA_URL` | `gcs://<bucket>/calnode` |

- ⛔ **The scheme is `gcs://`, not `gs://`.** `gs://` is the scheme every other Google tool
  uses, and Litestream rejects it with `unknown replica type in config: ""` and exit 1 —
  before any network call, so it looks nothing like a credentials or bucket problem.
- **Leave `LITESTREAM_ENDPOINT`, `LITESTREAM_REGION`, `LITESTREAM_ACCESS_KEY_ID` and
  `LITESTREAM_SECRET_ACCESS_KEY` unset.** The bundled `/etc/litestream.yml` needs no edit:
  its `endpoint:` and `region:` lines expand to empty strings, and a `gcs` replica ignores
  both.
- The service account attached to the instance needs read **and** write on the bucket
  (read is what restore uses). Everything else on this page — the restore drill, the
  private-bucket warning, the PII note — applies unchanged.

⚠️ **The trade: meeting recordings need the two S3 credential variables, so a native GCS
replica leaves recording storage unavailable.** `recordingStorage()`
(`internal/handler/livekit_recording.go`) reuses the backup bucket for recordings over the
S3 API and requires `LITESTREAM_ACCESS_KEY_ID` and `LITESTREAM_SECRET_ACCESS_KEY`; without
both it reports not-configured. It degrades cleanly rather than failing at record time —
**Settings → Storage** shows `recordings_storage_ready: false` and the room's Record button
stays hidden.

The provider table above has no GCS row, so "use the S3-compatible route" is not by itself
an answer on GCP. The two real options:

- **Put the replica on an S3-API bucket** (R2, B2, AWS, MinIO — the providers above) and
  set all five variables. This is the route this page documents end to end.
- **Or reach the same GCS bucket through Google's S3 interoperability API**: an HMAC key
  pair as `LITESTREAM_ACCESS_KEY_ID` / `LITESTREAM_SECRET_ACCESS_KEY`,
  `LITESTREAM_ENDPOINT=https://storage.googleapis.com`, and an `s3://` replica URL. That is
  the ordinary S3-compatible route with GCS as the provider, not the native `gcs://` one.
  ⚠️ We have not exercised recording this way — we run native `gcs://` with recordings off —
  so it is stated as the shape of the answer, not as a configuration we have verified.

`docs/VIDEO.md` §2 lists exactly what recording needs from either route.

### Enabling it
1. Create a **private** bucket and a **bucket-scoped** access key (read + write — read is needed for restore).
   *(Native GCS: create the bucket and grant the instance's service account read + write; there is no key.)*
2. Set the five variables on the service (Railway → Variables, or your platform's
   equivalent) — or, for native GCS, just `LITESTREAM_REPLICA_URL`.
3. Redeploy. On boot Litestream initialises and the first snapshot uploads within ~10s.
4. **Verify the round-trip** before relying on it (below).

> The backup contains booking PII (names, emails, intake answers) in plaintext —
> keep the bucket **private**. The encrypted secrets (SMTP/OAuth) stay sealed
> by the envelope-encryption key even inside the backup.

### Verifying / restoring
Recovery is automatic: if the volume comes up empty, `entrypoint.sh` restores the
latest replica from object storage before starting the app. To **prove the
round-trip without touching the live DB**, restore to a scratch path (never
`/data/calnode.db`) and confirm it's a valid SQLite file.

**Non-destructive drill, in the container (zero setup — litestream + creds + config are already there):**
```bash
railway ssh --service calnode sh -lc '
  rm -f /tmp/check.db
  litestream restore -config /etc/litestream.yml -o /tmp/check.db /data/calnode.db
  ls -l /tmp/check.db          # non-zero, ~ the live DB size
  head -c 16 /tmp/check.db; echo   # "SQLite format 3" = valid, replayed from the replica
  rm -f /tmp/check.db
'
```
It writes only to `/tmp` (ephemeral) and reads from the replica, so the running
app and the `/data` volume are untouched. (The runtime image has no `sqlite3` CLI,
hence the header check; for a full `PRAGMA integrity_check`, restore locally below.)

**Locally (full integrity check):** install the `litestream` binary, export the five
`LITESTREAM_*` vars, then:
```bash
litestream restore -config litestream.yml -o ./check.db /data/calnode.db
sqlite3 ./check.db "PRAGMA integrity_check;"   # expect: ok
```

Run a drill after first enabling backups, and periodically thereafter — an
unverified backup isn't a backup.

---

## 7. Built-in video & recording (LiveKit)

Optional — only if you want self-hosted in-browser meetings as a booking location.
Full setup guide, including recording consent, the AI notetaker, and host controls:
[docs/VIDEO.md](docs/VIDEO.md).

- **Credentials live in the app, not env vars.** Create a project at
  [cloud.livekit.io](https://cloud.livekit.io) (or self-host LiveKit), then enter the
  **Server URL** (`wss://…`), **API Key**, and **API Secret** in **Settings → Video**.
  The built-in video location then becomes selectable on an event type.
- **Recording reuses your Litestream bucket.** If `LITESTREAM_*` (§6) is configured,
  meeting recordings are written to that same bucket under a `recordings/` prefix — no
  extra storage to set up. Turn recording on in **Settings → Video / Storage**; downloads
  appear on the **Recordings** page (presigned, short-lived URLs).
- **Register the webhook (recommended).** In **Settings → Video**, copy the webhook URL
  (`https://<your-domain>/v1/livekit/webhook`) into **LiveKit Cloud → Project → Settings →
  Webhooks**, and attach the **same API key** you entered above (it signs the events).
  Recordings still finalize without it — the webhook just adds accurate duration and a
  clean "room closed" backstop. A healthy hit shows `POST /v1/livekit/webhook → 200`; a
  `403` means the webhook's API key doesn't match the one in Settings → Video.

---

## 8. First run

Open `https://<your-domain>/` → it redirects to `/admin/`. On a fresh database
you'll be guided through **first-run setup** (create the owner account). Then:
Settings → Email, → Google OAuth, → Branding (logo, business name), and create your
first event type + availability.

---

## 9. Troubleshooting

| Symptom | Likely cause |
|---|---|
| App won't start (prod) | Missing `CALNODE_ENCRYPTION_KEY` with an https `BASE_URL`. |
| Custom domain 502 / "Application failed to respond" | Custom-domain **target port ≠ the listening port** (use 8080 on Railway). |
| `ERR_CERT_COMMON_NAME_INVALID` on a new domain | Cert not issued yet — wait; ensure the DNS record is **DNS-only**, not proxied. |
| 403 on admin actions behind a proxy | Proxy not forwarding the original `Host` header (CSRF same-origin check). |
| OAuth `redirect_uri_mismatch` | Registered URI doesn't match `BASE_URL` + `/v1/...callback` exactly. |
| Email `550 domain not verified` | From address domain isn't verified with your email provider. |
| Logo broken in email when testing locally | Gmail's image proxy can't reach `localhost` — only loads from a public URL. |
| Litestream `InvalidAccessKeyId` / 403, log shows `endpoint=""` | `LITESTREAM_ENDPOINT` unset (or the running build predates the endpoint/region config) → Litestream defaults to AWS. Set the **account** endpoint (no bucket) + `region=auto` for R2, and redeploy so the config is live. |

---

## 10. Multi-tenant mode (this fork)

`MULTI_TENANT` turns one process into a host for many isolated workspaces. Read
**[docs/MULTI_TENANT.md](docs/MULTI_TENANT.md)** before deploying it: what follows is
the deployment shape, and that document is the contract.

### What it needs

1. **PostgreSQL 16+ with two roles.** An owner (`BYPASSRLS`) that owns the schema,
   and an application role (`NOBYPASSRLS`) that owns nothing and holds DML on the
   schema's tables. ⛔ Not one role with two DSNs: startup refuses that, because one
   role means the row-level security policies are inert against the connection they
   exist to confine, and nothing looks broken when they are.
2. `MULTI_TENANT`, both DSNs, `CALNODE_PLATFORM_TOKEN`, `BASE_URL`, and
   `CALNODE_SSO_SHARED_SECRET` when Google or Microsoft login is configured (the
   callbacks hand off through it). `TRUSTED_PROXY_CIDRS` if you are behind a proxy.
3. DNS for each workspace's `public_host` pointed at the instance, with TLS
   terminated for it. An unrecognised host is a **404**, never a fallback to a
   default tenant.

Boot order is migrate on the platform handle → enable row-level security → suspend
the seeded `default` workspace → verify both roles. The first, second and fourth are
fatal; the third is logged.

### Provisioning

Workspaces are created through `POST /v1/platform/workspaces`, not through the
console. One transaction writes the workspace, its settings row, the owner, the
first API key, a webhook subscribed to every event the codebase emits, and a default
event type with availability. The response carries `api_key` and `webhook_secret`
and shows them **once**. `docs/MULTI_TENANT.md` has the request body, the rest of
the platform routes (get, patch, delete, export, import, attendee erasure, the
member and API-key routes) and the rules each of them enforces.

### `ADMIN_SPA=off` and the bare root

With the embedded console off, `GET /{$}` does **not** 404. It serves a neutral
index: that workspace's public, active event types, each linking to `/book/<slug>`,
on the booking pages' own stylesheet and branding, translated through the same
plumbing, `noindex`, under the strict public CSP. A workspace with nothing public
gets the same page and one sentence.

That is a deliberate choice rather than a convenience. Trimming a booking link back
to the domain is a thing people do, and on a tenant's own public host a bare 404
says "no such path" when what is true is "the console that used to be here is gone".
The index lists nothing the booking pages do not already publish to the same
audience: no host names, no descriptions, no counts. The unknown-host 404 still
fires, earlier, and carries that other answer.

⛔ **With the console off, an SSO hand-off needs an explicit `next` outside
`/admin`.** The default destination is `/admin/`, which now 404s, so a hand-off
without one answers 404 **before** minting the session rather than seating a session
and landing the person on a dead end. An explicit `next` **under** `/admin` is
refused for the same reason: native clients send `next=/admin/` on every hand-off,
so deferring to the caller would have made the guard fire for nobody.

### Scraping `/metrics` from a cluster

`GET /metrics` is bearer-gated on `METRICS_TOKEN` and 404s without it, which is
right for a publicly reachable endpoint and wrong for the one caller that has to
read it: a Prometheus collector using annotation autodiscovery sends **one** bearer
token file to every target it scrapes, so pointing it at this token would present
this instance's secret to every other scraped pod on the cluster. Set
`METRICS_ALLOW_UNAUTHENTICATED_FROM` to the networks the collector dials from (on
Kubernetes, the cluster's pod CIDR) and those requests are served with no bearer.
⛔ It matches the TCP peer, so the endpoint must not be reachable through a proxy
inside the allowed range, or every request arrives wearing that proxy's address.
