# `warren/testing` — SPEC (the harness claims a protocol it does not serve)

| | |
|---|---|
| **Status** | **PROPOSED — NOT APPROVED. BLOCKER CLEARED 2026-08-31; the problem is still live.** Narrows `warrentest`'s protocol claim so a registered event subscription cannot be certified green while never running. The dependency on [`broker/SPEC.md`](../broker/SPEC.md) is discharged — that work is implemented in the working tree — while `testing/warrentest.go:177` still claims all three protocols on the harness's own behalf, unchanged. Nothing blocks this now except approval. |
| **Source** | Sandbox field report 2026-08-29 (SBX2-002 harness half, SBX2-003) |
| **Module** | core (`testing/`, package `warrentest`) — stdlib + dig |
| **Mode** | Build |
| **Retires** | Deleted when implemented and reviewed. |

## Why a spec exists

The change makes a **currently-passing test fail**. That is the right outcome and it is still a compatibility event for every user, so it is the human's. The reasoning being overturned is also explicit and deliberate, written into the code, and **it is not obviously wrong** — overturning it needs an argument, not a patch.

---

## Problem

`testing/warrentest.go:174-187`:

```go
	// So the harness claims every protocol on its own behalf. What it gives
	// up is the boot check that would have caught a route nobody serves —
	// which is a production concern, and which the application's own main
	// still gets.
	claimAll := warren.NewModule("warrentest/claims",
		warren.Providers(func(tbl *transport.Table) *protocolClaims {
			for _, p := range []transport.Protocol{
				transport.ProtocolHTTP, transport.ProtocolGRPC, transport.ProtocolEvent,
			} {
				tbl.Claim(p, "warren/testing")
			}
```

Reproduced by the field reporter and independently by the coordinator: a module with an `r.OnEvent` subscription, booted with `NewModuleTest(m, WithMemoryBroker())`, publishes an event that reaches **zero consumers**. No error, no warning, exit 0.

Production does refuse — `app.go:484` calls `table.Unserved()` inside `Start`. So the harness is the only place this passes, and it passes because the harness said it would serve something it does not.

### The existing reasoning is right for HTTP and gRPC — and it is stronger than the comment claims

The comment justifies claim-everything as giving up "a production concern". For HTTP and gRPC that is an understatement: **the harness could not require a server even if it wanted to.**

```
$ ls testing/go.mod   → No such file or directory   (testing/ IS the core module)
$ cat go.mod          → require go.uber.org/dig v1.19.0    ← the entire list
$ cat transport/http/go.mod → module .../transport/http; require .../warren
```

`warrentest` is in the **core** module; `transport/http` is an adapter module. Requiring an HTTP server in every module test would put an adapter in core's `go.mod` — invariant 1, and the ring direction. And `warrentest.Invoke` calls `app.Handler` **directly**, so a module test legitimately needs no server at all. **Claiming HTTP and gRPC is not a trade-off; it is a necessity. Keep it, and say so in the comment.**

### Events are materially different, and that asymmetry is the whole spec

Three facts separate them:

1. **The runtime is in core.** `broker/memory` has no `go.mod` — it is the core module. The harness already builds one: `warrentest.go` wraps `memory.New()` in a `Recorder` under `WithMemoryBroker()`. There is no module-boundary obstacle.
2. **The user opts in explicitly.** `WithMemoryBroker()` is a user saying *"events matter in this test"*. Registering subscriptions and delivering none is the harness contradicting an explicit request.
3. **There is no `Invoke` for events.** `Invoke` bypasses HTTP by design and reaches the handler — that is the whole point. There is no equivalent that bypasses the broker and reaches a subscription, so for events, "the harness does not serve it" means "the code under test does not run", full stop.

**HTTP unserved in a test is a route the test did not need. An event unserved in a test is a consumer that silently does not exist.** Those are not the same failure and should not share one policy.

---

## Goals

- `WithMemoryBroker()` **serves** the frozen table's event subscriptions, not merely binds the ports.
- A subscription registered with no way to deliver it **fails the test at boot**, with a one-line remedy.
- HTTP and gRPC keep claim-everything, with the comment corrected to state the real reason.
- No new dependency; no new module.

## Non-goals

- **Not a change to `Invoke`, `Resolve`, `Published`, `AssertPublished`, `AsCaller`, `Golden`, or `servertest`.**
- **Not an HTTP claim change.** Explicitly out of scope, and the reason is written above so it is not revisited.
- **Not a broker driver.** This consumes `broker/SPEC.md`'s adapter loop.

