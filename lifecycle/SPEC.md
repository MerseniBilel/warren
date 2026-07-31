# Spec — `warren/lifecycle`

**Status:** approved 2026-07-31 — all eleven decisions in §11 agreed. §11.4 changes
`docs/architecture.md §3`. §11.11 surfaced while writing §5.3, was first
recommended as detach-only, and was corrected to Kratos's detach-and-bound after
reading its source properly
**Prior art audited:** 2026-07-31 — `uber-go/fx` , `go-kratos/kratos`,
`zeromicro/go-zero`, and .NET's Generic Host; findings in §9.1
**Roadmap:** v0.1 · item 4 of 11
**Import path:** `github.com/MerseniBilel/warren/lifecycle`
**Depends on:** `context`, `sync`, `sync/atomic`, `runtime`, `time` · and
`warren/errors`, `warren/log`

---

## 1. Problem

A service holds things that must be opened and closed in an order: a connection
pool, a broker client, an HTTP listener, a scheduler. Getting the order wrong is
not a tidiness problem — it is a data-loss problem.

`docs/architecture.md §5` states the requirement and the failure it prevents:

> Start is ordered; stop is the reverse. The database pool starts before the
> broker, which starts before the HTTP listener. On shutdown, readiness flips to
> failing first — so a load balancer stops routing before the listener closes —
> then new work is refused, in-flight work drains, and `OnStop` hooks run in
> reverse order. **Get that order wrong and consumers fail at commit because the
> pool closed underneath them.**

Three failures this package exists to prevent:

- **`defer pool.Close()` in `main`.** It runs in the right order by accident, and
  it runs *before* the HTTP server has drained, because `defer` knows nothing
  about in-flight requests. The consumer that was mid-commit gets a closed pool.
  It also does not run at all on `SIGTERM`, which is how a process actually ends.
- **A start or a shutdown with no deadline.** A hook that never returns hangs the
  process, the orchestrator's grace period expires, and `SIGKILL` arrives
  mid-write. Every framework audited in §9.1 ships a deadline on both phases.
- **A listener that opens before its dependencies are ready.** The first request
  arrives while the pool is still dialling and gets a 500 that looks like a bug in
  the handler.

`docs/architecture.md §3` is explicit that this is why `fx` is not used: "Ordered
start, reverse stop, drain-before-close and readiness gating are product features,
not plumbing. This is why `uber-go/fx` is not used: it owns the lifecycle we need
to own."

## 2. Goals

1. **Start in dependency order, stop in exactly the reverse.** Not
   approximately — the same sequence, walked backwards.
2. **A failed start unwinds itself.** If the fourth hook fails, the three that
   started are stopped before the error is returned. A half-started process never
   reaches serving.
3. **Both phases have a deadline, and report what missed it.** The budget comes
   from the caller's context, and a hook that overruns it is named.
4. **Readiness flips before anything closes**, so a load balancer stops routing
   before the listener stops accepting.
5. **Every hook is observable.** One log line per hook, with its duration, because
   an operator watching a thirty-second boot needs to know which component is slow.
6. **Testable without signals, sleeps, or sockets.** Hooks are functions; the tests
   are channels and cancelled contexts.
7. Standard library only, permanently.

## 3. Non-goals

- **Signal handling.** `SIGTERM` belongs to `warren.Run`, which owns the process.
  A package that installs a signal handler cannot be tested twice in one binary,
  and cannot be embedded in something that already has one.
- **Deciding the grace period.** It arrives as a context deadline, for start and
  for stop alike. Reading it from configuration is `warren`'s job, and `config` is
  not built yet. §9.1 records what the others default to; picking Warren's numbers
  is `warren/SPEC.md`'s decision, not this one.
- **Forcing the process to exit.** go-zero calls `syscall.Kill` on itself after a
  fixed budget (`core/proc/shutdown.go:96`). That is the orchestrator's job, and a
  library that kills its own process cannot be embedded in anything.
- **Health endpoints.** Two HTTP handlers over the state this package publishes.
  `health` owns them — see §11.4, which settles which of the two owns the *state*.
- **Restart, or supervision.** A stopped lifecycle stays stopped, and §6.11 says so
  out loud. A process that wants to come back up is a new process, which is what an
  orchestrator is for.
- **Parallel start.** See §11.5. It would make boot faster and the ordering
  guarantee false.
- **Dependency ordering of its own.** `di/SPEC.md §7` settles this: hooks arrive in
  dependency order because constructors run in dependency order, so this package
  records the order it was given and never computes one. Recomputing it would be
  the same job done twice, differently.

## 4. Public API

