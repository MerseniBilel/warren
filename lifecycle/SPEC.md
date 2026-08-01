# `github.com/MerseniBilel/warren/lifecycle` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — implemented; the readiness-gate handle (`Ready()`) and the force-exit owner (`New(ForceExitDeadline(d))`) were settled the same day and warren.md §2.3 amended to carry them |
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

    // Ready reports the readiness state warren/health serves: false until
    // Start returns nil, true until Stop begins.
    Ready() bool
}

func New(opts ...Option) Lifecycle
func ForceExitDeadline(d time.Duration) Option // bounds the whole shutdown; default 30s
```

The additions over the original §2.3 surface — `Ready()`, `New`, and the
`ForceExitDeadline` option — were agreed on 2026-08-01 and warren.md §2.3 was
amended in the same change, together with the semantics the surface left
open: a nil `OnStart`/`OnStop` is a start-only or stop-only hook (skipped,
not an error); `Hook.Timeout` bounds each side individually and zero means no
per-hook timeout; `Stop` continues past failures and returns them all joined;
a failing `OnStart` stops the already-started hooks in reverse before
returning.

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

The force-exit deadline defaults to 30s and is owned by this package:
`New(ForceExitDeadline(d))`. §2.3 lists it as the last item of a six-item
sequence whose first item is readiness closing, so it bounds the whole
shutdown rather than step 10 alone — implemented as a deadline on the entire
`Stop` run (Open question 2, resolved).
`Hook.Timeout` bounds an individual hook (§1.3 step 10: "reverse order, per-hook
timeout, force-kill deadline").

### Readiness gating

`/readyz` is gated by lifecycle state (§2.8). The state transitions are: not
ready → ready at step 7 → not ready at step 9.

## Errors

`warren.md` fixes no error text for this package; the wording was agreed on
2026-08-01 and every row is covered by a golden file in
`lifecycle/testdata/`:

| Path | Condition | Text |
|---|---|---|
| Start (step 6) | A hook's `OnStart` returns an error | `lifecycle: hook "cache" failed during OnStart: <cause>` — the cause is wrapped, `errors.Is` reaches it. |
| Start (step 6) | A hook's `OnStart` exceeds `Timeout` | `lifecycle: hook "cache" exceeded its 50ms timeout during OnStart — raise Hook.Timeout or make the hook respect its context` |
| Stop (step 10) | A hook's `OnStop` returns an error | `lifecycle: hook "relay" failed during OnStop: <cause>` — the sequence continues; every failure is returned joined. |
| Stop (step 10) | A hook's `OnStop` exceeds `Timeout` | Same shape as the OnStart timeout, with `OnStop`. |
| Stop (step 10) | The force-exit deadline expires | `lifecycle: force-exit deadline (30s) expired with hooks still stopping: "wedged", "never-reached" — these hooks must respect their context's cancellation` — names every unfinished hook, current first. |

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

- [x] Spec approved.
- [x] Error text agreed for every row in Errors, with golden-file tests —
      `lifecycle/testdata/*.golden`, 2026-08-01.
- [x] Public API implemented exactly as in Public API above, with doc
      comments — `lifecycle/lifecycle.go`.
- [x] Start order and reverse stop order proven by test (A, B, C → C, B, A).
- [x] Readiness closes before the first `OnStop` — proven by test: an
      observing hook reads `Ready()` inside its own `OnStop`; readiness also
      opens only after the last `OnStart` and never opens on a failed boot.
- [x] Per-hook timeout and the force-exit deadline implemented and tested
      with no sleeps: timeouts are context deadlines, test hooks block on
      `ctx.Done()` rather than sleeping, and each hook runs in a goroutine so
      one that ignores its context cannot wedge the sequence past its bounds.
- [x] Contract suite green — the behaviours above, run against `New()`; kept
      internal pending the exported-suite home decision (errors/SPEC.md Open
      question 8).
- [x] Core module `go.mod` still lists stdlib + `go.uber.org/dig` only —
      enforced by `scripts/invariants.sh` in `make ci`.
- [x] `warren.md` amended in the same change: §2.3 gained `Ready()`, `New`,
      `ForceExitDeadline`, and the settled hook semantics.

## Open questions

1. **RESOLVED (2026-08-01) — `Ready() bool` on `Lifecycle` is the handle.**
   `warren/health` reads it per probe (an atomic load, 0 allocs — benchmark
   committed). `Start` flips it true on success; `Stop` flips it false as its
   first action, which is what makes "readiness closes before the first
   OnStop" a property of this package rather than a convention the caller
   must remember. warren.md §2.3 amended.
2. **RESOLVED (2026-08-01) — this package owns it.**
   `New(ForceExitDeadline(d))`, default 30s, bounding the whole of `Stop` —
   every `OnStop` together, matching §2.3's list where it is the last item of
   a sequence that starts at readiness. The root `App` passes its configured
   value through when it constructs the lifecycle; whether that is
   user-configurable is the root package's decision.
3. **RESOLVED (2026-08-01) — yes, both.** The field is on the shared struct;
   a bound that protects shutdown protects boot the same way. Each of
   `OnStart` and `OnStop` gets the full `Timeout` individually.
4. **RESOLVED (2026-08-01) — zero means no per-hook timeout.** `Stop` is
   still bounded by the force-exit deadline, so a zero-timeout hook cannot
   hang shutdown forever; `Start` is bounded by the caller's context.
   Inheriting an invented default would surprise more than it protects.
5. **RESOLVED (2026-08-01) — continue and aggregate.** A failed relay flush
   must not leave the pool open. Every failure is returned in one error via
   `errors.Join`, so the single-`error` signature stands and `errors.Is`
   reaches each cause.
6. **RESOLVED (2026-08-01) — time is injected through context deadlines; no
   clock abstraction.** Per-hook timeouts and the force-exit deadline are
   `context.WithTimeout`; test hooks block on `ctx.Done()` instead of
   sleeping, so tests are driven by deadline delivery, not wall-clock
   synchronization. Adding a clock type would be a public API addition with
   one consumer — not worth it.
7. **RESOLVED (2026-08-01) — yes.** A failing `OnStart` stops the
   already-started hooks in reverse before `Start` returns; the returned
   error carries the boot failure first, then any rollback failures, joined.
   Readiness never opens on that path. Proven by test.
8. **Is `Lifecycle` resolvable from the container?** Adapters "register
   hooks" (§2.3), which implies they inject a `Lifecycle`, but no provider or
   accessor is specified. **Deferred to the root package's spec:** the
   bootstrapper constructs the lifecycle and provides it into the root
   container; nothing in this package changes either way.
