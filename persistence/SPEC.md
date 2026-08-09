# `github.com/MerseniBilel/warren/persistence` — SPEC

| | |
|---|---|
| **Status** | **Approved and implemented (2026-08-02)** — ports, the `Track`/`Collect` enlistment seam, the in-process driver, and the exported contract suite ship in core. The spec's hard question (the aggregate-save registration mechanism) is answered by `domain.Aggregate` + context-carried enlistment; nested `Do` joins; `Specification.ToSQL` was removed from `domain` (a domain type obliged to emit SQL knows its persistence technology by name). Real transactions wait for `persistence/postgres`. |
| **Source** | [warren.md §3.3](../warren.md) |
| **Module** | core |
| **Mode** | Build (ports only) |
| **Wraps** | — |

## Problem

A DDD framework has exactly one hard persistence problem: **a state change and
the events announcing it must commit together, or the system lies.** Publish
first and a crash before commit announces something that never happened; commit
first and a crash before publish loses the announcement. Every "we'll publish it
after the save" implementation is that bug waiting for a bad afternoon.

`warren/persistence` is the two-interface port that closes it. `UnitOfWork.Do`
opens a transaction, runs the work, drains the events the aggregates raised
(`domain.AggregateRoot.PullEvents`, §3.1), writes them to the outbox **inside
the same transaction**, and commits. §3.3 states it directly: "**`UnitOfWork.Do`
is where DDD becomes real.**" Everything downstream — the outbox relay (§5.5),
the inbox dedupe (§5.6), the "swap Kafka for RabbitMQ is one line" claim
(§1.5) — rests on that one commit being atomic.

The second problem is smaller and older: a use case that knows it is talking to
Postgres cannot be tested without Postgres and cannot be moved. `Repository` is
the port that keeps `pgx` out of `Handle`. §10's use case is the proof —
"No net/http. No pgx. No kgo."

This package is CONTRACTS: "pure interfaces, zero implementations, in the core
module. This is what lets an adapter and a user's domain package depend on the
same type without meeting" (§3 preamble). `warren/persistence/postgres` and the
user's `package domain` both name `persistence.UnitOfWork`; neither imports the
other.

## Goals

1. Define a minimal aggregate-oriented `Repository` port, parameterised over the
   aggregate and its identifier, that a generated Postgres repository and a
   hand-written Mongo one satisfy identically (§6.1, §6.2–6.4).
2. Define `UnitOfWork` as a **single method taking a function**, so the
   transaction boundary is a lexical scope in the use case (§10) and cannot be
   left open.
3. Pin the six-step `Do` sequence (§3.3) as the normative contract every driver
   must implement, including the transaction-on-the-context propagation that
   makes `r.db(ctx)` work (§6.1).
4. Keep every driver type out of the signatures — invariant 3. No
   `*pgxpool.Pool`, no `pgx.Tx`, no `mongo.Session`.
5. Let a user declare a richer repository interface in their own domain package
   (§10 uses `h.users.ExistsByEmail`) without this port growing to accommodate
   it.

## Non-goals

- **An ORM. This is the deliberate omission.** §3.3, in full: "No GORM, no ent,
  no sqlc mandate. Generated Postgres repositories use plain pgx and are yours to
  edit. A team preferring sqlc writes the adapter with sqlc — the domain layer
  cannot tell the difference. Shipping an ORM would be the fastest way to lose
  the 'explicit, no magic' positioning." No identity map, no change tracking, no
  lazy loading, no dirty-checking, no session cache.
- **No query builder and no query language.** `Specification` (§3.1) is the only
  query abstraction warren.md defines, and it lives in `domain`.
- **No migrations.** Migration running belongs to the driver —
  `postgres.Migrations(embeddedFS, postgres.RunOnStart())` (§6.1) — via
  `pressly/goose`, which the core module may not import.