---

## Public API

**Ideally: none.** The change is behavioural, inside `NewModuleTest`.

```go
// WithMemoryBroker binds the in-process broker as Publisher and Subscriber in
// the root scope, wrapped in a recorder that Published and AssertPublished
// read — AND SERVES the event subscriptions the module registered with
// r.OnEvent, through the same consumer chain production uses.
//
// Without it, a module that registers r.OnEvent fails the test at boot rather
// than passing with a consumer that never runs.
func WithMemoryBroker() Option
```

The claim loop narrows to:

```go
protocols := []transport.Protocol{transport.ProtocolHTTP, transport.ProtocolGRPC}
if cfg.broker {
    // Claimed because it is genuinely served, not to silence a check.
    protocols = append(protocols, transport.ProtocolEvent)
}
```

and `WithMemoryBroker()` additionally installs `broker/SPEC.md`'s adapter loop over `Table.Events()` with the recorder as publisher and `inbox.NewMemoryStore()` as the dedupe store.

### The new failure, and its diagnostic

A module registering `r.OnEvent` without `WithMemoryBroker()` now fails at `NewModuleTest` with `Unserved()`. That text is written for production and would read oddly in a test, so the harness wraps it:

```
✗ this module registers 2 event subscription(s) and the test serves none

    module "booking" — r.OnEvent("seat.held", …)

  warrentest.Invoke reaches app.Handler directly, so HTTP and gRPC routes
  need no server here. Events have no such path: an unserved subscription
  is a consumer that does not run, and a test that passes anyway.

      warrentest.NewModuleTest(t, booking.Module(), warrentest.WithMemoryBroker())
```

**Open question 2** covers whether an explicit opt-out is also needed.

---

## Compatibility — this WILL break existing tests, and that is the change

Any test today that boots a module with `r.OnEvent` and no `WithMemoryBroker()` passes; after this it fails at boot. **That is the entire value.** Every such test was certifying a consumer that never ran.

The remedy is one line and it is in the diagnostic. Warren's user base is zero, so the cost is a rename; the benefit is that the failure mode which produced SBX2-002 cannot recur.

**In-repo blast radius, measured before this spec was written:** `grep -rn "OnEvent" --include='*_test.go' .` — every hit must be checked in stage 3, and any that gains `WithMemoryBroker()` is evidence the check works.

---

## SBX2-003 — `servertest` is undiscoverable, and it is the best thing in the package

The reporter wrote a ~60-line HTTP harness, found `servertest` afterwards via `go list`, deleted the harness, and called it *"the best-designed part of the framework I touched"* and *"a genuinely excellent API"*.

```
$ grep -rn "servertest" README.md GETTING_STARTED.md warren.md
warren.md:2971  ... warren.md:2974
```

**Present in the contributor manifest only. Absent from both user-facing documents.** A doc fix, no spec content — but it is the highest value-per-line item in this report and should land first, independent of everything else: a testing seam nobody can find is a testing seam nobody uses, and the SBX2-002 class of defect is exactly what it catches.

Fix: a `GETTING_STARTED.md` section showing `servertest.New(t, m)` + `Response.Code(t)`, and a README line. Note the reporter also observed it is **absent from published v0.2.1** — so the docs must not describe it until the next tag, or must name the version that carries it.

---

## Go 1.27: nothing

`Option` is `struct{ apply func(*config) }`; no type parameter, no generic method, no inference change, no new stdlib symbol applies to the harness itself.

**Corrected 2026-08-31.** This paragraph claimed `httptest.NewTestServer` "was assessed and rejected for `servertest` when that package was specced". It cannot have been: `httptest.NewTestServer` is **new in Go 1.27** (released 2026-08), and `transport/http`'s spec is retired, so the ruling it cites is uncheckable. The assessment has now actually been made, and the conclusion is the same for a different reason: `NewTestServer` runs on an in-memory network whose listener is **not exported** — `httptest.Server.Listener` is documented "not set for servers using the in-memory network" — and `servertest` boots a whole Warren `App` with its own `http.Server` rather than wrapping a bare `http.Handler`. `http.Listener(l net.Listener)` would accept an in-memory listener if one existed to pass. None does. Rejected on availability, not on taste.

---

## Testing

