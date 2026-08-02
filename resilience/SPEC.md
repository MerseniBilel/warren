# `github.com/MerseniBilel/warren/resilience` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2, and narrowed (decided 2026-08-02)** — see **Why it is deferred** below. Nothing in shipped code depends on it. |
| **Source** | [warren.md §7.3](../warren.md) |
| **Module** | own module (`github.com/MerseniBilel/warren/resilience`) |
| **Mode** | Wrap (warren.md §9) |
| **Wraps** | `sony/gobreaker`, `cenkalti/backoff/v4`, `x/time/rate` |


## Why it is deferred

**Most of this already ships.** `app.Retrying(policy)` is core middleware, and
`broker.ExponentialBackoff(attempts)` is a concrete `app.RetryPolicy` in the
core module — so retry-with-exponential-backoff is done, and `Timeout` is
`context.WithTimeout`. What is genuinely absent is the circuit breaker and the
rate limiter: roughly 30% of this spec.

It is also the least ready. Its own open question 1 asks *"is `Policy` a type
or a function?"* — warren.md §7.3 uses the same identifier both ways, and Go
will not allow it. Open question 4 admits that whether `Timeout` bounds each
attempt or the sequence, and whether the breaker sees pre- or post-retry
failures, "produce materially different systems". Open question 6 asks whether
`resilience.Policy` and `broker.WithRetry` are the same concept — which is now
a live redundancy in SHIPPED code, not a hypothesis.

**When it returns it is narrowed to the breaker and the rate limiter**, folding
retry into the shipped `app.RetryPolicy` rather than minting a second
vocabulary for it. `gobreaker` + `x/time/rate` have no transitive
dependencies.

## Problem

Timeout, retry, circuit breaking, and rate limiting are three or four libraries
with three or four different configuration shapes, and the place they get
configured is usually the call site — inside a handler. warren.md §7.3 states
the objection in one line: **"Nobody should configure gobreaker in a handler."**

A handler that constructs a `gobreaker.Settings` has imported a driver, spread
tuning across the codebase, and made the library unswappable — the exact failure
the wrap rule exists to prevent (AGENT.md § Modes).

## Goals

- Put `sony/gobreaker`, `cenkalti/backoff/v4`, and `x/time/rate` behind **one
  `Policy` interface** (§7.3).
- One composed declaration covers timeout, retry with backoff, and circuit
  breaking, as §7.3's example writes it.
- Feed `app.Retrying(policy)` — the core middleware of §3.2, which "retries on
  `CodeUnavailable`".

## Non-goals

- **Not a general-purpose resilience toolkit.** §7.3 gives four constructors and
  nothing else.
- **Not a per-call-site API.** The whole point is that a handler never configures
  any of the three libraries.
- **Not the owner of consumer retries.** §3.4 gives the broker its own
  driver-agnostic `Retry(backoff)` stage in the consumer middleware chain, and
  §5.1 configures it with `broker.WithRetry(broker.ExponentialBackoff(5))`.
  Whether that is this package's `Policy` under another name is unresolved — see
  Open questions.
- **Imports no other adapter** (invariant 4).

## Dependency audit

**Chosen:** `sony/gobreaker` (circuit breaker), `cenkalti/backoff/v4` (retry
backoff), and `golang.org/x/time/rate` (rate limiting). warren.md's stated
reason is the wrap boundary itself — "nobody should configure gobreaker in a
handler" (§7.3) — and §9 records the mode as Wrap with the note "one `Policy`
interface". No alternatives are evaluated.

**Two discrepancies in the source, both to be resolved before adoption:**

- §7.3 names **three** libraries; the §9 ledger row (Resilience) names **two**,
  omitting `x/time/rate`; §1.6's module map says "gobreaker + backoff". If rate
  limiting is in scope, §9 and §1.6 need amending; if it is not, §7.3 does.
- §7.3's surface has no rate-limiting constructor at all — `Timeout`, `Retry`,
  `Exponential`, and `CircuitBreaker` are the four shown. So the third library is
  named but unused by every example in warren.md.

