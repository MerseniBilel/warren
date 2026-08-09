# `github.com/MerseniBilel/warren/internal/panics` — SPEC

| | |
|---|---|
| **Status** | **APPROVED — IMPLEMENTED 2026-08-09.** Written 2026-08-09 by architect ruling on field test #13, findings F1 and F7. The code is in; the corrections the implementation forced are recorded in "Corrections" at the end. This spec retires once the `warren.md` and `GETTING_STARTED.md` amendments below have landed. |
| **Source** | [warren.md §2.1](../../warren.md) (boot), [§2.3](../../warren.md) (lifecycle), [§3.5](../../warren.md) (transport registration), [§2.2](../../warren.md) (the diagnostic format this copies) |
| **Module** | core (`github.com/MerseniBilel/warren`) — standard library only |
| **Mode** | Build |
| **Wraps** | — |

> **This spec amends `warren.md`.** It adds §2.3's missing hook-panic
> semantics, §3.5's missing `Register`-panic semantics, a new kernel-internal
> package to §1.6's repository layout, and the first exit-code statement the
> manifest has ever carried. Every amendment is listed in
> **"What warren.md must say"** at the end, and must land in the same change
> as the code. Do not implement this and leave the manifest behind.

---

## Problem

Warren has a working standard for a panic during boot and does not apply it
everywhere.

`warren/di` contains one. A constructor that panics is recovered at
`di/container.go:155`, rendered by `errConstructorPanicked`
(`di/diagnostic.go:280`) as `✗ constructor panicked` with the panic value
verbatim, an explanation, and a stack whose frames start at the user's own
code — no `go.uber.org/dig`, no `reflect`, no `runtime`, no `di`. The process
returns an error from `App.Start`, `main` prints it, and the exit status is 1.
Field test #13 graded that path 10/10.

Two other places invoke user code during boot or shutdown and contain nothing.

**F1 — a panic in a `lifecycle.Hook` voids the rollback guarantee that three
documents make.** `lifecycle/lifecycle.go:269` runs
`go func() { done <- fn(hctx) }()` with no `defer recover()`. A hook that
*returns* an error rolls back correctly: the already-started hooks' `OnStop`
run in reverse, `App.Start` returns, exit 1. A hook that *panics* produces a
raw Go dump, exit 2, and **the prior hook's `OnStop` does not run** — the
process dies holding whatever the prior hooks acquired. The panic escapes on a
goroutine Warren spawned, so the user cannot recover it in `main` either.

The guarantee it breaks is stated three times, in Warren's own voice:

- `warren.md:339-342` — "acquisition belongs in hooks, **whose rollback the
  lifecycle guarantees**".
- `warren.md:516-517` — "a failing `OnStart` stops the boot and stops the
  already-started hooks in reverse before returning".
- `GETTING_STARTED.md:139` — "A connection or a goroutine belongs in a
  lifecycle hook, whose rollback the framework guarantees."

The same line runs `OnStop`, so a panic in one stop hook **abandons the rest of
the drain** — every hook below it in the unwind, including the pools and the
outbox relay. That inverts §1.3's shutdown ordering, which is the single
ordering `lifecycle` is a Build rather than `fx` in order to own.

**F7 — a panic inside a controller's `Register` is a raw panic, exit 2.**
`app.go:429` calls `c.Register(r)` unrecovered. Two sibling checks in the same
function — a method written into the pattern, a duplicate route — call
`reg.fail(err); return` and produce clean `✗ route registration failed` blocks
with exit 1. `transport/transport.go:699` and `:661` `panic(...)` instead, for
a nil handler, and the reproduction is the shape `warren new` generates: a
controller struct field the constructor forgot to assign, so
`transport.Post(r, "/users", c.h)` is handed a nil handler. Any panic a user
writes into `Register` behaves the same way.

The two findings are one class. Warren has a standard and two places that do
not meet it, and both are places where the difference between exit 1 with a
diagnostic and exit 2 with a Go dump is the difference between the product and
a stack trace.

The reason this is a package rather than two patches: the rendering must be
**one implementation**. `di/diagnostic.go`'s `cleanStack` is the part that
earned the 10/10 — it knows which frames to drop and why, and every line of
that knowledge is a comment explaining a specific trace that once reached a
user. A second copy in `lifecycle` and a third in the root package would drift
inside a month, and the invariant they enforce (invariant 2: no dig frame
reaches a user) would then hold in one place out of three.

