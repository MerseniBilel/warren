# `warren/broker` — SPEC (the event protocol has no adapter)

| | |
|---|---|
| **Status** | **IMPLEMENTED (working tree, 2026-08-31) — PENDING REVIEW; retires on merge.** Corrected 2026-08-31: this line read "PROPOSED — NOT APPROVED" over code that exists. `broker/memory/module.go:55` claims `transport.ProtocolEvent` and `:71` serves `Table.Events()`, and `broker/consumer/` exists — so the gap this spec describes is closed, in the working tree, uncommitted. Nothing here is a proposal any more; what remains is review. Companion: [`testing/SPEC.md`](../testing/SPEC.md), whose harness fix is downstream of this one and is **still outstanding**. |
| **Source** | Sandbox field report 2026-08-29 (SBX2-002); [warren.md §5](../warren.md), §1.4 |
| **Modules touched** | core (`broker/memory`, `transport`), and `warren/broker/kafka` |
| **Mode** | Build (the adapter contract). No new dependency in either module. |
| **Retires** | Deleted when implemented and reviewed. Residue rehomes to `broker`'s package doc and warren.md §5. |

## Why a spec exists

Two reasons, and the second is the one that matters. **New public API** in two modules (`memory.Module()`, and a kafka counterpart). And **the framework's first claim is currently unfulfilled in the one protocol nobody checked** — that is an architecture question, not a defect fix, and CLAUDE.md puts it in front of the human.

---

## Problem

`warren/transport`'s package doc opens with the framework's first claim:

> *One `Register` call, three protocols: that is the framework's first claim made concrete.*

**Two of the three are served. The third is served by nothing.**

```
$ grep -rn "\.Claim(" --include='*.go' . | grep -v _test | grep -v "func (t \*Table)"
transport/http/server.go:41:  tbl.Claim(transport.ProtocolHTTP, ModuleName)
testing/warrentest.go:182:    tbl.Claim(p, "warren/testing")

$ grep -rn "\.Events()" --include='*.go' . | grep -v _test | grep -v "func (t \*Table)"
   → (nothing)
```

**`Table.Events()` has no consumer in the repository.** Not `broker/kafka`, which ships. Not `broker/memory`. Not the scaffold. `transport.EventRoute` is built at boot step 5, frozen into the table by `Builder.Fill`, and then read by nobody.

The two symptoms the field report found are one cause:

**In production it fails, with a remedy that names a module that does not exist.** `transport/transport.go`'s `Unserved()`:

```go
missing = append(missing, fmt.Sprintf("%d event subscription(s) — add a broker module to warren.New", n))
```

```
$ go doc ./broker/memory | grep -E '^func|^type'
type Broker struct{ ... }
type Option func(*Broker)

$ grep -rn "^func Module(" broker/
   → (nothing)
```

`broker/kafka` exports exactly one module constructor, `Broker(opts ...Option) warren.Module` — and it does not claim `ProtocolEvent` or read `Table.Events()` either. **There is no broker module a user can add that satisfies this diagnostic.** A user who hits it in production cannot act on it. That half is a plain defect with no design content.

**In tests it passes silently**, because `warrentest` claims all three protocols on its own behalf (`testing/warrentest.go:177-187`). The reporter deleted the scaffold's 60-line generated subscription, wrote `r.OnEvent(...)`, and got a green suite with a consumer that never ran. That half is the companion spec's.

### The scaffold is already writing this adapter by hand, once per subscription

`cli/internal/generate/templates/subscription.go.tmpl` generates ~60 lines per event: build a decode closure, call `broker.Pipeline(name, topic, handle, store, pub)`, append a `lifecycle.Hook` whose `OnStart` calls `sub.Subscribe(ctx, topic, pipeline)` with a context that keeps the boot context's values and drops its cancellation. The scaffold's own comment says what it is waiting for:

> *"When the transport adapters ship, this becomes one line in a Register method — `r.OnEvent(topic, h)` — and the pipeline is assembled for you."*

That is this spec. The reporter did exactly what the comment promised, and got silence.

### The adapter is small, because every piece already exists

```
$ go doc ./transport.EventRoute
	Topic   string
	Name    string
	Options []broker.SubscribeOption
	Request reflect.Type
	Bind    func(Codec) broker.MessageHandler

$ go doc ./broker.Pipeline
func Pipeline(subscription, topic string, h MessageHandler, store inbox.Store,
              dlq Publisher, opts ...SubscribeOption) (MessageHandler, func(context.Context) error)
```

