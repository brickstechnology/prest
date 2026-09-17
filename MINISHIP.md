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

**This table is the one place the fork point is stated.** Two programs read
its first two rows, so keep their shape — one row each, the value in
backticks:

- `miniship/`'s tests, which fail when the commit is not under this branch or
  the tag names another commit;
- miniship-cloud's `upstream prest` workflow, which opens an issue on that
  tracker for each upstream release or published advisory newer than the fork
  point.

A `prestd` built from this branch reports the tag on the miniship line:
`prestd version` ends in `2.4.2+miniship`.

Upstream's `LICENSE` (MIT) is kept unchanged. miniship's changes are commits on
`miniship` above the tag, and miniship's public monorepo pins one commit of this
branch.

## The patches

One entry for each change on `miniship`, oldest first: its name, why, and a
`Paths:` line naming every file or directory (with a trailing `/`) it changes.
`miniship/record_test.go` fails when the branch changes a path against the
fork point that no entry names, when an entry names a path the branch does
not change, and when the numbers skip or repeat — so a patch cannot land
unrecorded, and two pull requests that both add entry 5 cannot both merge
as written.

1. **CI** — `.github/workflows/miniship.yml` runs `go vet` and `go test` with
   Postgres on every push and pull request to `miniship`.
   Paths: `.github/workflows/miniship.yml`
2. **A database `rest` was not given is refused** — without a registry,
   upstream tried any path segment as a database of that name on its one
   configured host. `connection.Manager` now resolves a registry alias or the
   configured database and nothing else, `IsRegistered` agrees, and the table
   read answers such a name 404 without opening a connection.
   Paths: `adapters/postgres/internal/connection/conn.go`,
   `adapters/postgres/internal/connection/conn_test.go`,
   `adapters/postgres/internal/connection/given_test.go`,
   `adapters/postgres/postgres.go`, `adapters/postgres/postgres_test.go`,
   `adapters/postgres/query_registry_test.go`, `controllers/crud.go`,
   `controllers/crud_test.go`,
   `integration/postgres/adapters/postgres/postgres_test.go`
3. **The health answer touches no `Database`** — upstream's `/_health` pinged
   the default database, which would keep a suspended compute awake.
   `DefaultCheckList` is empty.
   Paths: `controllers/health_db.go`
4. **Two routes** — of the 26 that `router.RegisterRoutes` registers upstream,
   `rest` keeps `GET /{database}/{schema}/{table}` and `GET /_health`, and a
   method the table path does not serve answers 404 rather than 405. The
   handlers behind the removed routes stay in the tree, unrouted, so a rebase
   does not conflict on them. `router/surface_test.go` walks the router and
   fails on a third route; `app/surface_test.go` sends a request of every
   removed shape; `integration/postgres/router/surface_test.go` asks a real
   Postgres for a database `rest` was not given.
   Paths: `router/router.go`, `router/surface_test.go`, `app/surface_test.go`,
   `integration/postgres/router/surface_test.go`
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
10. **This record, and the check on it** — a vendored copy takes an upstream
    fix only when somebody notices it exists, so the fork point is written where
    a program can read it and the branch is held to what is written.
    `miniship/` parses this file; CI checks out the whole history so its tests
    can compare the branch with the fork point, and also runs on a
    `rebase/...` branch so a rebase is checked before it replaces `miniship`.
    Paths: `MINISHIP.md`, `miniship/`, `.github/workflows/miniship.yml`
11. **The binary names its upstream tag** — upstream's fallback version is
    `2.0.0`, and miniship's image is built without the `-ldflags` that replace
    it, so `prestd version` said `2.0.0` on a `v2.4.2` tree. It says
    `2.4.2+miniship` now, and `cmd/version_miniship_test.go` holds it to the
    fork point above, so a rebase that moves one and not the other is red.
    Paths: `helpers/prest.go`, `cmd/version_miniship_test.go`

## Taking an upstream fix

miniship-cloud's `upstream prest` workflow runs once a day and opens an issue
there, labelled `upstream-prest`, for each upstream release or published
security advisory newer than the fork point. An advisory usually names the
release that fixes it; an upstream fix often ships in a routine release
before its advisory is published, which is why releases are watched too.
**`miniship` is never force-pushed, reset or rewritten; it moves only by
merging a pull request.** The public monorepo pins commits of it, and a
rewrite would orphan every one of them. So the patches are rebased on a
branch of their own, and that branch is merged into `miniship` rather than
put in its place. Taking a fix is five steps, and the order matters:

