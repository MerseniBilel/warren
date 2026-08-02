# `warren/outbox` — SPEC

| | |
|---|---|
| **Status** | **Approved and implemented (2026-08-02)** — one `Store` port whose `Append` is the writer, the `Waiter` low-latency seam, `JSONEncoder`, `Elector`/`Standalone`, `Relay.DrainOnce`, and the in-process store ship in core. The relay module wrapper, `postgres.AdvisoryLock`, the SQL store and its migration, and CDC wait for `persistence/postgres`. |
| **Source** | [warren.md §5.5](../warren.md) |
| **Module** | **undecided** — §1.6's module list omits this package. See Open questions. |
| **Mode** | Build |
| **Wraps** | — |

## Problem

A service that saves an aggregate and then publishes its events has two ways to
be wrong and no way to notice: the commit succeeds and the publish fails
(silently lost event), or the publish succeeds and the commit rolls back
(phantom event). §3.3 makes the fix structural — `UnitOfWork.Do` drains
`PullEvents()` from every aggregate saved in the scope and inserts those events
into the outbox table **in the same transaction**, so that "state and events are
atomic". §1.5 states the other half: *"a separate **relay** — its own lifecycle
participant, leader-elected — drains the outbox to the broker."*

`warren/outbox` owns both halves. It is the reason the handler in §10 contains
no publish call at all:

```go
if err := h.uow.Do(ctx, func(ctx context.Context) error {
    return h.users.Save(ctx, u)
}); err != nil { ... }
// No net/http. No pgx. No kgo. That is the entire point.
```

## Goals

§5.5 names **two components with different lifecycles**, and keeping them apart
is the design:

- **Writer** — invoked by `UnitOfWork`, inserts events into `outbox` inside the
  business transaction (§5.5). It has no lifecycle of its own; it runs inside
  whatever transaction `UnitOfWork.Do` opened (§3.3 steps 3–5).
- **Relay** — a separate lifecycle component, **leader-elected**, that drains
  outbox → broker (§5.5, §1.5). It is a lifecycle participant registered in
  `main.go` alongside the drivers (§10), not a goroutine started by the writer.

Also:

- Two **modes**: polling (portable) and CDC (Postgres logical replication, lower
  latency) (§5.5).
- Publish through the `broker.Publisher` port (§3.4), never through a driver —
  which is what lets the same relay serve Kafka, RabbitMQ, NATS, and memory.
- Flush at exactly §2.3 step 4, after consumers have stopped and before broker
  connections close.
- **Mode Build** (§5.5): Warren owns it outright (AGENT.md § Modes).

## Non-goals

- **Owning the transaction.** `UnitOfWork.Do` begins it, puts it on the context,
  runs `fn`, drains events, calls the writer, and commits (§3.3). The writer
  participates; it does not manage.
- **Owning the outbox table's schema or its migration.** §6.1 says the Postgres
  adapter "Provides: ... outbox table + writer" — which is a direct conflict
  with §5.5 and is recorded in Open questions rather than resolved here.
- **Knowing which broker is in use.** The relay publishes through
  `broker.Publisher` (§3.4).
- **Importing a persistence adapter.** AGENT.md invariant 4: adapters never
  import each other, and nothing in the core ring may import an adapter at all.
  This is in direct tension with `outbox.PostgresAdvisoryLock` — see Open
  questions.
- Any third-party dependency. Mode Build.

## Dependency audit

**Not applicable — Mode Build, no dependency.** §5.5 names no library and §9's
ledger has no row for outbox. Nothing to audit.

The audit that *is* owed is architectural rather than third-party: §5.5's
`outbox.LeaderElection(outbox.PostgresAdvisoryLock)` and its CDC mode (Postgres
logical replication) name Postgres mechanisms from a Build package, and if this
package lives in the core module then core cannot import `persistence/postgres`
(invariant 1: stdlib + dig only; invariant 4: adapters are mutually invisible;
§1.1: "Kernel has no knowledge that HTTP, SQL, or Kafka exist"). See Open
questions — this is the single biggest unresolved design question in the
package.

## Public API

Given by §5.5 for the Relay. Doc comments added; nothing else added.

