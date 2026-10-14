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
as written. **A directory claim is a promise about everything under it**,
so a later file added there needs no entry of its own; name the files when
you mean the files, and the directory only when the patch owns all of it.

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
    Paths: `MINISHIP.md`, `miniship/record.go`, `miniship/record_test.go`,
    `.github/workflows/miniship.yml`
11. **The binary names its upstream tag** — upstream's fallback version is
    `2.0.0`, and the public monorepo builds this with no `-ldflags` that
    replace it, so `prestd version` said `2.0.0` on a `v2.4.2` tree. It says
    `2.4.2+miniship` now, set by an `init` in a file of miniship's own rather
    than by editing upstream's literal, which upstream moves on every version
    bump and would conflict on every rebase. `cmd/version_miniship_test.go`
    holds it to the fork point above, so a rebase that moves one and not the
    other is red.
    Paths: `helpers/version_miniship.go`, `cmd/version_miniship_test.go`
12. **One `rest` serves two `Database`s** — upstream's
    `AdapterSelectorMiddleware` wrapped the router from outside it, where
    `mux` has not matched and `mux.Vars` is empty, so it never saw
    `{database}` and every read ran on whichever adapter the registry's map
    handed back first. The table read now resolves the path's `Database` in
    the handler, once the route has matched, and reads on the adapter
    registered for it. Each adapter is also given that one entry as its whole
    registry, so it can resolve no other `Database`: a read that reached the
    wrong adapter is refused rather than quietly answered from the right
    connection. The middleware stays in the tree, unused, as the removed
    routes' handlers do.
    Paths: `app/app.go`, `app/app_test.go`, `app/two_projects_test.go`,
    `controllers/crud.go`, `integration/postgres/twoprojects/`
13. **Composing `rest` opens no connection** — `app.New` connected to and
    pinged every registry entry at start-up, and probed TimescaleDB first,
    which is a second connection to each. At 250 `Project`s that woke 250
    computes against a ceiling of 20 concurrently active, before a caller had
    asked for anything, and one unreachable entry stopped the other 249 being
    served at all. An adapter is now created unconnected and its pool fills on
    the first call that names its `Database`; the current database an adapter
    answers for, which upstream set as a side effect of connecting, is set at
    construction instead. TimescaleDB is not detected, because detecting it
    *is* that connection and `rest` serves a `Project`'s `public` schema over
    one grammar; `timescaledb` stays in the tree for a rebase.
    Paths: `app/app.go`, `app/app_test.go`, `app/two_projects_test.go`,
    `integration/postgres/twoprojects/`
14. **`rest`'s connections say who they are** — every connection the pool
    opens carries lib/pq's `fallback_application_name=rest`, so a `Database`
    can tell its `rest` sessions from every other one in `pg_stat_activity`.
    A fallback, so an operator who names their own keeps it; added where the
    connection is opened rather than in the URI the pool is keyed by, because
    what `rest` calls itself is not part of which `Database` a pooled
    connection is to. Whether a URI already names one is asked of the
    *setting* — the name starts the URI or follows a delimiter — so a
    password that happens to spell it is a password, and the connection
    carrying it still gets a name. `miniship.yml` reads the live-Postgres
    line from both of `rest`'s integration packages now, not one.
    Paths: `adapters/postgres/application_name.go`,
    `adapters/postgres/internal/connection/conn.go`,
    `adapters/postgres/internal/connection/conn_test.go`,
    `adapters/postgres/internal/connection/application_name_test.go`,
    `integration/postgres/twoprojects/`, `.github/workflows/miniship.yml`

