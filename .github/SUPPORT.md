# Getting help

District Scheduler is the scheduling engine behind District AI, published as an
Apache-2.0 fork of [Calnode](https://github.com/Calnode/calnode). Where to go depends
on what you are asking about.

- **A question about District AI, the product** (booking pages, plans, your account):
  <https://www.distronode.com/support>. This repository is the engine, not the product
  desk.
- **A bug in fork-only code** (multi-tenant mode, the platform API, row-level security,
  the PostgreSQL image): [this repository's Issues](https://github.com/distronode-corporation/district-scheduler/issues).
  None of it exists upstream, so it is ours to fix.
- **A bug in upstream code** (the booking engine, the calendar providers, the SQLite
  build, the admin UI, a locale): [Calnode/calnode/issues](https://github.com/Calnode/calnode/issues).
  Reporting it there gets every deployment the fix rather than only ours. We send fixes
  there ourselves, and we would rather your report reached the whole project.
- **A security vulnerability:** use this repository's **Security** tab, privately. See
  [SECURITY.md](../SECURITY.md). Never open a public issue for one.
- **Setup help or "how do I…":** Discussions, not Issues.

⚠️ **We run this ourselves; we are not a support desk for your deployment.** We read
issues about the code and we fix the ones we can reproduce. We cannot debug your host,
your reverse proxy, your database or your calendar provider's account, and an issue that
is really one of those will be closed with a pointer rather than an answer.