## Goals

1. **One containment primitive** in the core module, standard library only,
   that recovers a panic and renders it in the §2.2 diagnostic format with an
   actionable stack.
2. **`di` migrates onto it** with its rendered text byte-identical — the
   existing golden files must pass unchanged.
3. **A panicking `OnStart` behaves exactly as a failing `OnStart`**: rollback
   runs, `App.Start` returns an error, exit 1.
4. **A panicking `OnStop` does not abandon the drain**: the remaining hooks
   still stop, and the panic joins the returned errors.
5. **A panicking `Register` fails the boot with a diagnostic**, alongside the
   registration failures step 5 already accumulates.
6. **Two boot panics are removed**, because they fail AGENT.md's admission
   test on criterion 3. See "The admission test", below.
7. **Every claim this spec makes has a golden file or a behavioural test**, and
   every bug it fixes starts with a failing one.

## Non-goals

1. **Request-path panics.** They are already contained and this spec does not
   touch them. See "What is explicitly NOT contained".
2. **Catching what `recover` cannot catch.** Stack exhaustion, concurrent map
   writes, `runtime.Goexit`, and a nil-map write inside another goroutine the
   user spawned are not recoverable, and no document may claim otherwise.
3. **A public API.** This package is `internal/`. Nothing in it appears in a
   Warren exported signature, ever.
4. **Changing any diagnostic `di` already prints.**
5. **Removing the goroutine in `runHook`.** The abandonment semantics at
   `lifecycle/lifecycle.go:254-259` are deliberate and stay.

---

## Public API as Go

The package is internal; "public" here means the surface the three call sites
use. It is deliberately four declarations.

```go
// Package panics contains one recovered panic and renders it as a Warren
// diagnostic. It exists so that the frame-filtering rules — which are the
// difference between a diagnostic and a stack trace, and which enforce
// invariant 2 — have exactly one implementation.
package panics

// Frame is one stack frame a reader can act on: the function, and the
// file:line it was at.
type Frame struct {
	Func string // "user/application.NewRegisterUserHandler"
	At   string // "/home/me/svc/internal/modules/user/application/register.go:24"
}

// Caught is a recovered panic together with the stack at the point it was
// raised, reduced to the frames a reader can act on.
//
// Frames[0] is the code that panicked. The panic machinery, runtime and
// reflect frames, go.uber.org/dig's frames (invariant 2), and this package's
// own are never present.
type Caught struct {
	// Value is the panic value, untouched. For a Warren refusal the value IS
	// the diagnostic — several paragraphs of it — so it is reproduced
	// verbatim and never summarised.
	Value  any
	Frames []Frame
}

// Do runs fn and returns nil, or the panic fn raised.
//
// It re-panics with http.ErrAbortHandler-shaped values it is told to let
// through: see Passthrough. Nothing else escapes.
func Do(fn func(), passthrough ...any) *Caught

// Diagnostic renders the caught panic as a Warren error block:
//
//	✗ <headline>
//
//	    <Value, verbatim, every line indented four spaces>
//
//	  <detail>
//
//	  Where it came from:
//
//	    <Frame.Func>
//	        <Frame.At>
//
// hide names additional function-name prefixes to drop — a caller's own
// containment plumbing, which is never the answer to "where did this come
// from". The universal drops are built in and are not the caller's to
// choose.
//
// The returned error's message is the rendered block. It does not wrap the
// panic value: a panic value is not an error, and errors.As reaching into one
// would make an arbitrary user value part of Warren's error contract.
func (c *Caught) Diagnostic(headline, detail string, hide ...string) error
```

**Ring position.** Kernel. Imports `fmt`, `runtime/debug`, `strings` and
nothing else. It knows nothing of routes, containers or hooks — the three call
sites supply their own headline and detail, which is what keeps it out of the
contracts ring.

**Why `Do(fn func())` and not `Catch() *Caught` as a deferred helper.** A
`defer` helper has to be written correctly at each call site — `defer
panics.Into(&err)` is one forgotten `&` from silence — and `lifecycle`'s call
site is inside a goroutine where the result has to cross a channel anyway.
`Do` cannot be misused: it either returns nil or it returns the panic.

---

## Behaviour — the containment contract

This is the contract. Each clause is testable and each has a test named in
"Testing".

