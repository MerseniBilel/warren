<div align="center">

# Warren

**A DDD-first application framework and CLI for Go backends and microservices.**

*What you'd get if NestJS had been designed by Go developers: modules, DI, and a
generator — but explicit, compile-checked, and DDD-first.*

[![Go Reference](https://pkg.go.dev/badge/github.com/MerseniBilel/warren.svg)](https://pkg.go.dev/github.com/MerseniBilel/warren)
[![Go Version](https://img.shields.io/badge/go-1.26-00ADD8)](https://go.dev/doc/devel/release)
[![Licence](https://img.shields.io/badge/licence-Apache--2.0-blue)](LICENSE)

</div>

---

> [!WARNING]
> **Warren is pre-alpha and not usable yet.** The repository currently holds the
> product definition, architecture decisions, and quality gates. Framework code
> begins at v0.1 — see the [roadmap](#roadmap).

## The problem

Go is excellent for backend services and tells you nothing about how to organise
one. Every team rebuilds the same scaffolding, every codebase looks different,
and six months in the `domain` package imports `gorm` and nothing failed the
build.

Existing frameworks sit at the extremes: minimal routers that give structure to
nothing, or proto-first microservice platforms that treat architecture as a
folder convention. **Nobody owns "DDD-first, transport-agnostic,
architecture-enforcing" in Go.**

## The three ideas

**1. Transport-agnostic use cases.** Write a use case once. HTTP, gRPC, CLI, and
message consumers are thin adapters over the same handler.

```go
func (m *UserModule) Routes(r warren.Registrar) {
    r.HTTP.Post("/users", m.CreateUser)
    r.GRPC.Method("user.v1.UserService/Create", m.CreateUser)
    r.Events.On("billing.customer.created", m.CreateUser)
}
```

The handler imports no transport package. That is the whole point.

**2. DDD as real types**, not folder names. Aggregate roots, domain events,
repositories, unit of work, and the transactional outbox are framework
primitives with real types.

**3. Architecture enforced in CI.**

```bash
$ warren lint arch
internal/modules/user/domain/user.go:8:2: domain must not import infrastructure
  rule: layers.domain.may_import = []
  fix:  define the port in domain/ and implement it in infrastructure/
```

Structure that isn't enforced decays. This is the moat.

## What Warren is not

- **Not a web framework.** It composes routers; it does not write an HTTP stack.
- **Not an ORM.** Repository patterns, not a query builder.
- **Not a PaaS.** That's Encore's game.
- **Not magic.** If you cannot read the generated code and understand it in five
  minutes, the feature is wrong.

## Choose your own router

The HTTP port is shaped on `net/http`, so the whole middleware ecosystem works
unchanged. Pick what you already know:

| Router | Status | Notes |
|---|---|---|
| **chi** | Recommended default | Zero dependencies, 100% `net/http` |
| `net/http` | Supported | Zero HTTP dependencies at all |
| Echo | Supported | |
| Gin | Supported | |
| Fiber | Community | fasthttp-based, so it cannot share `net/http` middleware |

The reasoning, with the audit behind it, is in
[ADR-0002](docs/adr/0002-http-router-port.md).

## Project status

| | |
|---|---|
| **Stage** | Foundation — conventions and decisions in place, no framework code |
| **Go** | 1.26 (tracks the current major, [ADR-0007](docs/adr/0007-go-version-policy.md)) |
| **Licence** | Apache-2.0 |
| **Layout** | Multi-module; core has **zero** third-party dependencies |

### What exists today

- [Product requirements](prd.md) — the full specification
- [Architecture](docs/architecture.md) — module graph, layers, ports
- [Dependency audit](docs/dependencies.md) — every package, with evidence
- [Decision records](docs/adr/) — including what was rejected, and why
- [Testing strategy](docs/testing.md)
- [Agent integration](docs/agent-integration.md) — skills and MCP server
- Quality gates: `.golangci.yml`, CI matrix, module-invariant checks

## Roadmap

Six milestones. Week numbers are from [PRD §10](prd.md#10-roadmap) and assume one
maintainer; treat them as ordering, not as dates.

### v0.1 — Skeleton · weeks 1–6 · **in progress**

> The author can build a real service with it. Twelve specs, all `Draft`.

- [ ] [DI approach spike](plan/v0.1-skeleton/00-di-approach-spike/spec.md) — timeboxed, settles PRD §13.1
- [ ] [Errors](plan/v0.1-skeleton/01-errors/spec.md) — `warren/errors`, semantic codes every transport maps
- [ ] [Logging](plan/v0.1-skeleton/02-log/spec.md) — `warren/log`, six functions over `log/slog`
- [ ] [DI container](plan/v0.1-skeleton/03-di/spec.md) — `Provide[T]`, `Resolve[T]`, scopes, `Graph`
- [ ] [Lifecycle](plan/v0.1-skeleton/04-lifecycle/spec.md) — ordered start, reverse stop, graceful shutdown
- [ ] [Config](plan/v0.1-skeleton/05-config/spec.md) — port in core, loader in a submodule
- [ ] [Modules and bootstrap](plan/v0.1-skeleton/06-module-and-bootstrap/spec.md) — `warren.New`, the boot sequence
- [ ] [Handler and middleware](plan/v0.1-skeleton/07-app-handler/spec.md) — `app.Handler[Req, Res]`, the one idea
- [ ] [HTTP transport](plan/v0.1-skeleton/08-transport-http/spec.md) — port, contract suite, chi and `net/http` adapters
- [ ] [CLI foundation](plan/v0.1-skeleton/09-cli-foundation/spec.md) — generator engine, golden tests, skill generation
- [ ] [`warren new`](plan/v0.1-skeleton/10-cli-new/spec.md) — 0 → running service in under two minutes
- [ ] [`warren g module`](plan/v0.1-skeleton/11-cli-generate-module/spec.md) — generate and wire a module

### v0.2 — DDD core · weeks 7–12

> DDD primitives are real types, not a folder convention.

- [ ] Domain primitives — `Entity`, `ValueObject`, `AggregateRoot`, `Event`, `Specification[T]`
- [ ] In-process event bus
- [ ] `Repository[T, ID]` and `UnitOfWork` ports
- [ ] Postgres driver (`pgx` v5) and migrations
- [ ] Validation, command and query buses
- [ ] Generators: `g entity`, `g value-object`, `g event`, `g command`/`g query`, `g repository`
- [ ] Echo and Gin adapters — the test of whether the HTTP port is router-agnostic
- [ ] `--layout simple` — a module with no domain layer

### v0.3 — gRPC & messaging · weeks 13–20

> The transport-agnostic claim is proved, not asserted.

- [ ] gRPC transport — handler-first, optional proto generation
- [ ] Middleware shared across HTTP, gRPC, and consumers
- [ ] Broker ports, in-memory driver first, then Kafka (`franz-go`)
- [ ] Broker middleware — retry, DLQ, idempotency, tracing, graceful drain
- [ ] Transactional outbox and inbox
- [ ] `warren g consumer`, `warren g proto`, `cmd/worker` entrypoint

### v0.4 — Governance · weeks 21–26 · **the differentiators**

> PRD §10, in bold: *the differentiators land here — do not defer them.*

- [ ] `warren lint arch` — the moat; correct with zero configuration
- [ ] `warren doctor` — convention drift, missing wiring, dead providers
- [ ] `warren graph modules` / `graph di` / `graph events` / `explain di`
- [ ] OpenAPI 3.1 generation
- [ ] OpenTelemetry wired across every transport and broker by default
- [ ] Testing harness — boot a module in isolation, `assert.EventPublished[T]`
- [ ] MCP server — read-mostly resources over the project's structure

### v0.5 — Ecosystem · weeks 27–36

> Warren is usable by people who are not its author.

- [ ] Drivers: RabbitMQ, NATS, Mongo, MySQL, Redis
- [ ] Auth, resilience, and jobs modules
- [ ] `warren extract module` — lift a module into its own repository
- [ ] Presets and `warren dev`
- [ ] Documentation site
- [ ] Driver certification checklist for community-owned drivers

### v1.0 — Stability

> The API is frozen and the project is safe to depend on.

- [ ] API freeze — every exported identifier reviewed for whether it should be exported at all
- [ ] Semantic-versioning commitment and a stated deprecation window
- [ ] `warren upgrade` — migrates templates and config, rather than overwriting
- [ ] Benchmark suite, so the PRD §8 targets become regressions rather than memories
- [ ] Three or more production adopters, not counting the author's
- [ ] Governance — PRD §12 names bus factor 1 as a high risk

---

Two rules shape that order, and both are easier to state now than to defend
later:

- **v0.1 is dogfooded on a real service before v0.2 starts.** PRD §10 makes this
  an exit criterion, not a nice-to-have. Domain primitives designed against an
  imagined service are wrong in ways only usage reveals.
- **v0.4 does not slip.** Everything before it is table stakes that Kratos and
  go-zero already ship. The reason to choose Warren arrives in v0.4, so moving
  v0.1 scope into v0.2 is not a neutral trade.

### Two questions block the start of v0.1

Recorded rather than assumed, because both are hard to reverse:

- [ ] **Which DI approach wins** — the `dig` wrapper of
      [ADR-0001](docs/adr/0001-dependency-injection.md) or a hand-written
      generics container. PRD §13.1 asks for three prototypes; `google/wire`
      turned out to be archived, so it is a two-way comparison.
- [ ] **Where the core module boundary actually falls.**
      [docs/architecture.md](docs/architecture.md) puts `di` inside the core
      module; [docs/dependencies.md](docs/dependencies.md) maps
      `warren/di → dig`. Both cannot be true while core has zero third-party
      dependencies, and the answer decides what every generated `main.go`
      imports.

**Every feature is specified before it is built.** The spec is written and
approved first, and corrected in the same pull request whenever the code
diverges from it. The index, the per-milestone detail, and the twelve v0.1
specs are in [plan/](plan/).

### Not on the roadmap

Event sourcing (PRD §13.7 — post-1.0 at the earliest, and only if an adopter
asks), annotation-based route codegen (PRD §13.3 — explicit registration stays
the primary path), a Fiber adapter (community-owned; `fasthttp` cannot implement
a `net/http`-shaped port), and a deployment story.

## Contributing

Warren enforces its own rules — `make ci` runs every gate CI runs.

```bash
git clone https://github.com/MerseniBilel/warren && cd warren
make tools && make ci
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. Two rules catch people out:

- **The core module takes no third-party dependencies. Ever.**
- **No dependency is adopted without an audit row** in
  [docs/dependencies.md](docs/dependencies.md). That audit found two
  widely-recommended packages archived — `google/wire` and `git-chglog` —
  neither of which says so in its README.

Working with an AI agent? [AGENT.md](AGENT.md) is written for it.

## Licence

[Apache-2.0](LICENSE) © The Warren Authors