`EventRoute.Bind(codec)` returns precisely the `MessageHandler` that `Pipeline` consumes, and `Pipeline` returns the handler to subscribe plus the drain function `OnStop` needs. **The adapter is a loop over `Table.Events()` doing what the template does per subscription.**

---

## The ruling: is `r.OnEvent` premature?

The alternative on the table is to **withdraw `r.OnEvent` from the public `Registrar` until an adapter exists**. It is rejected, on three grounds.

1. **It retracts the headline.** *"One `Register` call, three protocols"* is `transport`'s opening sentence, warren.md §1.4's design, and the reason `EventRoute`, `Consumer`, `ProtocolEvent` and `Table.Events()` exist. Removing the verb does not make the claim honest; it makes it absent.
2. **The port is already proven.** `broker/kafka` ships, `broker/memory` ships, `broker/brokertest` certifies drivers, `broker.Pipeline` assembles the full consumer chain, and `inbox` deduplicates. Nothing here is speculative. What is missing is the twenty lines that read the table.
3. **The gRPC precedent points the other way, and shows the standard.** `Unserved()` already handles a genuinely-unbuilt protocol honestly:

   > *"transport/grpc is deferred to v0.2, so 'add grpc.Server(...)' would name a package that does not exist. Say what is true instead."*

   Events are not in that position — the drivers exist. If the decision were to defer, the *minimum* honest action is to give events the same treatment gRPC got, and that is the fallback below.

**Recommendation: build the adapter.** It is the smallest change that makes an existing claim true.

**Fallback if the human defers it:** change the `Unserved()` string to name reality, exactly as the gRPC branch does — *"warren serves event subscriptions through a broker module that is not built yet; wire the subscription by hand with broker.Pipeline, or drop the r.OnEvent calls"* — and say so in `transport`'s package doc. Shipping a remedy that names a nonexistent module is not an option under either ruling.

---

## Goals

- One place that turns `Table.Events()` into live subscriptions, so `r.OnEvent` works the way `r.Get` does.
- `memory.Module()` — an in-process event runtime usable in `main.go` for local development and by `warrentest`.
- A kafka counterpart, so the same registration serves production.
- Claim `ProtocolEvent`, so `Unserved()` stops being reachable when a broker IS wired, and stays reachable when it is not.
- Fix the `Unserved()` remedy so it names something that exists.
- Preserve the §1.3 orderings exactly: consumers subscribe at `OnStart`, stop fetching before pools close, in-flight messages ack.

## Non-goals

- **No new dependency**, in either module.
- **Not a change to `broker.Publisher`/`Subscriber`/`Pipeline`/`inbox`.** All are shipped and certified; this consumes them.
- **Not a replacement for hand-wired subscriptions.** `subscription.go.tmpl`'s shape stays legal and stays documented — a consumer needing a bespoke pipeline keeps writing one.
- **Not the outbox relay.** Publishing is already wired by `platform`; this is the consuming half.
- **No change to `EventRoute`.** It carries everything needed.

---

## Public API

### 1. `broker/memory` (core module)

```go
// Module serves the event subscriptions registered with r.OnEvent, in
// process. It is the development and test counterpart to a real broker
// adapter: one in-memory broker bound as Publisher and Subscriber, the
// consumer chain of broker.Pipeline around every subscription in the frozen
// route table, and a lifecycle hook that subscribes at OnStart and drains at
// OnStop.
//
// It claims ProtocolEvent, so a service that registers r.OnEvent and adds
// this module boots; one that registers r.OnEvent and adds no broker module
// still fails at boot, which is the point.
//
// It is NOT for production: an in-process broker loses every message when the
// process stops, and nothing is replayed. Use broker/kafka.
func Module(opts ...ModuleOption) warren.Module

// Inbox supplies the deduplication store Pipeline requires. The default is
// inbox.NewMemoryStore().
func Inbox(s inbox.Store) ModuleOption

// DeadLetters routes exhausted messages to p. The default is the module's own
// publisher, so a dead letter is observable in the same process.
func DeadLetters(p broker.Publisher) ModuleOption

// Codec decodes payloads. The default is transport.JSON().
func Codec(c transport.Codec) ModuleOption
```

**Ring check.** `broker/memory` is an ADAPTER (warren.md §1.1) that happens to live in the core module, so importing the contracts ring is correct for it. `scripts/invariants.sh` polices `di lifecycle config log errors validate health` only — `broker/memory` is not in that list, so this does not trip CI, and it should not: the rule exists to stop *kernel* packages knowing what a route is.

### 2. `broker/kafka`

