# `github.com/MerseniBilel/warren/persistence/postgres` — SPEC

| | |
|---|---|
| **Status** | **APPROVED AND IMPLEMENTED (2026-08-02)** — architect round complete, built, and verified against a real Postgres 18 (the integration suite passes, including `persistence.RunContract` unmodified). See **Divergences**. The dependency audit is done and **goose is rejected**; the spec is rewritten against `persistence.go` and `outbox.go` as they actually shipped, which answered eight of the previous draft's sixteen open questions. |
| **Source** | [warren.md §6.1](../../warren.md) |
| **Module** | own module (`warren/persistence/postgres`) |
| **Mode** | Wrap (`jackc/pgx/v5`) |
| **Wraps** | `jackc/pgx/v5`, and nothing else |

## Problem

`warren/persistence` declares `Repository` and `UnitOfWork` and contains zero
implementations, because it is a contract package (invariant 5). Something has
to open a pool, begin a transaction, put it where repositories find it, write
the outbox rows in that same transaction, and commit.

Doing that by hand in every service is where the DDD story usually breaks:
aggregate state and the events the aggregate raised have to land in **one**
commit, or the outbox is not an outbox and events are lost or double-published.
This package is the reference implementation of exactly that sequence.

## Dependency audit — 2026-08-02

| Package | Version | Licence | Stars | Last push | Archived | Modules compiled in | Mode |
|---|---|---|---|---|---|---|---|
| `jackc/pgx/v5` | v5.10.0 | MIT | 14 087 | 2026-08-01 | no | **6** | **Wrap** |
| `pressly/goose/v3` | v3.27.3 (2026-07-22) | MIT | 11 269 | 2026-08-01 | no | 5 | **REJECTED** |

`pgx` compiles in `jackc/pgpassfile`, `jackc/pgservicefile`, `jackc/pgx`,
`jackc/puddle`, `golang.org/x/sync`, `golang.org/x/text` — measured with
`go list -deps` on a program importing `pgxpool`, not read off a README. It is
the only Postgres driver with a native protocol implementation, connection
pooling, and `LISTEN`/`NOTIFY`, all of which this package needs. **Wrap**: a
`pgx` type appears in exactly one exported signature, `postgres.Raw`.

goose's licence reads `NOASSERTION` on the GitHub API only because the file
carries three copyright lines; it is MIT. It is healthy, and as a *library*
import it does **not** drag in MySQL or ClickHouse drivers — the fear the
previous draft recorded is unfounded. It is rejected anyway; see below.

### The migration ruling: no goose, and no migrations at boot — ever

**The framework never runs a migration from a lifecycle hook, and there is no
option to make it.** Under a rolling deploy of N replicas, boot-time migration
means N replicas race for the lock; N−1 block, push their boot past the
readiness deadline, and get killed; the winner applies DDL the *old* replicas —
still serving — were not written against; and one bad file puts every replica
into CrashLoopBackOff at once, taking down a service that was healthy a minute
earlier. No default fixes that, which is why `RunOnStart()` is deleted rather
than defaulted off.

With boot-time running gone, goose buys nothing: an ordered applier plus a
version table for our own two tables is ~100 lines, and against goal 2 a
contributor debugging "why did my migration not apply" reads those 100 lines
instead of goose's dialect/locker/provider layering. So the package ships
`postgres.Schema` (an `fs.FS` of numbered SQL files, in goose's *file format*
so a team already running goose, atlas or dbmate consumes them with one line)
and `postgres.Migrate`. warren.md §9 loses its goose row, and §1.7's "+
Postgres" row becomes **`warren/persistence/postgres`, `pgx`** — one third-party
direct dependency, not two.

`Outbox()`'s `OnStart` **verifies** its table exists and fails the boot naming
`postgres.Migrate` and the option of applying `postgres.Schema` with whatever
tool the project already runs. Verifying is safe under a rolling deploy;
migrating is not. (A `warren migrate` CLI command is worth adding, but the
diagnostic must not name it before it exists — that is the no-op-fix bug the
`transport/http` field test found.)

## Rulings

**1. The constructor parses; `OnStart` connects.** `pgxpool.ParseConfig` does
no I/O, so a malformed DSN, a bad `sslmode` or an unparseable pool size fails
at *wiring*, before any hook runs. Dialing and `Ping` happen in `OnStart`,
under `ConnectTimeout`, so an unreachable database rolls the boot back instead
of leaving a live pool, a background health goroutine and open sockets behind a
boot that failed later for an unrelated reason. "Constructors wire, `OnStart`
acquires" and "a bad DSN fails loudly" are not in tension: parsing is wiring,
dialing is acquisition.

