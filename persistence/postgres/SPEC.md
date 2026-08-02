# `github.com/MerseniBilel/warren/persistence/postgres` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §6.1](../../warren.md) |
| **Module** | own module (`warren/persistence/postgres`) |
| **Mode** | Wrap |
| **Wraps** | `jackc/pgx/v5`, `pressly/goose` |

## Problem

`warren/persistence` (§3.3) declares two ports — `Repository` and `UnitOfWork` —
and contains zero implementations, because it is a contract package (AGENT.md
invariant 5). Something has to actually open a connection pool, begin a
transaction, put it where repositories can find it, write the outbox rows in
that same transaction, and commit.

Doing that by hand in every service is where the DDD story usually breaks. §3.3
states the reason plainly: `UnitOfWork.Do` "is where DDD becomes real" — aggregate
state and the events the aggregate raised have to land in **one** commit, or the
outbox is not an outbox and events can be lost or double-published.

This package is the reference implementation of exactly that sequence for
Postgres. It is also the package the §1.7 dependency budget is measured against:
adding Postgres to a service must add `warren/persistence/postgres`, `pgx`, and
`goose` to the user's `go.mod` and nothing else.

## Goals

warren.md §6.1 lists six things this package provides. They are the goals, in
that order:

1. **`*pgxpool.Pool`** — a configured connection pool, available to the graph.
2. **A `UnitOfWork` implementation** — the §3.3 port, implementing the six-step
   sequence in Behaviour below.
3. **Transaction-context propagation** — the transaction begun by `Do` is put on
   the context, and repositories pick it up from there without being handed it.
4. **Outbox table + writer** — the writer that §5.5 says is "invoked by
   `UnitOfWork`" and "inserts events into `outbox` inside the business
   transaction".
5. **A health check** — §2.8: adapters self-register, and "`postgres` registers a
   ping check".
6. **Migration running at boot (optional)** — via `pressly/goose`, gated behind
   `postgres.RunOnStart()`.

Plus the one piece of surface a generated repository touches: `postgres.DB`,
which §6.1 calls "the only piece of framework magic and it does one thing:
return the ambient transaction if one exists, else the pool."

## Non-goals

- **Not an ORM.** §3.3: "The deliberate omission is an ORM. No GORM, no ent, no
  sqlc mandate. Generated Postgres repositories use plain pgx and are yours to
  edit." §9's ledger row for Postgres says the same in four words: "no ORM, by
  design". A team preferring sqlc writes the adapter with sqlc and the domain
  layer cannot tell.
- **Not a query builder, and not a migration authoring tool.** goose is the
  migration engine (§9, Mode **Vendor**); this package runs the set, optionally,
  at boot.
- **Not the ports.** `Repository` and `UnitOfWork` are declared in
  `warren/persistence` (§3.3) and specced there. This package implements them and
  redefines nothing.
- **Not the outbox relay.** §5.5 splits the outbox in two: the **writer** runs
  inside the business transaction, the **relay** is "a separate lifecycle
  component, leader-elected" that drains outbox → broker. Only the writer is in
  scope here. `warren/outbox` owns the relay — including
  `outbox.LeaderElection(outbox.PostgresAdvisoryLock)`, which is a Postgres
  mechanism configured from a non-Postgres package (see Open questions).
- **Never imports another adapter.** AGENT.md invariant 4: `broker/kafka` and
  `persistence/postgres` are mutually invisible. This package depends only on the
  core module's contract packages — which is what lets it publish events to a
  broker it has never heard of, through the outbox table.
- **Does not spec the generator.** The repository shown in §6.1 is produced by
  `warren g repository user/User --driver postgres`, a `warren/cli` command
  (§8) specced separately. This package owns the runtime surface that generated
  code compiles against; the CLI owns the template.

## Dependency audit

Two dependencies, per AGENT.md § Adding a dependency. **Neither audit has been
performed yet**, and that is outstanding work that blocks the `go.mod`.

