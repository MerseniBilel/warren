# `github.com/MerseniBilel/warren/jobs` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2 (decided 2026-08-02)** — see **Why it is deferred** below. Nothing in shipped code depends on it. |
| **Source** | [warren.md §7.4](../warren.md) |
| **Module** | own module (`github.com/MerseniBilel/warren/jobs`) |
| **Mode** | Wrap (warren.md §9) |
| **Wraps** | `robfig/cron/v3` |


## Why it is deferred

Nothing in shipped code references `warren/jobs`, and it cannot be built
without amending two orderings AGENT.md forbids changing silently: §1.3's boot
step 6 (`pool → repos → consumers → servers`) and §2.3's six-step shutdown both
omit jobs entirely.

All eight open questions are structural, and two are load-bearing. What a job
handler even IS: `app.Handler[Req, Res]` is request-shaped, and a tick has
neither a request nor a response. And whether `jobs.LeaderOnly()` shares the
elector `outbox` already has — `postgres.WithAdvisoryLock` ships — which, if it
does, has to move somewhere both adapters reach without importing each other
(invariant 4).

`robfig/cron/v3` has zero transitive dependencies. Again, not the blocker.

## Problem

Scheduled and background work in a Go service is usually a `go func()` started
somewhere in `main`. It starts before its dependencies are ready, it is not
waited on at shutdown, and it keeps running while the pool it queries is closing.
warren.md §7.4 states the contrast directly: cron and background workers are
**"lifecycle participants — they start after dependencies are ready and drain on
shutdown, unlike loose goroutines."**

