# `warren/broker/rabbitmq` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2 (decided 2026-08-02)** — see **Why it is deferred** below. Nothing in shipped code depends on it. |
| **Source** | [warren.md §5.2](../../warren.md) |
| **Module** | own module (`warren/broker/rabbitmq`) — [warren.md §1.6](../../warren.md) |
| **Mode** | Wrap |
| **Wraps** | `rabbitmq/amqp091-go` |


## Why it is deferred

**The job a second driver does is already done by a shipped module** — but
the sentence that used to be here overstated the evidence, and the correction
matters more than the deferral.

It said `broker/memory` "passes the same exported `broker/brokertest`
contract suite that `broker/kafka` passes". **`broker/kafka` did not pass it,
because it never ran it.** Until 2026-08-05, `brokertest.Run` had exactly one
caller: `broker/memory`. So the port's swappability was demonstrated by ONE
implementation and an interface, not by two — and an in-process broker is the
easiest possible subject, where "the subscription is live" is a map insert
and ordering is whatever order the sender used.

`broker/kafka/integration_test.go` now runs the suite behind the `integration`
tag, against a real broker. **It ran green on 2026-08-05** — all nine subtests
against `apache/kafka:3.9.0`, reproducible with `make integration` and
`WARREN_TEST_KAFKA_BROKERS` set. The certification claim this deferral rests
on is proven, and so is the community-adapter strategy that depends on the
same suite.

Proven at a price worth recording, because it is the argument FOR the suite
rather than against it. The first real run failed on four counts, three of
them driver defects the unit tests could not have caught: a 30-second
shutdown (`BlockRebalanceOnPoll` never released on a cancelled poll), a
cancelled subscription that kept receiving for the rest of a fetched batch,
and a deregistration that matched handlers by code pointer and so removed the
wrong sibling under fan-out. An in-process broker expresses none of these —
it has no group to leave, its batches are one message long, and its
subscriptions are map entries. A third driver that ships without running this
suite ships those bugs, whichever three they turn out to be.

A third driver still buys a bullet point and costs a module, an audit, and a
maintained integration suite — that part of the argument stands on its own.

It is also not settled. §5.2 gives four sentences, and among its eight open
questions is a **direct contradiction**: §3.4 calls `Message.Key` the routing
key, §5.2 says the TOPIC becomes the routing key. Both cannot be true, and that
is a port-semantics question worth resolving on its own merits — which is why
v0.2 fixes the manifest before it writes the driver.

`amqp091-go` has zero transitive dependencies.

## Problem

"Swapping Kafka for RabbitMQ changes one line in `main.go` and nothing else"
(§1.5) is a claim that has to be paid for by a second driver that really does
implement the same port. This package is that payment: the AMQP transport behind
`broker.Publisher` and `broker.Subscriber` (§3.4), so that a service moving off
Kafka replaces one block in `main.go` (§5.1 Usage) and leaves every consumer,
handler, retry policy, and DLQ declaration untouched.

AMQP's model is not Kafka's — there are exchanges, routing keys, queues, and
publisher confirms where Kafka has topics and offsets. Mapping that difference
is this package's whole job, and it is the reason the mapping belongs here and
not in user code.

## Goals

- Implement `broker.Publisher` and `broker.Subscriber` over `amqp091-go` —
  §5.2: "Same `Publisher`/`Subscriber` implementation."
- Map the port's topic onto AMQP: **topic → exchange + routing key** (§5.2).
- **Quorum queues by default** (§5.2).
- **DLQ via dead-letter exchange** (§5.2) — the port's
  `broker.WithDeadLetter(...)` (§5.1) resolves to a DLX binding, not to a
  RabbitMQ concept the consumer has to know about.
- **Publisher confirms on** (§5.2).
- Ship as a `warren.Module` created by `rabbitmq.Broker(opts...)`, a lifecycle
  participant with hooks at the §2.3 positions.

## Non-goals

- **Defining the port.** `Message`, `Publisher`, `Subscriber`,
  `MessageHandler`, and the per-subscription options `broker.WithRetry`,
  `broker.WithDeadLetter`, `broker.WithConcurrency` are `warren/broker`'s
  (§3.4, §5.1). Referenced, not restated.
- **Owning the middleware chain.** `Recover` → `TraceExtract` →
  `Deduplicate(inbox)` → `Retry(backoff)` → `DeadLetter` → `ConcurrencyLimit` →
  `Drain` is driver-agnostic and lives in the port package (§3.4). This driver
  supplies transport only.
- **Importing any other adapter** (§1.6 Rule; AGENT.md invariant 4). In
  particular this module knows nothing about `broker/kafka`.
- Exposing `amqp.Delivery` or any other `amqp091-go` type on the default path
  (AGENT.md invariant 3 names `amqp.Delivery` explicitly).

## Dependency audit

**Chosen: `rabbitmq/amqp091-go`**, Mode **Wrap**. §9 records the reason in one
line: "official successor to streadway/amqp". §5.2 records no rejected
alternatives — unlike §5.1 for Kafka, warren.md argues no comparison here.