| Library | Mode | Recorded in warren.md | Audit status |
|---|---|---|---|
| `jackc/pgx/v5` | **Wrap** (§6.1, §9) | §1.6 module list, §9 ledger ("no ORM, by design") | **Outstanding** |
| `pressly/goose` | **Vendor** (§9) | §1.6 module list, §9 ledger (no note) | **Outstanding** |

What warren.md records is the *decision* — which library, and its mode. What it
does not record is any of what AGENT.md step 1 demands: whether the repository is
archived, when it last shipped, what it pulls in transitively, and whether its
licence is compatible. There is no observation date against either row.

AGENT.md is explicit that this is not optional: "**A package with no written
audit does not go into a `go.mod`.** Star counts are not evidence." The initial
audit already caught two widely-recommended packages (`google/wire`,
`git-chglog`) being archived with no README admitting it. So:

- [ ] Audit `jackc/pgx/v5` — `gh api repos/jackc/pgx`,
      `gh api repos/jackc/pgx/releases/latest` — record findings and the
      observation date here, and add the date to the §9 ledger row.
- [ ] Audit `pressly/goose` the same way.
- [ ] Confirm the transitive set is small enough that §1.7 still holds: `+
      warren/persistence/postgres`, `pgx`, `goose` and nothing else appears as a
      *direct* dependency.

Note the mode mismatch worth resolving during the audit: §6.1's header line
carries a single **Mode Wrap** covering both libraries, while §9 records goose as
**Vendor**. Under AGENT.md § Modes those are different obligations — Wrap means
users must not import goose directly and a port sits in front of it; Vendor means
it is imported and used directly and swapping it is an accepted breaking change.
See Open questions.

## Public API

warren.md §6.1 gives the surface as usage, not as declarations. The Go below is
those call sites written out, with doc comments added. Every name that warren.md
does **not** state — the option types, the type `DB` returns, the constructor for
the `UnitOfWork` — is marked, and listed under Open questions rather than
invented.

```go
// Package postgres implements Warren's persistence ports against Postgres.
//
// It provides the connection pool, a UnitOfWork whose Do commits aggregate
// state and the outbox rows for the events those aggregates raised in one
// transaction, transaction propagation through the context, a ping health
// check, and optional migration running at boot.
//
// Repositories are plain pgx and plain SQL. There is no ORM, by design.
package postgres

import (
	"context"
	"embed"

	"github.com/MerseniBilel/warren"
)

// Module returns the Warren module that provides the connection pool, the
// UnitOfWork implementation, the outbox writer, and the ping health check, and
// that registers the lifecycle hooks which open the pool at start and close it
// at shutdown.
//
// It returns an inert value: nothing connects, migrates, or registers until the
// bootstrapper materialises the graph (warren.md §1.3, AGENT.md § Boot).
func Module(opts ...Option) warren.Module

// DSN sets the Postgres connection string. In the warren.md §2.4 configuration
// example it comes from Config.Postgres.DSN, which is validate:"required", so a
// missing WARREN_POSTGRES_DSN is a startup failure with the field path named.
func DSN(dsn string) Option

// MaxConns sets the maximum number of connections in the pool. The §2.4
// configuration example types this field as int32 with a default of 10.
func MaxConns(n int32) Option

// Migrations registers an embedded goose migration set. Without RunOnStart the
// set is registered but not applied.
func Migrations(fsys embed.FS, opts ...MigrationOption) Option

// RunOnStart applies the registered migrations during OnStart, before anything
// that depends on the schema starts.
func RunOnStart() MigrationOption

// DB resolves the handle a repository should use for one call: the transaction
// carried by ctx if UnitOfWork.Do put one there, and otherwise the pool.
//
// DB is a func type because warren.md §6.1 calls it — a generated repository
// holds a DB in a field and writes r.db(ctx).QueryRow(...). It is the only
// piece of framework magic in a generated repository, and it does one thing.
//
// warren.md does not name the type DB returns; see Open questions.
type DB func(ctx context.Context) /* query handle — type not fixed by warren.md */
```