```go
// Package lifecycle starts a service's components in order and stops them in
// reverse, with a deadline and a readiness state.
package lifecycle

// Hooks is the ordered set of start and stop callbacks a service has registered,
// and the state machine that runs them. Its zero value is not usable; construct
// one with New.
//
// Registration is not safe for concurrent use: hooks are appended from
// constructors, which run on one goroutine during di.Build. State is safe to read
// from any goroutine at any time, because a health endpoint does exactly that
// while Stop is running. Start and Stop are serialised against each other, so a
// SIGTERM that arrives mid-boot is defined behaviour rather than a race — see
// §5.6.
type Hooks struct{ /* unexported */ }

// New returns an empty Hooks.
func New(opts ...Option) *Hooks

// Option configures a Hooks at construction. There is one today and it is
// deliberately not a general extension point, for the reason di.Option is not.
type Option func(*Hooks)

// UnwindBudget bounds the rollback Start runs when a hook fails or the start
// deadline expires.
//
// It exists because that rollback cannot use the context it failed with: a
// start that ran out of time would hand every OnStop a dead context and close
// nothing. The rollback therefore runs detached, and this is what stops a hook
// that hangs during it from hanging the boot. warren supplies the number, as it
// does for the start and stop deadlines; unset, the rollback is unbounded.
//
//	lifecycle.New(lifecycle.UnwindBudget(cfg.ShutdownGrace))
func UnwindBudget(d time.Duration) Option

// Hook is one component's participation in the lifecycle.
//
// Name identifies it in every message this package produces, so it is required
// and should read as the thing being started: "postgres.pool", "http.server".
// Either callback may be nil; both being nil is an error.
type Hook struct {
	OnStart func(context.Context) error
	OnStop  func(context.Context) error
	Name    string
}

// Append registers a hook. It is called from a constructor, which is what makes
// the order correct:
//
//	func NewPool(hooks *lifecycle.Hooks, cfg *Config) (*Pool, error) {
//		pool, err := open(cfg.DSN)
//		if err != nil {
//			return nil, err
//		}
//
//		hooks.Append(lifecycle.Hook{
//			Name:    "postgres.pool",
//			OnStart: pool.Ping,
//			OnStop:  func(context.Context) error { pool.Close(); return nil },
//		})
//
//		return pool, nil
//	}
//
// Append returns nothing, for the reason di.Provide does: a malformed hook is
// recorded against its call site and reported by Start, so a constructor's happy
// path stays readable. See §5.2.
func (h *Hooks) Append(hook Hook)

// Close is Append for a component with nothing to start and a closer that cannot
// fail — a pgx pool, an in-memory broker.
//
//	hooks.Append(lifecycle.Close("postgres.pool", pool.Close))
func Close(name string, close func()) Hook

// Closer is Close for the io.Closer shape, which is what most of the standard
// library returns — sql.DB, os.File, net.Listener.
//
//	hooks.Append(lifecycle.Closer("postgres.pool", db.Close))
func Closer(name string, close func() error) Hook

// Start runs every OnStart in registration order, which is dependency order.
//
// On the first failure it stops what it already started, in reverse, and returns
// the failure — so a service either reaches Ready or reaches Stopped, never
// something in between.
//
// Any deadline on ctx is the budget for the whole sequence, exactly as it is for
// Stop: a hook still running when it expires is abandoned and named. ctx is
// otherwise the run context, not the boot context di.Build was given. A hook that
// wants to outlive Start — a listener's accept loop — starts a goroutine and
// registers its shutdown as OnStop; it must not retain ctx.
func (h *Hooks) Start(ctx context.Context) error

// Stop flips readiness to failing, then runs every OnStop in reverse order.
//
// The deadline on ctx is the grace period for the whole sequence. Hooks that have
// not finished when it expires are abandoned rather than waited for, and named in
// the returned error: the process is exiting, and a goroutine that outlives it by
// microseconds costs nothing next to a SIGKILL mid-write.
//
// Stop on a lifecycle that never started, or that already stopped, is a no-op
// returning nil — so `defer hooks.Stop(ctx)` is always safe.
func (h *Hooks) Stop(ctx context.Context) error

// State reports where the lifecycle is. It is safe to call from any goroutine.
func (h *Hooks) State() State

// State is a point in the lifecycle. Its zero value is StateNew, so a Hooks that
// has not started reads as not ready.
type State uint8

const (
	StateNew      State = iota // constructed, never started
	StateStarting              // OnStart hooks are running
	StateReady                 // every OnStart succeeded; this is the only ready state
	StateStopping              // OnStop hooks are running
	StateStopped               // fully stopped, terminal for this process
)

// String returns the state's name, for example "Ready".
func (s State) String() string

// Names returns the registered hook names, in start order. It is what
// warren doctor and warren graph read, and what a test asserts ordering on.
func (h *Hooks) Names() []string
```

That is the whole surface: one type, one struct, two helpers, three verbs, and two
readers.

**On `StateNew` being separate from `StateStopped`.** Four states would conflate
"never started" with "fully stopped", and then `Start` cannot tell a first boot
from a restart — §6.11 becomes undetectable, and `Stop`'s no-op promise needs a
hidden flag to implement. `fx` reached five states from the same pressure
(`stopped, starting, incompleteStart, started, stopping`), though for a different
reason: see §9.1 and §11.8.

## 5. Behaviour

### 5.1 Order is recorded, never computed

`Append` appends. `Start` walks forwards, `Stop` walks backwards. There is no
sorting, no graph, and no dependency declaration on `Hook` — because
`di/SPEC.md §7` already guarantees the arrival order is dependency order.

The consequence, stated because it is the load-bearing assumption of the whole
package: **a hook registered outside a constructor has no ordering guarantee.** A
module that appends hooks from a `Register` method gets module order, which is
not dependency order. §11.2 is the decision to accept that rather than defend
against it.

### 5.2 Malformed hooks are deferred, not returned

`Append` returns nothing. A hook with no name, or with neither callback, is
recorded against its call site — captured with `runtime.Caller`, as `di` does and
as `fx` does (§9.1) — and reported by `Start`. Errors are joined, so four bad hooks
report four times.

`Append` after `Start` cannot happen through the intended path: constructors run
during `di.Build`, which `warren` completes before calling `Start`. It is recorded
anyway and reported by `Stop`, which is the next place a caller would look. That
path is unreachable by design and defined by choice.

### 5.3 Start, the unwind, and the unwind's context