**Outstanding.** warren.md records **no observation date, no archived check, no
last-release date, no licence check, and no transitive footprint** for any of the
three. AGENT.md § Adding a dependency makes that audit a precondition of entering
a `go.mod`, and it is the section that names two archived-but-still-recommended
packages found in the initial audit. For `backoff/v4` the audit must also
establish whether a newer major exists and whether v4 is the right pin — the
`/v4` in §9's row is a pin nobody has yet justified in writing.
All three audits go here and into §9 before implementation.

## Public API

warren.md §7.3 gives usage, not signatures:

```go
resilience.Policy(
    resilience.Timeout(3*time.Second),
    resilience.Retry(3, resilience.Exponential(100*time.Millisecond)),
    resilience.CircuitBreaker(resilience.FailureRatio(0.5)),
)
```

Read literally, `Policy` is called as a **function** taking options. The
surrounding prose calls it *"one `Policy` interface"*. Both cannot be true of the
same identifier in Go; see Open question 1. The nesting is also two levels deep —
`Retry` takes a count and a backoff value, `CircuitBreaker` takes a
`FailureRatio` value — so `Exponential` and `FailureRatio` are constructors of
their own argument types, not options of `Policy`.

**No signature here is fixed by warren.md.** Provisional, pending Open
question 1:

```go
// Package resilience composes timeout, retry, circuit breaking, and rate
// limiting behind a single Policy, so that no handler ever configures a
// resilience library directly.
package resilience

// Policy composes the given resilience options into a single policy.
func Policy(opts ...Option) /* return type not fixed by warren.md */

// Timeout bounds how long the guarded call may take. Whether that is each
// attempt or the whole retried sequence is Open question 4.
func Timeout(d time.Duration) Option

// Retry retries a failed call using the given backoff. Whether n counts
// retries or total attempts is not stated by warren.md — see Open question 4.
func Retry(n int, backoff Backoff) Option

// Exponential returns an exponential backoff with the given base delay.
func Exponential(base time.Duration) Backoff

// CircuitBreaker opens the circuit according to the given trip condition.
func CircuitBreaker(trip Trip) Option

// FailureRatio trips the circuit when the failure ratio reaches r.
func FailureRatio(r float64) Trip
```

The names `Option`, `Backoff`, and `Trip` are placeholders for types warren.md
does not name.

## Behaviour

- **Core ring.** §3.2 lists `app.Retrying(policy)` among the built-in **core**
  middleware — it wraps `app.Handler[Req, Res]`, so a policy applies identically
  to HTTP, gRPC, and consumers (§1.4). Nothing in this package belongs to the
  edge ring.
- **`CodeUnavailable` is the retryable code.** §3.2 says `app.Retrying(policy)`
  "retries on `CodeUnavailable`", and §2.6 marks `CodeUnavailable` with the
  comment `// retryable` and maps it to 503 / `Unavailable` / "nack + backoff
  retry". So retry is driven by the semantic error vocabulary, not by inspecting
  a driver error — which is what lets a policy be transport-agnostic. Retrying
  `CodeInvalid` would be wrong: §2.6 sends it straight to the DLQ, "never retry".
- **Nothing runs per request from the container.** A policy is composed at boot
  and the composed closure is what the request path calls (invariant 7, §1.4).
- **`app.Retrying(policy)`'s type has the same unresolved mechanism as
  `app.Traced()` and `app.Authorized(policy)`.** `app` is a core contract
  package — stdlib-and-dig only (invariant 1), zero implementations (invariant
  5) — and this package is an adapter that imports gobreaker. For
  `app.Retrying(policy)` to compile in core, `policy`'s type must be declared in
  core. warren.md does not say where it lives. See Open questions.
- **Timeouts and `context.Context`.** `Timeout` bounds an attempt; the handler it
  wraps already takes a context as its first parameter (§3.2). warren.md does not
  state whether the timeout is enforced by deriving that context, and no other
  mechanism is available to a `Handler` that respects cancellation.

## Testing

- **No Docker, no network, no sleeps in unit tests** (AGENT.md § Testing). This
  bites here: retry, backoff, and circuit-breaker cooldowns are time-shaped, so
  time must be injectable and asserted on, never waited on. A policy that can
  only be tested with `time.Sleep` is a design defect, not a testing problem.