- **No connection pooling, no DSN, no health check.** Adapter concerns (§6.1).
- **No outbox table schema or relay.** The port *requires* an outbox write in
  step 4; the writer and relay are `warren/outbox` (§5.5) and the table is the
  driver's (§6.1 "outbox table + writer").
- **No caching.** Redis "provides cache + distributed lock rather than
  repositories" (§6.2–6.4).
- **Zero implementations** — invariant 5. Not even an in-memory repository; test
  fakes live in `warren/testing` (AGENT.md § Testing).

## Public API

Taken from warren.md §3.3 verbatim; doc comments added. `domain.Root` now
exists: §3.1 defines a `Root[T ID]` interface (`ID() T` + `PullEvents() []Event`)
that `AggregateRoot` satisfies, so the constraint below compiles as written —
see open question 1, resolved.

```go
// Package persistence defines Warren's storage ports: an aggregate-oriented
// repository and a unit of work. It contains no driver, no SQL, and no ORM.
//
// The unit of work is the reason the package exists: it commits aggregate state
// and the outbox rows for the events that state change raised in a single
// transaction.
package persistence

// Repository loads, stores, and removes a single aggregate type by identity.
// It is the minimum every driver implements; a user's own repository interface
// in their domain package may declare more (see warren.md §10's ExistsByEmail).
//
// Implementations pick the ambient transaction up from the context when one is
// present, and the pool otherwise, so a repository call inside UnitOfWork.Do
// joins that transaction without being told.
type Repository[T domain.Root[ID], ID domain.ID] interface {
	// FindByID returns the aggregate with the given identity, or an error
	// carrying errors.CodeNotFound when no such aggregate exists.
	FindByID(context.Context, ID) (T, error)
	// Save persists the aggregate. The events it raised are drained and
	// written to the outbox by the enclosing unit of work, not here.
	Save(context.Context, T) error
	// Delete removes the aggregate, or returns CodeNotFound. It takes the
	// ROOT and not an identity, and enlists it exactly as Save does:
	// removing an aggregate is precisely when OrderCancelled or
	// AccountClosed is raised, and those events live on the caller's
	// instance. Loading the aggregate inside Delete cannot repair an
	// id-shaped signature — that is a different object with zero pending
	// events, so enlisting it publishes nothing.
	Delete(context.Context, T) error
}

// UnitOfWork runs a function inside one transaction, and makes the aggregate
// state written by that function and the domain events it raised commit
// together or not at all.
type UnitOfWork interface {
	// Do begins a transaction, puts it on the context it passes to fn, runs
	// fn, drains PullEvents from the aggregates saved in the scope, writes
	// them to the outbox in the same transaction, commits, and then signals
	// the relay. If fn returns an error, nothing is committed and the error
	// reaches the caller in a form the §2.6 table can still classify; whether
	// it is returned bare or wrapped is Open question 4.
	Do(ctx context.Context, fn func(context.Context) error) error
}
```

Usage, from §10 — the transaction boundary is a lexical scope in the use case,
and the repository call inside it needs no transaction argument:

```go
u := domain.NewUser(email, cmd.Name)

// Aggregate state + outbox row commit in ONE transaction.
if err := h.uow.Do(ctx, func(ctx context.Context) error {
	return h.users.Save(ctx, u)
}); err != nil {
	return UserDTO{}, err
}
```

## Behaviour

### `UnitOfWork.Do` — the six-step sequence

This is the heart of the spec. Reproduced verbatim from §3.3, then made
normative:

> 1. Begin transaction, put it on the context
> 2. Run `fn` — repositories pick the transaction up from context automatically
> 3. Drain `PullEvents()` from every aggregate saved in this scope
> 4. Insert those events into the outbox table **in the same transaction**
> 5. Commit — state and events are atomic
> 6. Post-commit: signal the relay to publish

**Step 1.** The transaction handle is carried on the context and is not part of
any exported signature — invariant 3 forbids `pgx.Tx` appearing in one. The
context passed to `fn` is derived from the caller's context; cancellation of the
caller's context must reach the transaction.