```
StateNew → StateStarting → run OnStart forwards → StateReady
                         ↘ on failure: stop what started, reverse → StateStopped
```

`ctx.Err()` is checked before each hook, so a cancelled or expired boot stops
walking rather than pressing on into components it has no budget to open. `fx`
does the same, with the same comment (`internal/lifecycle/lifecycle.go:206`).

The unwind runs the `OnStop` of every hook whose `OnStart` **succeeded**, in
reverse, and never the failed hook's own `OnStop`: a component that could not
start was not holding anything to release. An error from the unwind is joined to
the original failure, with the original first — the start failure is the cause,
and the unwind failures are consequences. `fx` joins the same way round
(`multierr.Append(err, stopErr)`, `app.go:695`); .NET's Generic Host does not
unwind at all and leaves started services running, which is the wart §9.1 records.

**The unwind runs detached, and bounded** — §11.11:

```go
unwind := context.WithoutCancel(ctx)
if h.unwindBudget > 0 {
	unwind, cancel = context.WithTimeout(unwind, h.unwindBudget)
}
```

A start that failed *because* its deadline expired must still be able to close
what it opened; inheriting the dead context would hand every `OnStop` a context
that is already over and close nothing. The budget then stops a hook that hangs
during the rollback from hanging the boot, and an exhausted one reports §6.3
joined after the start failure — which is the shape §6.2 already shows.

This is Kratos's construction exactly (`app.go:105-111`): detach, then bound if a
budget was configured. §9.1 records that `fx` does neither, and that it is a
filed bug against `fx` rather than a reading of ours.

This is deliberately unlike `di.Build`, which leaves what it constructed
(`di/SPEC.md §5.4`). The difference is that `Build` has opened nothing that needs
an orderly close, whereas `Start` has.

### 5.4 Stop, the drain, and how a hook is abandoned

```
StateReady → StateStopping → run OnStop in reverse → StateStopped
```

**Readiness flips first**, before any hook runs, which is the load-balancer
requirement in `docs/architecture.md §5`. Kratos does the same thing by a
different route — it deregisters from the service registry before cancelling the
app context (`app.go:163`).

There is no separate "refuse new work" or "drain" phase, and that is the design
rather than an omission: **reverse order already is the drain guarantee.** The
HTTP listener started last, so its `OnStop` runs first, and a graceful
`http.Server.Shutdown` inside it is what refuses new connections and waits for
in-flight requests. By the time the pool's `OnStop` runs, the requests that needed
the pool have finished. A phase this package owned would duplicate what the
transport's own shutdown already does better.

Each hook receives the same `ctx`, so they share one budget rather than each
getting a fresh one, and `ctx.Err()` is checked before each one.

**The machinery, which the draft of this spec asserted without describing.** A
hook is `func(context.Context) error` and is therefore synchronous: to stop
waiting on one, the walk has to run somewhere else. So `Start` and `Stop` share a
single private phase runner:

- The whole phase runs in **one goroutine** — not one per hook. `fx` does the same
  (`app.go:787`), and one goroutine per hook buys nothing: the walk is sequential
  either way.
- The result channel is **buffered, size 1**. An abandoned hook that finishes later
  still has somewhere to send; unbuffered, it blocks forever on the send and a
  brief leak becomes a permanent one.
- The hook a phase was on is **derived from the counters**, not recorded in a
  field of its own: `started` counts only the hooks that succeeded, so it is the
  index of the one that did not, and the reverse walk is at `started-1-stopped`.
  `fx` keeps a separate `runningHook` field for this (`RunningHookCaller()`,
  `lifecycle.go:347`); a field is wrong for us, because an already-expired context
  is answered by `run` before the phase goroutine has recorded anything, leaving
  the field empty in exactly the common case. The counters are already correct
  there. This was found by reading the generated golden files, which named no
  hooks at all.
- The runner `select`s on the phase result against `ctx.Done()`. **When both are
  ready, the context error wins.** `fx` calls this out explicitly — *"This
  eliminates non-determinism in select-case selection"* (`app.go:806`) — and for
  us it is not a nicety: without it the golden file for §6.3 flakes.
- A hook that calls `runtime.Goexit()` — which is what `t.Fatal` inside a hook
  does — never returns *and never sends*. A deferred sentinel in the phase
  goroutine reports it, so the phase fails immediately instead of blocking until
  the deadline and then blaming the wrong hook. Taken from `fx`'s
  `errHookCallbackExited` (`app.go:783`).

A hook still running when the deadline passes is abandoned: the runner stops
waiting on it, does not run the remaining hooks, and returns an error naming
everything that did not finish. Abandoning leaks a goroutine into a process that
is about to exit. **The state is `StateStopped` either way** — a shutdown that
timed out is still a shutdown, and leaving the state at `StateStopping` would tell
a health endpoint the process is still working on it.

Unlike `Start`, `Stop` does not abort on the first hook that *fails*: it runs every
remaining hook and joins. `fx` — *"For best-effort cleanup, keep going after
errors"* — and .NET — `abortOnFirstException: false` — agree. Failing and
overrunning are different: one component refusing to close must not strand the
rest open, but there is no budget left to run the rest after a deadline expires.

**One error is returned as itself; several are joined.** This is the rule
`di/SPEC.md §5.2` discovered in implementation and `di/validate.go:21` records:
`errors.Join` holds several unrelated errors, so `errors.Detail` reports no fields
and no fix for a join of one — and the fields are where the hook name, the call
site, and the fix live. Joining a lone failure would silently empty §6.4.