**Mode justification.** Same wrap rule as every other driver: changing the AMQP
client would otherwise force edits across every publisher and consumer in every
user service, so it goes behind the port with a raw handle available as an
escape hatch (AGENT.md § Modes).

**Outstanding — blocks the `go.mod`.** AGENT.md § "Adding a dependency"
requires a written audit with an observation date before the dependency enters
any `go.mod`: archived check, last-shipped date, transitive dependency set,
licence compatibility. warren.md records none of these for `amqp091-go`, and
"official successor to streadway/amqp" is provenance, not a health check —
`streadway/amqp` itself is the cautionary example of a widely-recommended
package going quiet. Run `gh api repos/rabbitmq/amqp091-go` and
`gh api repos/rabbitmq/amqp091-go/releases/latest`, record the findings here,
and only then write the `go.mod`.

**Also outstanding:** warren.md considers no alternative (`wagslane/go-rabbitmq`
and others). AGENT.md's process asks for the rejection to be written down; §5.1
does that for Kafka and §5.2 does not. Either the comparison is made and
recorded here, or the human confirms that "official client" is sufficient
grounds on its own.

## Public API

warren.md §5.2 gives a call site, not signatures. The Go below is transcribed
from that call site with doc comments added; the return type and the parameter
types are read off the usage and are marked in Open questions where warren.md
does not state them.

```go
// Package rabbitmq implements the warren/broker port over rabbitmq/amqp091-go.
package rabbitmq

// Broker returns a warren.Module providing the RabbitMQ implementation of
// broker.Publisher and broker.Subscriber.
func Broker(opts ...Option) warren.Module

// URL sets the AMQP connection URL.
func URL(url string) Option

// Exchange declares the exchange messages are published to and consumed from,
// with its kind.
func Exchange(name string, kind ExchangeKind) Option

// PrefetchCount sets the consumer prefetch count. Whether it applies per
// consumer or per channel is not stated by §5.2 — see Open questions.
func PrefetchCount(n int) Option

// Topic is the topic exchange kind.
const Topic ExchangeKind = ...
```

Usage, verbatim from §5.2:

```go
rabbitmq.Broker(
    rabbitmq.URL(cfg.Rabbit.URL),
    rabbitmq.Exchange("events", rabbitmq.Topic),
    rabbitmq.PrefetchCount(20),
)
```

Consumer code is identical to the Kafka case — that is the point. The
subscription options in §5.1's consumer example (`broker.WithRetry`,
`broker.WithDeadLetter`, `broker.WithConcurrency`) are port-owned and honoured
here unchanged.

> `rabbitmq.Topic` appears in §5.2 as the second argument to `Exchange`. Its
> type is not named in warren.md; `ExchangeKind` above is a placeholder pending
> the decision in Open questions. Whatever it is named, it must not be an
> `amqp091-go` type (invariant 3).

## Behaviour

**Topic mapping.** §5.2: *topic → exchange + routing key.* A `Publish(ctx,
topic, msgs...)` call goes to the configured exchange with the topic carried as
the routing key. What a `Subscribe(ctx, topic, h)` call does on the AMQP side —
whether this driver declares a queue at all, and with what binding — is not
stated by §5.2; see Open question 7. `Message.Key` is described by
§3.4 as the "partition / routing key" — the interaction between `Message.Key`
and the port's `topic` argument on AMQP is an open question.

**Quorum queues by default** (§5.2). Queues this driver declares are quorum
queues unless a user opts out — warren.md states the default and gives no opt-out
option.

**Publisher confirms on** (§5.2) — three words is all warren.md gives. What an
unconfirmed publish does (error, retry, or timeout, and with what deadline) is
not stated; see Open questions.

**DLQ via dead-letter exchange** (§5.2). When a subscription declares
`broker.WithDeadLetter("...")`, this driver realises it as a DLX on the queue.
The consumer names a destination, not a RabbitMQ mechanism.

**Registration and boot.** `Broker(opts...)` returns an inert `warren.Module`;
nothing dials on construction (§1.3 steps 1–3, AGENT.md § Boot). Consumers build
their route tables at step 5 and start at step 6, in the order
`pool → repos → consumers → servers` (§1.3).

**Runtime.** Deliveries are converted to `broker.Message` (§3.4) and handed to
the port's chain: `recover` → `trace-extract` → `inbox dedupe` → (already seen:
**ack**; new: **Handler**) → (ok: **ack**; error: **retry w/ backoff**;
exhausted: **DLQ**) (§1.5). Trace context rides in `Message.Headers` (§7.1).
Consumers are lifecycle components, not goroutines someone forgot about (§1.5).

**Shutdown — normative positions in §2.3:**

```
SIGTERM
  1. readiness probe → 503
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack     ← this driver
  4. outbox relay flushes
  5. DB pools, broker connections close                  ← this driver
  6. force-exit deadline (default 30s)
```

At step 3 consumption stops and in-flight deliveries are acked; the AMQP
connection and channels stay open until step 5 so the outbox relay's step-4
flush can still publish through them. Closing at step 3 breaks the relay flush.

## Error mapping

The Consumer column of §2.6. The port's middleware decides; this driver executes
the decision in AMQP terms.

