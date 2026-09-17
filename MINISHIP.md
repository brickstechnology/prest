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
5. **This record, and the check on it** — a vendored copy takes an upstream
   fix only when somebody notices it exists, so the fork point is written where
   a program can read it and the branch is held to what is written.
   `miniship/` parses this file; CI checks out the whole history so its tests
   can compare the branch with the fork point, and also runs on a
   `rebase/...` branch so a rebase is checked before it replaces `miniship`.
   Paths: `MINISHIP.md`, `miniship/`, `.github/workflows/miniship.yml`

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

WALK-RESULT-PLACEHOLDER

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