### 5.5 A hook that panics is recovered and named

`recover` runs inside the phase goroutine, per hook, and converts the panic into
an ordinary error naming the hook — §6.9. A panic in `OnStart` is a start failure,
so the unwind runs; a panic in `OnStop` is a stop failure, so the remaining hooks
still run. §11.10 is the argument, and `di/SPEC.md §11.5` is the same decision
already taken for constructors.

`AGENT.md` forbids `panic` in library code; it says nothing about recovering from
a user's, and a raw panic with a stack full of runtime frames tells an operator
nothing about *which component* did it.

### 5.6 State is atomic, and Start and Stop are serialised

`State()` is read by a health endpoint from a request goroutine while `Stop` runs
on another. It is stored in a `sync/atomic` value, so no lock is taken on the read
path and a reader never blocks a shutdown.

`StateReady` is the only state that means ready. `StateNew` and `StateStarting` are
not — the listener is not open yet — and `StateStopping` is not, which is the whole
point of flipping it first.

**`Stop` while `Start` is running is defined, not racy.** A `SIGTERM` during a slow
boot is ordinary, not exotic: `fx` permits it explicitly (its `Stop` accepts the
`starting` state, `lifecycle.go:268`). One mutex serialises the two, so `Stop`
queues behind `Start` rather than interleaving with it; `Start`'s own context is
cancelled first, so `Stop` waits for the hook in flight and not for the whole boot.
By the time `Stop` proceeds, `Start` has already unwound what it opened, and `Stop`
finds `StateStopped` and returns nil.

### 5.7 Every hook is logged

`log.From(ctx)` yields the logger, so no logger parameter and no constructor
argument: `Start` and `Stop` already take a context, and `warren/log` already
carries the logger on it. One `Debug` line as each hook begins and one as it
returns, carrying the hook name, the phase, the duration, and `log.Err(err)` when
it failed.

Every framework audited logs per hook, and `fx` goes further — it keeps each
hook's runtime and reports the slowest when a phase times out. The duration on
each line gives an operator the same answer without the bookkeeping.

### 5.8 Cost

No budget, and no benchmark. `Start` and `Stop` each run once per process, and
their cost is the hooks' own. `State()` is an atomic load. Stating this is cheaper
than a benchmark nobody will read.

## 6. Every error message this package emits

Each carries `Op("lifecycle.Start")` or `Op("lifecycle.Stop")`, and a `Fix`.
Rendered here through `errors.Detail`. Every one is a golden file: twelve of them.

### 6.1 A hook failed to start

```
lifecycle.Start: starting postgres.pool failed: dial tcp 127.0.0.1:5432: connection refused

  hook        postgres.pool
  registered  internal/platform/module.go:22
  started     2 of 5 hooks, rolled back

  fix: read the cause above: the hook reported it, not the lifecycle
```

`errors.Internal`, wrapping the hook's own error so `errors.Is` reaches it. The
`started` field is what tells a reader whether the failure was early or late,
which is the first thing they want to know.

### 6.2 A hook failed to start, and the unwind failed too

```
lifecycle.Start: starting kafka.consumer failed: no brokers configured
lifecycle.Stop: stopping postgres.pool failed: context deadline exceeded
```

An `errors.Join` of the cause and each unwind failure, cause first. `Detail`
renders a joined error as its members' messages only, so `warren` prints each
branch separately — the same constraint `di/SPEC.md §5.2` names.

### 6.3 The grace period expired

```
lifecycle.Stop: the grace period expired with 2 hooks unfinished

  unfinished  http.server, postgres.pool
  stopped     3 of 5 hooks

  fix: raise the shutdown grace period, or find what is not draining — a hook that
       never returns is usually waiting on a connection nobody closed
```

`errors.Internal`. `unfinished` names the hook that overran *and* the ones never
reached, because both are things the operator has to explain.

### 6.4 A hook failed to stop

```
lifecycle.Stop: stopping postgres.pool failed: pool is already closed

  hook        postgres.pool
  registered  internal/platform/module.go:22
  stopped     4 of 5 hooks, continuing

  fix: read the cause above. Stop continues through a failure by design: one
       component refusing to close must not strand the rest open
```

### 6.5 A hook with no name

```
lifecycle.Start: a hook was registered without a name

  registered  internal/modules/orders/module.go:31

  fix: name the hook for the thing it starts, as in Name: "orders.scheduler" —
       every message in this package identifies a hook by its name
```

`errors.Invalid`, reported by `Start` rather than by `Append`. See §5.2.

### 6.6 A hook with nothing to do

```
lifecycle.Start: the hook orders.scheduler has neither OnStart nor OnStop

  registered  internal/modules/orders/module.go:31

  fix: give it a callback, or do not register it
```

### 6.7 Started twice

```
lifecycle.Start: already started

  state  Ready

  fix: warren.Run starts the lifecycle once; a service should not call Start itself
```

### 6.8 A hook appended after the lifecycle started

```
lifecycle.Stop: 1 hook was appended after the lifecycle started, and never ran

  hook        orders.scheduler
  registered  internal/modules/orders/module.go:31

  fix: append hooks from a constructor, which runs during di.Build — before Start
```

### 6.9 A hook panicked

```
lifecycle.Start: the hook postgres.pool panicked: runtime error: invalid memory address or nil pointer dereference

  hook        postgres.pool
  registered  internal/platform/module.go:22
  started     2 of 5 hooks, rolled back

  fix: this is a bug in the hook, not in the lifecycle
```

