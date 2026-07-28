# v0.4 — Governance

> **PRD §10, in bold: "The differentiators land here — do not defer them."**

**Specs are not written yet.** They get written when v0.3 ships.

Everything before this milestone is table stakes. Kratos, go-zero, and Sponge
all have modules, transports, and generators. **Nothing else in the Go ecosystem
enforces architecture in CI** — PRD §3.3 calls it the moat, and
[ADR-0004](../../docs/adr/0004-architecture-enforcement.md) commits to shipping
it at v0.4 rather than "later."

The risk to manage here is not technical. It is that v0.1–v0.3 overrun and this
slips, at which point Warren is a slightly different Kratos.

---

## Features

| # | Feature | Module | Scope |
|---|---|---|---|
| 01 | `warren lint arch` | `warren/cli` | The moat. `golang.org/x/tools/go/packages`; rules from `warren.yaml`; non-zero exit on violation. Zero-config correctness; relaxations visible in the config file. |
| 02 | `warren doctor` | `warren/cli` | Convention drift, missing wiring, dead providers. |
| 03 | `warren graph modules` | `warren/cli` | Module dependency graph as DOT, Mermaid, or SVG. |
| 04 | `warren graph di` | `warren/cli` | The DI graph, unused and ambiguous providers. Reads `di.Graph` from [v0.1 #03](../v0.1-skeleton/03-di/spec.md) §4 — which is why that returns plain data. |
| 05 | `warren graph events` | `warren/cli` | Who publishes and who consumes each event. |
| 06 | `warren explain di` | `warren/cli` | Trace how one dependency resolves. |
| 07 | OpenAPI 3.1 generation | `warren/openapi` | From handler and DTO metadata. |
| 08 | OpenTelemetry | `warren/observability` | Traces, metrics, and logs wired across every transport and broker by default. |
| 09 | Testing harness | `warren/testing` | Boot a module in isolation with fakes substituted by interface; testcontainers helpers; `assert.EventPublished[T]`. |
| 10 | MCP server | `warren/mcp` | Read-mostly resources over the project's structure ([ADR-0008](../../docs/adr/0008-agent-integration.md)). |

## Constraints already known

- **`warren lint arch` must be correct with zero configuration** (ADR-0004). A
  linter that needs tuning before it is right gets turned off in week two.
- **Warren dogfoods it.** If Warren's own CI cannot run `warren lint arch`
  against Warren, the feature is not finished
  ([architecture.md §3](../../docs/architecture.md)).
- **`fe3dback/go-arch-lint` was reviewed for design and not adopted**
  ([dependencies.md §3.11](../../docs/dependencies.md)): it is standalone and
  YAML-driven, where Warren's rules derive from module structure the framework
  already knows. Its existence confirms the niche is real and under-served.
- **The MCP server is read-mostly.** Mutation stays in the CLI, so there is one
  code path that writes files and one place golden tests cover
  ([agent-integration.md §3](../../docs/agent-integration.md)). Roots, sampling,
  and logging are deprecated in the 2026-07-28 MCP spec — do not build on them.
- **OTel is wired by default, not opt-in** (PRD §6.5). That makes startup cost
  and dependency weight a design constraint, not an afterthought.
- **`warren/testing` wraps testcontainers**, which is still v0 — a registered
  API-churn risk, which is precisely why it is wrapped.

## To settle when this milestone opens

1. **What is the default rule set for `lint arch`,** and is it derivable from the
   directory layout alone? If it needs a `warren.yaml` block to be useful, the
   zero-config requirement is not met.
2. **How does `lint arch` handle generated code and third-party imports** without
   a wall of exclusions? The exclusion list is where architecture linters go to
   die.
3. **Does OpenAPI generation need annotations?** If it does, PRD §13.3's question
   about annotation codegen has been answered by the back door, and that should
   be a deliberate decision rather than a consequence.
4. **Does the MCP server need the CLI installed**, or does it link the packages
   directly? Linking is faster and duplicates the CLI's own resolution logic.