12. **The query string is reviewed, and every read is bounded** — `rest` is the
    one door the internet reaches directly, and four of pREST's six advisories
    on this surface were the same class of mistake in `_select`, `_count` and
    `_groupby`, each found after the last was patched. `controllers/query_screen.go`
    now stands in front of upstream's builders and rebuilds the query string out
    of the parameters it recognises, so they never see a byte it did not put
    there: `_join`, `_or`, `_korder`, `_renderer`, `_time_bucket`, the
    `:tsquery` and `:vecdist` filters and `_groupby`'s `->>having` and
    function-expression forms are gone, and what is kept takes unqualified names
    only — `_join` accepted `pg_catalog.pg_class` and was a live path out of
    `public`. Reads are bounded: a page-size ceiling of 5000, capped rather than
    refused and given to a read that asks for no page, and a 30s
    `SET LOCAL statement_timeout` inside patch 5's transaction, answering 504.
    A third bound, a 16KB ceiling on the query string itself, answers 414 and is
    the one refusal that reads nothing of it. The four `ltree` operators go too:
    on a text column `~` is a caller's POSIX regular expression. An error answer
    is now one of a fixed set — upstream returned the driver's words, the
    Database's host and port included — and quotes nothing of the request back.
    [`docs/miniship/query-string.md`](docs/miniship/query-string.md) is the
    review: every parameter, kept, restricted or removed, and why, with the
    three bounds' figures and where each was taken from.
    Paths: `docs/miniship/`, `controllers/query_screen.go`,
    `controllers/query_screen_test.go`, `controllers/crud.go`,
    `controllers/crud_test.go`, `controllers/deps.go`,
    `controllers/helpers.go`, `adapters/postgres/anonymous_role.go`,
    `config/config.go`, `middlewares/middlewares.go`,
    `middlewares/middlewares_test.go`, `app/query_screen_test.go`,
    `app/error_body_test.go`,
    `integration/postgres/anonymousrole/query_bounds_test.go`,
    `integration/postgres/anonymousrole/anonymous_role_test.go`
16. **A `Project` is admitted while `rest` runs, by a lookup on a miss** —
    upstream reads its registry once at start-up and says in its own
    documentation that there is no runtime API to add a database, so
    `Project` 251 interrupted the other 250. A name with no adapter is now a
    miss, and a miss is where `rest` learns: it asks the process holding the
    `Database` plugin, once, for that `Project` alone, gets an address and a
    credential, opens the pool and keeps it. Later calls find it in the
    adapter registry and ask nothing. `rest` stores nothing — a restart
    forgets every `Project` and learns each one again on its first call.
    `admission/` is the lookup and the four things around it, which are the
    four Neon's own proxy has: the registry as the cache, a permit per
    `Project` so concurrent first calls cost one lookup, a window so a burst
    for a name with no `Project` costs one lookup, and an invalidation on an
    authentication failure so a rotated credential costs exactly one more.
    The lookup address is internal: `rest` proves itself with a short-lived
    HS256 service token minted from the route's derived key, aimed at that
    route alone — the public repository's `packages/api-client`'s
    `assertion.ts` is the original and `admission/assertion.go` is the Go half
    of it. Two failures found on the way: the SQL was built by whichever
    adapter was registered first, so a `rest` given no `Database` at all
    emitted a three-part cross-database reference; and a name `rest` was not
    given was quoted back in the answer, which #549's rule forbids. Both are
    fixed here. `integration/postgres/admission` creates a third `Database`
    while the process runs, rotates a credential under it, and tries one
    `Project`'s credential against another's `Database`.
    Paths: `admission/`, `config/admission.go`, `config/config.go`,
    `config/admission_test.go`, `controllers/admission.go`,
    `controllers/crud.go`, `controllers/deps.go`, `app/app.go`,
    `app/admission_test.go`, `integration/postgres/admission/`,
    `.github/workflows/miniship.yml`