| What | Assertion |
|---|---|
| `r.OnEvent` + `WithMemoryBroker()` | publish → handler runs, no sleep |
| **`r.OnEvent` without `WithMemoryBroker()`** | **boot fails with the diagnostic above — the SBX2-002 regression, and it must be observed red against today's harness** |
| `r.Get`/`r.Post` with no HTTP server | still boots and `Invoke` works — the HTTP policy is unchanged and pinned |
| `r.Method` with no gRPC | still boots |
| A module with no subscriptions and no `WithMemoryBroker()` | still boots |
| Two features on one topic | both receive; dedupe is per subscription |

---

## Implementation plan

**Stage 0 — SBX2-003 docs.** Independent of everything; ship first.
> `grep -rn "servertest" README.md GETTING_STARTED.md` returns hits.

**Stage 1 — depends on `broker/SPEC.md` stages 1–2.** Do not start before the adapter loop exists.

**Stage 2 — `WithMemoryBroker()` serves the table**, and the claim narrows.
> `cd . && go test -race ./testing/...`

**Stage 3 — sweep the repository's own tests.** `grep -rn "OnEvent" --include='*_test.go' .`; add `WithMemoryBroker()` where a subscription is registered. **Any test that needed it and did not have it is a defect this change found** — record each in the commit message.
> `cd . && go test -race ./...` and each adapter module.

**Stage 4 — the harness diagnostic + golden.**
> `cd . && go test ./testing/...`

**Stage 5 — correct the comment at `warrentest.go:174-180`** to state the real reason HTTP and gRPC are claimed (core cannot import an adapter; `Invoke` needs no server) rather than "a production concern".
> `make ci`

## Definition of done

- [ ] `make ci` green across all seven modules.
- [ ] A test registering `r.OnEvent` without `WithMemoryBroker()` fails, and the failure was observed red before the fix.
- [ ] A test registering only HTTP routes still boots with no server — pinned by a test, so the asymmetry is deliberate and cannot erode.
- [ ] `servertest` appears in both `README.md` and `GETTING_STARTED.md`.
- [ ] `warrentest.go`'s claim comment states the module-boundary reason.
- [ ] **This file is deleted.**

## The harness ruling — decided 2026-08-29, NOT YET IMPLEMENTED

**`warrentest` should enforce `Table.Unserved()` for EVENTS, and keep claiming
HTTP and gRPC on its own behalf.**

The asymmetry is structural rather than a preference:

- **HTTP/gRPC — the harness could not require an adapter even if it wanted to.**
  `warren/testing` is in the CORE module and `transport/http` is an adapter, so
  requiring one would put an adapter in core's `go.mod` and reverse the ring
  direction §1.1 fixes. Claim-everything there is a necessity, and the current
  comment should say so rather than calling it a trade-off.
- **Events differ on three counts.** The runtime (`broker/memory`) is in core,
  so requiring it costs nothing. `WithMemoryBroker()` is the user explicitly
  asking for one. And there is no `Invoke` equivalent: `Invoke` bypasses HTTP
  and still reaches the handler, whereas an unserved subscription means the
  code under test never runs at all.

*An HTTP route unserved in a test is a route the test did not need. An event
unserved is a consumer that silently does not exist.*

It will FAIL EXISTING TESTS BY DESIGN — that is the point, and it is why it is
recorded here rather than slipped in.

## Open questions

1. **Should `servertest` narrow the same way?** It boots a real HTTP server, so it genuinely serves `ProtocolHTTP` — but it inherits `warrentest`'s claim for events. A `servertest` test of a module with subscriptions has the same blind spot. **Recommend: yes, identically, via the same `WithMemoryBroker()` path** (`servertest.With(warrentest.WithMemoryBroker())` already exists). Confirm no separate mechanism is needed.
2. **Is an explicit opt-out needed?** A test that deliberately ignores a module's subscriptions must currently add `WithMemoryBroker()` and let them run. That is nearly free and arguably more honest than a `WithoutEventDelivery()`. **Recommend no opt-out** — adding one recreates the silence this spec removes — but it is a surface decision, and if a real case appears, `WithoutEventDelivery()` with a doc comment naming the risk is the shape.
3. **Does `Published`/`AssertPublished` change meaning?** Today they observe the recorder. Once subscriptions run in-process, a published event is also *consumed*, so a consumer that republishes could make `Published` see more messages than before. Behaviour-compatible for the common case; needs a test pinning it, and possibly a doc note.