**`Option` and `MigrationOption` are placeholder names.** warren.md §6.1 shows
`postgres.DSN(...)`, `postgres.MaxConns(...)`, `postgres.Migrations(fs,
postgres.RunOnStart())` as calls and never names their types. The names above
follow §4.1's `http.Option` / §5.1's `kafka.Option` pattern, and are marked as
open questions rather than treated as decided.

**Generated repository** — the shape this surface exists to support, quoted from
§6.1:

```go
type UserRepository struct{ db postgres.DB }   // DB resolves tx-from-context or pool

func (r *UserRepository) FindByID(ctx context.Context, id domain.UserID) (*domain.User, error) {
	row := r.db(ctx).QueryRow(ctx,
		`SELECT id, email, name, status FROM users WHERE id = $1`, id)
	var u domain.User
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.NotFound("user", id)
		}
		return nil, err
	}
	return &u, nil
}
```

Three things in that snippet are normative for this package:

1. `db` is a struct field of type `postgres.DB`, injected by the constructor —
   the context is **not** stored (AGENT.md § General); it is passed to `db` per
   call.
2. The mapping `pgx.ErrNoRows` → `errors.NotFound("user", id)` is how a
   driver-specific sentinel becomes a semantic error that the transport adapters
   can turn into 404 / `NotFound` / ack-and-log (§2.6). The generated template
   owns this line; this package must not swallow it upstream.
3. `ctx` is passed twice — once to `r.db(ctx)` to select the handle, once to
   `QueryRow(ctx, ...)` for the query itself. That is verbatim from §6.1 and is a
   consequence of `DB` being a resolver rather than a wrapper.

## Behaviour

### `UnitOfWork.Do` — the six-step sequence

warren.md §3.3, verbatim, is the contract this package implements. Postgres is
the reference implementation of exactly this:

1. **Begin transaction, put it on the context.**
2. **Run `fn`** — repositories pick the transaction up from context
   automatically. This is what `postgres.DB` does: `r.db(ctx)` returns the
   ambient transaction if one exists, else the pool.
3. **Drain `PullEvents()`** from every aggregate saved in this scope. `PullEvents`
   is declared on `domain.AggregateRoot` (§3.1) and §3.1 notes it is "drained by
   UnitOfWork".
4. **Insert those events into the outbox table in the same transaction.** §5.5:
   the writer "inserts events into `outbox` inside the business transaction".
5. **Commit** — state and events are atomic. §3.1: "Nothing is published until
   the `UnitOfWork` commits — that's what makes state changes and event
   publication atomic."
6. **Post-commit: signal the relay to publish.** The relay itself is a separate,
   leader-elected lifecycle component (§5.5) and is not this package.

§10 shows the caller's half — a handler wraps its saves in `h.uow.Do(...)` and
"Aggregate state + outbox row commit in ONE transaction" — and §3.2 shows the
same thing as core middleware, `app.Transactional(uow)`, which "wraps `Handle` in
a transaction; commits state + outbox atomically". Both routes drive this
implementation.

**How step 3 knows which aggregates were saved is not stated by warren.md.** It
says "every aggregate saved in this scope", which implies `Save` records the
aggregate somewhere the `UnitOfWork` can reach at commit time — plausibly on the
same context that carries the transaction. The mechanism is unspecified; see Open
questions.

### Lifecycle position

- **Boot (§1.3 step 6):** `OnStart` runs in dependency order, "pool → repos →
  consumers → servers". The pool is first. If `RunOnStart()` is set, migrations
  run here — before repositories, consumers, or servers exist to touch the
  schema.
- **Shutdown (§2.3, and AGENT.md § Shutdown), step 5 of 6:** "DB pools, broker
  connections close." That is **last**, after readiness has gone to 503 (1),
  after servers stopped accepting and drained (2), after consumers stopped
  fetching (3), and after the outbox relay flushed (4). The relay's final flush
  reads the outbox table — so this pool must still be open when step 4 runs. Only
  the force-exit deadline (6, default 30s) comes after.

