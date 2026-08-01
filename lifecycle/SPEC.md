# `github.com/MerseniBilel/warren/lifecycle` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — `Hook`/`Lifecycle` and both orderings are binding; conditions: the readiness-gate handle and the force-exit owner (Open questions 1–2) settled before the health and root seams are built |
| **Source** | [warren.md §2.3](../warren.md) |
| **Module** | core |
| **Mode** | Build |
| **Wraps** | — |

## Problem

Hand-rolled Go services get shutdown backwards: they stop the HTTP server first
and close readiness afterwards, so the load balancer keeps routing to a process
that has stopped accepting — which is why rolling deploys drop requests (§2.3,
§1.3). The same services start background work as loose goroutines that nobody
waits on, so consumers and cron jobs neither start after their dependencies are
ready nor drain on the way down.

## Goals

- Own ordered startup and shutdown, readiness gating, and drain (§2.3).
- Start in dependency order — pool → repos → consumers → servers — and stop in
  the exact reverse (§1.3 steps 6 and 10).
- **Close readiness before anything stops**, so the load balancer drains first
  (§1.3 step 9, §2.3).
- Per-hook timeout, and a force-exit deadline (default 30s) that bounds the whole
  shutdown (§1.3 step 10, §2.3).
- Make background work a lifecycle participant, not a goroutine someone forgot
  about: consumers (§1.5), the outbox relay (§1.5, §5.5), cron and workers
  (§7.4) are all hooks.

## Non-goals