| Code | Consumer action | Driver behaviour |
|---|---|---|
| `INVALID` | → DLQ (never retry) | Route to the dead-letter exchange, no redelivery attempt. |
| `NOT_FOUND` | ack + log | `basic.ack` and log. |
| `CONFLICT` | ack (idempotent replay) | `basic.ack` — the effect already happened. |
| `UNAUTHENTICATED` | → DLQ (never retry) | As `INVALID`: route to the dead-letter exchange, no redelivery attempt. A bad credential does not improve with retries (§2.6). |
| `PERMISSION_DENIED` | → DLQ (never retry) | As `INVALID`: route to the dead-letter exchange, no redelivery attempt. |
| `UNAVAILABLE` | nack + backoff retry | `basic.nack`; redelivered under the subscription's `broker.WithRetry` backoff. |
| `INTERNAL` | nack + retry, then DLQ | Redeliver under backoff; on exhaustion, dead-letter exchange (§1.5 "exhausted → DLQ"). |

warren.md does not say what happens to an error carrying no Warren code.
`INTERNAL` is the only row that could apply, but §2.6 does not make the choice —
see Open questions.

Note: §1.4 previously said `Conflict → DLQ`; it has been amended to
`Conflict → ack` and now agrees with §2.6, which this spec follows. See Open
question 9, resolved.

## Escape hatch

**warren.md names none for this package.** §5.1 gives Kafka "inject `*kgo.Client`
directly" and §4.1/§4.2 give `http.Raw` and `grpc.Raw`; §5.2 gives nothing.
AGENT.md invariant 3 assumes an escape hatch exists for each wrapped driver
("Raw handles are reachable through named escape hatches only") and names
`amqp.Delivery` as a type that must not otherwise appear. Naming this driver's
escape hatch is an open question, not something this spec may invent.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing).
  Delivery ↔ `broker.Message` conversion, topic → exchange + routing key
  mapping, DLX derivation from `broker.WithDeadLetter`, and the ack/nack table
  are all testable against a fake channel.
- **Real RabbitMQ behind `//go:build integration`** — quorum queue declaration,
  publisher confirms, DLX routing, prefetch behaviour, drain on shutdown (§7.5
  puts real brokers behind a build tag).
- **The `warren/broker` contract suite must pass unmodified** — the same suite
  the Kafka driver passes. If it passes for one driver and not the other, the
  "one line in `main.go`" claim is false.
- **Golden-file tests for every error message** this package emits (connection
  failure, exchange mismatch, confirm timeout).
- **Drain test:** §2.3 step 3 completes before step 5.

## Definition of done

1. `broker.Publisher` and `broker.Subscriber` implemented over `amqp091-go`;
   the `warren/broker` contract suite passes.
2. Quorum queues, publisher confirms, and DLX-backed DLQ are the defaults, with
   tests.
3. `rabbitmq.Broker` returns an inert module; nothing dials before step 6.
4. Lifecycle hooks at §2.3 steps 3 and 5, ordering proven by a test.
5. Every §2.6 Consumer-column row has a test.
6. No `amqp091-go` type in any exported signature (invariant 3), verified by a
   check; escape hatch named and agreed first.
7. No import of any other adapter module (invariant 4).
8. Dependency audit above completed with an observation date **before** the
   `go.mod` is written.
9. Doc comments on every exported identifier, starting with the identifier's
   name.

## Open questions

1. **What is `rabbitmq.Topic`'s type, and what are the other kinds?** §5.2 shows
   `Exchange("events", rabbitmq.Topic)` and nothing more. Direct, fanout, and
   headers exchanges are not mentioned. The type must be Warren-owned, not
   `amqp091-go`'s (invariant 3).
2. **Does `Broker` return `warren.Module`?** §5.1 states it for Kafka; §5.2
   shows only the call. Assumed by symmetry, needs confirming.
3. **What is this driver's escape hatch?** See above — warren.md gives none.
4. **How do `Message.Key` and the port's `topic` argument both map onto AMQP?**
   §3.4 calls `Key` the "partition / routing key" and §5.2 says the topic
   becomes the routing key. Both cannot be the routing key.
5. **Can quorum queues be turned off?** §5.2 says "by default", implying an
   opt-out that is never named.
6. **Is there a health check?** §2.8 says adapters self-register and names only
   postgres and kafka. Whether RabbitMQ registers one, and what it probes, is
   unstated.
7. **How are exchanges and queues declared — at boot, or lazily?** Unstated. It
   matters, because §1.3's rule is that every detectable error surfaces at boot.
8. **No `Rabbit` section exists in §2.4's `Config` example**, yet §5.2 reads
   `cfg.Rabbit.URL`. Cosmetic, but the config shape for this driver is undefined.
9. **RESOLVED (2026-08-01):** warren.md §1.4 was amended to `Conflict → ack`,
   in favour of §2.6; the two sections now agree, and this spec's reading was
   confirmed. **§1.4 contradicts §2.6 on `CONFLICT`** (DLQ vs ack). Resolve in warren.md;
   this spec follows §2.6.
