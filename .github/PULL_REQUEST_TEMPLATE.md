<!-- Thanks for the contribution. Delete any section that genuinely does not apply. -->

## What changed

<!-- One or two sentences. The diff says what; this says it in words. -->

## Why

<!-- The problem, not the patch. If it fixes an issue, link it (Fixes #123). If you hit
     it in practice rather than reading the code, say what you saw. -->

## How it was tested

<!-- Commands you actually ran, and what they said. "Should work" is not a test. -->

- [ ] `go test ./...` green (SQLite)
- [ ] `make build` succeeds
- [ ] Touches SQL, a migration, a constraint path or tenancy, and so was **also** run
      against PostgreSQL with `CALNODE_TEST_POSTGRES_DSN` set, with the
      `TestPostgres_*` cases confirmed RUNNING rather than skipped
- [ ] Touches `frontend/src/lib/components/ui/**`, `app.css` or the theme, and so
      `pnpm test:visual` was run
- [ ] Behaviour described in `docs/ARCHITECTURE.md` changed, and that section was
      updated in this PR

## Upstream

District Scheduler is a fork of [Calnode](https://github.com/Calnode/calnode) and most
of this code is shared. A fix in shared code is worth more upstream, because every
deployment gets it.

- [ ] **Should this go upstream to `Calnode/calnode`?** Rough test: it would make sense
      on an instance running SQLite, single-tenant, with no platform API.

Ticking that box is all that is asked; you do not have to open the upstream PR. We
forward changes under our own name and crediting you, and we ask you first. Leave it
unticked for fork-only code: multi-tenant mode, the platform API, row-level security,
the PostgreSQL build, `Dockerfile.district`.

## Anything a reviewer should know

<!-- A decision you were unsure about, something you deliberately left out, a follow-up
     you think is needed. Saying "I could not test X" here is useful, not a problem. -->
