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
> begins at v0.1 — see the [roadmap](prd.md#10-roadmap).

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