This ordering is not negotiable (AGENT.md § Two orderings you may not
rearrange). The hook this package appends must therefore be ordered such that
reverse-order teardown puts it at step 5, not earlier.

### Health

§2.8: adapters self-register health checks and "`postgres` registers a ping
check". The check implements the `health.Check` port — `Name() string` and
`Check(context.Context) error` — and is registered by `Module`, not by the user.
It feeds `/healthz`, `/readyz`, and the gRPC health service, all of which read the
same registry.

### Errors

warren.md fixes exactly one error mapping for this package, and it lives in the
generated repository, not in the runtime: `pgx.ErrNoRows` →
`errors.NotFound("user", id)`. Per AGENT.md § Errors, everything this package
returns is a `warren/errors` semantic error, wrapped with `%w`.

**No other error text is fixed by warren.md** — not for a failed connection, a
failed migration, a rollback failure, or a commit failure. AGENT.md requires a
spec to state every error message, so those strings have to be agreed before
implementation. They are listed in Open questions rather than guessed at here.
One constraint is already fixed: AGENT.md § Errors says error messages tell the
user how to fix it, so "connection refused" alone is a bug in the message.

### The `*pgxpool.Pool` question — how §6.1 and invariant 3 both hold

§6.1 says this module "provides `*pgxpool.Pool`". §1.2 shows `*pgxpool.Pool` as a
binding in the platform scope. §2.4 shows user code writing
`func NewUserRepository(cfg Config, pool *pgxpool.Pool) domain.UserRepository`.
AGENT.md invariant 3 names `*pgxpool.Pool` specifically as a type that "may
not appear in any Warren exported signature".

Both hold, and the distinction is whose signature it is:

- **Warren's exported signatures stay clean.** Nothing in the Public API above
  mentions `*pgxpool.Pool`. One way both this and invariant 3 can hold: the pool
  reaches the container through a constructor passed to `warren.Providers(...)`,
  which takes `...any` (§2.1), so that constructor need not be exported. That is
  this spec's proposed reconciliation, not warren.md's — §6.1 states only that
  the module "provides `*pgxpool.Pool`" and gives no mechanism. See Open
  question 2.
- **The pool is injectable into user code as a deliberate escape hatch.** The
  binding exists so that a team that needs raw pgx can take it, exactly as §5.1
  offers "inject `*kgo.Client` directly" for Kafka. The generated repository does
  **not** use it — it takes `postgres.DB`, which is the port-shaped path.

The residual tension is real and worth a decision: AGENT.md says raw handles are
reachable "through **named escape hatches only** … An escape hatch is an explicit
opt-out, never the default path", and §2.4 presents `pool *pgxpool.Pool` in an
ordinary repository constructor with no opt-out ceremony. See Open questions.

## Testing

Per AGENT.md § Testing.

- **The `warren/persistence` contract suite must pass.** AGENT.md: "Every port
  change updates the contract suite first, then the drivers." This driver is
  correct when it passes that suite unmodified — the suite is owned by the port
  package, not by this one.
- **Unit tests: no Docker, no network, no sleeps.** That rules out a real
  Postgres in the default suite. What is unit-testable without one: `DB`'s
  resolution rule (transaction-on-context vs pool), option application, hook
  ordering, and the boot/shutdown position of the hooks.
- **Real Postgres behind `//go:build integration`.** Everything that needs a
  server — the six-step `Do` sequence end to end, outbox rows committing in the
  same transaction as aggregate state, rollback on `fn` returning an error, the
  ping health check, goose running the embedded set at start. §7.5 notes
  integration helpers spin real Postgres behind a build tag.
- **Golden-file tests for generated repositories.** AGENT.md: "Every generator
  needs a golden-file test. Templates break silently otherwise." The
  `warren g repository user/User --driver postgres` output is the golden file;
  it lives with the generator in `warren/cli` but it is this package's public
  surface that it must compile against, so a change to `postgres.DB` that breaks
  the golden file is this package's problem.