**2. The transaction reaches a repository through the context, and writes
outside `Do` are refused.** `Do` puts the `pgx.Tx` on the context under this
package's own key and then calls `persistence.Collect`. `postgres.DB(ctx)`
returns that transaction if present and the pool otherwise.

Reads outside `Do` must work: `persistence.RunContract` calls
`repo.FindByID(context.Background(), …)` in four subtests, so a driver that
refuses everything outside a transaction fails the shipped contract suite.

Writes are the opposite, and this is the ruling that matters. Outside `Do`,
`persistence.Track` is a documented no-op — harmless for the in-memory driver,
because the aggregate is still in the caller's hand. Against Postgres the row
autocommits while the events stay pending on an object that goes out of scope
at the end of the function, and they are gone: silent, unrecoverable, no error
anywhere. That is precisely the loss the outbox exists to prevent, reintroduced
through the back door. **`postgres.RequireTx(ctx, op)` is the first line of
every `Save` and `Delete`**, and of `Store.Append`. One map lookup, zero cost
inside a transaction, a copy-pasteable diagnostic outside one.

Sniffing the `Queryer`'s methods instead — refuse `Exec`, allow `QueryRow` —
was considered and rejected: `INSERT … RETURNING id` goes through `QueryRow`,
so the refusal would be both leaky and confusing.

**3. Nested `Do` joins. No savepoints.** `persistence.go` already fixes this
and gives the reasons: savepoints would make the port driver-dependent (Mongo
and Redis have none) and would publish events for state that was rolled back.
A nested `Do` opens nothing, commits nothing, drains nothing, and returns
`fn`'s error — so an inner failure rolls back the outer transaction too. A
caller who "handles" the inner error and returns nil from the outer `fn` gets a
commit of state the inner call may have left inconsistent; that is the
documented cost of join semantics.

An `Option` passed to a nested `Do` returns `persistence.ErrNestedOptions()` —
**exported in this same change**, because a driver in another module that
re-implements the message drifts from the in-memory one. On Postgres the
refusal is not fastidiousness: `SET TRANSACTION ISOLATION LEVEL` must be the
transaction's first statement, so a nested `Isolation(Serializable)` is
unimplementable, and silently ignoring it would mean a handler asking for
Serializable and quietly getting Read Committed.

**4. The commit sequence.**

```
1  persistence.Configure(opts...)      → pgx.TxOptions
2  pool.BeginTx                        isolation is set HERE, first statement
3  ctx = context.WithValue(txKey{}, tx)
4  ctx, drain := persistence.Collect(ctx)
   defer: error or panic → Rollback; re-panic if panicking
5  err := fn(ctx)                      Save → RequireTx → persistence.Track
   err != nil → Rollback, return       nothing drained, nothing appended
6  events := drain()                   PullEvents, enlistment order, once
7  for each sink: sink(txCtx, events)  ← outbox.Sink → Store.Append → INSERT
   sink error → Rollback, errors.Unavailable("unit of work commit", err)
8  tx.Commit                           ONE commit: state and outbox rows
```

There is no step 9. The previous draft's "post-commit: signal the relay" is
wrong — the unit of work signals nothing. The wake-up is `outbox.Waiter`,
implemented here as `LISTEN warren_outbox` / `pg_notify`, and the `pg_notify`
is issued *inside* the transaction at step 7, because Postgres holds
notifications until commit and delivers them atomically with it. Free, correct,
and no post-commit path that could fail after the data is already durable.

Outbox rows are written at step 7 by a sink registered with `OnCommit`, using
`DB(txCtx)` — the same transaction. After `fn`, because only then is every
aggregate enlisted; after `drain`, because `PullEvents` is destructive and
`Collect` guarantees once; inside the transaction, because a rollback must take
the rows with it; before commit, because otherwise it is not atomic.

**5. Retryable errors are mapped in the runtime, not only in generated
repositories.** `40001` serialization_failure and `40P01` deadlock_detected →
`errors.Unavailable`, which is what makes `app.Retrying` re-run the whole `Do`
— without it `Isolation(Serializable)` is unusable. `23505` unique_violation →
`errors.Conflict`. `ErrNoRows` → the repository's own `errors.NotFound`.