```go
// Package outbox implements the transactional outbox: a Writer that records
// events inside the business transaction, and a Relay that drains them to the
// broker.
package outbox

// Relay returns a warren.Module running the outbox relay: a leader-elected
// lifecycle component that drains the outbox table to the broker.
func Relay(opts ...Option) warren.Module

// PollInterval sets how often the relay polls the outbox table for
// undispatched events.
func PollInterval(d time.Duration) Option

// BatchSize sets the maximum number of events the relay drains per poll.
func BatchSize(n int) Option

// LeaderElection sets the mechanism used to ensure exactly one relay instance
// drains the outbox at a time.
func LeaderElection(m LeaderElector) Option

// PostgresAdvisoryLock elects a leader using a Postgres advisory lock. §5.5
// shows the value passed as an argument and never its declaration form; a
// package-level var would conflict with AGENT.md's no-package-level-mutable-
// state rule, so the shape is undecided — see Open question 1.
PostgresAdvisoryLock // declaration form not fixed by warren.md
```

Usage, verbatim from §5.5:

```go
outbox.Relay(
    outbox.PollInterval(200*time.Millisecond),
    outbox.BatchSize(100),
    outbox.LeaderElection(outbox.PostgresAdvisoryLock),
)
```

And from §10's bootstrap, with no options at all — so every option has a
default:

```go
warren.New(
    ...
    postgres.Module(postgres.DSN(cfg.Postgres.DSN)),
    kafka.Broker(kafka.Brokers(cfg.Kafka.Brokers...)),
    outbox.Relay(),
    ...
).Run()
```

**The Writer has no surface in warren.md.** §5.5 describes it only as "invoked by
`UnitOfWork`, inserts events into `outbox` inside the business transaction", and
§3.3's `UnitOfWork` interface is `Do(ctx, fn) error` with no writer visible. How
`UnitOfWork` reaches the writer — a port the persistence adapter implements, a
constructor injected into the adapter, or something else — is undecided and is
recorded in Open questions. It is not invented here.

> `Option`, `LeaderElector`, and the defaults for `PollInterval`, `BatchSize`,
> and `LeaderElection` are not given by warren.md. `LeaderElector` above is a
> placeholder name for whatever type `outbox.PostgresAdvisoryLock` inhabits;
> §5.5 shows the value, never the type.

## Behaviour

### Writer

Runs inside `UnitOfWork.Do` (§3.3), between draining events and committing:

```
1. Begin transaction, put it on the context
2. Run fn — repositories pick the transaction up from context automatically
3. Drain PullEvents() from every aggregate saved in this scope
4. Insert those events into the outbox table IN THE SAME TRANSACTION   ← Writer
5. Commit — state and events are atomic
6. Post-commit: signal the relay to publish
```

If the transaction rolls back, the outbox rows roll back with it. That is the
whole guarantee, and it is why the writer must never open a connection of its
own.

Step 6 — "post-commit: signal the relay to publish" — is stated in §3.3 but has
no counterpart in §5.5's Relay surface, which is poll-driven. See Open
questions.

### Relay

- **Its own lifecycle participant** (§1.5), registered as a module in `main.go`
  (§10) — not a goroutine spawned by the writer, and not tied to a request.
- **Leader-elected** (§5.5, §1.5): exactly one instance drains at a time, so a
  three-replica deployment does not publish every event three times.
- **Polling mode (portable)**: every `PollInterval`, read up to `BatchSize`
  undispatched rows and publish them through `broker.Publisher` (§3.4). The
  claim/marking mechanism that stops a row being published twice is not stated
  by §5.5. §10:
  "outbox relay polls → publishes to Kafka via franz-go → billing module's
  consumer receives it".
- **CDC mode**: Postgres logical replication, lower latency (§5.5). How the mode
  is selected is not given by warren.md — Open questions.
- Publishing is through the port, so the relay is driver-agnostic — the same
  property §1.5 claims for the consuming side.

**Boot.** `outbox.Relay(...)` returns an inert `warren.Module`; nothing polls,
connects, or contends for the leader lock on construction (§1.3 steps 1–3,
AGENT.md § Boot). The relay starts at step 6 in dependency order
`pool → repos → consumers → servers` — it needs both the database and the
broker, so it starts after both.

**Shutdown — normative position in §2.3.** The relay owns step 4 exclusively:

```
SIGTERM
  1. readiness probe → 503
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack
  4. outbox relay flushes                                ← this package
  5. DB pools, broker connections close
  6. force-exit deadline (default 30s)
```

The position is the whole reason the ordering is written down. The relay flushes
**after** consumers have stopped — so the flush is not racing new inbound work —
and **before** DB pools and broker connections close — so it still has both a
database to read from and a broker to publish to. A relay that flushed at step 5
would find its pool closing underneath it; one that flushed at step 2 would keep
picking up rows written by requests still draining. The relay must also release
its leadership claim as part of stopping, or the next deployment's replica waits
out a lock nobody holds.

