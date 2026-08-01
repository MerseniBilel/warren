# `github.com/MerseniBilel/warren/broker` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01) for `Message`, `Publisher`, `Subscriber`, `MessageHandler`** — the middleware chain's home and the option types are carved out, blocked on Open questions 1–2 |
| **Source** | [warren.md §3.4](../warren.md), §1.5, §5.1 |
| **Module** | core |
| **Mode** | Build (ports only) — **the least negotiable wrap in the framework** |
| **Wraps** | — |

## Problem

§3.4 calls this "the least negotiable wrap in the framework", and states why in
one paragraph:

> **Driver-agnostic middleware** — written once, applies to Kafka, Rabbit, NATS,
> and memory identically: `Recover` → `TraceExtract` → `Deduplicate(inbox)` →
> `Retry(backoff)` → `DeadLetter` → `ConcurrencyLimit` → `Drain`.
>
> That property is the entire messaging pitch. It evaporates the moment a
> consumer touches `kgo.Record` directly — which is why the port is mandatory
> and the raw client is an explicit escape hatch, not the default path.

Messaging is where hand-rolled Go services accumulate their worst code:
at-least-once delivery means duplicates are certain (§5.6), retries need
backoff, poison messages need a dead-letter path, consumers need to drain on
SIGTERM rather than being goroutines someone forgot about (§1.5), and every one
of those is rewritten per project and per driver. Each is also completely
independent of whether the bytes came from Kafka, AMQP, JetStream, or a channel
in the same process — *provided* the consumer never sees the driver's record
type.

That proviso is the port. `Message`, `Publisher`, `Subscriber`, and
`MessageHandler` are the four types that let §5.1's promise hold: "**main.go** —
swapping to RabbitMQ changes only this block", and §5.4's: an in-process broker
with the same interface "is what makes `warren extract module` viable: modules
communicate through the broker port from day one, so extraction swaps the driver
rather than rewriting call sites."

## Goals

1. Define one driver-neutral message envelope carrying everything the
   runtime needs: an idempotency key, a type, a routing key, bytes, headers for
   trace propagation, and an occurrence time (§3.4).
2. Define `Publisher` and `Subscriber` so a use case, the outbox relay, and a
   consumer all name the same two interfaces regardless of driver.
3. Keep every driver type out of every signature — `kgo.Record`,
   `amqp.Delivery`, `*kgo.Client` (invariant 3). The raw client is a named
   escape hatch (§5.1), never the default path.
4. Carry the per-subscription options §5.1 demonstrates —
   `broker.WithRetry(broker.ExponentialBackoff(5))`,
   `broker.WithDeadLetter(topic)`, `broker.WithConcurrency(10)` — so retry,
   dead-lettering, and concurrency are configured in driver-neutral terms at the
   registration site.
5. Make the middleware chain in §3.4 the normative consumer pipeline every
   driver runs, so it is written once.

## Non-goals

- **No driver.** Kafka is `broker/kafka` (§5.1), Rabbit `broker/rabbitmq`
  (§5.2), NATS `broker/nats` (§5.3), in-process `broker/memory` (§5.4). Each is
  its own module; none may be imported here, and they never import each other
  (invariant 4).
- **No topic/exchange topology.** Partitions, offset-commit strategy, quorum
  queues, exchange bindings, prefetch and JetStream config are driver options —
  §5.1 is explicit: "Kafka-specific concerns (partition assignment, offset
  commit strategy) are `kafka.*` options on the module; the consumer code stays
  driver-neutral."
- **No outbox and no inbox.** The relay is `warren/outbox` (§5.5); the dedupe
  store is `warren/inbox` (§5.6). `Deduplicate(inbox)` in the chain *uses* the
  latter; this package is not it.
- **No serialisation format.** `Payload` is `[]byte`. Nothing here says JSON,
  proto, or Avro.
- **No transport registration.** `r.Events().On(...)` is
  `transport.EventRegistrar` (§3.5); this package supplies the options it takes.
- **Zero implementations** — invariant 5. Not even an in-memory publisher:
  §5.4's memory broker is its own package.

## Public API

Taken from warren.md §3.4 verbatim; doc comments added.

