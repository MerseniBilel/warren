# `warren/broker/memory` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §5.4](../../warren.md) |
| **Module** | **undecided** — §1.6's module list omits this package. See Open questions. |
| **Mode** | Build |
| **Wraps** | — |

## Problem

Two things in warren.md depend on an in-process broker existing, and both are
load-bearing:

1. **`warren extract module billing --into ../billing-service`** (§8). §5.4
   states the mechanism plainly: *"This is what makes `warren extract module`
   viable: modules communicate through the broker port from day one, so
   extraction swaps the driver rather than rewriting call sites."* A modular
   monolith whose modules call each other's services directly cannot be split
   without rewriting every call site. A modular monolith whose modules talk over
   `broker.Publisher`/`broker.Subscriber` (§3.4) can be split by changing which
   driver is registered in `main.go`. That difference is the entire migration
   story, and it only exists if the monolith has a broker to talk over on day
   one — without running Kafka.
2. **Tests.** §5.4 makes memory the "default in tests"; §7.5 shows
   `warrentest.WithMemoryBroker()` in the module-test helper. AGENT.md § Testing
   forbids Docker, network, and sleeps in unit tests, so a test that asserts on
   published events (`warrentest.AssertPublished[domain.UserRegistered]`, §7.5)
   needs an in-process broker or it cannot be a unit test at all.

## Goals

- In-process pub/sub **with the same interface** (§5.4) — the `warren/broker`
  port (§3.4), unmodified: `Publisher`, `Subscriber`, `MessageHandler`,
  `Message`.
- Be the **default in tests and in modular monoliths** (§5.4).
- Keep `warren extract module` a driver swap rather than a rewrite (§5.4, §8).
- **Mode Build** (§5.4): Warren owns it outright, no third-party equivalent is
  acceptable (AGENT.md § Modes). There is nothing to wrap — in-process pub/sub
  over the standard library is the whole implementation.
- Behave as a lifecycle participant like every other driver, at the §2.3
  positions, so that a service developed on memory and deployed on Kafka does
  not discover a different shutdown story on the way.

## Non-goals

- **Defining the port** — `warren/broker` owns `Message`, `Publisher`,
  `Subscriber`, `MessageHandler`, and the per-subscription options
  `broker.WithRetry`, `broker.WithDeadLetter`, `broker.WithConcurrency`
  (§3.4, §5.1).
- **Owning the middleware chain** — `Recover` → `TraceExtract` →
  `Deduplicate(inbox)` → `Retry(backoff)` → `DeadLetter` → `ConcurrencyLimit` →
  `Drain` is driver-agnostic and applies "to Kafka, Rabbit, NATS, and memory
  identically" (§3.4). This driver supplies transport only.
- **Durability.** warren.md claims none. Nothing in §5.4 says messages survive a
  restart, and an in-process broker cannot make the at-least-once promise §5.6
  assumes.
- **Being a production driver for a distributed deployment.** §5.4 scopes it to
  tests and modular monoliths.
- Any third-party dependency. Mode Build.

## Dependency audit

**Not applicable — Mode Build, no dependency.** §5.4 records no library, and §9's
ledger has no row for it. Nothing to audit, nothing to add to a `go.mod`.

This is also the fact that makes the module-placement question below decidable:
an implementation with no third-party imports satisfies the core module's
"stdlib and dig, nothing else" constraint (AGENT.md invariant 1) on the
dependency axis. Whether it satisfies it on the *layering* axis is the open
question.

## Public API

**warren.md gives no surface for this package.** §5.4 is three sentences and
names no constructor, no options, and no type. The one call site anywhere in
warren.md is `warrentest.WithMemoryBroker()` (§7.5) — and that is a
`warren/testing` helper, not this package's API.

By analogy the other drivers offer `kafka.Broker(opts...) warren.Module` (§5.1)
and `rabbitmq.Broker(...)` (§5.2), so a `memory.Broker(...)` returning a
`warren.Module` is the obvious shape — but "obvious" is not "agreed", and
AGENT.md is explicit that public API is the human's call. The surface is
recorded as an open question below rather than invented here.