## Error mapping

**This package has no consumer side.** §2.6's Consumer column describes how a
message *consumer* turns a handler error into ack, nack, or DLQ; the outbox
relay is a publisher. The column governs the far end of the flow §10 draws —
"outbox relay polls → publishes to Kafka → billing module's consumer receives
it → dedupe (inbox) → retry policy → its own handler" — where the receiving
driver applies it:

| Code | Consumer action |
|---|---|
| `INVALID` | → DLQ (never retry) |
| `NOT_FOUND` | ack + log |
| `CONFLICT` | ack (idempotent replay) |
| `UNAUTHENTICATED` | → DLQ (never retry) |
| `PERMISSION_DENIED` | → DLQ (never retry) |
| `UNAVAILABLE` | nack + backoff retry |
| `INTERNAL` | nack + retry, then DLQ |

`CONFLICT → ack (idempotent replay)` is the row that makes the outbox pattern
workable: the relay guarantees at-least-once publication, duplicates are
therefore certain (§5.6), and a redelivered event that has already had its
effect is acked rather than retried.

**What the relay does when its own publish fails is unspecified in warren.md** —
whether it retries in place, leaves the row undispatched for the next poll,
marks it failed after N attempts, or dead-letters it. `CodeUnavailable` is
annotated "retryable" in §2.6, which suggests the shape of an answer but does
not give one. Recorded in Open questions rather than guessed.

## Escape hatch

**None, and none applies.** Mode Build means there is no wrapped library and no
raw handle to reach (AGENT.md invariant 3 concerns Wrap packages). warren.md
names none.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing). A
  poll-interval-driven component invites `time.Sleep`; the clock must be
  injectable so it does not.
- **The writer's atomicity test** is the one that matters: events written by
  `PullEvents()` are visible only if the business transaction committed. Against
  a fake transaction for the unit test; against real Postgres behind
  `//go:build integration`.
- **Real Postgres and a real broker behind `//go:build integration`** for the
  end-to-end path in §10: commit → relay picks up → published → consumed.
- **Leader election tests**: two relays, one drains. Behind the integration tag,
  since the only named mechanism needs a real database.
- **Shutdown ordering test**: the relay flushes after consumers stop and before
  the pool and broker close — §2.3 steps 3, 4, 5 in that order, asserted.
- **Golden-file tests for every error message** this package emits (AGENT.md
  § Testing — untested error text rots).
- **Driver-agnosticism test**: the relay drains to the memory broker (§5.4) with
  no source change from the Kafka configuration.
- **The `warren/broker` contract suite is not this package's**, but the relay
  must publish only through `broker.Publisher` — assert no driver import.

## Definition of done

1. Writer records drained events inside the business transaction; rollback
   discards them. Proven by test.
2. Relay drains outbox → broker through `broker.Publisher`, in polling mode,
   with `PollInterval` and `BatchSize` honoured and defaults defined (§10 calls
   `outbox.Relay()` with none).
3. Leader election implemented for at least the one named mechanism, with the
   layering question in Open questions resolved first.
4. CDC mode either implemented or explicitly deferred in warren.md — §5.5 lists
   it as a mode, so silence is drift.
5. Relay is a lifecycle component whose stop hook lands at §2.3 step 4, proven
   by an ordering test. (Whether it releases leadership on stop, and the lease
   semantics generally, depend on the mechanism chosen in Open question 1 —
   warren.md states none of it.)
6. `outbox.Relay(...)` returns an inert module; nothing polls before boot
   step 6.
7. No import of any adapter module (invariant 4); no third-party dependency
   (Mode Build; invariant 1 if this lands in core).
8. Module placement decided and recorded in warren.md §1.6 (Open questions).
9. The §5.5 / §6.1 conflict over who owns the writer resolved in warren.md.
10. Doc comments on every exported identifier, starting with the identifier's
    name.

## Open questions

