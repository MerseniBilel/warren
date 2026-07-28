# Warren Delivery Plan

[prd.md](../prd.md) says what Warren is. This folder says how it gets built,
in what order, and what "done" means for each piece.

**The rule: no feature is implemented before its `spec.md` is written and
approved.** The PRD describes roughly five products (PRD §12 names scope
explosion as a high-severity risk). A spec per feature is how that gets cut
down to something buildable — the spec is where a feature is made small enough
to finish, and where it is discovered that two features are really one.

---

## How this works

1. **Write the spec.** Copy [TEMPLATE.spec.md](TEMPLATE.spec.md) into
   `plan/<milestone>/<nn>-<feature>/spec.md`. Status: `Draft`.
2. **If the spec is structural, write an ADR first** — a new dependency in a
   public API, a change to a port's shape, a module boundary, or overturning an
   existing decision. See [docs/adr/README.md](../docs/adr/README.md). An ADR
   before the spec is a design review; an ADR afterwards is paperwork.
3. **Review and approve.** Status: `Approved`. Code starts here, not before.
4. **Implement to the spec's Definition of Done** — which includes tests, docs,
   the skill if it is a CLI command, and a changelog fragment.
5. **Mark `Shipped`** and record the version it landed in.

**When the implementation diverges from the spec, the spec is corrected in the
same pull request.** A spec that no longer describes the code is worse than no
spec: it is a confident lie. This is the same rule the ADRs live under.

### Status vocabulary

| Status | Meaning |
|---|---|
| `Draft` | Being written. Do not implement against it. |
| `Approved` | Agreed. Implementation may start. |
| `In progress` | Being built. |
| `Shipped` | Merged, released, and the spec matches the code. |
| `Superseded` | Replaced. The header links to what replaced it. |
| `Cut` | Deliberately not built. The header says why — this is the record that the decision was made, not forgotten. |

### Layout

```
plan/
├── README.md              this file — process and the whole-project index
├── TEMPLATE.spec.md       the required shape of a spec
├── v0.1-skeleton/         specs written  ← current milestone
├── v0.2-ddd-core/         features named, specs deferred
├── v0.3-grpc-messaging/
├── v0.4-governance/
├── v0.5-ecosystem/
└── v1.0-stability/
```

**Only the current milestone carries written specs.** A detailed spec for a
v0.5 feature written today is fiction — it would be written against an API that
does not exist yet, and it would be rewritten before anyone read it. The later
milestone files name their features and record the constraints already known,
which is the part that is worth capturing now.

---

## Two questions block the start of v0.1

Both are recorded rather than assumed, because both are hard to reverse.

### 1. The DI approach is not settled

PRD §13.1 calls this "structural and hard to reverse" and asks for a week-1
prototype of three approaches. One of the three, `google/wire`, **is archived**
(verified 2026-07-27, [dependencies.md §3.2](../docs/dependencies.md)), so it is
a two-way comparison: the `dig` wrapper of
[ADR-0001](../docs/adr/0001-dependency-injection.md) versus a hand-written
generics container.

ADR-0001 is `Accepted`, but it names its own revisit criterion: *"the week-1
generics spike demonstrates the full feature set in under ~800 lines with better
error messages."* The spike is
[`v0.1-skeleton/00-di-approach-spike`](v0.1-skeleton/00-di-approach-spike/spec.md)
and it is timeboxed.

### 2. The core module boundary is contradictory as written

[docs/architecture.md §2](../docs/architecture.md) draws `di` inside the core
module. [dependencies.md §4](../docs/dependencies.md) places
`warren/di → go.uber.org/dig`. **Both cannot be true**, because the core module
takes no third-party dependencies — transitively included. If core's `go.mod`
requires `warren/di`, and `warren/di` requires `dig`, then a minimal Warren
service pulls `dig`, and the headline claim in
[AGENT.md invariant 1](../AGENT.md) stops being demonstrable.

This is not a documentation typo; it decides what `warren.New(...)` can accept
and therefore what every service's `main.go` imports. It is resolved in
[`06-module-and-bootstrap`](v0.1-skeleton/06-module-and-bootstrap/spec.md), and
resolving it needs an ADR that amends 0001 or 0003.