What this spec does commit to: this package provides implementations of
`broker.Publisher` and `broker.Subscriber` (§3.4) into the DI graph, such that
consumer code written against the port — `r.Events().On(topic, handler,
broker.WithRetry(...), broker.WithDeadLetter(...), broker.WithConcurrency(...))`
(§5.1) — runs unchanged against it. That is what "same interface" (§5.4) means
and it is the only claim §5.4 actually makes.

## Behaviour

**Same interface, in process.** `Publish` delivers to subscribers registered in
the same process. No network, no broker to run, no container.

**The chain is unchanged.** A message goes through the port's middleware exactly
as it would on Kafka (§1.5, §3.4): `recover` → `trace-extract` → `inbox dedupe`
→ (already seen: **ack**; new: **Handler**) → (ok: **ack**; error: **retry w/
backoff**; exhausted: **DLQ**). This is what makes a memory-broker test a
meaningful test of the retry and dedupe behaviour, and it is why §3.4 lists
memory alongside the network drivers.

**Consumers are lifecycle components** (§1.5), not goroutines. Subscriptions
register at boot step 5 and start at step 6 in the order
`pool → repos → consumers → servers` (§1.3).

**Shutdown — normative positions in §2.3:**

```
SIGTERM
  1. readiness probe → 503
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack     ← this driver
  4. outbox relay flushes                                ← must still be able to publish
  5. DB pools, broker connections close                  ← this driver
  6. force-exit deadline (default 30s)
```

The ordering matters here even without a network connection: at step 3 delivery
to consumers stops and in-flight messages ack, but the publish path must remain
open through step 4 so the outbox relay (§5.5) can flush. Only at step 5 does
the broker shut down. A memory driver that tore everything down at step 3 would
make the monolith's shutdown behave differently from the extracted service's —
which would defeat §5.4's purpose.

**The extraction property.** Because modules publish and subscribe through the
port from day one, `warren extract module billing --into ../billing-service`
(§8) replaces `memory.Broker(...)` with `kafka.Broker(...)` in `main.go` and
leaves handlers, consumers, retry policies, and DLQ declarations untouched —
the same "one line in `main.go`" property §1.5 claims for swapping Kafka for
RabbitMQ. This is the package's reason to exist and it is the property the
contract suite has to protect.

## Error mapping

The Consumer column of §2.6 binds every driver behind the port, so it binds this
one — and it must, or a test against the memory broker would prove nothing about
production behaviour:

| Code | Consumer action |
|---|---|
| `INVALID` | → DLQ (never retry) |
| `NOT_FOUND` | ack + log |
| `CONFLICT` | ack (idempotent replay) |
| `UNAUTHENTICATED` | → DLQ (never retry) |
| `PERMISSION_DENIED` | → DLQ (never retry) |
| `UNAVAILABLE` | nack + backoff retry |
| `INTERNAL` | nack + retry, then DLQ |

"ack" and "nack" are in-process bookkeeping here rather than a protocol frame,
but the observable outcomes — redelivered or not, dead-lettered or not — must
match what a network driver does, because that equivalence is what the contract
suite tests.

**What a DLQ means in process is unstated** in warren.md. See Open questions.

Note: §1.4 previously said `Conflict → DLQ`; it has been amended to
`Conflict → ack` and now agrees with §2.6. See Open question 8, resolved.

## Escape hatch

**None, and none is needed.** Escape hatches exist so a user can reach the
wrapped library's raw handle (AGENT.md invariant 3). Mode Build means there is
no wrapped library and no raw handle to reach. warren.md names none.

## Testing

- **Unit tests only — no Docker, no network, no sleeps** (AGENT.md § Testing).
  This driver is the one broker implementation that can be fully tested that
  way, and it is also what lets *other* packages' tests obey the rule.
- **No sleeps in its own tests either.** An in-process broker invites
  `time.Sleep` to wait for delivery; that is forbidden, so delivery completion
  must be observable deterministically. How, is part of the undecided surface.
- **The `warren/broker` contract suite must pass unmodified** — the same suite
  the Kafka, RabbitMQ, and NATS drivers pass. If the memory driver needs a
  relaxed suite, §5.4's "same interface" claim and the extraction story both
  fail.
