# `warren/broker/kafka` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §5.1](../../warren.md) |
| **Module** | own module (`warren/broker/kafka`) — [warren.md §1.6](../../warren.md) |
| **Mode** | Wrap |
| **Wraps** | `twmb/franz-go` |

## Problem

Warren promises that swapping Kafka for RabbitMQ is "one line in `main.go` and
nothing else" (§1.5, §5.1). That promise only holds if no application code ever
touches a Kafka client type. Today a Go team writing a consumer reaches for
`kgo.Record` directly, and from that moment the retry policy, the dedupe step,
the DLQ routing, and the trace extraction are all hand-rolled per service and
per driver.

`warren/broker/kafka` is the transport half of that promise: it implements the
`warren/broker` port (§3.4) over franz-go so that consumers stay driver-neutral
and the driver-agnostic middleware chain does the rest.

## Goals

- Implement `broker.Publisher` and `broker.Subscriber` (§3.4) over franz-go.
- Ship as a `warren.Module` created by `kafka.Broker(opts...)`, so the whole
  driver is one block in `main.go` (§5.1 Usage, §10 bootstrap).
- Participate in the lifecycle as a component, not as a loose goroutine (§1.5):
  register start/stop hooks at the exact positions §2.3 requires.
- Support Kafka transactions, because the outbox relay needs them for
  exactly-once publishing (§5.1).
- Keep Kafka-specific concerns — partition assignment, offset commit strategy —
  as `kafka.*` module options, never visible to consumer code (§5.1).
- Register a broker-metadata health check with `warren/health` (§2.8).
- Provide one named escape hatch: inject `*kgo.Client` (§5.1).

## Non-goals

- **Defining the port.** `Message`, `Publisher`, `Subscriber`,
  `MessageHandler`, and the per-subscription options (`broker.WithRetry`,
  `broker.WithDeadLetter`, `broker.WithConcurrency`) belong to `warren/broker`
  (§3.4, §5.1 Consumer). This package implements them; it does not restate them.
- **Owning the middleware chain.** `Recover` → `TraceExtract` →
  `Deduplicate(inbox)` → `Retry(backoff)` → `DeadLetter` → `ConcurrencyLimit` →
  `Drain` is written once in the port package and applies to Kafka, Rabbit,
  NATS, and memory identically (§3.4). This driver supplies transport only.
- **Importing any other adapter.** `broker/kafka` and `persistence/postgres`
  are mutually invisible (§1.6 Rule; AGENT.md invariant 4).
- Exposing franz-go types as the default path. Escape hatch only (invariant 3).

## Dependency audit

**Chosen: `twmb/franz-go`.** warren.md §5.1 records: feature-complete pure Go
covering Kafka 0.8.0 through 4.2+, targets every client KIP, and has
transactions — which the outbox relay needs for exactly-once publishing. §9
repeats the mode as **Wrap** with the note "pure Go, Kafka ≤4.2+, transactions".

**Rejected**, per §5.1:

| Client | Blocker |
|---|---|
| `segmentio/kafka-go` | Tested to Kafka 2.7.1; newer protocol features unimplemented. A framework can't ship a lagging driver. |
| `confluent-kafka-go` | cgo wrapper around librdkafka — breaks static builds and scratch containers for **every** user touching Kafka. Non-starter. |
| `IBM/sarama` | Low-level protocol surface, poor docs, pointer-passing causes heavy allocation. |

**Mode justification.** Wrap passes the wrap rule (AGENT.md § Modes): changing
the Kafka client would otherwise force edits across every consumer and publisher
in every user service. Port in front, raw handle behind a named escape hatch.

**Outstanding — blocks the `go.mod`.** AGENT.md § "Adding a dependency"
requires, before the dependency lands: the repository read (not the README),
an archived check, the last-shipped date, the transitive dependency set, and
licence compatibility — all recorded **with the observation date**. warren.md
records none of these for franz-go: no observation date, no archived/last-ship
check, no transitive or licence note. `gh api repos/twmb/franz-go` and
`gh api repos/twmb/franz-go/releases/latest` must be run and the findings
written into this section before `go.mod` is created. Until then this spec is
not implementable. The same check is owed for the SASL and TLS sub-packages if
`kafka.SASL` keeps a franz-go type (see Open questions).

## Public API

Transcribed from §5.1. Doc comments added; nothing else added.