`errors.Internal`. The recovered value is rendered into the message rather than
attached as a field — it is the one thing a reader needs, and a field would repeat
it. The `Stop` wording differs only in its op and its `stopped … continuing`
field, matching §6.4. See §5.5 and §11.10.

### 6.10 The start deadline expired

```
lifecycle.Start: the start deadline expired with 3 hooks unstarted

  unstarted  kafka.consumer, http.server, orders.scheduler
  started    2 of 5 hooks, rolled back

  fix: raise the start deadline, or find what is slow to open — a pool that dials
       for two seconds at boot is the pool's problem to fix
```

`errors.Internal`. §6.3's counterpart for boot, and the reason §1 lists a start
with no deadline alongside a shutdown with none.

### 6.11 Started after stopping

```
lifecycle.Start: this lifecycle has already stopped

  state  Stopped

  fix: restart is not supported — a process that wants to come back up is a new
       process, which is what an orchestrator is for
```

`errors.Invalid`. Distinct from §6.7, and detectable only because `StateNew` and
`StateStopped` are different values — see §4 and §11.8.

### 6.12 A hook exited without returning

```
lifecycle.Start: the hook orders.scheduler exited without returning

  hook        orders.scheduler
  registered  internal/modules/orders/module.go:31

  fix: a hook must return; calling runtime.Goexit inside one — which is what
       t.Fatal does — ends its goroutine and strands the phase
```

`errors.Internal`, and the message the `Goexit` sentinel in §5.4 exists to
produce. Without it the phase blocks until its deadline and then reports §6.3 or
§6.10, naming a hook that was never the problem. This message was added while
implementing §5.4; the draft spec described the sentinel but gave it no output.

## 7. Interoperability

- **`di` supplies it, and must not know about it.** `warren` does
  `di.Supply(c, lifecycle.New())` before `Build`, so any constructor can depend on
  `*lifecycle.Hooks`. `di` has no lifecycle awareness and this package has no
  container awareness; the only thing they share is the order constructors run in.
- **`warren` owns the sequence**: `Validate`, `Build`, `Start`, wait for a signal,
  `Stop`. It is the only caller that should print these errors, and it is where the
  start and stop deadlines are decided (§3).
- **`log` is read from the context, never held.** `log.From(ctx)` in `Start` and
  `Stop`; no logger field on `Hooks`, no logger parameter on `New`. A `Hooks`
  built before logging is configured still logs correctly, because the logger is
  resolved per call.
- **`health` reads `State()`** and renders it as two endpoints. Which package owns
  the state machine is §11.4.
- **A transport registers its own drain.** `transport/http`'s hook wraps
  `http.Server.Shutdown` in `OnStop`; nothing in this package knows what a request
  is.
- **`errors.Is` reaches a hook's own error**, so a caller can branch on it.
- **A test needs no goroutine plumbing**: hooks are `func(context.Context) error`,
  so a blocking hook is a channel receive and a slow shutdown is a cancelled
  context. No sleeps, per `AGENT.md § Testing`.

## 8. Enforcement

- `exhaustive` already applies to every enum, so `State` is covered the day it
  exists — which matters because `health` will switch over it.
- `depguard` needs no addition: `go.uber.org/fx` is already banned repository-wide
  with the reason "Warren owns lifecycle. See docs/architecture.md §3", and this
  package is why.
- `nonamedreturns` is already disabled with the reason "named returns are how a
  deferred function amends an error on the way out, which the lifecycle and
  unit-of-work code does deliberately" — this is that code, and §5.5's recover is
  the case it was written for.
- `containedctx` will bind if a `Hook` ever holds a context. It should not, and the
  linter is right.

## 9. Testing

### 9.1 Prior-art audit

Audited 2026-07-31, by reading the source rather than the documentation. All four
are alive: `uber-go/fx` (MIT, 7,618 stars, pushed 2025-12-27), `go-kratos/kratos`
(MIT, 25,829, pushed 2026-07-01), `zeromicro/go-zero` (MIT, 33,227, pushed
2026-07-31), and .NET's Generic Host (`dotnet/runtime`, MIT).

**No code was copied.** What is taken is a list of behaviours to handle, which is
not copyrightable and is worth more than the implementation anyway.