- **`t.Parallel()` and table-driven subtests named for behaviour.**
- **Golden-file tests for every error message** this package emits.
- **An extraction test** is the honest test of §5.4's claim: the same module
  code, exercised once against the memory driver and once against a driver
  behind `//go:build integration`, with no source change.
- **Drain test:** §2.3 step 3 completes before step 5, and publishing still
  works between them.

## Definition of done

1. `broker.Publisher` and `broker.Subscriber` implemented in process; the
   `warren/broker` contract suite passes unmodified.
2. Every §2.6 Consumer-column row has a test.
3. Lifecycle hooks at §2.3 steps 3 and 5, with a test proving publish still
   works at step 4.
4. `warrentest.WithMemoryBroker()` (§7.5) is wired to this package.
5. Zero third-party imports (Mode Build; AGENT.md invariant 1 if it lands in
   core).
6. Module placement decided and recorded in warren.md §1.6 — see Open
   questions — before any `go.mod` exists.
7. Doc comments on every exported identifier, starting with the identifier's
   name.
8. warren.md §5.4 amended in the same change to carry the agreed surface.

## Open questions

1. **Which module does this live in?** §1.6's repository layout lists
   `broker/kafka`, `broker/rabbitmq`, and `broker/nats` as their own modules and
   **omits `broker/memory` entirely** — it appears in neither the core module's
   contents nor the adapter module list. Two readings, and the human has to pick:
   - **Core module.** Mode Build and "default in tests" both argue for it: a
     test helper that requires adding a module to `go.mod` is friction on the
     path §7.5 says is the default, and with zero third-party imports it
     satisfies core's "stdlib + dig only" constraint (invariant 1) on the
     dependency axis.
   - **Its own module.** §1.1 puts every broker driver in the ADAPTERS ring,
     which is a ring of separate modules; a driver implementation inside the
     core module puts an adapter in the kernel's `go.mod` and blurs the ring
     boundary that `warren lint arch` is meant to enforce. Note that invariant 5
     ("contract packages contain zero implementations") is not violated either
     way, since `broker/memory` is a distinct package from `broker` — but the
     ring boundary question is real and is not a naming detail.

   Whichever is chosen, **§1.6 must be amended** — it is currently silent, and
   silence in the module map is how an empty module or a misplaced dependency
   gets created by accident (AGENT.md: "Do not create a new module unless its
   first real code lands in the same change").
2. **What is the public surface?** `memory.Broker(opts ...Option)
   warren.Module`, matching §5.1's shape? Or is the memory broker only reachable
   through `warrentest.WithMemoryBroker()` (§7.5) and a default when no driver
   is registered? warren.md shows only the test helper.
3. **What does a DLQ mean in process?** §3.4's chain includes `DeadLetter` and
   applies to memory "identically", and §5.1's consumers declare
   `broker.WithDeadLetter("billing.subscription.created.dlq")`. In process,
   does that become another in-memory topic, a captured list the test helper can
   assert on, or a log line? Unstated, and tests will want to assert on it.
4. **Is delivery synchronous or asynchronous?** It determines whether
   `AssertPublished` (§7.5) needs synchronisation, and whether §2.3 step 3's
   "in-flight messages ack" is ever non-trivial. Unstated.
5. **What are the bounds?** Queue depth, backpressure when a subscriber is
   slower than a publisher, and behaviour on overflow are unstated. A modular
   monolith running this in production (§5.4 permits it) makes these production
   questions, not test questions.
6. **Does the outbox relay run against the memory broker?** §5.5's relay drains
   the outbox to "the broker", and a modular monolith on Postgres + memory
   broker is a configuration §5.4 explicitly blesses. Whether the relay's
   exactly-once story (§5.1) means anything here is unstated.
7. **Ordering guarantees.** `Message.Key` is the "partition / routing key"
   (§3.4). Whether the memory driver preserves per-key ordering — and therefore
   whether a monolith relying on it keeps working after extraction to Kafka — is
   unstated, and this one directly threatens §5.4's extraction claim.
8. **RESOLVED (2026-08-01):** warren.md §1.4 was amended to `Conflict → ack`,
   in favour of §2.6; the two sections now agree. **§1.4 contradicts §2.6 on
   `CONFLICT`** (DLQ vs ack).