```go
// Package kafka implements the warren/broker port over twmb/franz-go.
package kafka

// Broker returns a warren.Module providing the Kafka implementation of
// broker.Publisher and broker.Subscriber. Swapping this block for another
// driver's is the only change a service makes to move off Kafka.
func Broker(opts ...Option) warren.Module

// Brokers sets the Kafka seed broker addresses.
func Brokers(addrs ...string) Option

// ConsumerGroup sets the consumer group this service joins.
func ConsumerGroup(name string) Option

// TLS configures the TLS settings used for broker connections.
func TLS(cfg *tls.Config) Option

// SASL configures the SASL mechanism used to authenticate to the brokers.
func SASL(m sasl.Mechanism) Option

// Transactional enables Kafka transactions, which the outbox relay requires
// for exactly-once publishing.
func Transactional(enabled bool) Option
```

Usage, from §5.1:

```go
// main.go — swapping to RabbitMQ changes only this block.
kafka.Broker(
    kafka.Brokers(cfg.Kafka.Brokers...),
    kafka.ConsumerGroup(cfg.Kafka.Group),
    kafka.Transactional(true),
)
```

Consumer code is driver-neutral and stays that way (§5.1 Consumer):

```go
func (c *BillingConsumer) Register(r transport.Registrar) {
    r.Events().On("billing.subscription.created", c.activate,
        broker.WithRetry(broker.ExponentialBackoff(5)),
        broker.WithDeadLetter("billing.subscription.created.dlq"),
        broker.WithConcurrency(10),
    )
}
```

`broker.WithRetry`, `broker.WithDeadLetter`, and `broker.WithConcurrency` are
port-owned per-subscription options (§3.4, §5.1); this driver honours them, it
does not define them.

> `Option` is unexported-constructor style: `Option` is the exported type,
> produced only by the functions above. warren.md does not give its underlying
> definition — see Open questions.

## Behaviour

**Registration.** `Broker(opts...)` returns an inert `warren.Module` value.
Nothing connects, dials, or registers on construction — boot steps 1–3 walk the
graph first (§1.3, AGENT.md § Boot).

**Boot.** Subscriptions declared through `r.Events().On(...)` build their route
table in memory at step 5. Connections and consumer-group membership start at
step 6, in dependency order `pool → repos → consumers → servers` — consumers
start before servers (§1.3).

**Health.** The driver self-registers a broker-metadata check with the
`health.Check` registry (§2.8), which feeds `/readyz`.

**Runtime.** Records fetched from franz-go are converted to `broker.Message`
(§3.4) and handed to the port's middleware chain — `recover` → `trace-extract` →
`inbox dedupe` → (already seen: **ack**; new: **Handler**) → (ok: **ack**;
error: **retry with backoff**; exhausted: **DLQ**) (§1.5). Trace context travels
in `Message.Headers`, so a span survives the trip through Kafka into the
consumer (§7.1). The driver's own job is exactly: fetch, convert, and apply the
ack/nack/DLQ decision the chain reaches.

**Kafka-specific concerns stay module-level.** Partition assignment and offset
commit strategy are `kafka.*` options; consumer code never sees them (§5.1).

**Shutdown — normative positions in §2.3.** This driver owns two of the six
steps and touches nothing else:

```
SIGTERM
  1. readiness probe → 503
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack     ← this driver
  4. outbox relay flushes
  5. DB pools, broker connections close                  ← this driver
  6. force-exit deadline (default 30s)
```

At step 3 the Kafka consumer stops fetching and lets in-flight messages reach an
ack. The client connection is **not** closed at step 3 — it must survive until
step 5 so that step 4's outbox relay can still publish its flush. Closing the
client at step 3 would break the relay flush, which is why the ordering is
normative rather than advisory.

## Error mapping

The Consumer column of §2.6, which is how this driver turns a handler error into
an ack, a nack, or a DLQ send. The decision is reached by the port's middleware;
the driver executes it against franz-go.

| Code | Consumer action (§2.6) | Illustrative — Kafka mechanics pending, see Open questions |
|---|---|---|
| `INVALID` | → DLQ (never retry) | Produce to the configured dead-letter topic, then ack the original. No retry attempt. |
| `NOT_FOUND` | ack + log | Ack and log. Not an error worth redelivering. |
| `CONFLICT` | ack (idempotent replay) | Ack. The message has already had its effect. |
| `UNAUTHENTICATED` | → DLQ (never retry) | As `INVALID`: produce to the dead-letter topic, then ack the original. A bad credential does not improve with retries (§2.6). |
| `PERMISSION_DENIED` | → DLQ (never retry) | As `INVALID`: produce to the dead-letter topic, then ack the original. |
| `UNAVAILABLE` | nack + backoff retry | Do not ack; redeliver under the subscription's `broker.WithRetry` backoff. |
| `INTERNAL` | nack + retry, then DLQ | Redeliver under backoff; on exhaustion route to `broker.WithDeadLetter`'s topic (§1.5 "exhausted → DLQ"). |