| Case | Source | Warren's behaviour |
|---|---|---|
| `Hook{OnStart, OnStop func(ctx) error}`, either nil | fx · `lifecycle.go:117` | §4, same shape, arrived at independently |
| Caller frame captured inside `Append` | fx · `lifecycle.go:172` | §5.2, same, via `runtime.Caller` |
| Registration order starts, LIFO stops | fx · `lifecycle.go:290` · .NET · *"stopped in LIFO order"* | §5.1 |
| Only hooks whose `OnStart` succeeded are unwound | fx · `numStarted` | §5.3 |
| Start aborts on the first failure | fx · .NET | §5.3 |
| Stop continues through failures and joins | fx · *"best-effort cleanup"* · .NET · `abortOnFirstException: false` | §5.4, §6.4 |
| Rollback joins the cause first | fx · `app.go:695` | §5.3, same ordering |
| **No rollback at all after a failed start** | .NET · *"Exceptions in StartAsync cause startup to be aborted"* | rejected — informed divergence, §5.3 unwinds |
| `ctx.Err()` checked before each hook, both phases | fx · `lifecycle.go:206`, `:291` | §5.3, §5.4 |
| **Phase runs in one goroutine, buffered chan, `select` on `ctx.Done()`** | fx · `withTimeout`, `app.go:787` | **§5.4 — adopted; the draft had no mechanism** |
| **Context error preferred when both fire at once** | fx · `app.go:806` | **§5.4 — adopted; without it §6.3's golden file flakes** |
| **`runtime.Goexit()` never returns and never sends** | fx · `errHookCallbackExited`, `app.go:783` | **§5.4 — adopted; a hook calling `t.Fatal` would have hung Stop** |
| The hook that overran is named from a guarded field | fx · `RunningHookCaller()`, `lifecycle.go:347` | §5.4, §6.3 — the requirement is adopted, the mechanism is not: we derive it from the phase counters, which are correct even when the phase goroutine never ran |
| Five states, including `incompleteStart` | fx · `lifecycle.go:128` | §4 — five states, but no `incompleteStart`: our unwind is inside `Start`, so a failed start is fully resolved before it returns |
| Default timeouts on both phases | fx · `DefaultTimeout = 15s` · .NET · `StartupTimeout`, `ShutdownTimeout` | §3 — `warren` picks the numbers; **§5.3 gains the start deadline the draft lacked** |
| `Stop` permitted while starting | fx · `lifecycle.go:268` | §5.6, serialised by a mutex |
| Per-hook events with caller, duration, and error | fx · `fxevent.OnStartExecuting`/`Executed` | §5.7, via `log.From(ctx)` |
| Hook names derived by reflection | fx · `OnStartName`, `fxreflect.FuncName` | rejected — informed divergence: reflection yields `main.NewPool.func1`; §4 requires a real `Name` |
| Panic in a hook is **not** recovered | fx · `RecoverFromPanics()` covers `Provide`/`Decorate`/`Invoke` only | rejected — informed divergence, §5.5 recovers |
| Panic in a teardown listener **is** recovered | go-zero · `RunSafe` → `rescue.Recover` | supports §5.5 |
| Exceptions aggregated per service, none allowed to escape | .NET · `exceptions.Add` | supports §5.5, §6.4 |
| Deregister from the registry before closing anything | kratos · `app.go:163` | §5.4 — the same requirement as readiness flipping first |
| **Cleanup context detached from the cancelled one, then bounded** | kratos · `context.WithoutCancel` + `context.WithTimeout`, `app.go:105-111` | **§5.3, §11.11 — adopted whole; the bound is what the first draft of §11.11 was missing** |
| **Rollback after a start timeout closes nothing** | fx · `withRollback` reuses the expired ctx (`app.go:684`), and `Stop` returns at its first `ctx.Err()` (`lifecycle.go:291`) | rejected — §5.3 detaches. Not a reading of ours: fx issue **#1035**, *"Stop hooks are not called when Start times out"*, opened 2023-02-08 |
| Hooks run asynchronously so a timed-out start can still resolve its state | fx · **named as the correct fix and deferred**: PR #1061, *"this requires a bigger refactor so that all lifecycle hook invocations are made asynchronous"* | §5.4 — Warren has it from the start, because abandonment required it anyway |
| A caller cleans up after a start timeout by calling Stop itself with a fresh context | fx · PR #1061 merged 2023-03-28, easing `Stop` to accept the `starting` state | rejected — §5.3 unwinds automatically. Cleanup a user has to remember is cleanup a user forgets |
| Parallel start, no ordering guarantee | kratos · `errgroup` · go-zero · *"the starting order of the added services is not guaranteed"* (`servicegroup.go:29`) | rejected, §11.5 — this is the differentiator |
| Concurrent start/stop as an opt-in flag, default off | .NET · `ServicesStartConcurrently` | rejected, §11.5 |
| Force-kill the process after a fixed budget | go-zero · `syscall.Kill`, `shutdown.go:96` | rejected, §3 — that is the orchestrator's job |
| Validating a hook's shape at all | none of the four validates; fx accepts a both-nil hook silently | §6.5, §6.6 — novel, following `di`'s precedent |

Six rows changed this spec: the phase runner, the select-determinism rule, the
`Goexit` sentinel, the running-hook field, the start deadline, and the detached
unwind context. Two more confirmed decisions that were already made for weaker
reasons. That is the return on an afternoon of reading, and it is why `AGENT.md`
forbids prototyping in favour of research.

### 9.2 The suite

Unit tests only — **no Docker, no network, no sleeps.** A timeout is a context
already past its deadline; a slow hook is a channel the test controls.

- Order: five hooks start forwards and stop backwards, asserted as a recorded
  sequence rather than as timings.
- The unwind: the fourth of five fails; hooks one to three stopped in reverse, the
  fourth's `OnStop` never called, state back to `StateStopped`.
- The unwind's own failure is joined to the cause, cause first.
- The unwind runs even when `Start`'s context is already expired — the §11.11
  claim, tested rather than asserted.
- `Stop` continues through a failing hook and still runs the rest.
- Grace period: a hook blocked on a channel, a context whose deadline has passed;
  the error names the blocked hook and the ones never reached, and the state is
  `StateStopped`.
- Start deadline: the same, for §6.10.
- A hook that panics, in each phase: recovered, named, and the phase behaves as
  §5.5 says — unwind on start, carry on during stop.
- A hook that calls `runtime.Goexit()` fails its phase instead of hanging it.
- State transitions, including that `StateNew`, `StateStarting`, and
  `StateStopping` are not ready, and that `State()` is readable from another
  goroutine under `-race` while `Stop` runs.
- `Stop` called concurrently with a `Start` still in flight, under `-race`.
- Idempotency: `Start` twice errors (§6.7); `Start` after `Stop` errors (§6.11);
  `Stop` twice is a no-op; `Stop` without `Start` is a no-op.