### C0 — the frames

For every diagnostic this spec produces, the rendered stack contains:

- **zero** frames whose function or file mentions `go.uber.org/dig`
  (invariant 2 — a dig *frame* leaks dig as surely as a dig error string),
- **zero** `runtime.` frames, `panic(` frames, `runtime/debug.Stack` frames,
  or `created by ` lines,
- **zero** `reflect.` frames,
- **zero** frames of the containment plumbing itself —
  `warren/internal/panics`, and whichever of `di`, `lifecycle` or the root
  `warren` package placed the recover,
- and `Frames[0]` is the user's own code, or Warren's own code when Warren
  raised the panic deliberately.

A rendered block with no frames at all is permitted (a panic raised from a
frame that was entirely filtered) and prints without the "Where it came from"
section rather than printing an empty one.

### C1 — a panic in `Hook.OnStart`

**Where.** `lifecycle/lifecycle.go:260-306`, `runHook` — the goroutine at
`:269` becomes `go func() { done <- runContained(hctx, h, fn, phase) }()`, or
equivalent. The containment is inside the goroutine, because that is the only
place the panic can be caught: it is raised on a stack Warren created and
`main` cannot see it.

**What happens.**

1. The panic is recovered and converted to an error, a new
   `*hookPanickedError` carrying the rendered block.
2. `runHook` returns it **exactly as it returns `errHookFailed`** — same
   position, same channel, same `classify` path. It is not a timeout and not
   an abandonment; the goroutine returned.
3. `Start`'s existing failure path at `lifecycle/lifecycle.go:163-170` then
   runs unchanged: state goes to `stateStopped`, `l.unwind(ctx)` stops every
   already-started hook in reverse, and `errors.Join(err, rollback)` is
   returned. **This is the whole rollback fix — no new unwind logic exists.**
4. `App.Start` returns that error; `App.Run` returns it; the generated
   `main.go` prints it and calls `os.Exit(1)`.
5. Readiness never opens. It could not have: `l.ready.Store(true)` is only
   reached when the loop completes.

**The diagnostic**, byte for byte (golden file
`lifecycle/testdata/hook_panicked_onstart.txt`):

```
✗ lifecycle hook panicked

    runtime error: invalid memory address or nil pointer dereference

  Hook "user" panicked during OnStart, so Warren stopped the hooks that had
  already started, in reverse order, and the process never opened readiness.
  A hook that RETURNS an error gets the same rollback; this one did not get
  the chance to return.

  A panic here is one of two things: a refusal Warren raises on purpose — the
  message above says so when it is — or a bug in the hook. Either way the
  process never served a request.

  Where it came from:

    infrastructure.(*Pool).Open
        /home/me/svc/internal/platform/pool.go:41
```

The rollback failures, if any, join it as they already do, each on its own
line, unchanged.

### C2 — a panic in `Hook.OnStop`

**Where.** The same `runHook`; the phase is `OnStop`.

**What happens.**

1. Recovered and returned as `*hookPanickedError`, as in C1.
2. `unwind` (`lifecycle/lifecycle.go:200-240`) appends it to `errs` on the
   ordinary `if err := runHook(...); err != nil` branch at `:225` — it is not
   a `hookAbandonedError`, so the force-exit short-circuit at `:226-233` does
   not fire — and **the loop continues to the next hook**. This is the drain
   fix: one panicking stop hook no longer strands the pools and the relay.
3. `Stop` returns `errors.Join(errs...)`. `App.Run` returns it, `main` prints
   it, exit 1.

**The diagnostic** differs from C1 in one paragraph (golden file
`lifecycle/testdata/hook_panicked_onstop.txt`):

```
✗ lifecycle hook panicked

    send on closed channel

  Hook "warren/broker/kafka" panicked during OnStop. The remaining hooks were
  stopped anyway — shutdown does not abandon the drain because one hook
  failed — and this is reported with whatever else failed on the way down.

  A panic here is one of two things: a refusal Warren raises on purpose — the
  message above says so when it is — or a bug in the hook. The resources this
  hook owns may not have been released.

  Where it came from:

    kafka.(*consumer).stop
        /home/me/svc/vendor/.../consumer.go:88
```

### C3 — a panic in `Controller.Register`