17. **A read holding a pool outlives the credential under it** — three things a
    rotation broke, each found by a subject that ran reads while one happened.
    The replaced pool was closed at the moment of the swap, under reads that
    were holding it, so a read that would have succeeded answered
    `sql: database is closed`; it is closed after a grace now, the 30s time
    limit a read is already given (#549), by which point a read still in flight
    has been cancelled by its own bound. And two answers named the wrong half
    of the system: a connection that went away during `SET LOCAL ROLE` was
    reported as the role not being enterable, which sends an operator to read
    the grants, and a `Database` refusing `rest`'s own login answered **400**,
    which tells a browser its request was wrong when nothing about it was. Both
    are the `Database` not being readable — 502 — and #549's `readFailure`
    already knew that; it only had to be given the error.
    A fourth thing, from the same slice: `ADMISSION_TIMEOUT`,
    `ADMISSION_WINDOW` and `ADMISSION_MAX_PROJECTS` are read unprefixed now, as
    `ADMISSION_URL` and `ADMISSION_KEY` already were. A compose file that
    spells two of the five without a prefix and finds the other three silently
    ignored is a trap, and it caught the public monorepo's own stack test
    first. A value that does not parse is left at its default *and says so*,
    rather than being read as zero.
    Paths: `admission/gate.go`, `app/app.go`, `controllers/crud.go`,
    `adapters/postgres/anonymous_role.go`, `config/admission.go`,
    `config/admission_test.go`,
    `integration/postgres/admission/admission_test.go`,
    `integration/postgres/twoprojects/two_projects_test.go`
18. **A verified token becomes the role it names** — miniship-cloud's ADR 0030
    has `rest` do what PostgREST does. A token that verifies against the
    install's JWKS makes the read that token's role, which is `service_role`
    for the install's api service token. A request with no `Authorization`
    stays its `Database`'s anonymous role. Anything else in `Authorization` is
    refused with 401 and a Bearer challenge, before any `Database` is asked
    about.
    Upstream's `JwtMiddleware` does not do this. It verifies ES256 against a
    JWKS, but it refuses a request that has no token, and it throws the
    token's claims away. miniship-cloud#801's thread has the measurement.
    `jwt.role_claim` (`PREST_JWT_ROLE_CLAIM`) switches on
    `middlewares/token_role.go` in upstream's place. It needs an asymmetric
    `jwt.algo` and a JWKS. `Debug` does not switch it off. A token also needs
    an expiry and a role. An answer read as a token's role is never written to
    the response cache.
    The lookup on a miss changes too. It sends the install's api secret key in
    `apikey` to the gateway's service listener, which swaps it for the api
    service token. Patch 16's HS256 service token, `admission/assertion.go`,
    is gone.
    Paths: `context/token_role.go`, `middlewares/token_role.go`,
    `middlewares/token_role_test.go`, `middlewares/config.go`,
    `config/config.go`, `config/admission.go`, `controllers/crud.go`,
    `controllers/crud_token_role_test.go`, `admission/admission.go`,
    `app/admission_test.go`, `integration/postgres/admission/`,
    `integration/postgres/anonymousrole/token_role_test.go`

19. **The Supabase SDK's read, on `/rest/v1/{table}`** — miniship-cloud#1302,
    public ADR 0077: the owner chose that the fork answers PostgREST's language
    itself. `GET` and `HEAD` on `/rest/v1/{table}` read with the SDK's query
    string (`select`, the filters `eq` to `in` and `not.` of each, `or` and
    `and` groups, `order`, `limit`, `offset`), the single-object `Accept`, a
    count in `Prefer` with its `Content-Range`, and `Accept-Profile` held to
    `public`. Writes, function calls and embedded reads are refused by name;
    they are #1303 to #1305. The path names no Project: `PROJECT_HEADER` names
    the header a gateway hands it in, read only when set, and with none the
    lookup is asked for the install's one Project, at its address with no name.
    A User's token is carried past patch 18's check on this address alone and
    verified in the handler against the key set the auth api publishes for that
    Project, read from `PROJECT_KEYS_URL` with `{project}` in its path, kept for
    the five minutes the auth api says, and read again on an unknown key id at
    most once a window; it must name the role `authenticated` and the audience
    `authenticated`. Every read runs as its caller with the token's claims in
    `request.jwt.claims` and `request.jwt.claim.sub`, as PostgREST hands them.
    The bounds are patch 15's; over one is a refusal in PostgREST's shape
    (`MS100` rows, `MS101` query string, `57014` the time limit). The route
    table gains one route, and `router/surface_test.go` with it.
    Paths: `adapters/caller_read.go`, `adapters/postgres/caller_read.go`,
    `admission/admission.go`, `admission/gate.go`, `app/app.go`,
    `config/config.go`, `config/postgrest.go`, `context/token_claims.go`,
    `controllers/crud.go`, `controllers/deps.go`,
    `controllers/postgrest_query.go`, `controllers/postgrest_read.go`,
    `middlewares/token_role.go`, `projectkeys/`, `router/router.go`,
    `router/surface_test.go`,
    `integration/postgres/anonymousrole/postgrest_read_test.go`
20. **The Supabase SDK's read brings related rows** — miniship-cloud#1305,
    public ADR 0077: patch 19's read answers `select=*,other(*)` as PostgREST
    v12 does. A table named in `select` is an object where the relationship is
    many-to-one or one-to-one and an array where it is one-to-many or
    many-to-many through a join table (one whose two keys are part of its
    primary key); `alias:other(*)`, `other!fk_name(*)` (a constraint, a column
    or the join table), `other!inner(*)` and nesting to five deep are read, and
    the embedded table is filtered and ordered with `other.col=eq.1`,
    `other.order`, `other.limit` and `other.offset`. The relationship is found by
    PostgREST's own rules (`findRel`, Plan.hs at v12.2.3), from the foreign keys
    of `public` read from the catalog in the read's own transaction, once for
    each read that embeds and cached nowhere: none is `PGRST200` (400, with its
    "Perhaps you meant" hint), more than one is `PGRST201` (300, the candidates
    in `details`). Related rows are read by the caller's role in that
    transaction, so a row its rules hide is absent and a table it holds no grant
    on refuses the read. An embedded table's page is held to `MaxPageSize` as the
    top one is. The select also reads `alias:column`, `column::type` and
    `column->a->>b`, and a filter or an order may name a json path. A spread
    (`...other(*)`) and aggregates are refused by name.
    Paths: `controllers/postgrest_embed.go`, `controllers/postgrest_embed_test.go`,
    `controllers/postgrest_query.go`, `controllers/postgrest_read.go`,
    `controllers/postgrest_select.go`,
    `integration/postgres/anonymousrole/postgrest_embed_test.go`,
    `MINISHIP.md`

21. **The Supabase SDK's writes, on `/rest/v1/{table}`** — miniship-cloud#1303,
    public ADR 0077. `POST`, `PATCH` and `DELETE` on the address patch 19
    reads, in PostgREST v12's language and statuses, which the SDK at 2.117.3
    expects. `POST` inserts an object or an array of objects (201);
    `Prefer: resolution=merge-duplicates` upserts, on `on_conflict` or the
    primary key (200 when no row was new, as v12), and `ignore-duplicates`
    leaves a conflicting row. `columns` names the columns of a bulk body, and
    `missing=default` gives the columns their defaults where a row omits the
    key. `PATCH` sets the columns of its object on the rows the filters match.
    `DELETE` removes them. `return=representation` brings the changed rows
    back in the columns `select` names (201 for an insert, 200 for the others);
    without it nothing comes back (204 for `PATCH` and `DELETE`). `count=`
    answers `Content-Range`; a single-object `Accept` holds the write to one
    row; `tx=rollback` and `handling=strict` with `max-affected` are answered.
    A write runs as its caller's role with its claims, in one read-write
    transaction that commits only on success (`adapters.CallerWriter`), so a
    refusal leaves no partial change. Values are read by `json_to_recordset`
    against the column types the catalog reports; nothing of a body is SQL.
    An update or a delete that names no filter is refused, 400 and `21000`
    "requires a WHERE clause", as pg-safeupdate refuses it under Supabase.
    A refusal of the Database is passed on with its own message, SQLSTATE,
    details and hint and the status PostgREST maps the SQLSTATE to; the
    Database itself withholds a row's details from a role row security binds.
    **The row quota** is the read's page, `PG_MAX_PAGE_SIZE` (5,000 by
    default): a body of more rows is refused before the Database is asked,
    and a statement that changes more rows is rolled back, either way whole
    (400, `MS104`). A body longer than 8 MiB is refused (413, `MS105`). The
    time limit is the read's. `limit` and `offset` on a write, and embedded
    tables in its `select`, are refused by name. The route table gains one
    route, and `router/surface_test.go` with it.
    Paths: `adapters/caller_write.go`, `adapters/postgres/caller_write.go`,
    `controllers/postgrest_mutation.go`, `controllers/postgrest_write.go`,
    `controllers/query_screen.go`, `router/router.go`,
    `router/surface_test.go`,
    `integration/postgres/anonymousrole/postgrest_read_test.go`,
    `integration/postgres/anonymousrole/postgrest_write_test.go`