1. **How does a Build package in the core ring name a Postgres mechanism?**
   `outbox.LeaderElection(outbox.PostgresAdvisoryLock)` (§5.5) puts the word
   Postgres in the outbox package's exported surface, and the CDC mode is
   Postgres logical replication. But §1.1 says the kernel "has no knowledge that
   HTTP, SQL, or Kafka exist", invariant 1 caps core at stdlib + dig, and
   invariant 4 makes adapters mutually invisible. Three readings, and the human
   picks:
   - **Port + adapter:** `outbox` defines a `LeaderElector` port; the advisory-
     lock implementation lives in `persistence/postgres` and is registered as
     `postgres.AdvisoryLock`. This is AGENT.md's stated move — "define the port
     in core, implement it in a submodule" — but it **contradicts §5.5's spelling
     `outbox.PostgresAdvisoryLock`**, so warren.md needs amending.
   - **`outbox` is its own adapter-ring module** that may depend on Postgres.
     Then it is not core, and §1.6 must list it.
   - **`outbox.PostgresAdvisoryLock` is an inert descriptor** — a value naming a
     mechanism that a persistence adapter resolves — so core names Postgres
     without importing it. Cheapest reconciliation with §5.5's text, and the
     ugliest layering.
   The same question governs CDC mode, which cannot be implemented without
   Postgres logical replication code somewhere.
2. **Which module does this live in?** §1.6's repository layout **omits
   `warren/outbox` entirely** — it is in neither the core module's listed
   contents nor the adapter module list, despite §10 registering it in
   `main.go` like any other module. Blocked on question 1.
3. **Who owns the writer — `warren/outbox` or `persistence/postgres`?** §5.5
   makes Writer one of this package's two components; §6.1 says the Postgres
   adapter "Provides: `*pgxpool.Pool`, a `UnitOfWork` implementation,
   transaction-context propagation, **outbox table + writer**, a health check".
   Both cannot be true as stated. The reconciliation is probably "outbox defines
   the writer port and the table contract; postgres implements them" — but that
   is a decision, not a reading.
4. **What is the Writer's public API?** None is given. How `UnitOfWork.Do`
   (§3.3) reaches it is unstated.
5. **What is the outbox table's schema, and who migrates it?** §6.1 mentions
   migrations at boot; §5.5 names the table `outbox` and nothing else. The
   `domain.Event` shape (§3.1: `EventName()`, `OccurredAt()`, `AggregateID()`)
   and `broker.Message` (§3.4: `ID`, `Type`, `Key`, `Payload`, `Headers`,
   `OccurredAt`) are close but not identical, and how an event becomes a message
   — including who assigns `Message.ID`, the idempotency key the inbox dedupes
   on (§5.6) — is unstated.
6. **How is the mode selected?** §5.5 names polling and CDC; the surface has no
   mode option.
7. **Poll or signal?** §3.3 step 6 says "Post-commit: signal the relay to
   publish"; §5.5's surface is `PollInterval` and §10 says "outbox relay polls".
   Whether the signal is an optimisation on top of polling, a separate mode, or
   a leftover is unstated.
8. **How does the relay use Kafka transactions?** §5.1 justifies franz-go partly
   because "the outbox relay needs [transactions] for exactly-once publishing",
   but the relay publishes through `broker.Publisher` (§3.4), which has no
   transactional concept. Either the port grows one, or the driver applies
   transactions internally under `kafka.Transactional(true)`, or the relay
   special-cases Kafka — which invariant 4 forbids. Unresolved, and it is a
   headline claim.
9. **What does the relay do when a publish fails?** See Error mapping.
10. **What are the defaults?** §10's `outbox.Relay()` takes no options, so
    `PollInterval`, `BatchSize`, and `LeaderElection` all need defaults — and a
    default leader-election mechanism implies a default database, which
    question 1 says core cannot know about.
11. **Two leader-election mechanisms exist with no shared abstraction:**
    `outbox.LeaderElection(...)` here and `jobs.LeaderOnly()` in §7.4. Whether
    they share a port is unstated.
12. **Is the relay's flush bounded?** §2.3 step 6 sets a 30s force-exit
    deadline; whether a flush with a large backlog is capped by a per-hook
    timeout (`lifecycle.Hook.Timeout`, §2.3) or by that deadline is unstated.

Carried forward from the retired domain spec (2026-08-01):

13. **Does an `Event` carry a payload, and how does it become
    `broker.Message.Payload []byte`?** `domain.Event` exposes only name,
    time, and aggregate ID; §3.3 step 4 inserts events into the outbox and
    §5.5's relay drains them to the broker. The serialisation boundary —
    a marshalling method on `Event`, JSON-encoding of the concrete type by
    the outbox writer, or a codec registered at boot — is unspecified and is
    the seam between §3.1 and §3.4. *(was domain's open question 4)*