**Step 2.** Repositories resolve the transaction from the context, or fall back
to the pool. §6.1 names this as the single piece of framework magic and bounds
it: "`r.db(ctx)` is the only piece of framework magic and it does one thing:
return the ambient transaction if one exists, else the pool." A repository call
*outside* any `Do` therefore still works, on the pool, in its own implicit
transaction.

**Step 3.** `PullEvents` is destructive (§3.1), so exactly one drain happens per
aggregate per `Do`, and a fact cannot be written to the outbox twice. Scoping —
"every aggregate saved in this scope" — means the aggregates passed to `Save`
during this `Do`, not every aggregate in the process. How the unit of work
learns which those were is unspecified; see open question 2.

**Step 4.** The insert is part of the same transaction. This is the whole
guarantee, and the one line of the sequence a driver may not optimise. A driver
that writes the outbox after commit does not implement this port.

**Step 5.** Commit. On success the state change and its events are durable
together.

**Step 6.** After commit — never before — the relay is signalled. This is a
liveness hint only: §5.5's relay also polls (`outbox.PollInterval(200ms)`), so a
lost signal delays publication, it does not lose it. A relay signal that
happened before commit would let the relay read an uncommitted outbox row.

**Failure.** If `fn` returns an error, the transaction rolls back and nothing is
published; §10's handler simply returns `Do`'s error, so the error must be
returned in a form the §2.6 table can still classify.

### `Repository`

- **`FindByID` misses are `errors.NotFound`.** §6.1's generated code is
  explicit: `if errors.Is(err, pgx.ErrNoRows) { return nil, errors.NotFound("user", id) }`.
  The driver translates its own sentinel; the use case sees the semantic code
  and, through §2.6, a 404 / `NotFound` / ack-and-log.
- **`Save` is upsert-or-insert as the driver defines it.** warren.md's generated
  example shows only `FindByID`; the write side is unspecified beyond the
  signature. See open question 4.
- **The port is a floor, not a ceiling.** §10's handler calls
  `h.users.ExistsByEmail(ctx, email)`, and §2.1 exports
  `domain.UserRepository` — the user's own interface, in their domain package,
  which the driver's concrete type also satisfies. `persistence.Repository` is
  what generators target and what the contract suite tests; a project's real
  repository interface is usually wider and lives in `domain`.
- **Drivers are interchangeable by construction.** §6.2–6.4: "Same `Repository`
  and `UnitOfWork` ports. Mongo's UoW uses sessions; Redis provides cache +
  distributed lock rather than repositories."

### Ring position

`persistence` is CONTRACTS, in the core module. It imports `domain` (its type
parameters name `domain.Root` and `domain.ID`) and may import KERNEL packages.
It imports no adapter, no driver, and nothing from `transport` or `broker`.
Note that step 4's outbox is written by `warren/outbox` (§5.5), which raises a
placement question — open question 3.

## Errors

Every error crossing this port is a `warren/errors` semantic error (§2.6);
translation of driver sentinels is the driver's job, and it is the reason
`errors.Is(err, pgx.ErrNoRows)` appears in §6.1 and nowhere near a use case.

Fixed by warren.md:

| Situation | Code | Source |
|---|---|---|
| `FindByID` finds nothing | `CodeNotFound` (`errors.NotFound("user", id)`) | §6.1 |

Everything else is unstated and is open question 4 — in particular: the code for
a unique-constraint violation on `Save` (`CodeConflict` is the obvious reading of
§2.6's "409 / `AlreadyExists` / ack (idempotent replay)", but warren.md does not
say it), for a lost connection (`CodeUnavailable` would make it retryable by
`app.Retrying`), for a commit failure, and for `Delete` on a missing row.

These are not cosmetic choices: each one picks a column in the §2.6 table and
therefore decides an HTTP status, a gRPC code, and whether a message is acked or
dead-lettered. AGENT.md requires a golden-file test for every error message in a
spec, so they must be decided before implementation.