That is the same claim §1.5 makes about consumers ("lifecycle components, not
goroutines someone forgot about") and §5.5 makes about the outbox relay, and it
is what §2.3 exists to deliver.

## Goals

- Schedule work on a cron expression: `jobs.Cron(schedule, handler, ...)` (§7.4).
- Run repeating background work: `jobs.Worker(handler, jobs.Interval(d))` (§7.4).
- Make both **lifecycle participants** — started after their dependencies are
  ready, drained on shutdown (§7.4, §2.3).
- Run a job on one instance only: `jobs.LeaderOnly()` (§7.4).

## Non-goals

- **Not a distributed job queue.** No enqueue, no persistence, no delayed jobs,
  no per-job retry surface appears in warren.md. Asynchronous work triggered by a
  domain event goes through the broker (§3.4, §5.x), not through here.
- **Not a scheduler UI or job history.** Nothing of the kind is described.
- **Not the outbox relay.** §5.5 gives the relay its own package and its own
  leader election; whether it shares a mechanism with `LeaderOnly()` is an open
  question, not an assumption.
- **Imports no other adapter** (invariant 4) — which is precisely what makes
  Open question 3 sharp.

## Dependency audit

**Chosen:** `robfig/cron/v3`, recorded in §9 as Cron · Wrap · "lifecycle-aware".
That note is the whole stated reason: the library parses schedules and fires
timers, and Warren's contribution is attaching that to the lifecycle so a job
starts and stops in the right order. The wrap mode follows the standard test
(AGENT.md § Modes): a cron type reaching every job declaration would make the
library unswappable.

**Outstanding.** warren.md records **no observation date, no archived check, no
last-release date, no licence check, and no transitive footprint** for
`robfig/cron/v3`. AGENT.md § Adding a dependency requires all of it before the
library enters a `go.mod`, and specifically warns that a widely recommended
package can be archived without its README saying so. The audit must be run,
recorded here, and added to §9 before implementation.

Note also that `jobs.Worker(handler, jobs.Interval(30*time.Second))` is a ticker,
not a cron schedule, and needs nothing from `robfig/cron` — a point worth
recording when the audit justifies the dependency's weight.

## Public API

warren.md §7.4 gives two usage lines and no signatures:

```go
jobs.Cron("0 2 * * *", cleanupHandler, jobs.LeaderOnly())
jobs.Worker(reconcileHandler, jobs.Interval(30*time.Second))
```

Provisional, and **not fixed by warren.md** — the return type, the option type,
and above all the handler type are all unstated:

```go
// Package jobs runs cron schedules and background workers as lifecycle
// participants: they start after their dependencies are ready and drain on
// shutdown, unlike loose goroutines.
package jobs

// Cron schedules handler on the given cron expression.
func Cron(schedule string, handler /* type not fixed */, opts ...Option) warren.Module

// Worker runs handler repeatedly in the background.
func Worker(handler /* type not fixed */, opts ...Option) warren.Module

// LeaderOnly restricts the job to a single elected instance.
func LeaderOnly() Option

// Interval sets how often a worker runs.
func Interval(d time.Duration) Option
```

`warren.Module` as the return type is an inference from the other adapters —
`http.Server`, `kafka.Broker`, `postgres.Module`, `outbox.Relay`, and
`observability.Module` all return one and are passed to `warren.New` (§2.1,
§10) — but §7.4 shows neither of its calls inside a `warren.New` block, so this
needs confirming rather than assuming.

The **handler type is the largest gap**. Warren's handler abstraction is
`app.Handler[Req, Res]` (§3.2), which takes a request and returns a response;
a cron tick has neither. See Open questions.

## Behaviour

- **Lifecycle position — start.** §7.4 says jobs "start after dependencies are
  ready". §1.3 step 6 gives the `OnStart` order as `pool → repos → consumers →
  servers` and **does not name jobs**, so their exact position in that sequence
  is not fixed by warren.md (Open question 2). What is fixed is the constraint:
  after the dependencies a job resolves, and never before the graph is validated
  and instantiated (§1.3 steps 3–5).
- **Lifecycle position — stop.** §7.4 says jobs "drain on shutdown". §2.3's
  ordered shutdown is: readiness → 503; servers stop accepting; consumers stop
  fetching; outbox relay flushes; pools and connections close; force-exit
  deadline (default 30s). Jobs are **not named in that list either**. Draining
  must complete before step 5 — a job holding a pool that is closing is the bug
  this package exists to prevent — but where exactly it sits relative to
  consumers and the relay is undetermined (Open question 2).
- **A job in flight when SIGTERM arrives.** §2.3's model finishes in-flight work
  (requests finish, messages ack) under a per-hook timeout and a force-kill
  deadline. warren.md does not say whether a long-running cron job is waited on,
  cancelled via context, or both.
- **`LeaderOnly()` implies leader election, and warren.md never specifies it
  here.** §5.5 does specify one for the outbox relay —
  `outbox.LeaderElection(outbox.PostgresAdvisoryLock)` — with a pluggable
  strategy. §7.4's `LeaderOnly()` takes no argument and names no backend. Whether
  the two share a mechanism is unstated and matters: if they do, the mechanism
  belongs somewhere both can reach, and this module cannot import `outbox`'s
  Postgres strategy without either depending on `persistence/postgres` or
  breaking invariant 4. This is the structural decision in this package and it is
  the human's call (AGENT.md § When you are unsure).
- **Middleware ring.** If a job's work is an `app.Handler`, the core-ring
  middleware of §3.2 applies to it as it does everywhere else — a job is not a
  transport, so it has no edge ring. warren.md does not state this; it follows
  from §1.4 only if Open question 1 is answered "yes".

## Testing

- **No Docker, no network, no sleeps in unit tests** (AGENT.md § Testing). A
  scheduler is time-shaped, so time must be injectable: tests advance a fake
  clock and assert the job fired, never `time.Sleep` until it does. Leader
  election backed by a real Postgres advisory lock goes behind
  `//go:build integration`.
- **Ordering is the property under test**, since it is the package's entire
  claim. A job must not run before its dependencies' `OnStart` completed, and
  must have drained before pools close: assert on a recorded event sequence
  across a full `Start`/`Stop` cycle (§2.1 exposes `Start(ctx)`/`Stop(ctx)`
  "for tests").
- Cases: a schedule fires at the expected instants; an invalid cron expression
  fails at **boot**, not on the first tick (§1.3's rule — every detectable error
  surfaces at boot); a worker's interval is honoured; overlapping runs (a job
  still running when the next tick arrives) behave as specified once Open
  question 5 is answered; `LeaderOnly()` runs on exactly one of N instances.
