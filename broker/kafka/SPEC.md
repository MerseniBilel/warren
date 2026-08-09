# `warren/broker/kafka` — SPEC

| | |
|---|---|
| **Status** | **APPROVED (2026-08-02)** — architect round complete; the dependency audit is done and every open question ruled. Two corrections carry weight: **the exactly-once claim is dropped** (warren.md §5.5 had already refuted §5.1 and this spec inherited the contradiction), and `SASL(sasl.Mechanism)` becomes a Warren-owned type plus a named carve-out. **Zero changes required to core `broker` or `outbox`.** |
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
- **NOT** Kafka transactions, and **not** exactly-once — see the ruling below.
  The producer is idempotent, which removes duplicates caused by producer
  retries within a session; duplicates are prevented downstream by the inbox
  dedupe the consumer chain applies by default.
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

## Rulings — architect round, 2026-08-02

### The dependency audit, done

| Package | Version | Licence | Stars | Last push | Archived | Modules compiled in |
|---|---|---|---|---|---|---|
| `twmb/franz-go` | v1.21.5 | BSD-3-Clause | 2 971 | 2026-07-31 | no | **4** |

Measured with `go list -deps` on a program importing `kgo` and `sasl/scram`:
`klauspost/compress`, `pierrec/lz4`, `twmb/franz-go`, `golang.org/x/crypto`
(the last is SCRAM's). For scale in this repository: core 1,
`transport/http` 0, `persistence/postgres` 6, `observability` 24. This is the
second-smallest adapter footprint in the framework.

Alternatives, same date: `IBM/sarama` v1.60.1 (MIT, 12 499); `segmentio/kafka-go`
v0.4.51 (MIT, 8 597, last push 2026-04 — the only one going quiet);
`confluent-kafka-go` v2.15.0 (Apache-2.0, 5 153) is cgo over librdkafka, which
costs cross-compilation and a C toolchain in every build image. franz-go stands
on being pure Go, covering Kafka 0.8.0–4.2+, and tracking every client KIP.

`franz-go/pkg/kfake` is a SEPARATE module and enters the go.mod as a
**test-only** dependency; the 4 above are the non-test build and must stay so.

### THE EXACTLY-ONCE CLAIM IS DROPPED, and `Transactional` is deleted

warren.md §5.1 says franz-go was chosen partly for "transactions — which the
outbox relay needs for exactly-once publishing." **warren.md §5.5 already
refutes that**, and this spec inherited the contradiction without noticing:

> *Kafka transactions do not make this exactly-once. The outbox ack is in
> Postgres and the publish is in Kafka — two systems.*

`outbox.Relay.DrainOnce` confirms it mechanically: it calls `Publish(...)` and
then `MarkPublished(...)` — two systems, two calls, and a crash between them
republishes. A Kafka transaction around the produce closes nothing, because the
gap is the *Postgres* write and no Kafka primitive spans it.

What transactions would actually buy is atomic multi-partition produce (the
relay does not need it: `disposition()` already gives every record its own
verdict) and read-process-write EOS entirely inside Kafka (unreachable by
construction, because Warren consumers write to Postgres). What they would cost
is a `transactional.id` that must be unique and stable per instance — a
deployment concern the framework would have to invent an answer for — zombie
fencing, hung-transaction timeouts that block `read_committed` consumers on
unrelated topics, and produce latency.

**Rulings:** `Transactional(bool)` is not implemented. The port does not grow a
transactional publish — that would be a Kafka-only concept every other driver
must fake, and it still would not buy exactly-once. The relay does not
special-case Kafka (invariant 4). What ships instead is the **idempotent
producer, on by default**, documented as exactly what it is: no duplicates from
*producer retries within a session*. warren.md §5.1 is amended in the same
change.

**One hard requirement this puts on the driver.** `Publish` must return errors
carrying the right `warren/errors` code, because `outbox.disposition` switches
on it: dial failure, no leader and `NOT_ENOUGH_REPLICAS` are `Unavailable`
(left for the next drain, never parked); `RECORD_TOO_LARGE`,
`UNKNOWN_TOPIC_OR_PARTITION` with auto-create off and `INVALID_RECORD` are
`Invalid` (parked immediately). Get this backwards and **a broker outage parks
the entire outbox.** It gets its own table-driven test against `kerr` values,
which needs no network.

### SASL: a Warren-owned type, plus one named carve-out

Invariant 3 is not narrower than it reads, and `persistence/postgres` set the
precedent — named carve-outs, documented as such, not "driver types are fine
inside the driver's own options".

```go
func SASL(m Mechanism) Option           // Warren-owned
func Plain(user, pass string) Mechanism
func SCRAM256(user, pass string) Mechanism
func SCRAM512(user, pass string) Mechanism
func RawSASL(m sasl.Mechanism) Option   // the carve-out
```

Those three cover essentially all managed Kafka. OAUTHBEARER, AWS MSK IAM and
GSSAPI need a token callback or a keytab; they go through `RawSASL` in v0.1 and
modelling them is a v0.2 decision, not a v0.1 blocker.

### The escape hatches: `Raw` and `Configure`, and NOT the container

`*kgo.Client` is **not** provided into the container. A driver type any
constructor can inject is not an escape hatch, it is a second default path —
and a consumer reaching `kgo.Record` has left the driver-agnostic chain behind,
which is the entire thing this package exists to prevent.

`Configure(...kgo.Opt)` runs before the client is built, because hooks
(`kgo.WithHooks`, how `observability` would instrument this) and a custom
partitioner are read at construction. `Raw(func(ctx, *kgo.Client) error)` runs
in `OnStart` after the metadata fetch. Same split, same reasons, as
`postgres.Configure`/`postgres.Raw`.

### The consumer loop: one client, in-process fan-out, mark-the-prefix

franz-go assigns consume topics **client-wide**, not per `Subscribe`. So: one
shared consumer client, `AddConsumeTopics` per subscription, one poll loop
demultiplexing on `record.Topic`. A client per `Subscribe` would put N members
of one group in a single process, splitting partitions against itself.

**The load-bearing consequence, which the draft did not mention: two
`Subscribe` calls on the same topic must fan out IN-PROCESS.** Two real group
members would *split* partitions rather than duplicate, and
`brokertest`'s fan-out test would fail. This is safe only because
`Deduplicate` scopes its key by subscription name — the sibling handler that
already succeeded sees its own key marked and acks without re-running. The
design anticipated it; the driver must pass `EventRoute.Name` as the
subscription, not the topic.

**Marking is prefix-only.** A Kafka offset is a high-water mark, so marking
record k+1 commits past a failed record k. A nack therefore **seeks the
partition back** to the failed offset and discards the buffered remainder.
Head-of-line blocking on that partition is not a bug — it is what per-key
ordering costs, and `Retry` has already exhausted the policy before the driver
sees the error. `BlockRebalanceOnPoll` plus `AllowRebalance` after dispositions
means a partition is never revoked mid-flight.

**Offset commit strategy is a fixed invariant, not an option.** Auto-commit by
time would advance offsets past messages the chain has not disposed of,
silently turning at-least-once into at-most-once. `CommitInterval` is the only
knob and it is a duplicate-window knob, never a correctness one.

### The lifecycle: two hooks, ordered by a dependency edge

`lifecycle.Hook` has no phase field — ordering is append order with
reverse-order teardown. §2.3 needs consumers to stop at step 3 and connections
to close at step 5, so:

- the **client** constructor appends its hook first, so it stops LAST (step 5);
- the **subscription runner** depends on the client, so it is constructed
  second, appended second, and stops FIRST (step 3): cancel the run context,
  wait on every `Pipeline` drain, then commit marked offsets.

The client is deliberately not closed at step 3 — the outbox relay's step-4
flush still publishes through it. The runner's `OnStart` must not capture the
boot context, which `outbox` documents as a trap.

### Open questions 2, 5, 6, 7 — closed

**2.** `Option` is `struct{ apply func(*config) }`, matching
`persistence/postgres`: unforgeable outside the package, and room for
validation later.
**5.** Partition assignment is `PartitionAssignment(Balancer)`, cooperative by
default — a stop-the-world rebalance fights the drain. Documented hazard:
cooperative and eager balancers cannot coexist in one group, so changing it
needs a full group restart, not a rolling deploy.
**6.** No, `Transactional` is not needed for the DLQ. `DeadLetter` already
publishes and acks only on success, nacking on failure because silent loss is
the forbidden outcome. The residual failure — DLQ publish succeeds, commit
fails, duplicate DLQ row — is at-least-once, which is what the rest of the
system is.
**7. STALE.** `broker.ExponentialBackoff` already exists in
`broker/middleware.go` and is `Pipeline`'s default. The §5.1 snippet is valid
Go today.

### Testing

Unit-testable with no Docker, no network and no sleeps: `Message` ↔
`kgo.Record` both directions (headers, key, timestamp, nil-versus-empty
payload), option assembly asserted on the unexported config rather than on
opaque `kgo.Opt` values, **the `kerr` → `errors.Code` table** (load-bearing for
the outbox, per the ruling above), **the mark/seek decision extracted as a pure
function** — that is where offset bugs live, so it stays out of the I/O — and a
golden file per diagnostic.

Behind `//go:build integration`: `brokertest.Run` entire, group rebalancing,
drain-before-close ordering, and DLQ production. The suite reads
`WARREN_TEST_KAFKA_BROKERS` and skips with the command that produces one.
`brokertest`'s 10s readiness and 5s await budgets may prove tight for a cold
group join; if so they are raised in the SUITE first and the drivers follow —
never weakened in a driver to fit.

## Open questions — ALL CLOSED by the rulings above, kept for the audit trail

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