---

## v0.1 — Skeleton

**Goal (PRD §10): the author can build a real service with it.** Nine features
from the roadmap, plus the spike and the CLI foundation the two generators sit
on. Ordered so that each depends only on what precedes it.

| # | Feature | Module | Status |
|---|---|---|---|
| 00 | [DI approach spike](v0.1-skeleton/00-di-approach-spike/spec.md) | — (throwaway) | Draft |
| 01 | [Errors](v0.1-skeleton/01-errors/spec.md) | `warren/errors` | Draft |
| 02 | [Logging](v0.1-skeleton/02-log/spec.md) | `warren/log` | Draft |
| 03 | [DI container](v0.1-skeleton/03-di/spec.md) | `warren/di` | Draft |
| 04 | [Lifecycle](v0.1-skeleton/04-lifecycle/spec.md) | `warren/lifecycle` | Draft |
| 05 | [Config](v0.1-skeleton/05-config/spec.md) | `warren/config` | Draft |
| 06 | [Modules and bootstrap](v0.1-skeleton/06-module-and-bootstrap/spec.md) | `warren` | Draft |
| 07 | [Handler and middleware](v0.1-skeleton/07-app-handler/spec.md) | `warren/app` | Draft |
| 08 | [HTTP transport](v0.1-skeleton/08-transport-http/spec.md) | `warren/transport/http` | Draft |
| 09 | [CLI foundation](v0.1-skeleton/09-cli-foundation/spec.md) | `warren/cli` | Draft |
| 10 | [`warren new`](v0.1-skeleton/10-cli-new/spec.md) | `warren/cli` | Draft |
| 11 | [`warren g module`](v0.1-skeleton/11-cli-generate-module/spec.md) | `warren/cli` | Draft |

See [v0.1-skeleton/README.md](v0.1-skeleton/README.md) for the ordering
rationale, the critical path, and the milestone exit criteria.

**v0.1 must be dogfooded on a real service before v0.2 starts** (PRD §10). That
is a milestone exit criterion, not a nice-to-have.

---

## Later milestones

Features named, specs written when the milestone opens.

| Milestone | Theme | Features |
|---|---|---|
| [v0.2](v0.2-ddd-core/README.md) | DDD core | Domain primitives, `Repository`/`UnitOfWork`, Postgres driver, migrations, validation, entity/command/query generators |
| [v0.3](v0.3-grpc-messaging/README.md) | gRPC & messaging | gRPC transport, unified middleware, broker ports, Kafka + in-memory drivers, outbox, consumer generator |
| [v0.4](v0.4-governance/README.md) | Governance | `lint arch`, `doctor`, `graph`, OpenAPI, OTel, testing harness, MCP server |
| [v0.5](v0.5-ecosystem/README.md) | Ecosystem | RabbitMQ, NATS, Mongo, auth, resilience, jobs, `extract module`, presets, docs site |
| [v1.0](v1.0-stability/README.md) | Stability | API freeze, semver commitment, migration tooling, benchmarks, 3+ adopters |

**v0.4 is where the differentiators land.** PRD §10 says in bold: *do not defer
them.* Everything before it is table stakes that Kratos and go-zero already
have — the reason to choose Warren ships in v0.4, so slipping v0.1 scope into
v0.2 is not a neutral trade.

---

## What is deliberately not in this plan

- **Event sourcing.** PRD §13.7 asks whether it belongs in scope at all. Post-1.0
  at the earliest, and only with an adopter asking for it.
- **Annotation-based route codegen.** PRD §13.3 — "the thing most likely to make
  Go developers close the tab." Explicit registration ships first and stays the
  primary path regardless.
- **Fiber adapter.** Community-owned, not in the v0.x support matrix
  ([ADR-0002](../docs/adr/0002-http-router-port.md)). Its `fasthttp` base means
  it cannot implement a `net/http`-shaped port.
- **A deployment story.** PRD §1.3 — that is Encore's game.