- **Not fx.** fx would impose *its* lifecycle, and Warren needs readiness gating
  and drain ordering that fx does not model (§2.2, §9 ledger: "Lifecycle — Build
  — fx rejected: imposes its own lifecycle"). This ordering is the reason the
  package is Build rather than borrowed (AGENT.md § Two orderings).
- **Not transport-aware.** The kernel has no knowledge that HTTP, SQL or Kafka
  exist (§1.1). The named shutdown sequence in §2.3 is what *emerges* from
  reverse ordering when the adapters have registered their hooks — it is not a
  table hard-coded here.
- **Not a health endpoint.** `/healthz` and `/readyz` belong to `warren/health`
  (§2.8); this package owns the lifecycle state that gates readiness.
- **Not the user's API.** Most users never touch this package directly; adapters
  register hooks, and users declare `warren.OnStart` / `warren.OnStop` on a
  module (§2.3).

## Public API

`warren.md` §2.3 fixes the following surface. Doc comments are added here; no
signature is changed.

```go
// Hook is one participant in the application lifecycle. OnStart runs in
// dependency order during boot step 6; OnStop runs in the exact reverse during
// shutdown step 10. Whether either function may be nil — a start-only or
// stop-only hook — is not stated by warren.md; see Open questions.
type Hook struct {
    // Name identifies the hook in logs and in lifecycle errors.
    Name string

    // OnStart runs during startup, in registration order.
    OnStart func(context.Context) error

    // OnStop runs during shutdown, in reverse registration order.
    OnStop func(context.Context) error

    // Timeout bounds this hook.
    Timeout time.Duration
}

// Lifecycle collects hooks at boot and runs them in order on the way up and in
// reverse on the way down.
type Lifecycle interface {
    // Append registers a hook. Registration order is start order.
    Append(Hook)

    // Start runs every hook's OnStart in registration order. It is boot step 6.
    Start(context.Context) error

    // Stop runs every hook's OnStop in reverse registration order, bounded by
    // the force-exit deadline. It is shutdown step 10.
    Stop(context.Context) error
}
```

Users reach this through the root package's module options (§2.3):

```go
warren.NewModule("cache",
    warren.Providers(NewRedisClient),
    warren.OnStart(func(ctx context.Context) error { return cache.Warm(ctx) }),
    warren.OnStop(func(ctx context.Context) error { return cache.Flush(ctx) }),
)
```

## Behaviour

### Startup — boot steps 6 and 7

```
 6  OnStart              dependency order: pool → repos → consumers → servers
 7  readiness opens      health endpoint flips green
 8  serve
```

`Start` runs hooks in registration order. Registration order is dependency order
because hooks are appended as their owning singletons are instantiated in
topological order at step 4. Readiness opens only after every `OnStart` has
returned successfully.

### Shutdown — steps 9 and 10

```
SIGTERM
  1. readiness probe → 503        ← load balancer drains BEFORE anything stops
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack
  4. outbox relay flushes
  5. DB pools, broker connections close
  6. force-exit deadline (default 30s)
```

Step 1 of that list is boot-sequence step 9 and happens **before** `Stop` is
called at all: readiness closes first, then hooks stop. Steps 2–5 are the
consequence of running `OnStop` in reverse registration order once servers,
consumers, relay and pools have registered in that dependency order — this
package does not know what any of them are.

Closing readiness before stopping servers is the ordering most hand-rolled Go
services get backwards, and it may not be rearranged (AGENT.md § Two orderings
you may not rearrange).

The force-exit deadline defaults to 30s. §2.3 lists it as the last item of a
six-item sequence whose first item is readiness closing, so it bounds the whole
shutdown rather than step 10 alone — but warren.md gives it no field, option, or
owner. See Open question 2.
`Hook.Timeout` bounds an individual hook (§1.3 step 10: "reverse order, per-hook
timeout, force-kill deadline").

### Readiness gating

`/readyz` is gated by lifecycle state (§2.8). The state transitions are: not
ready → ready at step 7 → not ready at step 9.

## Errors

`warren.md` does not fix any error text for this package. Recorded here is what
must be produced; **all wording is open** and must be pinned before
implementation.

| Path | Condition | Text |
|---|---|---|
| Start (step 6) | A hook's `OnStart` returns an error | **Open.** Must name the hook and wrap the cause with `%w`. |
| Start (step 6) | A hook's `OnStart` exceeds `Timeout` | **Open.** Must name the hook and the timeout that was exceeded. |
| Stop (step 10) | A hook's `OnStop` returns an error | **Open.** Must name the hook. Whether one failure aborts the rest of the sequence is an open question below. |
| Stop (step 10) | A hook's `OnStop` exceeds `Timeout` | **Open.** Must name the hook and the timeout. |
| Stop (step 10) | The force-exit deadline expires | **Open.** Must name which hooks had not finished. |

Per AGENT.md § Errors, each message must tell the user how to fix the problem,
naming the hook and what it was doing.

## Testing

- **Golden-file test for every error message** above, once the text is agreed.
- **Ordering test — the load-bearing one.** Hooks A, B, C appended in order;
  assert `Start` runs A, B, C and `Stop` runs C, B, A.
- **Readiness-first test.** Assert readiness has already closed before the first
  `OnStop` runs, and that readiness opens only after the last `OnStart` returns.
- **Contract suite for `Lifecycle`.** Any implementation must satisfy: ordered
  start, reverse-ordered stop, readiness transitions at steps 7 and 9, per-hook
  timeout, force-exit deadline.
- **Failure paths.** A failing `OnStart` stops boot before readiness opens; the
  already-started hooks are stopped in reverse.
- **No sleeps.** Timeout and deadline behaviour must be driven without wall-clock
  waits — see Open question 6 on how time is injected.
- **Allocation benchmark.** This package has no request path; readiness state is
  read per probe, not per request.
- Unit tests: no Docker, no network.

## Definition of done

- [ ] Spec approved.
- [ ] Error text agreed for every row in Errors, with golden-file tests.
- [ ] Public API implemented exactly as in Public API above, with doc comments.
- [ ] Start order and reverse stop order proven by test.
- [ ] Readiness closes before the first `OnStop` — proven by test.
- [ ] Per-hook timeout and the 30s force-exit deadline implemented and tested
      without sleeps.
- [ ] Contract suite green.
- [ ] Core module `go.mod` still lists stdlib + `go.uber.org/dig` only.
- [ ] `warren.md` amended in the same change if any signature diverged.

## Open questions

1. **Where does readiness live?** §2.3 says this package owns readiness gating;
   §2.8 says `/readyz` is "gated by lifecycle state" and is served by
   `warren/health`. No API connects the two. What is the exported handle —
   a method on `Lifecycle`, a state value, or something `health` reads?
2. **Who owns the force-exit deadline?** It is stated as "default 30s" but there
   is no field for it on `Hook` and no option on `Lifecycle`. Is it a parameter
   of the root `App`, and is it configurable?
3. **Does `Hook.Timeout` apply to `OnStart` as well as `OnStop`?** Step 10 says
   "per-hook timeout" for shutdown only; the field is on the shared struct.
4. **What is the default `Hook.Timeout` when a hook leaves it zero?** Unstated.
   Is zero "no timeout" or "inherit a default"?
5. **Does `Stop` abort on the first failing hook, or continue and aggregate?**
   Continuing is the usual choice for shutdown, but §2.3 gives
   `Stop(context.Context) error` — a single error — and no aggregation rule.
6. **How is time injected so tests avoid sleeps?** AGENT.md forbids sleeps in
   unit tests; `warren.md` fixes no clock abstraction, and adding one would be a
   public API addition.
7. **If `Start` fails partway, are the already-started hooks stopped?** The
   symmetric behaviour is the obvious one but `warren.md` does not state it.
8. **Is `Lifecycle` resolvable from the container?** Adapters "register hooks"
   (§2.3), which implies they inject a `Lifecycle`, but no provider or accessor
   is specified.