```go
// Consumers serves the event subscriptions registered with r.OnEvent over
// this broker, and claims ProtocolEvent. It is separate from Broker so that a
// service which only PUBLISHES does not acquire a consumer group.
func Consumers(opts ...ConsumerOption) warren.Module
```

**Open question 1** covers whether this should instead be an option on the existing `Broker(...)`.

### 3. `transport` — the diagnostic only

`Unserved()`'s event branch changes from `"add a broker module to warren.New"` to name what exists:

```
%d event subscription(s) — add memory.Module() (development) or
      kafka.Consumers() (production) to warren.New
```

No behaviour change; a string and its golden.

---

## Behaviour — the contract any event adapter must meet

Written here because a second adapter (NATS, RabbitMQ) will copy it, and because the scaffold's hand-written version is the reference implementation.

For each `EventRoute` in `Table.Events()`, at boot:

1. `h := route.Bind(codec)` — once, at boot, never per message.
2. `pipeline, wait := broker.Pipeline(route.Name, route.Topic, h, inboxStore, dlq, route.Options...)`.
   `route.Name` is `"<module>.<handler>"` and is the subscription identity that scopes deduplication — two features consuming one topic must not suppress each other.
3. `OnStart`: `sub.Subscribe(ctx, route.Topic, pipeline)`. **The loop's context keeps the boot context's VALUES and drops its cancellation** — `context.WithCancel(context.WithoutCancel(bootCtx))`. Both halves are load-bearing and the scaffold template already documents why: OnStart's context dies when boot finishes, and it carries `app.Telemetry`, so `context.Background()` severs trace continuation silently.
   **`Subscribe` must not be wrapped in a goroutine** — it returns once the subscription is LIVE, and reporting "started" before it exists discards anything published in the gap, through a `Publish` that reports success.
4. `OnStop`: cancel, then `wait(ctx)`.
5. `tbl.Claim(transport.ProtocolEvent, ModuleName)` at construction, exactly as `transport/http/server.go:41` does for HTTP.

**Ordering.** §1.3 requires readiness to close first, then servers stop accepting, **then consumers stop fetching**. The hook must register so that its `OnStop` runs after the HTTP server's. Note `transport/transport.go`'s own correction — the doc comment that claimed consumers start before servers and stop after them *"was not true, and a reader who wired a subscription expecting that ordering would have got it by luck"* — so this spec fixes the ordering explicitly rather than inheriting an assumption.

**RULED BY THE OWNER, 2026-08-29: add ordering to `lifecycle`. NOT YET IMPLEMENTED.**

The problem was measured, not inferred. `lifecycle` unwinds hooks in reverse
REGISTRATION order, and registration happens when a provider is BUILT — so the
order is a property of module build order and nothing declares it. Observed
today, with a hook registered before the adapters (standing in for the outbox
relay, which is exactly the component that publishes late):

```
publish during shutdown: err=<nil>
messages the consumer received: 0
```

Consumers stop FIRST, and `Publish` reports SUCCESS for a message that will
never be delivered. That is the worst of both: the ordering §1.3 requires is
inverted, and the caller is told nothing.

The ruling is to give `lifecycle.Hook` a declared phase or priority, so an
adapter states where it belongs instead of inheriting build order, and the
framework's own guarantee holds by construction. It touches `lifecycle`
(kernel) and every adapter that registers a hook, so it wants its own change
and its own review.

Two narrower options were considered and rejected as the primary fix: making
the in-process broker refuse a late publish (honest, but leaves the ordering
dependent on build order), and documenting the constraint (cheapest, and the
outbox does exist for exactly this — but it leaves a stated guarantee unmet).

**A duplicate subscription is a boot failure.** Two routes on one `(name, topic)` pair already fail in `Builder.Fill`; nothing new is needed.

---

## Go 1.27: no, it buys nothing here

Assessed rather than assumed:

- **Generic methods** — the adapter declares none. `Module(opts ...ModuleOption)` is a plain constructor and `EventRoute.Bind` is a struct field.
- **`encoding/json/v2`** — the codec is `transport.Codec`, already chosen, and `transport.JSON()`'s leniency is a *deliberate* ruling for exactly this path: `transport.go`'s doc says a strict codec would dead-letter 100% of a consumer's traffic when a producer adds a field. Nothing about v2 changes that, and adopting it would make it worse (v2 rejects duplicate names and invalid UTF-8 by default).
- **`testing/synctest`** — tempting for the subscribe-then-publish race, and **unusable**: a bubbled test may not touch a real network, and `brokertest` certifies real drivers.
- **`goroutineleak` profile** — genuinely useful for the `OnStop` drain, and it is a *testing* technique, not a design input. Noted in Testing.

