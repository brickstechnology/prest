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