## Testing

**Contract suite — the centre of this spec.** AGENT.md: "Every port change
updates the contract suite first, then the drivers." Every driver claiming to
implement `UnitOfWork` or `Repository` — `postgres`, `mongo`, `mysql`, and the
fakes in `warren/testing` — runs the same exported suite. It must cover:

*Unit of work — the six steps, one assertion each:*

- Committing `Do` makes the aggregate state readable afterwards.
- `fn` returning an error rolls back: no state change, **and no outbox row**.
- A repository call inside `fn` joins the ambient transaction — asserted by
  reading through a second, independent handle and seeing nothing until commit.
- A repository call outside any `Do` still works, on the pool.
- Events raised by aggregates saved inside `Do` land in the outbox, in the same
  transaction: for every committed state change there is an outbox row, and for
  every rolled-back one there is not. This is the atomicity property and it is
  the single most important test in the framework.
- `PullEvents` is drained exactly once — a second `Do` over the same aggregate
  writes no duplicate outbox rows.
- The relay is signalled **after** commit, never before, and never on rollback.
- Context cancellation mid-`fn` aborts and rolls back.
- Panic inside `fn`: the transaction must not be left open. (Behaviour on panic
  is unstated in warren.md — open question 6 — but the suite pins whatever is
  decided.)

*Repository:*

- `FindByID` on a missing identity returns an error satisfying
  `errors.Is(err, errors.CodeNotFound)` — for every driver.
- Save then `FindByID` round-trips the aggregate, identity included.
- `Delete` then `FindByID` yields `CodeNotFound`.
- No driver type appears in any exported signature — a compile-time assertion
  plus an `go list`-based check, since invariant 3 is the swappability claim.

**Constraints.** The suite itself is driver-neutral and must run in unit mode
against an in-memory fake from `warren/testing`: no Docker, no network, no
sleeps (AGENT.md § Testing). The Postgres/Mongo runs of the same suite sit
behind `//go:build integration` with testcontainers (§7.5).

**Benchmarks.** `UnitOfWork.Do` is on the request path — §10's `POST /users`
goes through it — so it gets an allocation benchmark against the in-memory fake,
per AGENT.md and invariant 7. The measured property is that the per-request cost
of the transaction-on-context mechanism is a context value write and a lookup,
with no reflection.

## Definition of done

- [ ] `Repository` and `UnitOfWork` compile as written — open question 1 is
      resolved: §3.1's `Root[T ID]` interface makes the constraint compile.
- [ ] The contract suite exists, passes against the `warren/testing` fake, and
      is exported before any driver is written.
- [ ] Every error code the port can produce is decided, documented in the table
      above, and has a golden-file test.
- [ ] `go list -deps` shows this package importing `domain` and stdlib only.
- [ ] Allocation benchmark for `Do` committed with its number.
- [ ] §10's handler compiles verbatim against this port as a test.
- [ ] Open questions answered and this spec corrected in the same change.

## Open questions

