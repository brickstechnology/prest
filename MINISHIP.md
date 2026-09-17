# miniship's patch line

This repository is `brickstechnology/prest`, a GitHub fork of
[`prest/prest`](https://github.com/prest/prest) in its own fork network. It
builds `rest`, the service that serves a miniship `Project`'s `public` schema
over HTTP. Why a fork of pREST, and not PostgREST or something written from
scratch, is recorded in miniship-cloud's ADR 0017.

## The fork point

| | |
| --- | --- |
| Upstream tag | `v2.4.2` |
| Upstream commit | `9070bda7e9ab6b6484e04a0983b8afd61c8315e6` |
| Branch | `miniship`, the fork's default branch |
| Go module path | `github.com/prest/prest/v2`, kept so an upstream rebase does not conflict on every import |

Upstream's `LICENSE` (MIT) is kept unchanged. miniship's changes are commits on
`miniship` above the tag, and miniship's public monorepo pins one commit of this
branch.

## The patches

One line for each change on `miniship`, oldest first.

1. **CI** — `.github/workflows/miniship.yml` runs `go vet` and `go test` with
   Postgres on every push and pull request to `miniship`.
2. **A database `rest` was not given is refused** — without a registry,
   upstream tried any path segment as a database of that name on its one
   configured host. `connection.Manager` now resolves a registry alias or the
   configured database and nothing else, `IsRegistered` agrees, and the table
   read answers such a name 404 without opening a connection.
3. **The health answer touches no `Database`** — upstream's `/_health` pinged
   the default database, which would keep a suspended compute awake.
   `DefaultCheckList` is empty.
4. **Two routes** — of the 26 that `router.RegisterRoutes` registers upstream,
   `rest` keeps `GET /{database}/{schema}/{table}` and `GET /_health`, and a
   method the table path does not serve answers 404 rather than 405. The
   handlers behind the removed routes stay in the tree, unrouted, so a rebase
   does not conflict on them. `router/surface_test.go` walks the router and
   fails on a third route; `app/surface_test.go` sends a request of every
   removed shape; `integration/postgres/router/surface_test.go` asks a real
   Postgres for a database `rest` was not given.
5. **A read becomes the anonymous role** — upstream reads as the login it
   connects with. The table read now opens a read-only transaction, runs
   `SET LOCAL ROLE` to the database's configured role (`anon_role` on a
   registry entry, `DATABASE_ANON_ROLE_<n>` beside its URL, or `pg.anon_role`
   with no registry), reads, and rolls back. There is no default: a database
   given no role answers 500, and so does a role that cannot be entered, and
   neither reads as the login instead. A privilege the role lacks answers 403
   in Postgres's words. `adapters/anonymous_role.go` holds the two new
   interfaces, so upstream's `QueryExecutor` and its mocks are unchanged.
   `rest` runs with pREST's own access list off (`access.restrict = false`),
   so the database's grants and row security decide which rows arrive.
   `integration/postgres/anonymousrole` asks a real Postgres for rows the role
   may not have, and prints how many of its subjects got that Postgres;
   `miniship.yml` fails when that line is missing.
   Paths: `adapters/anonymous_role.go`, `adapters/postgres/anonymous_role.go`,
   `adapters/timescaledb/anonymous_role.go`, `config/config.go`,
   `config/database_registry.go`, `config/anonymous_role_test.go`,
   `controllers/crud.go`, `controllers/crud_test.go`,
   `controllers/crud_anonymous_role_test.go`, `controllers/deps.go`,
   `app/surface_test.go`, `integration/postgres/anonymousrole/`,
   `integration/postgres/router/surface_test.go`,
   `.github/workflows/miniship.yml`
6. **`public` alone** — the table read answers 404 for any other schema,
   before any SQL is built.
   Paths: `controllers/crud.go`, `controllers/crud_anonymous_role_test.go`,
   `app/surface_test.go`
7. **CORS for reads only** — the defaults allow any origin `GET`, `HEAD` and
   `OPTIONS`, and never credentials. Upstream allowed the write verbs, with
   credentials.
   Paths: `config/config.go`, `config/anonymous_role_test.go`
8. **`prestd health`** — asks the server in the same container for `/_health`
   on loopback, or at the address it is given, and exits 0 on 200, because the
   image carries no shell and no HTTP client for a container health check to
   run.
   Paths: `cmd/health.go`, `cmd/health_test.go`, `cmd/root.go`
9. **A bounded pool for an environment registry entry** — upstream gave a
   `DATABASE_URL_<n>` entry 0 open connections, which `database/sql` reads as
   no limit. It now takes `pg.maxopenconn` and `pg.maxidleconn`.
   Paths: `config/database_registry.go`, `config/anonymous_role_test.go`

## Upstream's tests that do not run here

`integration/suites/...` and parts of `integration/postgres/...` drive a
deployed `prestd` through upstream's whole route table, and skip unless
`make test-integration` has started one. They assert routes this fork removes,
so that target is not run here. In `miniship.yml` they skip, 109 of them, and
80 Postgres-backed tests run.

## Upstream workflows, and which run here

Upstream's `.github/workflows/` files stay in the tree so a rebase does not
conflict on them. Those that cannot or must not run in this fork are disabled
in the repository's Actions settings, not deleted.

| Workflow | Here | Why |
| --- | --- | --- |
| `miniship.yml` | runs | miniship's check |
| `build.yml` | disabled | pushes images to Docker Hub and ghcr.io and cuts releases, with secrets this fork does not hold |
| `coverage-pr-comment-fork.yml` | disabled | needs the `PR_TOKEN` organisation secret of `prest` |
| `duplicate-issue-detector.yml` | disabled | upstream's issue triage |
| `test-unit.yml` | disabled | `miniship.yml` runs the same tests; its coverage-comment jobs need a baseline run on `main` |
| `test-integration.yml` | disabled | drives a deployed `prestd` through upstream's whole route table, which this fork cuts to two routes |
| `test-integration-timescaledb.yml` | disabled | the same, against TimescaleDB |
| `codeql-analysis.yml` | kept | analysis only; it triggers on `main`, so here it runs on its weekly schedule |
| `lint.yml` | kept | `golangci-lint`, which upstream marks `continue-on-error` |
| `studio.yml` | kept | checks the Studio frontend when `studio/` changes |