**6. Testing.** Unit tests take no Docker, no network and no sleeps, and cover:
option parsing and every diagnostic (golden files), **DSN redaction — the
password must never reach a log line, an error, or a diagnostic**, `DB(ctx)`
resolution across tx/pool/neither, `RequireTx`'s refusal, the nested-option
refusal (it precedes all I/O), the advisory-lock key derivation (a collision is
a silent double-drain), and the `Append` SQL string and argument order.

Everything else is behind `//go:build integration`: the six-step sequence,
rollback, the two atomicity tests, isolation and the 40001 mapping,
`LISTEN`/`NOTIFY`, the advisory lock under contention, `Migrate`'s idempotence,
and **`persistence.RunContract` unmodified** — which is what the exported suite
is for. The suite reads `WARREN_TEST_POSTGRES_DSN`; unset, it skips with the
`docker run` line to set it. Each test creates `warren_test_<rand>` as a
schema, sets `search_path`, migrates, and drops it in `t.Cleanup`, which is
what makes `t.Parallel()` safe against one shared server.

**7. Invariant 3.** No `pgx` type in any exported signature except
`postgres.Raw`. `Queryer`, `Row` and `Rows` are declared here and are
structurally satisfied by `pgx` at zero wrapping cost — `Row` is byte-identical
to `pgx.Row`, `Rows` is the four-method subset `pgx.Rows` already has. Only
`Queryer` needs a wrapper, because `Exec` returns `int64` rather than
`pgconn.CommandTag`, and that wrapper is boxed once per transaction and once
per process, never per call. `DB(ctx)` is a map lookup returning an
already-boxed interface: **no allocation on the request path.**

warren.md §6.1's "provides `*pgxpool.Pool`" and §2.4's example constructor
taking a pool are both amended in this change — a raw handle as the *default*
path is what Wrap mode exists to prevent.

## Public API

```go
// Package postgres implements Warren's persistence, outbox and inbox ports
// against Postgres.
//
// It provides a UnitOfWork whose Do commits aggregate state and the outbox
// rows for the events those aggregates raised in ONE transaction, resolves
// that transaction onto repositories through the context, ships a durable
// outbox store with LISTEN/NOTIFY wake-up, an advisory-lock leader elector, a
// durable inbox dedupe store, and a ping health check.
//
// Repositories are plain SQL over a small driver-free query handle. There is
// no ORM, by design, and no pgx type in any signature here except Raw's.
//
// It never runs a migration. Schema is a deploy step: see Schema and Migrate.
package postgres

const ModuleName = "warren/persistence/postgres"

type Option struct{ /* unexported */ }

func Module(opts ...Option) warren.Module

func DSN(dsn string) Option
func MaxConns(n int32) Option                  // default 10
func MinConns(n int32) Option                  // default 0
func MaxConnLifetime(d time.Duration) Option   // default 1h
func MaxConnIdleTime(d time.Duration) Option   // default 30m
func ConnectTimeout(d time.Duration) Option    // default 5s
func StatementCacheMode(m CacheMode) Option    // pgbouncer transaction pooling
func HealthTimeout(d time.Duration) Option     // default 2s
func Raw(fn func(context.Context, *pgxpool.Pool) error) Option

type CacheMode string
const (
    StatementCachePrepare  CacheMode = "prepare"  // default
    StatementCacheDescribe CacheMode = "describe" // transaction-pooling pgbouncer
)

// --- the query handle ---

// DB resolves the handle a repository uses for one call: the transaction ctx
// carries if UnitOfWork.Do put one there, and the pool otherwise.
type DB func(ctx context.Context) Queryer

type Queryer interface {
    Query(ctx context.Context, sql string, args ...any) (Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) Row
    Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

type Row interface{ Scan(dest ...any) error }

type Rows interface {
    Next() bool
    Scan(dest ...any) error
    Err() error
    Close()
}

var ErrNoRows error

// RequireTx returns a diagnostic unless a transaction is in scope. Every
// repository Save and Delete calls it first.
func RequireTx(ctx context.Context, op string) error

// --- the unit of work ---

type UnitOfWork struct{ /* unexported */ }

var _ persistence.UnitOfWork = (*UnitOfWork)(nil)

func (u *UnitOfWork) Do(ctx context.Context, fn func(context.Context) error, opts ...persistence.Option) error
func (u *UnitOfWork) OnCommit(fn func(context.Context, []domain.Event) error)

// --- outbox, inbox, elector ---

// Options of Module, not sibling modules — see Divergences 1.
func WithOutbox(opts ...OutboxOption) Option
func Encoder(e outbox.Encoder) OutboxOption
func Table(name string) OutboxOption           // default "warren_outbox"
func Retention(d time.Duration) OutboxOption   // default 24h; 0 keeps forever

func WithAdvisoryLock(opts ...LockOption) Option
func LockKey(name string) LockOption            // default "warren/outbox"
func RetryInterval(d time.Duration) LockOption  // default 5s

func WithInbox(opts ...InboxOption) Option
func InboxRetention(d time.Duration) InboxOption // default 24h

// --- schema ---

// Schema is the DDL for the tables this package owns, as numbered SQL files
// in goose's file format. Warren never applies them for you.
var Schema fs.FS

// Migrate applies every unapplied file in fsys, in name order, each in its own
// transaction, under an advisory lock. Called by your deploy job — never
// from a lifecycle hook.
func Migrate(ctx context.Context, dsn string, fsys fs.FS) error
```

