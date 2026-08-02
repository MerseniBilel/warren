# `github.com/MerseniBilel/warren/health` — SPEC

| | |
|---|---|
| **Status** | **Approved and implemented (2026-08-02)** — the registry, both probe verdicts, and the root-scope binding ship in core; the two HTTP routes and the gRPC health service land with their adapters. Key decisions: liveness runs NO checks (a database blip must not restart every replica); readiness is `lifecycle.Ready()` first, then critical checks, run concurrently on the probe (freshness is the product; caching is expressible later as a decorator, the reverse is not). |
| **Source** | [warren.md §2.8](../warren.md) |
| **Module** | core |
| **Mode** | Build |
| **Wraps** | — |

> **This spec is thin because its source is thin.** warren.md §2.8 is eight
> lines and one four-line interface. Everything below traces to those lines,
> to the two orderings that gate readiness (§1.3 and §2.3), or is recorded as an
> Open question. Padding it out with a plausible registry API would be
> inventing the contract rather than specifying it.

## Problem

Two questions get asked of a running service, and conflating them is how rolling
deploys drop requests:

- **Liveness** — is this process alive, or should the orchestrator kill it?
- **Readiness** — should the load balancer send this process traffic *right
  now*?

Readiness is not a property of the process; it is a property of the **lifecycle
state**. warren.md §1.3 makes it a boot step of its own — step 7, "readiness
opens: health endpoint flips green", after `OnStart` has run in dependency order
and before step 8, serve. And §1.3 step 9 closes it *first* on SIGTERM, ahead of
everything else, so the load balancer drains before anything stops. §2.3 states
the shutdown ordering: step 1 is "readiness probe → 503 ← load balancer drains
BEFORE anything stops".

warren.md calls that ordering "the reason this is built, not borrowed" and "why
`lifecycle` is Build and not `fx` — fx does not model readiness gating or drain
ordering" (AGENT.md § Two orderings you may not rearrange). This package is the
thing that ordering flips.

The second half of the problem: the checks themselves belong to the adapters. A
Postgres pool knows how to ping; a Kafka client knows how to fetch broker
metadata. The kernel knows neither exists (warren.md §1.1). So the kernel owns a
registry and an interface, and the adapters fill it.

## Goals

- Define the one interface an adapter implements to contribute a check
  (warren.md §2.8).
- Hold the registry that `/healthz`, `/readyz`, and the gRPC health service all
  read from — warren.md §2.8: "gRPC health service is served by the gRPC adapter
  **from the same registry**." One registry, several exposures.
- Gate readiness on lifecycle state, per §1.3 steps 7 and 9 and §2.3 step 1.
- Depend on the standard library only. Kernel (§1.1).

## Non-goals

- **Warren does not write the checks.** "Adapters self-register (`postgres`
  registers a ping check, `kafka` a broker-metadata check)" (§2.8). This package
  ships the interface, not implementations of it.
- **Not a metrics or alerting system.** `app.Metered()` (§3.2) and
  `warren/observability` (§7.1) own that.
- **Not an HTTP server.** The kernel "has no knowledge that HTTP, SQL, or Kafka
  exist" (§1.1) and no driver type may appear in a public signature (AGENT.md
  invariant 3). warren.md §2.8's wording says this package "exposes `/healthz`
  and `/readyz`" — see Open questions 1, because taken literally that puts HTTP
  in the kernel.

## Public API

warren.md §2.8 gives exactly one type. Reproduced with doc comments added:

```go
// Package health holds the registry of checks a service can be asked about,
// and the readiness gate that the lifecycle opens at boot and closes on
// SIGTERM.
//
// Adapters self-register their checks: postgres registers a ping, kafka a
// broker-metadata fetch. Liveness, readiness, and the gRPC health service are
// three exposures of this one registry.
package health

import "context"

// Check is one thing a service can be asked about — a database ping, a broker
// metadata fetch. Adapters implement it and self-register.
type Check interface {
	// Name identifies the check — by convention the dependency it covers,
	// such as "postgres" or "kafka". Whether the probe response body carries
	// it is undecided; see Open question 3.
	Name() string

	// Check reports whether the dependency is usable right now. It returns nil
	// when healthy, and an error naming what was unreachable otherwise. It
	// must honour ctx's deadline: a probe that hangs is a probe that fails
	// the orchestrator's timeout instead of its own.
	Check(context.Context) error
}
```