1. **Rebase the patches, on a `rebase/<tag>` branch.** In a worktree off a
   fresh `origin/miniship`, fetch upstream's tags and replay the patch chain
   onto the new tag. The chain is every commit since the last rebase, which
   the tag `patches/<old tag>` marks; before the first rebase, it is
   `<old tag>..origin/miniship`. Past pull requests' merge commits are
   dropped and the patches replayed one by one:

   ```sh
   git fetch upstream --tags
   git fetch origin --tags
   git switch -c rebase/v2.5.0 origin/miniship   # first rebase only
   git rebase --onto v2.5.0 v2.4.2
   ```

   After the first, start from the last chain and add what landed since:

   ```sh
   git switch -c rebase/v2.6.0 patches/v2.5.0
   git cherry-pick $(git rev-list --reverse --no-merges took/v2.5.0..origin/miniship)
   git rebase --onto v2.6.0 v2.5.0
   ```

   Resolve each conflict in favour of upstream's change, then put the patch's
   intent back on top of it.
2. **Update this record, in a commit on that branch.** The two fork-point
   rows, `helpers.PrestVersionNumber`, and any `Paths:` line the rebase
   changed. The tests read all three, so they cannot pass until the record is
   right. Tag the chain as it stands, `git tag patches/v2.5.0`: it is where
   the next rebase starts.
3. **Run the fork's suite.** `go vet ./...`, and `go test ./...` with the
   Postgres `.github/workflows/miniship.yml` starts. Pushing the `rebase/...`
   branch runs that workflow on it.
4. **Merge it into `miniship`.** Record the old line as a parent while keeping
   the rebased tree exactly, then open a pull request and merge it with a merge
   commit. Tag the merge, so step 1 can find what lands after it:

   ```sh
   git merge -s ours origin/miniship -m "rebase: the patches, on v2.5.0"
   git push origin rebase/v2.5.0 patches/v2.5.0
   # pull request rebase/v2.5.0 -> miniship, merge commit
   git tag took/v2.5.0 <that merge> && git push origin took/v2.5.0
   ```

   `git diff v2.5.0 origin/miniship` is then the patch set and nothing else,
   and every commit ever pinned is still in `miniship`'s history.
5. **Bump the public pin.** `ARG REST_COMMIT` in miniship's
   `docker/Dockerfile`, to the new head of `miniship`, with the upstream tag
   named in the comment beside it. Then close the issue with a link to that
   pull request.

### Walked once, 17 September 2026

Upstream's newest release is still `v2.4.2`, the tag this record already names,
so there was nothing real to rebase onto. The walk was made against upstream's
`main` head instead — `254f30a`, two commits past `v2.4.2` — in a scratch clone,
under a local stand-in tag `v2.4.3` at that commit. **Nothing from the walk was
pushed**, and no tag named `v2.4.3` exists in this repository or upstream.

| | |
| --- | --- |
| Step 1, rebase | `git rebase --onto v2.4.3 v2.4.2` replayed **9 commits**, dropping pull request #1's merge. **One conflict**, in `adapters/postgres/postgres_test.go` |
| Step 2, record | the two fork-point rows and `helpers.PrestVersionNumber`, to `v2.4.3` and `2.4.3+miniship`. No `Paths:` line moved |
| Step 3, suite | `go vet` clean; **22 unit packages ok**, `miniship` and `cmd` among them; integration exit 0, **80 top-level tests ran and 126 skipped** |
| Step 4, merge | `git merge -s ours` onto the old line, then `git diff v2.4.3 HEAD` — **20 files, and every one is a path a `Paths:` line above names** |
| Hands-on | under half an hour, most of it the one conflict. Wall clock was longer, waiting on a loaded machine rather than on the procedure |

**The conflict was an add/add, and both sides were kept.** Upstream's two new
commits ([#1032], [#1033]) append a block of join-permission tests to the end of
`adapters/postgres/postgres_test.go`, which is where patch 2 appends
`TestDbFromCtx_refusesADatabaseItWasNotGiven`. Nothing about the two changes
disagrees; git could not tell that from position alone. Resolving it was closing
upstream's last function and putting the miniship test after it. **Expect this
one again**: patch 2 and patch 4 both end their files, so an upstream commit
that also ends one conflicts on position every time.

**The skip count moves with upstream, and that is not a regression.** The line
above records 80 run and 109 skipped on this branch. The walk skipped 126,
and the 17 are exactly upstream's new
`integration/postgres/controllers/join_restrict_test.go`, which needs a deployed
`prestd` like the rest of `integration/suites/...`. A rebase should expect the
second number to grow and the first to hold.

**What the walk did not prove.** It ran on a stand-in tag, so it could not run
step 4's pull request or watch `miniship.yml` fire on a `rebase/**` push; those
two steps are written and untried. And a real upstream tag may carry changes
this one did not — two commits is a small sample of what a release moves.

[#1032]: https://github.com/prest/prest/pull/1032
[#1033]: https://github.com/prest/prest/pull/1033

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