The one 1.27 fact that constrains the design is already recorded in `transport`'s Standing Constraints: **`reflect` cannot see generic methods**, so nothing here may discover subscriptions reflectively. It reads the frozen table. That is what the design does.

---

## Testing

| What | Where |
|---|---|
| A module registering `r.OnEvent` + `memory.Module()` boots, publishes, and the handler runs | `broker/memory` |
| The same module WITHOUT a broker module fails at boot with the new `Unserved()` text | golden |
| `Subscribe` is live when `OnStart` returns — publish immediately after boot, assert delivery with no sleep | `broker/memory` |
| Deduplication is scoped per subscription — two features on one topic both receive | `broker/memory` |
| An `INVALID` decode dead-letters without retry; `UNAVAILABLE` nacks and retries (§2.6) | `broker/memory` |
| `OnStop` drains in-flight messages before returning | `broker/memory` |
| **No goroutine survives `OnStop`** — `pprof.Lookup("goroutineleak")`, two `runtime.GC()`, assert `total 0` from the written profile (**`Count()` returns 0 for this profile; read the profile text**) | `broker/memory` |
| Ordering: readiness closes, HTTP stops, then consumers stop | root `warren` |
| Kafka serves the same registration | `broker/kafka`, behind the existing integration tag |

---

## Implementation plan

**Stage 1 — the shared adapter loop.** An unexported helper in `broker` that turns `Table.Events()` + a `Subscriber` + an `inbox.Store` + a dlq `Publisher` into hooks. Both adapters call it, so the ordering and context rules exist once.
> `cd . && go test ./broker/...`

**Stage 2 — `memory.Module()`** over stage 1, claiming `ProtocolEvent`.
> `cd . && go test -race ./broker/memory/...`

**Stage 3 — the `Unserved()` string + golden.**
> `cd . && go test ./transport/...`

**Stage 4 — ordering + leak tests**, including the root-level ordering test.
> `cd . && go test -race ./...`

**Stage 5 — `kafka.Consumers()`.**
> `cd broker/kafka && go test ./...`

**Stage 6 — the scaffold.** `subscription.go.tmpl` collapses to an `r.OnEvent` line in `Register`; `platform` gains the broker module. **Do this last** — it is the proof, and it deletes ~60 generated lines per subscription.
> Scaffold, `go test ./...`, and assert the event is delivered over the generated wiring.

**Stage 7 — docs.** warren.md §5 and §1.4; `transport`'s package doc; the `warren-generate` skill.
> `make ci`

## Definition of done

- [ ] `make ci` green across all seven modules.
- [ ] `grep -rn "\.Events()" --include='*.go' . | grep -v _test` returns **at least one production consumer** — the standing proof the third protocol is served.
- [ ] A service with `r.OnEvent` and no broker module still fails at boot; the message names `memory.Module()` and `kafka.Consumers()`, both of which exist.
- [ ] `subscription.go.tmpl` no longer hand-assembles a pipeline.
- [ ] The goroutine-leak assertion passes after `OnStop`.
- [ ] Root `go.mod` gains **no new require**.
- [ ] **This file is deleted.**

## Open questions

1. **`kafka.Consumers()` as its own module, or an option on `Broker(...)`?** Separate is recommended: a publish-only service should not acquire a consumer group, and `kafka.ConsumerGroup(...)` is already a `Broker` option, so the split has to be drawn deliberately rather than inherited. **Human's call — it is public API in an adapter module.**
2. **How is consumer-stop ordering guaranteed relative to the HTTP server's?** §1.3 fixes the order; `lifecycle` runs `OnStop` in reverse dependency order, and this hook has no dependency edge to the HTTP server. Either an explicit ordering seam is needed, or module declaration order is being relied on — and `transport.go` already records that a previous ordering claim here was true "by luck". **This must be settled before stage 4, and it may be the largest hidden cost in the spec.**
3. **Does `memory.Module()` belong in `broker/memory` or in a new `broker/inproc`?** `broker/memory` currently exports a driver (`New`); adding a module makes it both driver and adapter. That is what `postgres.Module` already does, so precedent says yes. Recorded because the naming will be questioned.
4. **What claims `ProtocolEvent` when a user wires subscriptions by hand?** The `subscription.go.tmpl` pattern stays legal, and such a service registers no `r.OnEvent`, so `Unserved()` never fires. Confirm there is no third state.