`context.Context` is `Check`'s first (and only) parameter, per AGENT.md
§ General.

**The registration surface is not in warren.md.** §2.8 says adapters
self-register but names no function to register *with*, and no type for the
registry. Adapters are separate Go modules (§1.6), so whatever it is must be
exported. This is a structural call — AGENT.md § When you are unsure reserves
those for the human — and is recorded as Open question 2 rather than invented
here.

## Behaviour

### Two probes, one registry

warren.md §2.8:

- **`/healthz`** — liveness.
- **`/readyz`** — readiness, **gated by lifecycle state**.
- **gRPC health service** — served by the gRPC adapter, from the same registry.

warren.md does not say which checks feed which probe, or whether liveness runs
any checks at all. See Open question 4.

### The readiness gate — the part that is load-bearing

From §1.3 (boot) and §2.3 (shutdown), which AGENT.md § Two orderings you may not
rearrange forbids rearranging:

```
 6  OnStart              dependency order: pool → repos → consumers → servers
 7  readiness opens      health endpoint flips green      ← this package
 8  serve
──────────────────────── SIGTERM
 9  readiness closes     LB drains BEFORE anything stops  ← this package, first
10  OnStop               reverse order, per-hook timeout, force-kill deadline
```

And §2.3's shutdown sequence in full, for the position of step 1:

```
SIGTERM
  1. readiness probe → 503        ← load balancer drains BEFORE anything stops
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack
  4. outbox relay flushes
  5. DB pools, broker connections close
  6. force-exit deadline (default 30s)
```

Three consequences that are fixed by warren.md:

1. **Readiness is red before step 7.** During boot steps 0–6 the service is not
   ready, so a probe arriving then must not report green.
2. **Readiness goes red at SIGTERM, before anything else stops.** §2.3 fixes the
   response for this state: **503**.
3. **The servers keep serving after readiness closes.** Step 2 of §2.3 is
   servers stopping, and it is *after* step 1 — so between them the process
   still answers requests, including the probe itself. Readiness closing is a
   signal to the load balancer, not a shutdown.

warren.md fixes the 503 for the not-ready state. It does not state the status
code for the ready state, nor the response bodies, nor whether a failing *check*
(as opposed to lifecycle state) also turns `/readyz` red. See Open questions 3
and 4.

### Who registers what

