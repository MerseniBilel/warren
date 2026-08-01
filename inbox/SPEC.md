# `warren/inbox` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved. **Substantially unspecified — see Open questions.** |
| **Source** | [warren.md §5.6](../warren.md) |
| **Module** | **undecided** — §1.6's module list omits this package. See Open questions. |
| **Mode** | **not stated** — §5.6 has no Mode line and no §9 ledger row, so no mode is recorded anywhere. See Open questions. |
| **Wraps** | — (none stated) |

> **Read this first.** warren.md §5.6 is two sentences: *"Idempotent
> consumption. Dedupe store (Postgres or Redis) keyed on `Message.ID` with TTL.
> Enabled by default — at-least-once delivery means duplicates are certain, not
> hypothetical."* Three further mentions exist: §1.5's runtime diagram
> (`inbox dedupe` → already seen: **ack**; new: **Handler**), §3.4's middleware
> chain (`Deduplicate(inbox)`), and §10's flow (`dedupe (inbox) → retry policy
> → its own handler`). **warren.md gives this package no public API, no Mode,
> and no module.** This spec records what is there and puts the rest in Open
> questions rather than inventing a surface.

## Problem

Every broker Warren supports delivers at-least-once. §5.6 states the
consequence in the sharpest available terms: *duplicates are certain, not
hypothetical*. §5.5's outbox relay makes them likelier still — a relay that
publishes a batch and crashes before marking the rows dispatched republishes
them on the next poll, which is the correct behaviour and the reason the outbox
pattern works.

So every consumer is either idempotent or wrong. Making each handler responsible
for its own deduplication means every team re-derives the same table, the same
TTL, and the same race — and gets it wrong in a way that only shows up under
redelivery. `warren/inbox` makes it a step in the shared chain instead:
`Deduplicate(inbox)` (§3.4), on by default, before the handler ever runs.

## Goals

Everything here traces to §5.6, §1.5, §3.4, or §10. There is nothing else.

- **Idempotent consumption** (§5.6).
- A **dedupe store keyed on `Message.ID`** (§5.6). `Message.ID` is the port's
  declared "idempotency key" (§3.4), so the key is fixed by the port, not chosen
  here.
- **TTL** on dedupe records (§5.6) — the store is bounded, not a permanent log.
- **Postgres or Redis** as the store (§5.6).
- **Enabled by default** (§5.6) — a consumer gets deduplication without asking
  for it, which is the only way "duplicates are certain" is answered honestly.
- Supply the `Deduplicate(inbox)` step of the port's driver-agnostic chain
  (§3.4), positioned after `TraceExtract` and before `Retry(backoff)`.

## Non-goals

- **Defining the port.** `Message` and `Message.ID` are `warren/broker`'s
  (§3.4).
- **Owning the middleware chain.** `Recover` → `TraceExtract` →
  `Deduplicate(inbox)` → `Retry(backoff)` → `DeadLetter` → `ConcurrencyLimit` →
  `Drain` lives in the port package (§3.4). This package supplies the dedupe
  store the chain consults; it does not own the chain.
- **Deduplicating on the publish side.** That is the outbox's problem (§5.5).
- **Importing a persistence adapter.** Invariant 4 makes adapters mutually
  invisible, and §1.1 keeps the kernel ignorant that SQL exists — which the
  "Postgres or Redis" store immediately puts in tension. See Open questions.

## Dependency audit

**No dependency is stated, so nothing is audited.** §5.6 names no library and
§9's ledger has no row for inbox.

Two things are nevertheless owed before any code lands:

1. **A Mode.** AGENT.md § Modes requires every third-party decision to carry
   one, and §5.6 carries none. (§5.3 lacks one too, but NATS is at least
   recorded as Wrap in §9's ledger; inbox has no ledger row at all.) If the
   answer is Build (as §5.5's outbox is), say so in warren.md.
2. **A placement decision for the store.** "Postgres or Redis" (§5.6) means the
   dedupe store's implementations live where those drivers live —
   `persistence/postgres` (`jackc/pgx`) and `persistence/redis`
   (`redis/go-redis`, §9, whose ledger note is "cache + lock"). Those
   dependencies are already audited in §9 for their own modules; what is not
   decided is whether `warren/inbox` defines a port they implement, or whether
   it is an adapter-ring module that imports one of them. See Open questions.

## Public API

**warren.md gives none.** No constructor, no option, no store interface, no type
name. The only Go-shaped mention anywhere is `Deduplicate(inbox)` in §3.4's
chain — which names a `warren/broker` middleware taking something called
`inbox`, and does not tell us what that something's type is.

`kafka.Broker(opts...) warren.Module` (§5.1) and `outbox.Relay(opts...)` (§5.5)
suggest a module-and-options shape, and "enabled by default" (§5.6) suggests
there may be no `main.go` registration at all. Both are guesses. AGENT.md is
explicit that public API is the human's call, and §5.6 is precisely the kind of
under-specified entry where inventing a plausible surface would produce a spec
that reviews cleanly and describes nothing. The surface is in Open questions.

What this spec commits to, because §5.6 and §3.4 do say it: a dedupe store keyed
on `broker.Message.ID` with a TTL, consulted by the port's `Deduplicate` step,
active unless a consumer opts out.

## Behaviour

Only what warren.md fixes:

**Position in the chain.** §1.5's runtime diagram is normative:

```
Subscriber ─▶ recover ─▶ trace-extract ─▶ inbox dedupe
                                              │
                                ┌─────────────┴──────────┐
                           already seen              new
                                │                     │
                              ack                 Handler
                                                      │
                                ┌─────────────────────┤
                              ok                    error
                                │                     │
                              ack           retry w/ backoff
                                                      │
                                            exhausted → DLQ