- **Golden-file tests for error text.** AGENT.md: "Every error message in a spec
  gets a golden-file test." That includes the `pgx.ErrNoRows` →
  `errors.NotFound("user", id)` rendering, and every message agreed under Open
  questions.
- `t.Parallel()` and table-driven subtests named for behaviour.
- **An atomicity test is the load-bearing one.** A test that commits an aggregate
  and asserts the outbox row is present, and a test that fails after `Save` and
  asserts *neither* is present. If those two pass, `Do` is doing its job; if the
  suite has neither, it does not matter what else it has.

## Definition of done

- [ ] Both dependency audits written up here with observation dates, and the §9
      ledger rows updated (AGENT.md § Adding a dependency).
- [ ] Open questions answered by the human and folded into this spec, in the same
      change that implements them.
- [ ] `Module`, the options, and `DB` exist with agreed signatures, each with a
      doc comment starting with the identifier's name.
- [ ] `UnitOfWork` implementation passes the `warren/persistence` contract suite.
- [ ] The six-step `Do` sequence is implemented and covered by the two atomicity
      tests above.
- [ ] Ping health check self-registers; no user wiring required (§2.8).
- [ ] Lifecycle hooks land at boot step 6 (first) and shutdown step 5 (last), with
      a test that asserts the ordering rather than a comment claiming it.
- [ ] Migrations run at boot only when `RunOnStart()` is set.
- [ ] No `*pgxpool.Pool`, `pgx.Tx`, or any other pgx or goose type in an exported
      signature (invariant 3), checked by a test or by `warren lint arch`.
- [ ] No import of any other adapter module (invariant 4).
- [ ] No committed `replace` directive (invariant 8).
- [ ] A hello-world Postgres service's direct `go.mod` requirements are
      `warren`, `warren/persistence/postgres`, `pgx`, `goose` and nothing else
      (§1.7), checked with `go mod graph`.
- [ ] `make ci` passes (once the Makefile exists — AGENT.md § Repository state).

## Open questions

1. **What type does `postgres.DB` return?** §6.1 fixes the call shape —
   `r.db(ctx).QueryRow(ctx, sql, args...)` returning something with `Scan` — but
   never names the type. If it is pgx's own interface, that is a driver type in
   an exported signature and invariant 3 forbids it. If it is a Warren-declared
   interface, its method set has to be agreed (does it carry `Query`, `Exec`,
   `CopyFrom`, `SendBatch`?), and every method added is a method a
   non-pgx implementation of the same idea would have to provide. This is the
   single most consequential undecided thing in the package.
2. **Is `*pgxpool.Pool`-into-user-code an acceptable default path?** §2.4's
   example constructor takes it with no opt-out ceremony, while AGENT.md
   invariant 3 requires raw handles to be reached "through named escape hatches
   only … never the default path". Either §2.4's example should be changed to
   take `postgres.DB`, or a named hatch (`postgres.Raw(func(pool *pgxpool.Pool)
   {...})`, matching `http.Raw`) should be specified and §6.1's "provides
   `*pgxpool.Pool`" reworded. Human's call.
3. **What are the option type names?** `Option` and `MigrationOption` above are
   inferred from §4.1/§5.1. Is `RunOnStart()` a `MigrationOption`, a plain
   `Option`, or a variadic bool-ish flag? warren.md shows only the call.
4. **Is the migration filesystem `embed.FS` or `fs.FS`?** §6.1 says
   `embeddedFS`. `fs.FS` is the wider interface and costs nothing; `embed.FS` is
   what the name says.
5. **How does the `UnitOfWork` learn which aggregates were saved?** Step 3 drains
   `PullEvents()` from "every aggregate saved in this scope", which requires the
   repositories' `Save` calls to register the aggregate with the in-flight unit.
   Via the context? Via a registry the `DB` resolver also reaches? warren.md
   never says, and this is the mechanism the atomicity guarantee rests on.
6. **What is the outbox table's schema, and who creates it?** §6.1 says this
   package provides "outbox table + writer"; §5.5 says `warren/outbox` owns the
   writer. Which package owns the DDL, and does it arrive as a goose migration
   this package ships, or is the user expected to author it?