## Definition of done

1. Every function above implemented with those signatures and a doc comment
   starting with its identifier.
2. `persistence.ErrNestedOptions` exported in the same change, and the
   in-memory driver switched to it.
3. Unit tests as listed in ruling 6, with golden files for every diagnostic and
   a test proving the DSN password is redacted everywhere.
4. Integration tests behind `//go:build integration`, including
   `persistence.RunContract` unmodified.
5. `go.mod` contains the core module and `jackc/pgx/v5`, and nothing else that
   is not stdlib.
6. `persistence/postgres` added to the Makefile's `MODULES` list.
7. warren.md corrected in the same change: §6.1 loses "provides
   `*pgxpool.Pool`", §2.4's example takes `postgres.DB`, §9 loses the goose row
   and gains the pgx audit, §1.7's "+ Postgres" row becomes two modules.
8. This spec corrected wherever the implementation diverged, and retired once
   the package is implemented and reviewed.

## Deferred out of v0.1

- **CDC / logical replication** — a second, incompatible delivery path whose
  replication slots silently fill the disk when a consumer stops. The
  table-based outbox is correct and shipped, and latency has not been measured
  as a problem.
- **Down migrations** — an undo you write forwards is an undo you can test.
- **`CopyFrom`/`SendBatch` in `Queryer`** — every method added is a method a
  Mongo or MySQL driver must also provide. Reachable through `Raw` today.
- **Read-replica routing for `persistence.ReadOnly()`** — needs a second pool,
  a staleness contract and a read-after-write rule. `ReadOnly()` maps to
  `BEGIN READ ONLY` on the primary, which is honest.
- **`SKIP LOCKED` multi-drainer outbox** — `AdvisoryLock` gives single-drainer
  correctness; parallel draining trades `DrainOnce`'s global ordering for
  throughput, and that renegotiation comes first.
- **Inbox partitioning** — v0.1 sweeps with a periodic `DELETE`.

## Divergences — what the implementation changed, and why

**1. The outbox, inbox and elector are OPTIONS of `Module`, not sibling
modules.** The approved API had `Outbox() warren.Module`. Built that way it
does not boot: `Outbox()` needs the `*pool`, and a sibling module cannot see
another module's providers — which is exactly the encapsulation the framework
exists to enforce. The alternatives were to export `*pool` (leaking the driver
handle the Wrap mode exists to hide) or to make every user hand the outbox
module an import of the postgres module. Both are worse than one option:

```go
warren.New(postgres.Module(
    postgres.DSN(cfg.DatabaseURL),
    postgres.WithOutbox(),
    postgres.WithAdvisoryLock(),
))
```

Found by running the code, not by reading it: the boot diagnostic named the
missing provider, the scope, and both fixes.

**2. `redact` does not use `net/url`, and that is the point.** The first
implementation parsed the DSN and redacted the parsed form. `url.Parse` REFUSES
a malformed DSN — and a malformed DSN is precisely when `errBadDSN` fires, so
the password leaked on the one path guaranteed to print it. The package's own
golden file caught it within ten minutes. Redaction is now string surgery that
cannot fail, the malformed cases are in the table test, and a `password=` in a
query string is redacted too.

**3. `Retention` and `InboxRetention` are enforced by a sweeper.** They were
written as documented options with a `sweep` method nothing called — an option
that promises a retention window and never applies it is worse than no option.
`startSweeper` runs on an interval of retention/12, clamped to [1m, 1h], and
stops on `OnStop`. The linter's "unused method" was the tell.

