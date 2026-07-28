# Spec: Lifecycle

| | |
|---|---|
| **Module** | `warren/lifecycle` (core — standard library only) |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [01-errors](../01-errors/spec.md), [02-log](../02-log/spec.md) |
| **Blocks** | [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md), [08-transport-http](../08-transport-http/spec.md) |
| **PRD** | §4.2, §6.1, §6.6 |
| **ADRs** | [ADR-0001](../../../docs/adr/0001-dependency-injection.md) — lifecycle is why `fx` was rejected |
| **Date** | 2026-07-28 |

---

## 1. Problem

Every Go service rebuilds the same shutdown sequence, and most get it subtly
wrong: the HTTP listener stops accepting before in-flight requests drain, or the
database pool closes while a request still holds a connection, or a `SIGTERM`
during startup leaves a half-initialised process running.

Warren owns this deliberately. ADR-0001 rejected `uber-go/fx` on exactly this
point: *"it owns application lifecycle, and Warren's lifecycle (ordered
start/stop, readiness gating, graceful consumer drain) is a product feature."*
Having rejected `fx` for it, this has to be better than `fx`.

## 2. Goals

1. **Ordered start, reverse-ordered stop.** What starts last stops first, so a
   dependency is never torn down under something using it.
2. **Graceful shutdown on `SIGINT`/`SIGTERM`** with a total deadline; exceeding
   it is reported, naming the hook that hung.
3. **Readiness gating**: the process reports not-ready until every start hook has
   completed, and not-ready immediately when shutdown begins.
4. **A failed start unwinds cleanly** — everything already started is stopped, in
   reverse order, before the process exits.
5. **Startup overhead under 50 ms** for a minimal service (PRD §8).

## 3. Non-goals

- **No supervision or restart.** A hook that fails is fatal. Restart policy
  belongs to the orchestrator; a framework that restarts its own components
  hides failures from the thing designed to see them.
- **No health checks.** `warren/health` is a separate package; lifecycle only
  supplies the ready/not-ready signal it reports.
- **No consumer drain semantics yet.** The broker's drain (v0.3) is a stop hook
  that uses this; the ordering guarantee here is what makes it possible.

## 4. Public API

```go
package lifecycle

// Hook is one participant in the lifecycle. Name is required: it is what a
// timeout message names, and "hook 4 timed out" is not a usable message.
type Hook struct {
    Name    string
    OnStart func(context.Context) error
    OnStop  func(context.Context) error
}

// Lifecycle collects hooks and runs them in order. The zero value is not
// usable; construct with New.
type Lifecycle struct { /* unexported */ }

func New(opts ...Option) *Lifecycle

// Append registers a hook. Order of registration is start order; stop runs in
// reverse. Safe for concurrent use, because providers may register from any
// constructor.
func (l *Lifecycle) Append(h Hook)

// Start runs every OnStart in registration order. If one fails, everything
// already started is stopped in reverse order and the original error is
// returned — not the unwind error, which is attached as a field.
func (l *Lifecycle) Start(ctx context.Context) error

// Stop runs every OnStop in reverse registration order. It always attempts
// every hook: one failing stop does not skip the rest, and all errors are
// joined.
func (l *Lifecycle) Stop(ctx context.Context) error

// Run starts, waits for ctx cancellation or a termination signal, then stops
// within the shutdown timeout. It is what warren.App.Run calls.
func (l *Lifecycle) Run(ctx context.Context) error

// Ready reports whether every start hook has completed and shutdown has not
// begun. Safe for concurrent use; warren/health reads it.
func (l *Lifecycle) Ready() bool

type Option func(*config)

// WithShutdownTimeout sets the total deadline for Stop. Default 30s, chosen to
// sit inside Kubernetes' default 30s terminationGracePeriodSeconds without the
// pod being SIGKILLed mid-drain.
func WithShutdownTimeout(d time.Duration) Option

// WithHookTimeout sets a per-hook deadline. Default 0, meaning only the total
// deadline applies.
func WithHookTimeout(d time.Duration) Option

// WithSignals overrides the signals Run listens for. Default SIGINT, SIGTERM.
func WithSignals(sig ...os.Signal) Option
```

## 5. Behaviour

- **Start is sequential, not parallel.** Parallel start would be faster and would
  make ordering unprovable. The 50 ms budget is met by hooks being fast, not by
  racing them.