7. **Where does the outbox writer actually live?** §5.5 places the writer in
   `warren/outbox` (Mode Build); §6.1 lists "outbox table + writer" among what
   `warren/persistence/postgres` provides. One of the two entries is wrong, or
   the split is writer-interface-in-`outbox` / Postgres-implementation-here — in
   which case `warren/outbox` needs a port and §5.5 does not describe one.
8. **`outbox.LeaderElection(outbox.PostgresAdvisoryLock)` names a Postgres
   mechanism from a non-Postgres package.** Advisory locks are a `pg_advisory_lock`
   call and need a connection from this pool. Does `warren/outbox` therefore
   import this module — which invariant 4 forbids between adapters — or is
   `PostgresAdvisoryLock` a symbol exported from here and passed in? The current
   spelling in §5.5 implies the former.
9. **How does `Do` signal the relay post-commit (step 6)?** The relay is a
   separate lifecycle component, possibly in a different process (it is
   leader-elected). Is the signal in-process only, an optimisation over the poll
   interval, or a `NOTIFY`?
10. **CDC mode.** §5.5 lists "CDC (Postgres logical replication, lower latency)"
    as an outbox mode. Logical replication is unambiguously a Postgres feature —
    does its implementation live here or in `warren/outbox`, and is it in scope
    for the first version at all?
11. **Is goose Wrap or Vendor?** §6.1's header covers both libraries with a single
    **Mode Wrap**; §9's Migrations row says **Vendor**. Under AGENT.md § Modes
    these are different obligations. Vendor reads correct for a boot-time
    migration runner, but the manifest should say one thing.
12. **What is the `UnitOfWork` implementation's constructor called, and is it
    exported?** §10 injects it as `h.uow`, so something provides
    `persistence.UnitOfWork` into the graph. If `Module` provides it via an
    unexported constructor, nothing needs exporting; if users are meant to wire it
    themselves, it needs a name.
13. **Is the ping health check's type exported, and can users configure it** —
    timeout, name, whether it gates readiness or only liveness? §2.8 says it
    self-registers and says nothing else.
14. **What are the error messages?** Connection failure at start, migration
    failure at start, `fn` returned an error and the rollback also failed, commit
    failed after the outbox rows were written. AGENT.md requires each to be
    stated here and golden-file tested, and requires each to tell the user how to
    fix it.
15. **This package owns the Postgres inbox dedupe store — decided 2026-08-02,
    rehomed here when `inbox/SPEC.md` was retired.** `warren/inbox` holds the
    port and a memory store only; the durable one ships beside the outbox
    table, in this package's migration set. What is still open is the schema
    and the migration, and four constraints bind it:

    - **The key is `"<subscription>\x00<Message.ID>"`, not the bare message
      id** — `broker.Pipeline` builds it. A stored key format is a migration,
      so this cannot be changed later without one.
    - **The dedupe row is written inside the handler's own `UnitOfWork`
      transaction.** That is the whole reason a Postgres store is worth
      having: it makes handler-success and mark-seen atomic, closing the
      crash-after-success duplicate window the memory store cannot. `ctx` is
      on both `Store` methods precisely to carry the transaction.
    - **Re-marking must refresh the TTL** — core's memory store does
      (`inbox_test.go`), Redis `SET … EX` does natively, and Postgres needs
      an explicit `ON CONFLICT … DO UPDATE`. A `warren/inbox/inboxtest`
      contract suite is owed so every driver is held to this rather than
      re-deriving it; §5.6 records the obligation.
    - **Expiry needs a reclaim strategy.** A row per message for 24h at any
      real rate is a large table; decide between a partial index plus a
      periodic `DELETE`, and native partitioning by day.
16. **`Repository[T domain.Root[ID], ID domain.ID]` (§3.3) constrains `T` to
    `domain.Root`, but §3.1 declares `AggregateRoot[T ID]` and no `Root`.** The
    port package owns the fix; flagged here because generated repositories have to
    satisfy whichever name survives.
