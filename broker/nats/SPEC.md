# `warren/broker/nats` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved. **Substantially unspecified — see Open questions.** |
| **Source** | [warren.md §5.3](../../warren.md) |
| **Module** | own module (`warren/broker/nats`) — [warren.md §1.6](../../warren.md) |
| **Mode** | Wrap |
| **Wraps** | `nats-io/nats.go` |

> **Read this first.** warren.md §5.3 is a single line: *"`warren/broker/nats`
> — JetStream. Same ports."* Two more facts exist elsewhere: §1.6 assigns the
> module its own `go.mod` with `nats-io/nats.go`, and §9 records the mode as
> Wrap with the note "JetStream". That is the entire source material. This spec
> states those four facts and puts everything else in Open questions. It does
> not derive a surface by analogy with `broker/kafka`, because an invented API
> reviewed as if it were agreed is exactly the "confident lie" AGENT.md § Spec-
> driven development warns about. This spec is not implementable as written;
> the questions below have to be answered by a human first.

## Problem

Warren's messaging pitch is that consumers are written once against
`warren/broker` (§3.4) and the driver is one block in `main.go` (§5.1). NATS
JetStream is the third driver that has to make that true. Beyond that framing,
warren.md has not yet decided what this package looks like.

## Goals

Everything below traces to §5.3, §1.6, or §9. There is nothing else.

- Implement the `warren/broker` port — `Publisher`, `Subscriber`,
  `MessageHandler` over `Message` (§3.4). §5.3: "Same ports."
