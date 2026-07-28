# v0.1 — Skeleton

> **Goal (PRD §10): the author can build a real service with it.**

Not "the API is beautiful," not "the feature list is complete." A real service,
built by the author, in production-shaped conditions. Everything in this
milestone is judged against that sentence.

**PRD §10 sequencing note:** v0.1 must be dogfooded on a real service *before*
v0.2 starts. A framework designed in the abstract will be wrong in ways only
usage reveals — so the dogfooding is an exit criterion below, not a follow-up.

---

## Order and critical path

Each feature depends only on what precedes it. The numbering is the build
order.

```
00 spike ──▶ 03 di ──┬──▶ 04 lifecycle ──┐
                     │                    ├──▶ 06 module + bootstrap ──▶ 08 http ──▶ 09 cli ──▶ 10 new
01 errors ──▶ ───────┼──▶ 05 config ──────┘                              ▲                └──▶ 11 g module
                     │                                                   │
02 log ──────────────┘                            07 handler ────────────┘
```

**The critical path is `00 → 03 → 06 → 08 → 09 → 10`.** Everything else can be
built in parallel with it. `01 errors` is the one thing worth building first
regardless: every other package returns its types, and retrofitting an error
model is a rewrite of every signature that touches one.

| # | Feature | Why here |
|---|---|---|
| 00 | [DI approach spike](00-di-approach-spike/spec.md) | Structural and hard to reverse (PRD §13.1). Timeboxed; produces a decision, not shipping code. |
| 01 | [Errors](01-errors/spec.md) | Zero dependencies. Every other package returns these. |
| 02 | [Logging](02-log/spec.md) | Zero dependencies. Needed by lifecycle to report what it is doing. |
| 03 | [DI container](03-di/spec.md) | The engine under modules. Blocked on 00. |
| 04 | [Lifecycle](04-lifecycle/spec.md) | Ordered start/stop; the reason `fx` was rejected. |
| 05 | [Config](05-config/spec.md) | Independent of DI; the port lives in core, koanf in the submodule. |
| 06 | [Modules and bootstrap](06-module-and-bootstrap/spec.md) | `warren.New(...).Run()`. Where the core-boundary question is settled. |
| 07 | [Handler and middleware](07-app-handler/spec.md) | The one idea (architecture.md §1). No dependencies; could be built first. |
| 08 | [HTTP transport](08-transport-http/spec.md) | First adapter. Proves the handler abstraction or disproves it. |
| 09 | [CLI foundation](09-cli-foundation/spec.md) | Cobra root, template engine, golden harness, skill generation. Both generators sit on it. |
| 10 | [`warren new`](10-cli-new/spec.md) | The < 2 minute target (PRD §8). |
| 11 | [`warren g module`](11-cli-generate-module/spec.md) | Surgical AST wiring. The generator rule that is hardest to get right. |

---

## What is explicitly out of v0.1

Deferred here so it is not re-litigated mid-milestone:

| Deferred | Lands in | Note |
|---|---|---|
| Domain primitives, repositories, `UnitOfWork` | v0.2 | v0.1 services hold their own persistence |
| gRPC, brokers, outbox | v0.3 | The handler is designed so these are additive |
| `lint arch`, `doctor`, `graph`, OpenAPI | v0.4 | **The differentiators.** Do not pull them earlier and do not push them later |
| Command/query buses | v0.2 | `Handler` ships in v0.1; the bus over it is not needed to build a service |
| Echo, Gin adapters | v0.2 | chi + stdlib prove the port is router-agnostic; a third adapter only re-proves it |
| MCP server | v0.4 | Skills ship per command from v0.1 ([ADR-0008](../../docs/adr/0008-agent-integration.md)) |
| `--layout simple` (no domain layer) | v0.2 | PRD §12 risk mitigation; needs the domain layer to exist first to be optional |

---

## Exit criteria

v0.2 does not start until all of these are true. Each is checkable, not a
judgement call.

1. **A real service runs on it in production**, built by the author (PRD §10).
2. **`warren new` to a running service in under 2 minutes**, measured on a cold
   machine and recorded (PRD §8).
3. **`warren new` to a first endpoint with a passing test in under 10 minutes**,
   measured the same way (PRD §8).
4. **Framework startup overhead under 50 ms**, with a committed benchmark rather
   than a one-off measurement (PRD §8).
5. **A missing provider prints the resolution chain, the requesting file, and a
   copy-pasteable fix** — verified by a golden test on the message itself, since
   PRD §8 calls this the single most common reason DI frameworks are abandoned.
6. **Both generators have golden-file tests** (PRD §9), and both have skills
   ([ADR-0008](../../docs/adr/0008-agent-integration.md)).
7. **`make ci` green** on Linux, macOS, and Windows.
8. **The core module still has zero third-party dependencies**, verified by
   `make lint-modules` and by reading the generated service's `go.mod`.
9. **Every documented concept has a runnable, CI-compiled example** (PRD §8).

Criterion 1 is the one at risk of being quietly skipped, and it is the one the
PRD calls out by name. The others can be met by a framework nobody has used.