- A golden file for each of the eleven messages in §6.
- Every hook name appears in `Names()` in start order.
- One log line per hook, asserted against a `slog` handler the test installs with
  `log.Into`.
- `Example` functions for `Append`, `Close`, `Closer`, `Start`, `Stop`, and `State`.
- An integration-shaped test with `di`: three constructors appending hooks, built
  through a container, asserting start order matches dependency order — the claim
  §5.1 rests on, tested rather than asserted.

## 10. Definition of done

- [x] `lifecycle/` implements §4 exactly, standard library plus `warren/errors`
      and `warren/log`
- [x] Every exported identifier has a doc comment starting with its name
- [x] Twelve golden files committed under `lifecycle/testdata/`
- [x] The `di` integration test in §9.2 passing, since it is what §5.1 claims —
      the providers are registered in the wrong order deliberately, so the test
      fails if hook order ever follows registration order
- [x] `.golangci.yml` unchanged — no new exception was needed, and the only
      `//nolint` is the one `di`'s golden harness already carries for its test flag
- [x] `Example` functions for all six entry points
- [x] `docs/roadmap.md` v0.1 item 4 ticked
- [x] `docs/architecture.md` §3 corrected for §11.4 — `health` is two endpoints
      over `lifecycle.State()`, not a second state machine
- [x] This spec corrected wherever the code diverged — §4 (`Option`,
      `UnwindBudget`), §5.3 (detached and bounded unwind), §5.4 (the phase runner,
      and the in-flight hook derived from counters rather than a field), §6.12
      (new), §9.1 (four rows from the fx issue trail), §11.11
- [x] `make ci` green, with the output quoted

## 11. Decisions

§11.1 to §11.10 agreed 2026-07-31, all as recommended. §11.11 was agreed the same
day, against a corrected recommendation.

### 11.1 The type is `Hooks`, not `Lifecycle`

**Agreed: `Hooks`.** `lifecycle.Lifecycle` is what `fx` calls it and it stutters,
which `AGENT.md § Naming` forbids outright — the rule that gives `broker.Publisher`
rather than `broker.BrokerPublisher`.

`Hooks` reads correctly at the call site a user actually writes, which is a
constructor parameter:

```go
func NewPool(hooks *lifecycle.Hooks, cfg *Config) (*Pool, error)
```

and `hooks.Start(ctx)` reads as "start the hooks", which is what it does. The
alternatives considered were `Registry` (accurate for registration, wrong for
running) and `Sequence` (accurate for both, and nobody would guess it).

### 11.2 Hooks are registered from constructors, not declared by modules

**Agreed: constructors.** The alternative is a module method —
`func (m *Module) Hooks() []lifecycle.Hook` — which keeps infrastructure
constructors free of any framework import and reads more declaratively.

It is rejected because **it silently breaks the one guarantee this package makes.**
Module registration order is not dependency order, so hooks collected that way
would stop in an order that has nothing to do with what depends on what — the exact
failure `docs/architecture.md §5` describes. Registering during construction is
what makes reverse order correct.

The cost, stated plainly: an infrastructure constructor takes a `*lifecycle.Hooks`
parameter, so `internal/adapters/postgres` imports a Warren core package. That is
not a layering violation — `lifecycle` is core and transport-free — but it is a
coupling a purist would notice, and it is the price of the guarantee.

### 11.3 wire's cleanup-return form, re-examined — from `di/SPEC.md §11.6`

**Agreed: still no, and `Close`/`Closer` are the answer to the ergonomics.**

`di/SPEC.md §11.6` declined `wire`'s `(T, func(), error)` and said to re-read it
here, "if registering a stop hook turns out to be verbose enough that people skip
it". It is four lines, which is enough to be worth reducing but not enough to route
teardown around the ordering machinery.

Two things the cleanup form cannot express, both of which this package needs:

- **A name.** Every message in §6 identifies a hook by name. An anonymous `func()`
  gives a reader "a cleanup failed" and nothing to act on.
- **`OnStart`.** A pool pings, a listener listens. The cleanup form is stop-only,
  so half the components would use hooks anyway and a codebase would have both.

Two helpers cover the common cases in one line each and keep one mechanism:
`Close` for a closer that cannot fail, `Closer` for the `io.Closer` shape that
`sql.DB`, `os.File`, and `net.Listener` actually have. `fx` solved the same
ergonomic problem with a generic `Callable` constraint accepting four signatures
(`lifecycle.go:67`); two named helpers are the stdlib-only version of that and
read better at the call site.

### 11.4 `lifecycle` owns the readiness state, and `health` renders it

**Agreed, and `docs/architecture.md §3` is corrected in the same change**, since it
currently describes `health` as "two endpoints **and a state machine**".

Two state machines over one process's readiness is a bug waiting to happen: they
disagree the first time a transition is added to one and not the other. The state
belongs to the thing that performs the transitions, and that is this package.
`health` then becomes what its own line should say: **two endpoints over
`lifecycle.State()`**.

This also unblocks something noticed while deciding it: `health` has no roadmap
slot at all today.

### 11.5 Sequential start, not parallel

**Agreed: sequential.** Starting independent components concurrently would cut
boot time, and the 50 ms budget in `docs/roadmap.md` makes that tempting.