```go
// Package broker defines Warren's messaging ports: one driver-neutral message
// envelope, a publisher, a subscriber, and a message handler.
//
// Consumers written against these types run identically over Kafka, RabbitMQ,
// NATS, and the in-process broker. A consumer that touches a driver's record
// type loses that property, which is why the raw client is an explicit escape
// hatch and not the default path.
package broker

// Message is the driver-neutral envelope every adapter translates to and from.
type Message struct {
	// ID is the idempotency key. Inbox dedupe is keyed on it, so it must be
	// stable across redeliveries of the same fact.
	ID string
	// Type is the fact's name, such as "user.registered" — the same value a
	// domain.Event reports from EventName.
	Type string
	// Key is the partition or routing key. Whether the port promises any
	// ordering for messages sharing a key is Open question 9.
	Key string
	// Payload is the encoded body. This package does not define its format.
	Payload []byte
	// Headers carries metadata across the broker; trace context propagates
	// here, so a span survives the trip into the consumer.
	Headers map[string]string
	// OccurredAt is when the fact happened, not when it was published.
	OccurredAt time.Time
}

// Publisher sends messages to a topic. The outbox relay is its primary caller;
// use cases publish through the unit of work, not directly.
type Publisher interface {
	Publish(ctx context.Context, topic string, msgs ...Message) error
}

// Subscriber consumes a topic, invoking the handler for each message. A
// subscription is a lifecycle component: it starts after its dependencies are
// ready and drains on shutdown, not a goroutine left running.
type Subscriber interface {
	Subscribe(ctx context.Context, topic string, h MessageHandler) error
}

// MessageHandler processes one message. Returning nil acknowledges it;
// returning an error hands it to the retry and dead-letter middleware, which
// decide by the error's warren/errors code (see warren.md §2.6).
type MessageHandler func(context.Context, Message) error
```

**Per-subscription options.** §3.4 does not list these, but §5.1 uses all three
at a registration site and they are `broker.*`, so they are part of this
package's surface:

```go
r.Events().On("billing.subscription.created", c.activate,
	broker.WithRetry(broker.ExponentialBackoff(5)),
	broker.WithDeadLetter("billing.subscription.created.dlq"),
	broker.WithConcurrency(10),
)
```

warren.md gives the **call forms only** — it names neither the option type nor
`ExponentialBackoff`'s return type. The placeholder names below are marked and
are open question 2; they are not decisions this spec is making.

```go
// WithRetry sets the retry policy for a subscription. It configures the
// Retry(backoff) stage of the consumer middleware chain.
func WithRetry(b Backoff) Option        // ← Option, Backoff: names NOT in warren.md

// ExponentialBackoff returns an exponential retry backoff with the given
// number of attempts.
func ExponentialBackoff(attempts int) Backoff

// WithDeadLetter sets the topic a message is routed to once its retries are
// exhausted. It configures the DeadLetter stage of the chain.
func WithDeadLetter(topic string) Option

// WithConcurrency caps the number of messages a subscription processes
// concurrently. It configures the ConcurrencyLimit stage of the chain.
func WithConcurrency(n int) Option
```

`With` here is the standard Go functional-options prefix on a *function*, which
AGENT.md § Naming explicitly permits; no type is named `With…`.

## Behaviour

### The consumer chain

§3.4 fixes the order:

```
Recover → TraceExtract → Deduplicate(inbox) → Retry(backoff) → DeadLetter → ConcurrencyLimit → Drain
```