- **Stop always attempts every hook.** A stop hook that returns an error is
  logged and the sequence continues; all errors are joined with `errors.Join`.
  Abandoning shutdown on the first error is how connections leak.
- **The stop context is not the start context.** `Stop` receives a fresh context
  carrying the shutdown deadline — the start context is usually already cancelled
  by the signal, and passing it would give every stop hook a dead context, which
  is the classic graceful-shutdown bug.
- **A second signal during shutdown exits immediately** with a non-zero status
  and a message saying so. An operator pressing Ctrl-C twice means it.
- **Failure during start unwinds in reverse** and returns the original error.
  The unwind's own errors are attached as fields, never substituted — the first
  error is the one that explains what happened.
- **`Ready()` is false** until the last start hook returns, and false again from
  the instant shutdown begins. It is not "the process is alive."
- **Every transition logs at `Info`** with the hook name and its duration, so a
  slow boot is diagnosable from logs alone rather than by bisecting.

## 6. Errors

| Condition | Code | Message |
|---|---|---|
| Start hook returned an error | wrapped | The hook name, its position, and the underlying error via `%w` |
| Start hook exceeded its timeout | `CodeDeadlineExceeded` | The hook name, the timeout, and that `WithHookTimeout` raises it |
| Shutdown exceeded the total deadline | `CodeDeadlineExceeded` | Every hook that had not finished, named, plus the deadline and how to raise it |
| Stop hook returned an error | joined | Each hook named; joined so one failure does not mask the others |
| `Append` after `Start` | `CodeFailedPrecondition` | That hooks must be registered during module construction, and where to move the call |
| Hook with an empty `Name` | `CodeInvalid` | Rejected at `Append`, naming the file — an unnamed hook makes every later message useless |

## 7. Configuration

| Option | Default | Why |
|---|---|---|
| `WithShutdownTimeout` | 30s | Fits inside Kubernetes' default grace period |
| `WithHookTimeout` | 0 (none) | Per-hook deadlines are opt-in; a global deadline is enough for most services |
| `WithSignals` | `SIGINT`, `SIGTERM` | The two a container runtime and a terminal actually send |

## 8. Testing

Unit only — no sleeps, per [docs/testing.md](../../../docs/testing.md). Timeouts
are tested with a controllable clock and cancellable contexts, not with
`time.Sleep`, which is what makes lifecycle tests flaky in every other project.

- **Order**: five hooks assert start order 1→5 and stop order 5→1.
- **Unwind on failure**: hook 3 fails; assert hooks 2 and 1 stopped, hooks 4 and
  5 never started, and the returned error is hook 3's.
- **Stop continues past a failure**: hook 4's stop errors; assert 3, 2, 1 still
  ran and all errors are present in the joined result.
- **Fresh stop context**: cancel the start context, then assert stop hooks
  receive a live context with the deadline.
- **`Ready()` transitions**: false before, true after start, false at the first
  moment of shutdown.
- **Second signal exits immediately.**
- **Concurrency**: `Append` from 50 goroutines under `-race`.
- **Benchmark**: start and stop of a 50-hook lifecycle, against the 50 ms budget.

## 9. Invariants touched

- **Invariant 1** — `os/signal`, `context`, `errors` are all standard library.
  Core placement is correct and nothing here creates pressure to change that.

## 10. Definition of done

- [ ] Public API matches §4
- [ ] Unit tests per §8, `-race -shuffle=on`, no `time.Sleep` anywhere
- [ ] Committed benchmark for the 50 ms budget
- [ ] `make ci` green
- [ ] Doc comment on every exported identifier
- [ ] `docs/` concept page: lifecycle and graceful shutdown, including the ordering diagram
- [ ] Runnable example in `examples/lifecycle/`
- [ ] Changelog fragment

## 11. Open questions

1. **Should `Start` support parallel groups?** A service with ten independent
   pools would boot faster. It also makes ordering unprovable and the failure
   mode much harder to explain. Not at v0.1; revisit only if the dogfooding
   service actually misses the 50 ms budget on start.
2. **Does readiness belong here or in `warren/health`?** The signal originates
   here; the HTTP probe endpoint is clearly `health`'s. Splitting the flag from
   its consumer risks two sources of truth. Leaning: lifecycle owns the boolean,
   health owns the endpoint and the dependency checks.
3. **Post-stop hooks** — flushing traces and logs after everything else has
   stopped. OTel (v0.4) will want this. Decide then, but do not design the hook
   list in a way that forbids it.