It is declined because the ordering guarantee is the product. Parallel start needs
a dependency graph to know what may overlap — which means rebuilding the graph
`di/SPEC.md §7` deliberately keeps out of this package — and a partial failure mid
fan-out has no well-defined unwind order. Sequential start costs the sum of the
hooks' own latency, and a hook should not be slow: a pool that dials for two
seconds at boot is the pool's problem to fix.

§9.1 makes the competitive position concrete. Kratos starts every server
concurrently through an `errgroup` and has no ordering concept at all; go-zero
states the consequence in its own doc comment — *"the starting order of the added
services is not guaranteed"*. The two implementations that take lifecycle
seriously, `fx` and .NET, are both sequential, and .NET makes concurrency an
opt-in flag that defaults to off. **Ordering is the differentiator, and it is
verified in their source rather than assumed from their marketing.**

### 11.6 A hook that overruns the grace period is abandoned

**Agreed: abandon it.** The alternative is to wait, which converts a slow hook
into a hung process and hands the decision to the orchestrator's `SIGKILL` — the
worst available outcome, since it lands mid-write.

Abandoning leaks a goroutine and possibly a file descriptor into a process that is
about to exit, and it means a hook's error may be reported *after* `Stop` returned.
Both are acceptable at that point in a process's life. What is not acceptable is
silence, so the hook is named in §6.3.

### 11.7 `Start` has a deadline too

**Agreed.** The draft named "a shutdown with no deadline" as one of three failures
this package prevents and then left boot with none: a pool that dials forever hung
the process with no message and no state past `StateStarting`.

`fx` defaults both phases to 15 seconds; .NET has `StartupTimeout` and
`ShutdownTimeout`. Four of four ship a stop deadline, three of four ship a start
deadline. The policy is the same as §11.6's — it arrives on the caller's context,
`warren` picks the number, this package never invents one — plus the `ctx.Err()`
check in §5.3 and the message in §6.10.

### 11.8 Five states, with `StateNew` as the zero value

**Agreed.** Four states with `StateStopped` at zero conflate "never started" with
"fully stopped": `Start` cannot then reject a restart (§6.11 is undetectable), and
`Stop`'s no-op promise needs a hidden boolean to implement — state that exists but
is not in the type.

`fx` also has five, though its fifth is `incompleteStart` — the state a lifecycle
sits in after a failed start whose rollback has not run, because its rollback lives
one layer up in `App.start` (`app.go:700`). Ours unwinds inside `Start`, so that
state cannot be observed and is not needed. Same count, different fifth, and the
difference is worth understanding before anyone tries to copy fx's here.

Both `StateNew` and `StateStopped` read as not-ready, so `health` is unaffected and
`exhaustive` covers the new case the day it exists.

### 11.9 `lifecycle` logs, through the context

**Agreed.** The draft said nothing about logging, and a boot that prints nothing
for thirty seconds is the operability gap `warren/log` exists to close. Every
framework audited logs per hook.

The mechanism is `log.From(ctx)`, not a logger parameter: `Start` and `Stop`
already take a context and `warren/log` already carries the logger on it, so
`New()` keeps its empty signature and a `Hooks` constructed before logging is
configured still logs correctly. See §5.7.

### 11.10 A hook that panics is recovered and named

**Agreed: recover, per hook, and convert it to an error** — specified as §6.9.
This is `di/SPEC.md §11.5` applied to hooks, and it keeps one policy across boot.

`fx` is the odd one out here: `RecoverFromPanics()` covers `Provide`, `Decorate`,
and `Invoke` but not lifecycle hooks, so a panicking hook takes the process down
with a stack full of runtime frames and no component name. go-zero recovers in its
shutdown listeners; .NET catches per service and aggregates. Two of three contain
it, and the one that does not gives the operator nothing to act on.

The cost is one more path to test and a recovered panic that is no longer a stack
trace. The original value is rendered into the message, so nothing a reader needs
is lost.

### 11.11 The unwind runs detached and bounded — Kratos's construction

**Agreed: `context.WithoutCancel`, then `context.WithTimeout` when a budget was
configured.** This surfaced while writing §5.3, was first recommended as detach
only, and was corrected by reading Kratos properly.

Once `Start` has a deadline (§11.7), the unwind has a problem: the context that
just expired is the same context the unwind would run on, so every `OnStop` sees a
dead context and refuses to do anything. The pool opened three hooks ago is leaked,
and the process exits holding a connection.

**`fx` has exactly this hole, and it is a filed bug rather than our reading of its
source.** Issue #1035, *"Stop hooks are not called when Start times out"*, opened
2023-02-08: `withRollback` passes the expired context to `Stop` (`app.go:684`),
which returns at its first `ctx.Err()` check (`lifecycle.go:291`) having run
nothing. PR #1061 merged a partial fix — `Stop` now accepts the `starting` state,
so a *caller* can clean up by calling it with a fresh context — and names the real
fix it could not afford:

> Ideally the app's lifecycle state should transition into `incompleteStart` when a
> long-running Start hook causes the context to timeout… However, this requires a
> bigger refactor so that all lifecycle hook invocations are made asynchronous.

That refactor is §5.4's phase runner, which this package needs anyway for
abandonment. We get fx's stated ideal for free, and we do not leave cleanup as
something the caller has to remember.

Detaching alone was the first recommendation, and its stated cost was that the
unwind could not itself be abandoned: a hook that hangs while unwinding hangs the
boot. Kratos shows that cost is avoidable — it detaches **and** bounds
(`app.go:105-111`) — so `UnwindBudget` carries the number and §3 still holds,
because `warren` decides it rather than this package. Unset, the rollback is
unbounded, which is Kratos's default too.