```

- Dedupe runs **after** `recover` and `trace-extract` and **before** the
  handler — so a duplicate costs a store lookup, not a handler invocation.
- **Already seen → ack.** The message is acknowledged and the handler is never
  called. This is the package's entire observable behaviour on the hot path.
- **New → Handler**, and the retry/DLQ machinery downstream is the port's, not
  this package's (§3.4).

**Keyed on `Message.ID` with TTL** (§5.6). `Message.ID` is the port's
idempotency key (§3.4). The TTL bounds the store; warren.md gives no default
value and no eviction policy.

**Enabled by default** (§5.6).

**Driver-agnostic.** The chain applies "to Kafka, Rabbit, NATS, and memory
identically" (§3.4), so dedupe behaves the same on every driver and in tests.

**Lifecycle.** warren.md assigns this package no position of its own in the §2.3
shutdown sequence. Two consequences follow from the sequence and are stated here
because they are consequences, not additions:

```
SIGTERM
  1. readiness probe → 503
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack     ← dedupe runs inside this
  4. outbox relay flushes
  5. DB pools, broker connections close                  ← the store's backing pool
  6. force-exit deadline (default 30s)
```

- The dedupe step runs inside consumer message handling, so it must keep working
  through step 3 while in-flight messages drain.
- Its store rides on a Postgres or Redis pool, which closes at step 5 — after
  step 3, so the ordering already protects it. A store holding its own
  connection that closed earlier would break the drain.

Whether a dedupe record is written before or after the handler succeeds — and
therefore whether a crash mid-handler causes a lost message or a duplicate one —
is **unstated in warren.md** and is the most consequential of the open questions.

## Error mapping

**This package has no ack/nack/DLQ decisions of its own except one**, and that
one is from §1.5 rather than §2.6: **already seen → ack**.

Everything else in the Consumer column of §2.6 is decided downstream of dedupe,
by the port's chain and executed by the driver:

| Code | Consumer action |
|---|---|
| `INVALID` | → DLQ (never retry) |
| `NOT_FOUND` | ack + log |
| `CONFLICT` | ack (idempotent replay) |
| `UNAUTHENTICATED` | → DLQ (never retry) |
| `PERMISSION_DENIED` | → DLQ (never retry) |
| `UNAVAILABLE` | nack + backoff retry |
| `INTERNAL` | nack + retry, then DLQ |

`CONFLICT → ack (idempotent replay)` is the row that most closely resembles this
package's job, and the pair is worth naming: the inbox catches duplicates it has
a record of, and `CONFLICT` catches the ones it does not — a handler that
detects the replay itself. They are complementary, not alternatives.

**What happens when the dedupe store itself is unavailable is unstated.** Fail
open (deliver, risking a duplicate) or fail closed (nack, per `UNAVAILABLE`)? The
answer changes the delivery guarantee, and warren.md does not give it. See Open
questions.

Note: §1.4 previously said `Conflict → DLQ`; it has been amended to
`Conflict → ack` and now agrees with §2.6. See Open question 12, resolved.

## Escape hatch

**None stated.** No Mode is recorded and no library is wrapped, so AGENT.md
invariant 3's named-escape-hatch requirement has nothing to attach to. If the
store ends up as a port with Postgres and Redis implementations, the raw handles
are already reachable through those adapters and no new hatch is needed — but
that is contingent on Open question 2.

## Testing

The rules that bind regardless of the undecided surface (AGENT.md § Testing):

- **Unit tests: no Docker, no network, no sleeps.** The dedupe decision — seen
  vs new, TTL expiry — is testable against an in-memory store with an injectable
  clock. TTL testing without an injectable clock means sleeps, which are
  forbidden, so the clock must be injectable.
- **Real Postgres and Redis behind `//go:build integration`** for the actual
  store implementations, including concurrent-consumer races on the same
  `Message.ID`.