**Where.** `app.go:427-431`, boot step 5. The recover is **per controller**,
inside the inner loop, so that several broken controllers report together —
which is the property the comment at `app.go:385-386` already claims ("Every
registration problem is reported together") and which an uncontained panic
destroys after the first one.

**What happens.**

1. `panics.Do(func() { c.Register(r) })` per controller.
2. A caught panic is rendered and **added to the builder's accumulated
   registration failures**, the same list `reg.fail` appends to. It is not a
   special channel: a panicking `Register` and a `Register` that registered a
   duplicate route are both "this controller's registration did not work",
   and they must appear in one report.
3. The loop continues to the next controller and the next module.
4. **If any registration panic was recorded, the consequence checks are
   skipped**: `b.Fill(table)` at `app.go:432` and `table.Unserved()` are not
   run, and the accumulated failures are returned joined instead. A controller
   that panicked halfway through `Register` has left a partial route table;
   "no adapter serves protocol EVENT" and "route nobody serves" are then
   artefacts of the panic, not independent facts, and printing them buries the
   one block the reader needs under two they cannot act on.
5. `App.Start` returns; exit 1.

**The diagnostic** (golden file `testdata/register_panicked.txt` at the repo
root, beside the other root-package golden files):

```
✗ controller registration panicked

    runtime error: invalid memory address or nil pointer dereference

  *user.Controller panicked while module "user" was registering its routes
  at boot step 5. The routes it had already registered are discarded and the
  boot is abandoned; no adapter was built and nothing listened.

  The usual cause is a controller field the constructor does not assign: the
  struct literal in NewController omits it, the field is nil, and Register
  hands that nil to a route. Check that every handler NewController takes is
  assigned to a field, and that every field Register reads is one of them.

  Where it came from:

    user.(*Controller).Register
        /home/me/svc/internal/modules/user/controller.go:31
```

### C4 — two boot panics become registration failures

`transport/transport.go:699` (`register`) and `:661` (`OnEvent`) panic on a
nil handler. They become `reg.fail(...)` and `return`, matching the two
sibling checks in the same functions.

**The diagnostic** (golden file `transport/testdata/nil_handler.golden`). The
entry carries its own `✗ nil handler` headline — see correction 8, which
records the ruling that put it there and the text this replaced:

```
✗ route registration failed

  ✗ nil handler

      POST /users was registered with a nil handler
        in module "user"

    transport.Post was given a nil app.Handler[Req, Res]. The usual cause is a
    controller field the constructor does not assign: the struct literal in
    NewController omits it, so the field is nil and Register passes that nil
    straight through.

    Check that every handler NewController takes is assigned to a field, and
    that every field Register reads is one of them.
```

The `OnEvent` variant names the topic instead of the verb and pattern:
`topic "user.registered" was subscribed with a nil handler`.

`transport/transport.go:696` and `:656` — `panic("transport: registration with
a foreign Registrar — the interface is sealed")` — **stay panics**. At that
point `reg` is nil and there is no channel to fail through, and `Registrar` is
sealed by unexported methods (`transport/transport.go:42-45`) so the branch is
unreachable from user code. They are assertions, not refusals. C3 contains
them, so even an unreachable assertion cannot reach a user as a raw dump.

### C5 — `di` is refactored, not changed

`di/container.go:155` and `di/diagnostic.go:280-368` move onto
`internal/panics`. `errConstructorPanicked`'s rendered text does not change by
one byte, and `di`'s existing golden files are the proof. If the shared
renderer cannot reproduce the existing text exactly, the shared renderer is
wrong and gets fixed — not the golden file.

### C6 — exit codes, stated

For the first time anywhere in the project, and therefore an amendment to
`warren.md` §2.1:

| Outcome | Status |
|---|---|
| Boot succeeded, ran, drained cleanly | 0 |
| Any boot failure, including a contained panic | 1 |
| Any shutdown failure, including a contained panic | 1 |
| A panic Warren does not contain, or a runtime-fatal condition | 2 (Go's) |

Exit 1 comes from the generated `main.go` printing `warren.App.Run`'s error and
calling `os.Exit(1)`
(`cli/internal/scaffold/templates/cmd__APPNAME__main.go.tmpl`). Warren itself
never calls `os.Exit`. **After this change, no Warren-invoked user code during
boot or shutdown produces exit 2.**

---

## What is explicitly NOT contained

Stated here so the next round does not re-derive it, and so no document claims
more than the code delivers.

1. **A panic in a request handler.** Already contained, twice, and this spec
   adds nothing: `transport/http/edge.go:33-65` recovers as the outermost,
   non-removable edge middleware — 500, `CodeInternal`, correlation ID to the
   client, stack to the log, never to the client; `broker/middleware.go:201`
   does the consumer's equivalent (`Recover()`, warren.md §3.4:1313, outermost
   *and* innermost in the pipeline at :1373/:1379); and gRPC's `Recovery()` is
   documented as not removable at warren.md:1708-1710. **Ruling: in
   jurisdiction, already satisfied, no change.** A second layer inside `app`
   would catch the same panic earlier and rob the adapter of the correlation
   ID and the status mapping.
2. **`http.ErrAbortHandler`.** `edge.go:44-46` re-panics it deliberately —
   net/http raises it by design when a client goes away mid-write. `panics.Do`
   grows a `passthrough` parameter for exactly this and no other reason, and
   the HTTP edge is its only caller.
3. **A panic inside `persistence.UnitOfWork.Do`.** warren.md:1260-1262 rules
   that it **rolls back and re-panics**, on purpose: "a leaked transaction
   holds locks, and swallowing the panic would convert a bug into a 503 and
   destroy the stack." Unchanged. The re-panic then meets the adapter's
   recover in (1), which is the correct place for it to become a 500.
4. **A panic in a constructor.** Contained since `di` shipped; C5 refactors the
   implementation and changes no behaviour.
5. **A panic in a `health.Check`.** Contained at `health/health.go:216`.
   Unchanged.
6. **A panic in `main` before `warren.New`, in a `warren.Option`, in a
   `Substitution`, or in a goroutine the user spawned from a hook and did not
   recover.** Warren did not invoke it and cannot see it. The `Hook.OnStart`
   doc comment (`lifecycle/lifecycle.go:32-42`) already tells users how to
   derive that goroutine's context; it must now also say that a panic in it is
   theirs to recover.
7. **Anything `recover` cannot catch** — stack exhaustion, out of memory,
   concurrent map read/write, `runtime.Goexit`. The containment is
   `recover`-shaped and the docs must say "contained" and never "cannot crash".

---

## The admission test

AGENT.md § General requires that any change to the sanctioned-boot-panic list
be argued against the four criteria added on 2026-08-09, and that the line
itself be amended. **This change adds no boot panic and removes two.**

Running the test on `transport/transport.go:699` and `:661`, the nil-handler
panics:

| # | Criterion | Verdict |
|---|---|---|
| 1 | Detectable at composition time, no runtime state | **Holds.** `h == nil` at registration. |
| 2 | No correct reading | **Holds.** A nil handler serves nothing. |
| 3 | The API cannot return an error | **FAILS.** `reg.fail` exists at `transport/transport.go:556`, boot step 5 accumulates registration failures and reports them together (warren.md:1561), and the two sibling checks in the very same functions already use it. |
| 4 | The alternative is silent data loss | **FAILS.** The alternative is a clean boot failure, which is what every other registration problem gets. |

Two criteria fail. AGENT.md: "A candidate failing any one of them is a doc
comment, a `warren lint` rule, or a returned error." It is a returned error.

The sanctioned list is therefore **unchanged** — `di.MustResolve`, `app.Chain`'s
composition guards and their consumer-ring mirrors, and `app.RetryingOn`'s
terminal-code refusal — and AGENT.md § General needs no amendment for this
spec. The two panics being removed were never on the list; they were never
argued at all, which is precisely what the admission test was written to stop.

---

## Errors

Every message in this spec gets a golden file (AGENT.md § Testing). The full
list, with its file:

| Message | Golden file |
|---|---|
| `✗ lifecycle hook panicked` (OnStart) | `lifecycle/testdata/hook_panicked_onstart.txt` |
| `✗ lifecycle hook panicked` (OnStop) | `lifecycle/testdata/hook_panicked_onstop.txt` |
| `✗ controller registration panicked` | `testdata/register_panicked.txt` |
| `✗ route registration failed` (nil handler, HTTP) | `transport/testdata/nil_handler.txt` |
| `✗ route registration failed` (nil handler, event) | `transport/testdata/nil_handler_event.txt` |
| `✗ constructor panicked` | unchanged — `di/testdata/…` must still pass |

Stack paths in golden files are unstable across machines. The golden files
carry the block with the "Where it came from" section elided to a marker line,
and the frame assertions live in the behavioural tests below — a golden file
that has to be regenerated per machine is a golden file people stop reading.

---

## Testing

**Every one of these must fail before the fix and pass after.** A bug fix
starts with a failing test (AGENT.md § Testing).

**`internal/panics`**

1. `TestDoReturnsNilWhenNothingPanics`.
2. `TestDoCapturesTheValueVerbatim` — a multi-paragraph string value, the
   `app.Chain` refusal shape, survives unchanged.
3. `TestFramesDropTheMachinery` — asserts C0 by substring: no `go.uber.org/dig`,
   no `runtime.`, no `reflect.`, no `panic(`, no `created by `, no
   `internal/panics`.
4. `TestFramesStartAtTheCode` — `Frames[0].Func` is the test's own helper.
5. `TestPassthroughRepanics` — a value in `passthrough` escapes `Do`.
6. `TestDiagnosticRendersWithNoFrames` — no empty "Where it came from".

**`lifecycle`**

7. `TestPanickingStartHookRunsPriorStopHooks` — **the F1 regression test.**
   Two hooks; the second's `OnStart` panics; assert the first's `OnStop` ran,
   that `Start` returned a non-nil error, and that the test process did not
   die.
8. `TestPanickingStartHookNeverOpensReadiness` — `Ready()` is false.
9. `TestPanickingStopHookDoesNotAbandonTheDrain` — **the second F1 regression
   test.** Three hooks; the middle one's `OnStop` panics; assert the first
   hook's `OnStop` still ran and that `Stop`'s joined error carries both the
   panic block and any other failure.
10. `TestPanickingHookDiagnosticIsGolden` — both phases.
11. `TestPanickingHookIsNotReportedAsAbandoned` — the returned error does not
    satisfy `errors.As(&hookAbandonedError{})`, so the force-exit
    short-circuit at `:226` is not taken.

**root `warren`**

12. `TestPanickingRegisterFailsTheBootNotTheProcess` — **the F7 regression
    test.** A controller whose `Register` dereferences a nil field; assert
    `App.Start` returns an error, that the test process survives, and that the
    message is the C3 block.
13. `TestTwoPanickingControllersBothReport` — both blocks present in one error.
14. `TestPanickingRegisterSuppressesConsequenceChecks` — the returned error
    contains no `Unserved`/`Claim` text.
15. `TestRegisterPanicStackHasNoFrameworkFrames` — C0, on this path.

**`transport`**

16. `TestNilHandlerIsARegistrationFailureNotAPanic` — `transport.Post` with a
    nil handler; the builder's error list has one entry; nothing panicked.
17. `TestNilHandlerDiagnosticIsGolden`, both HTTP and event.
18. `TestNilHandlerJoinsOtherRegistrationFailures` — a nil handler and a
    duplicate route in one `Register` both appear, **and**
    `TestEveryJoinedFailureLeadsWithItsOwnHeadline` — the same two failures,
    asserting each carries its own `✗` line and that all such lines sit at one
    indent, so neither failure nests under the other. Golden file
    `transport/testdata/nil_handler_and_duplicate.golden`. See correction 8.

**`di` — regression only**

19. The existing constructor-panic golden test must pass unchanged after C5.
    If it needs regeneration, C5 has been violated.

**Cross-cutting**

20. `TestNoWarrenInvokedUserCodePanicsUncontained` — a table over the three
    sites, each asserting an error return rather than a process death.

No Docker, no network, no sleeps. All unit tests.

---

## Definition of done

1. `internal/panics` exists, standard library only, with the four declarations
   above and doc comments starting with the identifier's name.
2. `di` is refactored onto it; its golden files are untouched and pass.
3. C1–C6 hold, with the tests above.
4. Every golden file listed exists and is committed.
5. `lifecycle.Hook`'s doc comment states what happens when `OnStart` or
   `OnStop` panics, and states that a panic in a goroutine the hook spawned is
   the user's to recover.
6. `transport.Controller`'s doc comment states that a panic in `Register` fails
   the boot.
7. `warren.md` carries every amendment in the next section.
8. `GETTING_STARTED.md:139` is either true or corrected — see the next section.
9. `make ci` is green across all seven modules, and the run is quoted in the
   pull request.
10. This spec is **deleted** in the same change, its open questions rehomed.

---

## What warren.md must say

Cited loudly, per CLAUDE.md: these are amendments to the manifest, and they
land in the same change as the code.

1. **§2.3 `warren/lifecycle` (line 485-552), after the failure paragraph at
   511-519** — add: a panicking `OnStart` is contained and behaves exactly as a
   failing one, including the reverse-order rollback; a panicking `OnStop` is
   contained, does not abandon the remaining hooks, and joins the returned
   errors; both render `✗ lifecycle hook panicked`; a panic in a goroutine the
   hook spawned is not Warren's to catch.
2. **§2.1 `warren` (line 235-386), at the boot-step-5 description near
   320-331** — add: a panic in `Controller.Register` is contained per
   controller, reported with the registration failures step 5 already
   accumulates, and suppresses the consequence checks.
3. **§2.1, new subsection** — the exit-code table of C6. The manifest currently
   contains no exit code anywhere except "non-zero exit" for `lint arch`
   (line 2572).
4. **§3.5 `warren/transport` (line 1431-1572), at the registration-failure
   claim on 1561-1564** — add: a nil handler is a registration failure, not a
   panic, with the admission-test reasoning in one sentence; and name
   `reg.fail`'s effect as the accumulation mechanism, which the section
   currently describes only by its outcome.
5. **§1.6 repository layout (line 186-220)** — add `internal/panics` as a
   kernel-internal package, with one line saying it holds the single
   implementation of panic containment and frame filtering, and that it is
   `internal/` because no part of it may appear in an exported signature.
6. **§4.1 `warren/transport/http` (line 1576-1653)** — the HTTP edge's
   non-removable recover is implemented (`edge.go:33`) and undocumented, while
   §4.2 documents gRPC's at 1708-1710. Add the same two lines for HTTP,
   including the `http.ErrAbortHandler` passthrough.
7. **§9 ledger (line 2629)** — no new dependency; no row.

## What GETTING_STARTED.md must say

`GETTING_STARTED.md:139` — "A connection or a goroutine belongs in a lifecycle
hook, whose rollback the framework guarantees" — becomes true when C1 lands,
and it must be extended, because the document currently contains **no
`OnStart`/`OnStop` example at all**: the sentence promises a mechanism the page
never shows. Add a short hook example and one sentence on what a panic in it
does.

---

## Open questions

1. **Should a contained panic be logged as well as returned?** Every other boot
   failure is returned and printed once by `main`; a log line would be a second
   copy with the newlines escaped, which is exactly what the template's comment
   at `cmd/…/main.go` warns against. Recommendation: no. Recorded here so the
   next round does not re-open it without new evidence.
2. **Should `App.Start` recover a panic from anywhere else in boot** — a
   `warren.Option`, a `Substitution`, `flatten` — as a last-resort net? It
   would turn a Warren bug into a Warren diagnostic that says "this is a Warren
   bug, please report it", which is what `di.errInternal` already does for
   container failures. Not in this round; it is a different argument (defending
   against ourselves, not containing user code) and it deserves its own one.
   **Rehome to:** whichever spec next touches `app.go`'s boot sequence.
3. **`transport/grpc` is unimplemented and its spec claims a non-removable
   `Recovery()`.** When it is built, C0 and the request-path exemption in
   "What is explicitly NOT contained" apply to it verbatim. **Rehome to:**
   `transport/grpc/SPEC.md` before this spec is retired.

---

## Corrections — what the implementation forced (2026-08-09)

AGENT.md rule 4: the spec is corrected in the same change as the code. These
are the places the code does not do what the text above says, and why.

1. **Golden files are `.golden`, not `.txt`.** Every golden in this repository
   is `<name>.golden`, and the three `assertGolden` helpers append that
   extension. The files are `lifecycle/testdata/hook_panicked_onstart.golden`,
   `lifecycle/testdata/hook_panicked_onstop.golden`,
   `testdata/register_panicked.golden`, `transport/testdata/nil_handler.golden`
   and `transport/testdata/nil_handler_event.golden`. Their rendered text is
   byte-for-byte what §C1–C4 print, with the frames elided as the spec allows.

2. **Four boot panics are removed, not two.** Goal 6 and the admission test
   name `transport/transport.go`'s `register` and `OnEvent`. `Raw` carries the
   identical nil-handler panic, three lines from the `reg.fail` its own
   empty-pattern check already uses, and it fails the admission test on the
   same two criteria. Leaving it would have made the same mistake produce a
   diagnostic or a Go dump depending on which registration function the user
   reached for. Two more golden files exist for the variants the spec did not
   enumerate: `transport/testdata/nil_handler_grpc.golden` (`transport.Method`
   shares `register` with the HTTP verbs) and
   `transport/testdata/nil_handler_raw.golden`.

3. **`transport.Builder` grows one exported method, `Failures() error`.** C3
   requires the boot to abandon step 5 BEFORE `Fill`, and until now `Fill` was
   the only way to read what the registrars had accumulated. `Failures`
   reports the same list `Fill` does, so a panicking controller and a
   duplicate route in another module still arrive in one error. It is an
   addition to §3.5's public surface and `warren.md` must record it.

4. **`Frame.Func` carries the package, not the module path** —
   `"application.NewRegisterUserHandler"`, not
   `"user/application.NewRegisterUserHandler"`. That is what `di` has always
   rendered, and C5 forbids changing it.

5. **`runHook`'s `classify` gained a case rather than reusing an existing
   one.** C1 says the panic returns "same classify path" as `errHookFailed`;
   passing it through `errHookFailed` would have wrapped the rendered block in
   `lifecycle: hook %q failed during %s: …` and buried it. A
   `*hookPanickedError` is returned unwrapped, which is what makes the golden
   files above the whole of the returned error.

6. **`Do`'s `passthrough` has no production caller yet.** The spec says the
   HTTP edge is its only caller; `transport/http/edge.go` still has its own
   `recover` and re-panic, and moving it onto this package is a change in
   another module that this round did not make. The parameter is implemented
   and tested; the edge's migration is a separate change, and until it happens
   the sentence in "What is explicitly NOT contained" describes intent, not
   code.

7. **`scripts/invariants.sh`'s invariant-2 check was narrowed.** It was one
   grep for the string `go.uber.org/dig` in any `.go` file outside `./di/`,
   and it failed on the code and the tests that ENFORCE the invariant: this
   package's frame filter, whose job is to drop dig's frames, and the tests
   across `lifecycle`, the root package and here that assert a rendered
   diagnostic contains no dig wording — asserting an absence requires writing
   the absent thing down. It is now two checks: no import line outside
   `warren/di`, and no mention in a non-test file outside `warren/di` and
   `warren/internal/panics`. Both halves were verified to still fail on a real
   violation.

8. **The nil-handler diagnostic gained a `✗ nil handler` headline of its own —
   architect ruling, 2026-08-09.** As first implemented it had none, because
   C4 gave its rendered text byte for byte and that text was the whole
   `✗ route registration failed` block. Every sibling entry
   (`✗ duplicate route`, `✗ empty route pattern`) carries one, so a nil handler
   reported ALONGSIDE a sibling blended into the report's header while the
   sibling stayed nested under it — and which of the two appeared nested
   depended only on the order the routes were registered in. The ruling: it
   gets its own headline. Field test #13 graded the raw panic 2/10 precisely
   for lacking the framework's diagnostic shape, and an entry that borrows a
   neighbour's header is a milder version of that same defect; every other
   failure in Warren leads with `✗`.

   The change is the headline plus two spaces of indent on the body, so the
   entry matches `errDuplicate`'s shape — headline at column 0, subject at 4,
   body at 2, all shifted two more by `errRegistration`'s `indent`. **The body
   text is unchanged.** C4 above now shows the ruled text; the five goldens
   (`nil_handler`, `nil_handler_event`, `nil_handler_grpc`, `nil_handler_raw`
   and the new `nil_handler_and_duplicate`) were regenerated with it, and
   `TestEveryJoinedFailureLeadsWithItsOwnHeadline` — written first, and
   observed failing with `no headline "✗ nil handler" of its own` — is what
   holds it.

9. **Two tests were replaced, not added.** `TestNilHandlerPanicsAtRegistration`
   and `TestRawNilHandlerPanicsAtRegistration` asserted the panic this change
   removes. They now assert the opposite, under the names
   `TestNilHandlerDoesNotPanicAtRegistration` and
   `TestRawNilHandlerIsARegistrationFailure`, each carrying the admission-test
   reasoning in its doc comment. The behaviour they pinned was never argued
   against the admission test, which is what that test exists to stop.