- `warren/persistence/postgres` — a ping check (§2.8, and §6.1 lists "a health
  check" among what the module provides).
- `warren/broker/kafka` — a broker-metadata check (§2.8).
- `warren/transport/grpc` — serves the gRPC health service from this registry.
  §4.2: "Reflection and the health service are on by default."

## Errors

**warren.md fixes no error message for this package**, and §2.8's surface
returns no error of its own — the only `error` in it is the one a `Check`
implementation returns, and that text belongs to the adapter that wrote the
check.

Two texts will exist once the registration surface is decided, and both are
currently open:

| Error | Text |
|---|---|
| A check returning non-nil | **Open**, and owned by the adapter, not by this package. AGENT.md § Errors still binds it: it must name what was unreachable. |
| Rendering a red probe | **Open.** Whether the probe response names the failing check and its error, and in what shape, is undecided. |

AGENT.md § Errors — "error messages tell the user how to fix it" — applies with
force here, because a red probe is read by an operator during an incident. A
body that says only "not ready" fails that bar; one that names which check
failed and what it could not reach meets it. The wording is the human's call and
then becomes golden-file-tested contract.

## Testing

- **Golden-file test for every error message** (AGENT.md § Testing). Once the
  probe response shape is agreed: a golden file for the healthy response, one
  for the not-ready-yet response, one for the draining (503) response, and one
  for a failing named check.
- **No Docker, no network, no sleeps** (AGENT.md § Testing). This matters more
  here than anywhere else in the kernel, because the obvious way to test a
  health check is to start a real database, and the obvious way to test a
  timeout is `time.Sleep`. Neither is allowed: checks under test are
  hand-written fakes (AGENT.md § Testing — "Hand-written fakes live in
  `warren/testing`"), and deadline behaviour is driven with an already-cancelled
  `context.Context` rather than a sleep. Real-dependency probes go behind
  `//go:build integration`.
- **The gate transitions are the tests that matter.** Readiness reports red
  before step 7, green after it, and red again the moment shutdown begins and
  before any server stops. That ordering is what AGENT.md forbids rearranging,
  so it is asserted, not assumed.
- **Allocation benchmark on the request path** — probes are requests, and a
  liveness probe can arrive every second, but they are not the hot path the
  benchmark rule targets. A benchmark of the registry read is cheap and worth
  having; the number to defend is that a probe does not allocate proportionally
  to the number of registered checks.
- `t.Parallel()`, table-driven, subtests named for behaviour.

## Definition of done

- [ ] `Check` exists exactly as in Public API, with doc comments starting with
      the identifier's name.
- [ ] The registration surface (Open question 2) is agreed, written into Public
      API above, and warren.md §2.8 amended to carry it — AGENT.md
      § Spec-driven development requires the manifest and the spec to agree.
- [ ] Open question 1 — whether this package or the HTTP adapter owns
      `/healthz` and `/readyz` — is answered, because it decides whether the
      package compiles inside the kernel at all.
- [ ] Package compiles under the core module importing the standard library
      only.
- [ ] Readiness transition tests cover: red before boot completes, green after,
      red on SIGTERM before servers stop.
- [ ] Golden files exist for every probe response shape.
- [ ] `make ci` passes (once the Makefile exists).

## Open questions

1. **Does this package expose HTTP endpoints, or does the HTTP adapter?**
   warren.md §2.8 says `warren/health` "exposes `/healthz` (liveness) and
   `/readyz`". Taken literally that puts route registration in the kernel, which
   §1.1 says "has no knowledge that HTTP ... exist[s]" and which AGENT.md
   invariant 3 forbids by way of driver types in public signatures. The very
   next sentence of §2.8 describes the correct shape for the other protocol —
   "gRPC health service is served by the gRPC **adapter** from the same
   registry" — which suggests HTTP should read the same way: this package owns
   the registry and the gate; `warren/transport/http` serves the two paths.
   Confirm, and amend §2.8's wording.
2. **What is the registration surface?** §2.8 says adapters self-register but
   names nothing to register with. A `Registry` type? A free `Register(Check)`?
   A `warren.Module` an adapter returns? Adapters are separate modules, so it
   must be exported. Not guessing.
3. **What is the ready response?** §2.3 fixes **503** for not-ready. The ready
   status code, and the body of both responses (empty? JSON per check?), are
   unstated.
4. **What is the difference between `/healthz` and `/readyz` in terms of
   checks?** Does liveness run any checks, or only report that the process is
   up? Does a registered check failing turn `/readyz` red, or is readiness
   purely the lifecycle gate? §2.8 says readiness is "gated by lifecycle state"
   and nothing about checks feeding it — but a Postgres ping check exists for
   some reason, and if it feeds nothing it is decoration.
5. **Are checks run on each probe, or on a schedule and cached?** Running a
   Postgres ping per probe means a probe every second is a query every second;
   caching means a stale answer. warren.md says neither, and the choice needs a
   timeout and interval that also do not exist in §2.8.
6. **Who flips the gate?** `warren/lifecycle` (§2.3 owns "readiness gating,
   drain") or the bootstrapper in `warren` (§2.1 owns "the run loop")? §1.3 step
   7 sits between `OnStart` and serve, so both are plausible. This decides which
   package imports which.
7. **Can a check be marked non-critical?** A degraded cache should probably not
   drain a service the way a dead database should. warren.md draws no such
   distinction, so as specified every check is critical — confirm that is
   intended.