- **The `warren/broker` contract suite must pass** with dedupe enabled — it is
  on by default (§5.6), so the default configuration is what the suite exercises.
- **Duplicate-delivery test:** the same `Message.ID` delivered twice invokes the
  handler once and acks twice (§1.5).
- **The outbox round trip** (§10): relay republishes a batch after a simulated
  crash; the consumer's handler runs once. This is the scenario §5.6 exists for
  and it belongs behind the integration tag.
- **Golden-file tests for every error message** this package emits.

## Definition of done

Not reachable until the Open questions are answered — a definition of done is a
checklist against a public API, and none exists. What is fixed:

1. Duplicate `Message.ID` is acked without invoking the handler (§1.5, §5.6).
2. Records are keyed on `Message.ID` and expire on a TTL (§5.6).
3. Enabled by default, with the opt-out named and documented (§5.6).
4. Store implementations exist for Postgres and Redis (§5.6).
5. Dedupe survives the §2.3 step-3 drain and its store closes no earlier than
   step 5.
6. No import of any adapter module from a core-ring package (invariant 4),
   subject to Open question 2.
7. Mode and module placement recorded in warren.md §5.6 and §1.6.
8. Doc comments on every exported identifier, starting with the identifier's
   name.
9. warren.md §5.6 amended in the same change to carry the agreed surface —
   AGENT.md requires the spec and the manifest to agree, and here the manifest
   is close to empty.

## Open questions

1. **What is the public API?** No constructor, no options, no store interface,
   no TTL setting, no opt-out. `Deduplicate(inbox)` (§3.4) implies the port
   takes an inbox *value* of some type this package defines, but that type is
   unnamed. Everything about this package's surface is undecided.
2. **Port here, implementations in the adapters — or an adapter module?**
   "Postgres or Redis" (§5.6) cannot be reached from the core module (invariant
   1: stdlib + dig; §1.1: the kernel does not know SQL exists). AGENT.md's
   stated move is "define the port in core, implement it in a submodule", which
   would put a dedupe-store port in `warren/inbox` and the implementations in
   `persistence/postgres` and `persistence/redis`. But **neither adapter's
   "Provides" list mentions a dedupe store** — §6.1's Postgres list is pool,
   UnitOfWork, tx propagation, outbox table + writer, health check, migrations;
   §6.2–6.4 say Redis "provides cache + distributed lock rather than
   repositories". So §5.6 promises two implementations that §6 does not
   acknowledge.
3. **What Mode is this?** §5.6 carries no Mode line, and unlike §5.3 — which
   also lacks one but is recorded as Wrap in §9 — the ledger has no row for
   inbox either, so no mode is recorded anywhere.
4. **Which module?** §1.6's repository layout **omits `warren/inbox` entirely**,
   as it omits `warren/outbox` and `warren/broker/memory`. Blocked on
   question 2.
5. **Is the dedupe record written before or after the handler runs?** Before
   gives at-most-once semantics under a crash (message lost); after gives
   at-least-once (handler may run twice). Warren promises idempotent consumption
   over at-least-once delivery (§5.6), which argues for after — but §1.5's
   diagram shows dedupe as a single step before the handler with no second
   write, and the two readings have different failure modes. This is the most
   consequential unanswered question in the package.
6. **What happens on a dedupe-store failure?** Fail open or fail closed? See
   Error mapping.
7. **What is the default TTL, and what governs it?** §5.6 says TTL and gives no
   value. It has to exceed the maximum redelivery window — which is set by
   `broker.WithRetry` (§5.1) and by broker-side redelivery — and warren.md does
   not connect the two.
8. **How is it disabled?** "Enabled by default" (§5.6) implies an opt-out that
   is never named. Per-subscription (alongside `broker.WithRetry`,
   `broker.WithDeadLetter`, `broker.WithConcurrency` in §5.1) or global?
9. **What does the inbox do in tests and modular monoliths?** §5.4 makes the
   memory broker the default in tests; a dedupe store defaulting to Postgres
   would drag a database into every unit test, which AGENT.md § Testing
   forbids. An in-memory store must therefore exist for that path, and warren.md
   names only Postgres and Redis.
10. **Who writes the dedupe table's schema and migration?** Same unanswered
    question as the outbox table (§5.5 / §6.1), and unanswered in the same way.
11. **Is `Message.ID` guaranteed present and unique?** §3.4 calls it the
    "idempotency key" but nothing states who assigns it — the outbox writer, the
    relay, the publisher, or the user. Dedupe keyed on an empty or reused ID
    fails silently, which is the worst available failure mode.
12. **RESOLVED (2026-08-01):** warren.md §1.4 was amended to `Conflict → ack`,
    in favour of §2.6; the two sections now agree. **§1.4 contradicts §2.6 on
    `CONFLICT`** (DLQ vs ack).