- Use **JetStream** as the mode of operation (§5.3, and §9's ledger note).
- Wrap `nats-io/nats.go` (§1.6, §9) — Mode **Wrap**, so users never import it
  and `*nats.Conn` is reachable only through a named escape hatch
  (AGENT.md invariant 3).
- Ship in its own module (§1.6).

## Non-goals

- **Defining the port.** `Message`, `Publisher`, `Subscriber`,
  `MessageHandler`, and the per-subscription options `broker.WithRetry`,
  `broker.WithDeadLetter`, `broker.WithConcurrency` belong to `warren/broker`
  (§3.4, §5.1).
- **Owning the middleware chain.** `Recover` → `TraceExtract` →
  `Deduplicate(inbox)` → `Retry(backoff)` → `DeadLetter` → `ConcurrencyLimit` →
  `Drain` is driver-agnostic and applies "to Kafka, Rabbit, NATS, and memory
  identically" (§3.4). This driver supplies transport only.
- **Core NATS (non-JetStream) delivery.** §5.3 says JetStream; warren.md
  mentions no plain-NATS mode.
- **Importing any other adapter** (§1.6 Rule; AGENT.md invariant 4).

## Dependency audit

**Chosen: `nats-io/nats.go`**, Mode **Wrap**, per §1.6 and §9. §9's entire
recorded justification is the word "JetStream". No alternative was considered
and none was rejected — unlike §5.1, which argues franz-go against three
competitors.

**Outstanding — blocks the `go.mod`.** AGENT.md § "Adding a dependency" requires
a written audit before the dependency lands: repository read, archived check,
last-shipped date, transitive dependency set, licence compatibility, all with
the observation date. warren.md records **none** of these for `nats-io/nats.go`,
and records no argument for choosing it. Run `gh api repos/nats-io/nats.go` and
`gh api repos/nats-io/nats.go/releases/latest`, record the findings here, and
note that the JetStream API in `nats.go` has shipped under more than one
package path — which of them Warren targets is itself an open question below.

**Mode justification.** Wrap follows the same wrap rule as the other two
drivers: changing the NATS client would otherwise force edits across every
consumer and publisher in every user service (AGENT.md § Modes).

## Public API

**warren.md gives none.** No `nats.Broker(...)`, no options, no return type, no
escape-hatch signature. `kafka.Broker(opts...) warren.Module` (§5.1) and
`rabbitmq.Broker(...)` (§5.2) suggest a shape, but a suggestion is not a
decision, and JetStream's stream/consumer/subject model does not map onto
Kafka's brokers-and-groups options anyway. Writing the Go here would be
inventing the public API of a module — AGENT.md § "When you are unsure":
"Module boundaries, port shapes, and public API are the human's call, not yours
to discover."

The only surface this spec commits to is the one it inherits: this package
provides implementations of `broker.Publisher` and `broker.Subscriber` (§3.4)
into the DI graph, and consumers registering through `r.Events().On(...)` with
port-owned options (§5.1) work against it unchanged.

## Behaviour

Only what warren.md fixes for every broker driver:

- **Consumers are lifecycle components, not goroutines someone forgot about**
  (§1.5). Subscriptions register at boot step 5 and start at step 6, in the
  order `pool → repos → consumers → servers` (§1.3).
- **The middleware chain is the port's, not this driver's** (§3.4, §1.5):
  `recover` → `trace-extract` → `inbox dedupe` → (already seen: **ack**; new:
  **Handler**) → (ok: **ack**; error: **retry w/ backoff**; exhausted:
  **DLQ**). This driver converts JetStream messages to `broker.Message`, hands
  them to the chain, and executes the chain's ack/nack/DLQ decision.
- **Trace context propagates in `Message.Headers`** (§3.4, §7.1).
- **Shutdown — normative positions in §2.3**, identical to every other driver:

```
SIGTERM
  1. readiness probe → 503
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack     ← this driver
  4. outbox relay flushes
  5. DB pools, broker connections close                  ← this driver
  6. force-exit deadline (default 30s)
```

  The connection must survive step 3 so that the step-4 outbox flush can still
  publish, and closes only at step 5.

Everything else about this driver's behaviour — stream and consumer
provisioning, subject naming, ack policy, durable vs ephemeral consumers,
at-least-once guarantees, whether streams are created at boot — is unstated in
warren.md. See Open questions.

## Error mapping

The Consumer column of §2.6 binds every consumer driver, so it binds this one:

| Code | Consumer action |
|---|---|
| `INVALID` | → DLQ (never retry) |
| `NOT_FOUND` | ack + log |
| `CONFLICT` | ack (idempotent replay) |
| `UNAUTHENTICATED` | → DLQ (never retry) |
| `PERMISSION_DENIED` | → DLQ (never retry) |
| `UNAVAILABLE` | nack + backoff retry |
| `INTERNAL` | nack + retry, then DLQ |

**How each of these is realised in JetStream is unspecified.** JetStream's
acknowledgement vocabulary is not `ack`/`nack`, and warren.md says nothing about
how `broker.WithDeadLetter("topic")` (§5.1) becomes a JetStream destination.
That mapping is an open question, not something this spec fills in.

Note: §1.4 previously said `Conflict → DLQ`; it has been amended to
`Conflict → ack` and now agrees with §2.6. See Open question 11, resolved.

## Escape hatch

**warren.md names none.** By AGENT.md invariant 3 a Wrap driver must have one —
raw handles are reachable "through named escape hatches only" — and `*nats.Conn`
is the obvious handle, but §5.3 does not name it and this spec will not invent
its signature. Open question.

## Testing

The rules that bind regardless of the undecided surface (AGENT.md § Testing,
§7.5):

- **Unit tests: no Docker, no network, no sleeps.** Message conversion, header
  propagation, and the ack/nack/DLQ decision table are testable against a fake
  transport.
- **Real NATS/JetStream behind `//go:build integration`.**
- **The `warren/broker` contract suite must pass unmodified** — the same suite
  the Kafka and RabbitMQ drivers pass. §5.3's "Same ports" means exactly this
  and is the one testable claim the source makes.
- **Golden-file tests for every error message** this package emits.
- **Drain test:** §2.3 step 3 completes before step 5.

## Definition of done

This package cannot reach a definition of done until the Open questions are
answered, because a definition of done is a checklist against a public API that
does not yet exist. What is fixed:

1. The `warren/broker` contract suite passes unmodified (§5.3 "Same ports").
2. JetStream is the mode of operation (§5.3).
3. Lifecycle hooks at §2.3 steps 3 and 5, ordering proven by a test.
4. Every §2.6 Consumer-column row has a test.
5. No `nats.go` type in any exported signature outside the named escape hatch
   (invariant 3).
6. No import of any other adapter module (invariant 4).
7. Dependency audit completed with an observation date **before** the `go.mod`
   is written.
8. warren.md §5.3 amended to carry the agreed surface, in the same change that
   this spec is approved — AGENT.md: "a spec that contradicts [warren.md] needs
   `warren.md` amended in the same change", and here warren.md is not
   contradicted but empty.

## Open questions

Every one of these is a decision warren.md has not made. The list is long
because §5.3 is one line, and that is the honest state of this package.

1. **What is the module surface?** Is it `nats.Broker(opts...) warren.Module`,
   matching §5.1's shape? Nothing in warren.md says so.
2. **What options exist?** Server URLs, credentials/auth, TLS, stream name,
   durable consumer name, ack wait, max deliver — none are named.
3. **How does a port `topic` map to JetStream?** Subject? Stream plus subject?
   Who creates the stream, and when — at boot, or out of band by an operator?
4. **How is `broker.WithDeadLetter(...)` realised?** JetStream has no
   dead-letter exchange; §5.1's option must land somewhere.
5. **How are `broker.WithRetry` and `broker.WithConcurrency` realised** against
   JetStream's own redelivery and flow-control mechanisms? Two retry mechanisms
   stacked is a real risk and warren.md does not address it.
6. **Which JetStream client API?** `nats.go` has shipped JetStream under more
   than one package path with different ergonomics. The choice affects the
   audit, the pinned version, and the code.
7. **What is the escape hatch?** `*nats.Conn`, a JetStream context, or both —
   and under what name?
8. **Is there a health check?** §2.8 says adapters self-register and names only
   postgres and kafka.
9. **Does the outbox relay's exactly-once story survive here?** §5.1 justifies
   franz-go partly on transactions for exactly-once publishing; §5.5 gives the
   relay no driver-specific behaviour, and JetStream's guarantees differ. What
   the relay promises on NATS is unstated.
10. **Is core NATS (non-JetStream) ever supported?** §5.3 implies not; worth
    confirming, since "same ports" over core NATS would silently weaken
    delivery guarantees the inbox depends on (§5.6).
11. **RESOLVED (2026-08-01):** warren.md §1.4 was amended to `Conflict → ack`,
    in favour of §2.6; the two sections now agree. **§1.4 contradicts §2.6 on
    `CONFLICT`** (DLQ vs ack).