- Table-driven cases per behaviour: retry count exhausted; retry stops on a
  non-retryable code; `CodeUnavailable` retries and `CodeInvalid` does not (§2.6,
  §3.2); backoff delays grow exponentially from the base; the circuit opens at
  the configured failure ratio and short-circuits subsequent calls.
- Composition order is observable and must be pinned by tests: whether the
  timeout bounds each attempt or the whole retried sequence changes behaviour
  visibly — and warren.md does not state which (Open question 4).
- **Golden-file tests for error text** (AGENT.md § Testing). warren.md fixes no
  message here; whatever an exhausted retry or an open circuit reports is new
  text and gets a golden file.
- No gobreaker or backoff type may appear in an assertion on a public signature —
  that is invariant 3, and a test that needs one has found a leak.
- `t.Parallel()`, subtests named for behaviour.

## Definition of done

- [ ] Dependency audits for `sony/gobreaker`, `cenkalti/backoff/v4`, and
      `x/time/rate` run, recorded above with their observation date, and added to
      warren.md §9 — including whether `x/time/rate` is in scope at all.
- [ ] warren.md §7.3 / §9 / §1.6 reconciled on the library list.
- [ ] `Policy` resolved as a type or a function (Open question 1) and warren.md
      amended if the answer changes §7.3's example.
- [ ] The §7.3 example compiles exactly as written.
- [ ] `app.Retrying(policy)` retries `CodeUnavailable` and nothing else, with
      tests per §2.6's table.
- [ ] No gobreaker, backoff, or `x/time/rate` type in any exported signature
      (invariant 3); raw access, if offered, is a named escape hatch.
- [ ] Time is injectable; no unit test sleeps.
- [ ] Golden files for every error message this package emits.
- [ ] `make ci` passes (once the Makefile exists — AGENT.md § Repository state).

## Open questions

1. **Is `Policy` a type or a function?** §7.3 calls it —
   `resilience.Policy(...)` — and describes it as "one `Policy` interface" in the
   same breath. Likely the interface is the *return* type under another name, but
   Go will not allow both spellings of `Policy`. Which is it?
2. **Where is the policy type that `app.Retrying(policy)` accepts declared?**
   Core cannot import this module. Is there a core port — in `app`? — that this
   package implements? This is the same unanswered question as `app.Traced()`
   (`observability/SPEC.md`) and `app.Authorized(policy)` (`auth/SPEC.md`), and
   one answer should cover all three.
3. **Is `x/time/rate` in scope, and what is its surface?** §7.3 names it; §9 and
   §1.6 do not; no example uses it. If rate limiting ships, what does it limit —
   inbound calls per handler, outbound calls per dependency — and is it core or
   edge middleware?
4. **How do the stages compose?** Does `Timeout` bound each attempt or the whole
   retried sequence? Does the circuit breaker see per-attempt failures or
   post-retry failures? These produce materially different systems and warren.md
   fixes neither.
5. **Is a policy named or scoped?** A circuit breaker is only meaningful per
   dependency — one breaker for Postgres and another for a payment API. §7.3's
   example creates an anonymous policy with no target. How is a policy bound to
   the thing it protects, and how is state shared between the calls that share a
   breaker?
6. **Is `resilience.Policy` the same concept as `broker.WithRetry(...)`?** §3.4
   and §5.1 give consumers their own retry-with-backoff and DLQ stage, spelled
   `broker.ExponentialBackoff(5)` rather than
   `resilience.Retry(5, resilience.Exponential(d))`. Two vocabularies for one
   idea is a smell; is the broker's retry meant to accept a `resilience.Policy`,
   or are they deliberately separate because the broker's exhausted retry ends in
   a DLQ?
7. **Where does a policy get attached?** §7.3 shows a policy value being
   constructed and never being used. Is it a provider in a module, a per-route
   option like `auth.RequireScope`, or an argument to `app.Retrying` at handler
   construction?
8. **Are retries safe by default?** Retrying a non-idempotent command duplicates
   work. §5.6's inbox makes *consumption* idempotent, but nothing in warren.md
   says a retried HTTP command is safe. Is idempotency a precondition the user
   must guarantee, and is that documented at the point of use?