- **Golden-file tests for error text** (AGENT.md § Testing) — including the
  invalid-schedule boot failure, which must name the bad expression and the job.
  No such text is fixed by warren.md, so all of it is new.
- `t.Parallel()`, table-driven subtests named for behaviour.

## Definition of done

- [ ] Dependency audit for `robfig/cron/v3` run, recorded above with its
      observation date, and added to warren.md §9.
- [ ] Open questions 1–3 answered by the human before code — the handler type,
      the lifecycle position, and the leader-election mechanism are structural.
- [ ] Both §7.4 lines compile exactly as written.
- [ ] Jobs register lifecycle hooks; the ordering test proves start-after-deps
      and drain-before-close.
- [ ] An invalid cron expression fails the boot with a message naming the job.
- [ ] `LeaderOnly()` verified against the agreed election mechanism, with the
      integration test behind a build tag.
- [ ] No `cron` type in any exported signature (invariant 3).
- [ ] Golden files for every error message this package emits.
- [ ] warren.md amended if any of the above changes §7.4 or §2.3's shutdown list.
- [ ] `make ci` passes (once the Makefile exists — AGENT.md § Repository state).

## Open questions

1. **What is a job handler?** `cleanupHandler` and `reconcileHandler` are passed
   with no type. `app.Handler[Req, Res]` (§3.2) is request/response-shaped and a
   tick has neither. Is a job a `func(context.Context) error`, a
   `Handler[struct{}, struct{}]`, or a new type? The answer decides whether the
   core middleware of §3.2 — transaction, retry, tracing, metrics — applies to
   jobs at all.
2. **Where do jobs sit in the boot and shutdown orders?** §1.3 step 6 lists
   `pool → repos → consumers → servers`, and §2.3's shutdown lists six steps.
   Neither mentions jobs. Both lists are marked in AGENT.md as orderings that may
   not be rearranged silently, so adding jobs to them is an amendment to
   warren.md, not an implementation detail.
3. **Does `LeaderOnly()` share a mechanism with `outbox.LeaderElection(...)`?**
   §5.5 has a pluggable strategy (`outbox.PostgresAdvisoryLock`); §7.4 has a
   no-argument marker. If they are the same election, where does it live so that
   `jobs` and `outbox` can both reach it without one adapter importing another
   (invariant 4)? If they are different, why does a service run two?
4. **What is the leader-election backend when the service has no Postgres?**
   `LeaderOnly()` names none, and a Redis lock is mentioned only in §6.2–6.4
   ("Redis provides cache + distributed lock"). Is `LeaderOnly()` a no-op without
   a configured elector, an error at boot, or does it default to something?
5. **What happens when a run overlaps the next tick?** Skip, queue, or run
   concurrently? Every cron library makes this configurable and warren.md is
   silent.
6. **Do `Cron` and `Worker` return `warren.Module` values?** And are they passed
   to `warren.New` at the top level, or declared inside a feature module
   alongside its providers, so the job can resolve that module's private
   dependencies (§2.1's encapsulation rule)?
7. **How does a job get its dependencies?** Every other component is
   constructor-injected from the container (§2.2). A handler value passed to
   `jobs.Cron` at module-declaration time has to have been constructed already —
   but `NewModule` returns an inert value and nothing is instantiated until §1.3
   step 4. Is a job registered by constructor rather than by value?
8. **Is a failing job observable?** No error handling, no retry, no dead-letter
   equivalent, and no metric is described for a job that returns an error.
