# `warren/app` — SPEC (a timeout the retry loop cannot see)

| | |
|---|---|
| **Status** | **PROPOSED (2026-08-31) — NOT APPROVED.** A semantic ruling on the error table, which AGENT.md calls load-bearing. Six lines of code and three document corrections. **Changes shipped behaviour.** |
| **Source** | Field test #14 (`fieldtest14/REPORT.md`, finding 4), verified against `app/timeout.go`, `app/middleware.go:400-411`, `errors/errors.go:318-330`, `persistence/postgres/postgres.go:736-756` |
| **Module** | core (`app/`) — stdlib + dig |
| **Mode** | Build |
| **Retires** | Deleted when implemented and reviewed. Residue rehomes to `app/timeout.go`'s doc comment and warren.md §3.2. |

## Problem

warren.md §3.2 and GETTING_STARTED both recommend `app.Timeout` composed
**inside** `app.Retrying`, and single that composition out as the reason the
inside/outside distinction is worth explaining: *"inside Retrying it bounds
each ATTEMPT"*. Field test #14 wrote exactly that and measured:

```
status=500  elapsed=3.002836875s  code=INTERNAL  handler attempts = 1
```

The mechanism, all three steps verified in source:

1. `app.Timeout` derives a deadline and returns whatever the handler returns
   (`app/timeout.go:61-65`). The stubbed dependency returned `ctx.Err()` —
   a bare `context.DeadlineExceeded` — which is what every Go engineer writes.
2. `errors.CodeOf` maps an error carrying no Warren code to `CodeInternal`
   (`errors/errors.go:329`).
3. `app.Retrying`'s predicate is `retryable` (`app/middleware.go:400-411`),
   which does `stderrors.As(err, &e)` on `*errors.Error` and **returns false
   for anything that is not one**. INTERNAL is not in its set anyway.

So the composition the documents recommend can never produce a second attempt
from a timeout. *"Bounds each attempt"* is true and useless. A slow downstream
also answers **500**, so every load-balancer and client retry policy keyed on
503 does nothing.

### Why this is an unfinished decision, not a new one

Warren has already ruled that a deadline expiry means UNAVAILABLE — three
times, in three packages, each time in code Warren owns:

- `persistence/postgres/postgres.go:745` — and its comment is this field
  test's finding, written down a year early:

  > A statement that ran out of time is a database that could not serve, so it
  > carries the same code as a dial failure: UNAVAILABLE, retryable, 503.
  > Without this it fell through unclassified and became a 500, which tells a
  > caller the bug is theirs and stops `app.Retrying` re-running it.

- `broker/memory/memory.go:111` — `errors.Unavailable("memory broker", ctx.Err())`
- `broker/middleware.go:640` — `errors.Unavailable("subscription", ctx.Err())`

The rule exists. It is applied wherever Warren wrote the adapter, and nowhere
else — so a user's own port implementation, which is exactly what Warren's
architecture pushes them to write, inherits nothing. In a real Postgres
service the trap does not fire, because postgres maps it. It fires on the
memory drivers and hand-written stubs the docs tell you to develop against.

## Ruling — `app.Timeout` categorises its own expiry. Nothing else changes.

When `timeoutHandler.Handle` returns, it already knows something no other
component knows: **whether the deadline it created expired**. If it did, and
the handler returned an error that carries no Warren code and is rooted in
`context.DeadlineExceeded`, the middleware wraps it as
`errors.Unavailable(...)`.

Three conditions, all required:

1. **`Timeout`'s own derived context expired.** Checked on the context it
   made, so a caller's shorter deadline that fired first is still attributed
   here — correctly, because the middleware's contract is "the call I bounded
   did not finish".
2. **The error carries no Warren code.** `errors.CodeOf(err) == CodeInternal`
   via the uncoded path. A handler that returned `errors.Internal("…", ctx.Err())`
   has *declared the failure final*, and `RetryingOn`'s outermost-code rule says
   this must be honoured. Timeout never overrides a coded error.
3. **`errors.Is(err, context.DeadlineExceeded)`.** A handler that timed out
   and returned an unrelated uncoded error is reporting that other thing.

### Why UNAVAILABLE is the right code, positively

`RetryingOn`'s doc comment already argues the exact hazard a timeout has:

> an UNAVAILABLE can mean the call arrived and only the reply was lost, so
> re-invoking the handler charges the card twice

That is a timeout's hazard, described before a timeout was assigned to it. A
timeout joins the code whose documented caveat is already its own. `CONTENTION`
is wrong (it means *nothing was written*, which a timeout cannot claim);
`INTERNAL` is wrong (it means *unanticipated*, and a deadline you set is
anticipated); a ninth code for it would break the closed set every client
switches on.

Consumer disposition is unchanged in shape: today INTERNAL nacks, retries and
dead-letters; UNAVAILABLE nacks with backoff and dead-letters when the
pipeline's budget is spent. Field test #14 observed the bounded ladder
(`warren-attempts:3` then DLQ). And `broker/middleware.go:640` already
produces UNAVAILABLE for a cancelled subscription, so the consumer ring is
being made consistent with itself, not changed.

### Three things deliberately NOT done

- **`errors.CodeOf` is not taught about `context.DeadlineExceeded`.** It would
  not work: `retryable` matches on `*errors.Error`, not on `CodeOf`, so
  `Retrying` still would not retry, and the two functions would disagree about
  what an error means. It would also open the `sql.ErrNoRows → NOT_FOUND`
  slope, where the mapping is wrong as often as right. `CodeOf`'s contract
  stays "a Warren code, or INTERNAL".
- **`context.Canceled` is not mapped.** Following the reason postgres already
  wrote down: *"that is the CLIENT hanging up, not the database failing, and
  reporting it as a service problem would make every abandoned request look
  like an outage."*
- **A user's own deadline is still INTERNAL.** A handler that gives its
  `http.Client` a 2s timeout and returns the bare error gets INTERNAL, and
  that is correct: uncoded means unanticipated. Warren maps what Warren's own
  machinery produced. The documents must say so — see below.

## Documents that are wrong today

1. **`app/timeout.go:36-38`** — *"An adapter that respects the context
   typically returns CodeUnavailable — postgres does"*. True of Warren's
   adapters and silently untrue of the reader's own port implementations,
   which is the whole trap. It must state the one line a port must write:

   ```go
   if errors.Is(err, context.DeadlineExceeded) {
       return werrors.Unavailable("catalog", err)
   }
   ```

2. **warren.md §3.2** and **GETTING_STARTED** — both present `Timeout` inside
   `Retrying` as producing retried attempts. After this change they are true
   for a timeout; they remain misleading unless they also say that a handler
   returning a bare stdlib error is INTERNAL and terminal.

3. **`app/timeout.go`** must state the new cost plainly: `Retrying` will now
   actually retry a timeout, so a slow dependency costs
   `attempts × timeout`, not `1 × timeout`. A team that wants fail-fast
   composes `Timeout` outside `Retrying`, or
   `app.RetryingOn(p, errors.CodeContention)`.

## What goes red

**ESCALATE.** Any test asserting that a timed-out handler produces 500 or
INTERNAL, and any test counting attempts through a `Retrying`+`Timeout` stack.
Field test #14's `TestTimeoutBoundsTheHandler` will change from 1 attempt/500
to N attempts/503, which is the point. In-repo breakage must be enumerated
before implementation; the change is confined to one middleware, so the set is
small.

## Definition of done

- [ ] `Timeout` inside `Retrying`, handler returning bare `ctx.Err()`:
      attempts == the policy's budget, final status 503, code UNAVAILABLE.
- [ ] Handler returns `errors.Internal("…", ctx.Err())` after the deadline:
      stays INTERNAL, one attempt. The outermost-code rule is pinned.
- [ ] Handler returns `errors.NotFound(…)` after the deadline: stays NOT_FOUND.
- [ ] Deadline did not expire, handler returns an unrelated uncoded error:
      stays INTERNAL.
- [ ] Caller's shorter deadline fires first: still UNAVAILABLE, and
      `Retrying`'s `ctx.Done()` guard stops the loop rather than retrying into
      a dead context. Both halves asserted.
- [ ] `context.Canceled` after the deadline did NOT expire: stays INTERNAL.
- [ ] The consumer ring: a timed-out event handler nacks and dead-letters on
      budget exhaustion, with `warren-error-code:UNAVAILABLE` in the headers.
- [ ] Golden diagnostic for the 503 body.
- [ ] The three documents above corrected in the same change (AGENT.md rule 4).