Every stage is driver-agnostic (§1.5: "Every box in that chain is driver-agnostic
middleware, which is why swapping Kafka for RabbitMQ changes one line in
`main.go` and nothing else"). Stage by stage, with only what warren.md states:

| Stage | What warren.md says |
|---|---|
| `Recover` | First in the chain (§3.4); the recover box in §1.5's diagram. |
| `TraceExtract` | Reads trace context out of `Message.Headers` (§3.4 field comment, §7.1) so the consumer's span continues the producer's. |
| `Deduplicate(inbox)` | Keyed on `Message.ID`, backed by the §5.6 store (Postgres or Redis) with a TTL. **Enabled by default** — "at-least-once delivery means duplicates are certain, not hypothetical." §1.5: already-seen → ack without invoking the handler. |
| `Retry(backoff)` | On handler error, retry with backoff (§1.5). Configured per subscription by `WithRetry`. |
| `DeadLetter` | "exhausted → DLQ" (§1.5). Topic set by `WithDeadLetter`. |
| `ConcurrencyLimit` | Caps in-flight messages. Set by `WithConcurrency`. |
| `Drain` | Shutdown step 3 (§1.3, §2.3): "consumers stop fetching, in-flight messages ack" — before pools and broker connections close (step 5). |

Note that §1.5's diagram and §3.4's list are not the same shape; §1.5 places
retry and DLQ *after* the handler as outcomes and omits `ConcurrencyLimit` and
`Drain` entirely. See open question 4.

### Error semantics — the Consumer column

The consumer half of the §2.6 table is this package's behaviour, and it is why
the chain can be driver-agnostic: the middleware branches on the
`warren/errors` code, not on the driver's error type.

| Code | Consumer behaviour (§2.6) |
|---|---|
| `INVALID` | → DLQ (never retry) |
| `NOT_FOUND` | ack + log |
| `CONFLICT` | ack (idempotent replay) |
| `UNAUTHENTICATED` | → DLQ (never retry) |
| `PERMISSION_DENIED` | → DLQ (never retry) |
| `UNAVAILABLE` | nack + backoff retry |
| `INTERNAL` | nack + retry, then DLQ |

`nil` acks. §1.5: "ok → ack". A handler that maps a code to an ack decision
itself has broken the ring — that decision belongs to the chain.

### Publishing

"Publishing runs the other way: `UnitOfWork` writes aggregate state and outbox
rows in one transaction, and a separate **relay** — its own lifecycle
participant, leader-elected — drains the outbox to the broker" (§1.5). So the
normal caller of `Publisher.Publish` is the relay (§5.5), not a use case. §10's
handler publishes nothing directly; it raises events on the aggregate and
commits. Kafka's transactional producer exists for exactly this: franz-go "has
transactions — which the outbox relay needs for exactly-once publishing" (§5.1).

`Publish` is variadic in messages, so a relay batch (`outbox.BatchSize(100)`,
§5.5) is one call.

### Lifecycle

Consumers are lifecycle components, not loose goroutines (§1.5). They start at
boot step 6 in dependency order — "pool → repos → consumers → servers" (§1.3) —
and stop at shutdown step 3, before the relay flushes (step 4) and before pools
and broker connections close (step 5). `Subscribe` takes a context, and
cancellation is what drives the drain.

### Ring position

CONTRACTS, core module. Imports KERNEL packages only. No driver, no OTel — trace
context travels as strings in `Headers`, which is precisely how §7.1's claim
survives the core module's stdlib-only rule.

## Errors

This package defines no error values; it defines how *codes* are interpreted.
The table above is the contract, and it is normative for every driver and for
the middleware chain.

Unstated by warren.md, and therefore open (question 5): what code a publish
failure carries; what happens when the DLQ publish itself fails; what code a
decode failure carries before the handler is reached — `CodeInvalid` would send
it straight to the DLQ per §2.6, which is probably right, but warren.md does not
say it; and whether a `MessageHandler` returning a *non-*`warren/errors` error
is treated as `CodeInternal`.

Per AGENT.md, every error message this package specifies gets a golden-file test.

## Testing

**Contract suite — mandatory, and written before the drivers.** AGENT.md: "Every
port change updates the contract suite first, then the drivers." One exported
suite; `broker/kafka`, `broker/rabbitmq`, `broker/nats`, and `broker/memory` all
run it. §5.2 already asserts the property the suite exists to verify —
RabbitMQ is the "same `Publisher`/`Subscriber` implementation" with topic mapped
to exchange plus routing key — and that claim is worth nothing untested.

It must cover:

- **Envelope round-trip.** Every `Message` field survives publish → subscribe
  unchanged: `ID`, `Type`, `Key`, `Payload` byte-for-byte, every header, and
  `OccurredAt`. Headers matter most: losing them silently breaks distributed
  tracing across the broker (§7.1), and nothing else would notice.
- **At-least-once.** Every published message is delivered at least once; the
  suite does not assert exactly-once, because the framework's answer to
  duplicates is inbox dedupe (§5.6), not delivery semantics.
- **The §2.6 consumer table, one case per tabulated code.** A handler returning
  each of the seven codes produces the tabulated ack / nack / retry / DLQ
  outcome. §2.6 now carries rows for `UNAUTHENTICATED` and `PERMISSION_DENIED`
  (both → DLQ, never retry), so all seven are testable.
  This is the single most valuable test in the suite: it is the property that
  makes the middleware driver-agnostic.
- **Chain order.** The stages run in the §3.4 order — asserted with an
  append-only trace, and locked by a golden test, since a reordering that put
  `Deduplicate` after `Retry` would count retries as duplicates.
- **Dedupe.** The same `Message.ID` delivered twice invokes the handler once and
  acks both (§1.5, §5.6).
- **Retry and DLQ.** `WithRetry(ExponentialBackoff(n))` produces n attempts;
  exhaustion routes to the `WithDeadLetter` topic with the envelope intact.
- **Concurrency.** `WithConcurrency(n)` never exceeds n in-flight handler
  invocations.
- **Drain.** Cancelling `Subscribe`'s context stops fetching, lets in-flight
  messages finish and ack, and returns — matching shutdown step 3 before pools
  close at step 5.
- **Recover.** A panicking handler does not kill the subscription.
- **No driver type in any exported signature** — invariant 3, checked as part of
  the suite because it is the swappability claim itself.

**Constraints.** The suite runs in unit mode against `broker/memory`: **no
Docker, no network, no sleeps** (AGENT.md § Testing). Backoff and drain tests
inject a clock rather than sleeping — a suite that sleeps for a backoff is a
suite nobody runs. Kafka, RabbitMQ, and NATS run the same suite behind
`//go:build integration` with testcontainers (§7.5).

**Benchmarks.** Consumer dispatch is a request path (§1.4 puts `Kafka msg`
alongside HTTP and gRPC in the same spine), so allocation benchmarks are
required: one message through the full chain against the memory broker, and
`Publish` of a batch. Invariant 7's "no reflection on the hot path" applies to
consumers exactly as it does to HTTP.

## Definition of done

- [ ] `Message`, `Publisher`, `Subscriber`, `MessageHandler` compile as written.
- [ ] The option constructors exist with agreed type names (open question 2
      answered first).
- [ ] The chain's package home is decided (open question 1) and the stages
      exist there, in the §3.4 order.
- [ ] The contract suite exists, passes against `broker/memory`, and is exported
      **before** any driver is written.
- [ ] The §2.6 consumer table has one passing test per code.
- [ ] `go list -deps` shows stdlib only — no OTel, no driver.
- [ ] Allocation benchmarks committed with numbers.
- [ ] §5.1's consumer registration compiles verbatim as a test.
- [ ] Open questions answered and this spec corrected in the same change.

## Open questions

1. **Where does the middleware chain live, and what is its Go shape?** §3.4
   names seven stages but gives no signatures, and `app.Middleware[Req, Res]`
   does not fit a `MessageHandler`. Is there a
   `broker.Middleware func(MessageHandler) MessageHandler`? Are the stages
   exported from `broker`, or assembled by each driver, or by a runtime package
   warren.md does not list? Invariant 5 makes this sharp: if the stages live in
   `broker` they are implementations in a contract package.

2. **The option types are unnamed.** §5.1 uses `broker.WithRetry`,
   `broker.ExponentialBackoff`, `broker.WithDeadLetter`, `broker.WithConcurrency`
   without naming what they return or take. `Option`/`Backoff` above are
   placeholders. Also: what consumes them — `transport.EventRegistrar.On`
   accepts them (§5.1) but `Subscriber.Subscribe` has no options parameter, so
   either `Subscribe` grows one or the options are applied by the chain around
   it. And are they subscription options only, or does `Publish` take options
   too?

3. **`Deduplicate(inbox)` needs the inbox store, which is not in this package.**
   §5.6's dedupe store is Postgres or Redis — adapter modules. Core cannot
   import them, so there must be a store port. warren.md defines none. Where
   does it live and what is its shape (`Seen(ctx, id) (bool, error)` and a TTL,
   presumably — but that is a guess, not a citation)?

4. **§1.5's diagram and §3.4's chain do not agree.** §1.5 shows
   `recover → trace-extract → inbox dedupe → Handler`, with retry-with-backoff
   and DLQ as *post-handler outcomes*, and shows neither `ConcurrencyLimit` nor
   `Drain`. §3.4 lists all seven as one linear chain with `Retry`, `DeadLetter`,
   `ConcurrencyLimit`, `Drain` positioned *before* the handler. Which is the
   real topology? The chain reading is the one this spec follows, since
   middleware wrapping a handler naturally surrounds it — but the two passages
   should be reconciled in warren.md.

5. **Error codes the chain itself produces.** Publish failure code; DLQ-publish
   failure; decode failure before the handler; and how a plain non-`warren`
   error from a `MessageHandler` is classified. Each picks a row of §2.6 and
   therefore an ack/nack/DLQ outcome.

6. **§5.1's consumer handler is not a `MessageHandler`.**
   `r.Events().On("billing.subscription.created", c.activate, ...)` passes
   `c.activate`, an `*ActivateUserHandler` — i.e. an `app.Handler[Req, Res]`
   (§3.5: "`c.register` is an `app.Handler[RegisterUser, UserDTO]`"). But
   `Subscriber.Subscribe` takes `MessageHandler func(context.Context, Message)
   error`. Something decodes `Message.Payload` into `Req` and adapts one to the
   other, and warren.md never says what, where it lives, or how the codec is
   chosen. This is the same type-erasure gap recorded in `app/SPEC.md` (open
   question 7) and `transport/SPEC.md` (open question 1); it should be answered
   once, for all three.

7. **Does `Message` carry a `Topic`?** `Publish` takes the topic as a parameter
   and `Subscribe` takes it as a parameter, so a message delivered to a handler
   does not know which topic it arrived on. Consumers subscribed to several
   topics through one handler, and DLQ routing that wants the original topic,
   both need it. Deliberate or a gap?

8. **`BillingConsumer` implements `transport.Controller`, not a broker type.**
   §5.1's consumer has method `Register(r transport.Registrar)` — the
   `Controller` interface from §3.5 — yet §2.1 registers consumers through
   `warren.Consumers(...)`, a different module option from
   `warren.Controllers(...)`. If both satisfy `transport.Controller`, what does
   the distinction between the two options actually change? Recorded in
   `transport/SPEC.md` too.