22. **The Supabase SDK's rpc(), on `/rest/v1/rpc/{function}`** —
    miniship-cloud#1304, public ADR 0077. `POST`, `GET` and `HEAD` call a
    function of `public` as PostgREST v12.2.3 does, and `PUT`, `PATCH` and
    `DELETE` are 405 `PGRST101` ("Cannot use the DELETE method on RPC",
    Error.hs, `InvalidRpcMethod`). `POST` sends the arguments as one JSON
    object; `GET` and `HEAD` send them as the query parameters that are not
    filters, a value that begins with an operator (`gt.2`, `not.in.(1,2)`)
    being a filter on the result and any other an argument (QueryParams.hs,
    `parse` with `isRpcRead`). The Database is told the arguments by name
    (`fn(a := 1)`, QueryBuilder.hs, `callPlanToQuery`), so one left out takes
    its default, and the function runs once, in a CTE the page and the count
    are read from (Statements.hs, `prepareCall`).
    The overload is found as Plan.hs's `findProc` finds it, by the argument
    names a call sends and not by their types: the names must cover every
    required argument and may add optional ones and nothing else. None is 404
    `PGRST202` with Error.hs's message, details and "Perhaps you meant to call
    the function" hint (the closest name when none has the name asked for, the
    closest argument list when one has, by edit distance where PostgREST uses a
    fuzzy set); several is 300 `PGRST203`. `Prefer: params=single-object`, and
    a body for a function of one unnamed json or jsonb argument, hand the
    function the whole body; a `VARIADIC` argument takes a JSON array, or every
    value of a repeated query parameter. The catalog query is SchemaCache.hs's
    `funcsSqlQuery`, narrowed to one name in `public` and read in the call's own
    transaction, once for each call and cached nowhere; where it zips
    `proargnames` with `proargtypes`, which lists only the input arguments and
    so misaligns an OUT argument before an IN one, this reads `proallargtypes`
    and the modes. A function can be seen by any role and run only by one with
    `EXECUTE`: a role without it is 42501, 401 for the anonymous role and 403
    for another, as PostgREST gives it.
    What comes back follows what the function returns (SqlFragment.hs, `asJsonF`;
    Response.hs, `actionResponse`): a scalar is the JSON value, one row of a
    table or of OUT arguments an object, a set an array of values or of objects,
    and `void` is 204; a single-object `Accept` holds a set to one row. A set
    takes the read's filters, order, `limit`, `offset` and `count`, and is cut at
    the read's page (`PG_MAX_PAGE_SIZE`; a `limit` above it is `MS100`).
    **`GET` and `HEAD` run in a read-only transaction**, which is how PostgREST
    refuses a function that writes: Plan.hs gives every `InvRead` the read mode,
    RpcSpec.hs ("only for GET rpc") expects 405 of a function that mutates, and
    the Database answers 25006. A `VOLATILE` function that writes nothing is
    answered on `GET`, as it is there. `POST` runs in a read-write transaction
    that commits only on success, as the caller's role with its claims in
    `request.jwt.claims`; `tx=rollback` and `handling=strict` with
    `max-affected` are answered. A function's own error is passed on with its
    message, SQLSTATE, details and hint and PostgREST's status for the SQLSTATE
    (P0001 is 400), and a function that raises SQLSTATE `PTnnn` answers `nnn`
    (Error.hs, `pgErrorStatus`).
    **The write quota counts a function's writes.** For a `VOLATILE` function
    called by `POST`, the rows the transaction has inserted, updated and deleted
    (`pg_stat_xact_user_tables`, the Database's own per-transaction counters)
    are read before the call and after it, and a call that changed more than the
    read's page is rolled back whole and refused, 400 `MS104`. It counts work
    done, so a row inserted and deleted by the same call counts twice, and it
    sees only what the counters see: `TRUNCATE` is not counted, and the call has
    run to its end before it is refused, within the time limit. Not answered,
    and refused by name: an embedded table in a function's select, `columns`,
    a body that is not JSON, any `Content-Type` but JSON, and a JSON array as
    the arguments of a function that takes them by name. The route table gains
    one route, and `router/surface_test.go` with it.
    Paths: `controllers/postgrest_call.go`, `controllers/postgrest_call_test.go`,
    `controllers/postgrest_routine.go`, `controllers/postgrest_query.go`,
    `controllers/postgrest_write.go`, `router/router.go`,
    `router/surface_test.go`,
    `integration/postgres/anonymousrole/postgrest_call_test.go`,
    `MINISHIP.md`

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
**97** Postgres-backed tests run, measured on a CI runner rather than on a
developer's machine. The first number is upstream's and moves with
a rebase; the second is this branch's and moves whenever a patch adds a subject,
so treat both as the last measurement rather than a promise. `miniship.yml`
prints them on every run's summary, beside the line
`integration/postgres/anonymousrole` prints about its own subjects.

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