**4. No diagnostic names `warren migrate`, because that command does not exist
yet.** The messages name `postgres.Migrate(ctx, dsn, postgres.Schema)` and the
option of applying `postgres.Schema` with the tool the project already runs.
Naming a command that does not exist is the no-op-fix bug the `transport/http`
field test found; it is not repeated here. Adding `warren migrate` to the CLI
remains worth doing.

**5. A generated repository's `Delete` must check rows-affected.** The contract
suite deletes twice and requires the second to be `NOT_FOUND`; a bare
`DELETE ... WHERE id = $1` returns nil for a missing row. The reference
repository in the integration suite carries the check, and the CLI's Postgres
template must too.

## Field-test round — 2026-08-02

An engineer who had never seen Warren built an orders service on this package
and tried to break it. Three code bugs, all fixed with regression tests that
run against a real database:

**1. `QueryRow` never classified its errors.** `Query` and `Exec` routed
through `mapError`; `QueryRow` returned the pgx row bare, so `Scan`'s error was
never classified. Two proven consequences: a duplicate key from
`INSERT … RETURNING id` was a **500 instead of a 409**, and a serialization
failure never became `CodeUnavailable` — so `app.Retrying` never re-ran it and
**`Isolation(Serializable)` was unusable by this package's own stated
standard**. `mapError` now also classifies connection failures, so a read
during an outage is a 503 like the write beside it, instead of a 500.

**2. The password redaction leaked, on exactly the path it was written for.**
`redact` cut the URL authority at the first `/` or `?` — and both occur inside
passwords, because `openssl rand -base64` emits `/` routinely. The `@` then
fell outside the slice and nothing was redacted. pgx's own error redacted it
correctly while Warren's line printed it in full: the wrapper was less safe
than the thing it wraps. It now finds the last `@` first, which can
over-redact a path containing one — the right way to be wrong.

**3. Validation `details` carried a fabricated field.** Two missing fields
produced `"customer, cents": "required"` beside the real per-field entries — a
key no client can map to a form control. Caused by `errors.Invalid` recording
its reason under the field name (added the iteration before, to fix a real
gap), meeting `validate`'s comma-joined field list. `Invalid` now skips the
auto-detail when the field names several fields at once.

Also from that round: `WithOutbox()` writes rows nothing publishes unless a
relay is wired, and `Retention` only sweeps PUBLISHED rows — so an undrained
table grows forever under an option promising bounded growth. The store now
warns at boot when rows are already waiting. And warren.md §6.1 documented a
deleted migration API and a repository example missing `RequireTx` and
`persistence.Track` — the two calls without which events are silently lost.
Both corrected, and `GETTING_STARTED.md` gained the Postgres path it never had.

## Open questions

1. **`warren g repository --driver postgres` does not exist**, and warren.md
   advertised it. The three rules a Postgres repository must follow —
   `RequireTx` first, `db(ctx)` for the handle, `persistence.Track` to enlist —
   are enforced by no compiler and taught only by example. That generator is
   the highest-value item left in the CLI.
2. **No `postgres.Connect(ctx, dsn)` for tooling.** A deploy job or a test
   harness that must reach the database without a booted `App` currently
   imports `pgx` directly — the exact driver dependency the architecture keeps
   out of application code.
3. **A `warren/testing` seam for a real database.** The schema-per-test
   harness in this package's integration suite is the right shape and is not
   exported, so every project reimplements it.
4. ~~**A `warren/inbox/inboxtest` contract suite is owed.**~~ **DONE
   (2026-08-03)** — it ships in the port package, and a Postgres store runs it
   in one line. Three of its checks are aimed squarely here: `MarkSeen` must
   be `ON CONFLICT … DO UPDATE` (a `DO NOTHING` keeps the first deadline and a
   `GREATEST(…)` cannot shorten one — both are refresh violations); `Seen`
   must not upsert; and the key column must be `text`, not `VARCHAR(n)`, on a
   case-sensitive collation. Note also that the key can hold no NUL — Postgres
   rejects `0x00` in `text` — which core now guarantees (U+001F separator,
   `RequireMessageID` refuses a NUL id).
5. **pgbouncer beyond `StatementCacheDescribe`.** `LISTEN`/`NOTIFY` and
   session-level advisory locks both need session pooling; that is a
   documentation section, but it is not written yet.