warren.md does not say what happens to an error carrying no Warren code.
`INTERNAL` is the only row that could apply, but §2.6 does not make the choice —
see Open questions.

Note: §1.4's transport-spine diagram previously said `Conflict → DLQ`; it has
been amended to `Conflict → ack` and now agrees with §2.6. See Open question 8,
resolved.

## Escape hatch

Inject `*kgo.Client` directly (§5.1). That is the only sanctioned route to a
franz-go type, it is an explicit opt-out, and it is never the default path
(AGENT.md invariant 3). A consumer that touches `kgo.Record` has left the
driver-agnostic chain behind, and §3.4 says so plainly: the port's value
"evaporates the moment a consumer touches `kgo.Record` directly."

warren.md gives the escape hatch as prose, not as a signature — the exact form
is an open question.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing). Record
  ↔ `broker.Message` conversion, header propagation, option assembly, and the
  ack/nack/DLQ decision table are all testable against a fake transport.
- **Real Kafka behind `//go:build integration`** — group rebalancing,
  transactions, DLQ production, drain-on-shutdown. §7.5 puts real brokers behind
  a build tag; this driver follows that.
- **The `warren/broker` contract suite must pass unmodified.** Per AGENT.md
  § Testing, a port change updates the contract suite first, then the drivers.
- **Golden-file tests for every error message** this package emits, including
  connection and authentication failures. Untested error text rots (invariant 2,
  AGENT.md § Testing).
- **Drain test:** in-flight messages ack before the client closes — i.e. §2.3
  step 3 completes before step 5.

## Definition of done

1. `broker.Publisher` and `broker.Subscriber` implemented over franz-go; the
   `warren/broker` contract suite passes.
2. `kafka.Broker` returns an inert module; nothing dials before step 6.
3. Lifecycle hooks land at §2.3 steps 3 and 5, with a test proving the
   ordering.
4. Every §2.6 Consumer-column row has a test.
5. Broker-metadata health check registered (§2.8).
6. `*kgo.Client` escape hatch exists and is the only franz-go type reachable
   from this module's public surface — verified by a check that no franz-go type
   appears in an exported signature (invariant 3).
7. This module imports no other adapter module (invariant 4).
8. Dependency audit above completed with an observation date **before** the
   `go.mod` is written.
9. Doc comments on every exported identifier, starting with the identifier's
   name.

## Open questions

1. **`SASL(sasl.Mechanism)` puts a franz-go type in an exported signature.**
   `sasl.Mechanism` is `github.com/twmb/franz-go/pkg/sasl`. AGENT.md invariant 3
   forbids a driver type in any Warren exported signature and permits raw
   handles "through named escape hatches only". §5.1 nevertheless lists this
   option. Either invariant 3 is narrower than it reads (driver types are
   permitted inside the driver module's own options), or §5.1 needs amending to
   a Warren-owned mechanism type. This needs a human decision, and it changes
   the public surface either way. `TLS(*tls.Config)` is fine — `crypto/tls` is
   stdlib.
2. **What is `Option`?** warren.md gives the constructor functions but not the
   type. A function type over an unexported config struct is the obvious
   reading, but it is not stated.
3. **How does the outbox relay reach Kafka transactions?** §5.1 justifies
   franz-go partly because "the outbox relay needs [transactions] for
   exactly-once publishing", but the relay talks to `broker.Publisher` (§3.4),
   which has no transactional concept — just `Publish(ctx, topic, msgs...)`.
   Either the port grows a transactional publish, or the driver applies
   transactions internally when `Transactional(true)` is set, or the relay
   special-cases Kafka (which would break invariant 4). Unresolved in warren.md.
4. **What is the escape hatch's exact form?** "inject `*kgo.Client` directly"
   describes DI availability, not a named function like
   `http.Raw(func(*chi.Mux))` (§4.1). Is it `kafka.Raw(func(*kgo.Client))`, or
   is `*kgo.Client` simply provided into the container?
5. **Which partition-assignment and offset-commit options exist?** §5.1 says
   these are `kafka.*` module options but names none.
6. **Is `Transactional(true)` also required for the DLQ path?** Not stated.
7. **`broker.ExponentialBackoff(5)` is used in §5.1 but is not in §3.4's port
   surface.** The port spec must define it; recorded here because the only place
   warren.md shows it is this section.
8. **RESOLVED (2026-08-01):** warren.md §1.4 was amended to `Conflict → ack`,
   in favour of §2.6; the two sections now agree, and this spec's reading was
   confirmed. **§1.4 contradicts §2.6 on `CONFLICT`** (DLQ vs ack). Resolve in warren.md;
   this spec follows §2.6.