1. **RESOLVED (2026-08-01):** warren.md §3.1 now defines a `Root[T ID]`
   interface (`ID() T` + `PullEvents() []Event`) that `AggregateRoot` satisfies —
   the interface option below is the one that was chosen, and §3.1 and §3.3 were
   amended together, so `Repository[T domain.Root[ID], ID domain.ID]` compiles
   as written. **`domain.Root` does not exist — §3.1 defines `AggregateRoot`.** §3.3 writes
   `Repository[T domain.Root[ID], ID domain.ID]` while §3.1 defines
   `AggregateRoot[T ID]` and no `Root` of any kind. This is a straight
   contradiction inside warren.md and it is **not** fixed here, because fixing it
   requires a decision, not a rename:

   - `AggregateRoot[T]` is a **struct**. A struct type used as a type-parameter
     constraint admits exactly that one type, so `Repository[*User, UserID]`
     would not compile — `*User` is not `AggregateRoot[UserID]`, it *embeds*
     it. The constraint as written cannot express "any aggregate root".
   - So the real question is what the constraint should be: an interface
     (something like a `Root[ID]` interface exposing `ID() ID` and
     `PullEvents() []Event`, which `AggregateRoot[ID]` satisfies), or a looser
     `T any`, or the constraint dropped entirely.
   - Whichever is chosen, `warren.md` §3.1 and §3.3 must both be amended in the
     same change (AGENT.md: "a spec that contradicts it needs `warren.md`
     amended in the same change").

2. **How does the unit of work know which aggregates were saved?** Step 3 says
   "drain `PullEvents()` from every aggregate saved in this scope". The
   repository holds the aggregate; the unit of work holds the transaction; §6.1's
   generated repository has no reference to a unit of work and does not register
   anything. Options warren.md leaves open: the repository registers saved
   aggregates on the context; `Save` writes outbox rows itself; the unit of work
   keeps a registry keyed by transaction. Each has visible consequences for what
   a hand-written repository must do, so this is contract, not detail — and it is
   the single largest gap in this spec.

3. **Step 4 writes to an outbox that `persistence` cannot import.** §5.5 puts
   the writer in `warren/outbox` ("**Writer** — invoked by `UnitOfWork`"), and
   §6.1 puts the outbox table in the Postgres adapter. `warren/outbox` is not in
   the §1.6 repository layout at all, so its module is unstated. If it is a
   separate module, core-module `persistence` cannot name its types; if the
   writer is a port, that port needs defining and warren.md does not define it.

4. **Error codes for the write path.** Unique-violation on `Save`, connection
   loss, commit failure, `Delete` of a missing row — none are specified. Each
   picks a column of §2.6 and therefore an HTTP status, a gRPC code, and an
   ack/nack/DLQ decision. See Errors above.

5. **Nested `Do`.** §10's handler calls `uow.Do` itself, while §3.2 offers
   `app.Transactional(uow)` as middleware around that same handler. When both
   are present, does the inner `Do` join the ambient transaction, open a
   savepoint, or fail? warren.md shows both patterns and reconciles neither.
   Recorded in `app/SPEC.md` too; it needs one answer.

6. **Panic inside `fn`.** AGENT.md forbids `panic` in library code but user code
   panics. Does `Do` recover and roll back, or let it propagate with the
   transaction abandoned to the driver's connection cleanup? Unstated.

7. **Read-only and isolation-level control.** Nothing in `Do`'s signature
   admits an isolation level, a read-only hint, or a timeout, and no options
   parameter exists. Deliberate minimalism, or a gap? Worth settling before
   drivers ship, because adding a parameter later is a breaking change to the
   port.

8. **`mysql` is specified but not in the module list.** §6.2–6.4 is titled
   "`mysql` / `mongo` / `redis`" and says all three implement these ports, but
   §1.6's repository layout lists only `persistence/postgres`,
   `persistence/mongo`, and `persistence/redis`. Peripheral to this port's
   shape, but the contract suite's driver list depends on it.

Carried forward from the retired domain spec (2026-08-01):

7. **`Specification.ToSQL()` puts a persistence concern in the domain
   contract.** §3.1 defines it on a type in `warren/domain`, while an ORM is
   the deliberate omission and Mongo/Redis have no SQL. Options left open:
   keep it and accept SQL as a domain-visible detail; move translation to a
   persistence-side `SpecificationTranslator`; or make `Specification` a
   marker with translation entirely in the adapter. Port-shape decision,
   settle before this port is implemented. *(was domain's open question 1)*
8. **The generated repository must reconstitute, not scan into fields.**
   `Entity[T].id` is unexported and set only through `NewAggregateRoot`, so
   §6.1's `row.Scan(&u.ID, ...)` template cannot compile; the template needs
   redesigning around the constructor path, and warren.md §6.1 needs amending
   to match. *(was domain's open question 3)*
