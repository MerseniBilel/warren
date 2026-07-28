<div align="center">

# Warren

**A DDD-first application framework and CLI for Go backends and microservices.**

*What you'd get if NestJS had been designed by Go developers: modules, DI, and a
generator — but explicit, compile-checked, and DDD-first.*

[![Go Version](https://img.shields.io/badge/go-1.26-00ADD8)](https://go.dev/doc/devel/release)
[![Licence](https://img.shields.io/badge/licence-Apache--2.0-blue)](LICENSE)
[![Status](https://img.shields.io/badge/status-pre--alpha-red)](docs/roadmap.md)

</div>

---

> [!WARNING]
> **Nothing is implemented yet.** This repository holds the product definition,
> the architecture, and the roadmap. There is no Go code, no release, and
> nothing to install. Framework code begins at v0.1 — see the
> [roadmap](docs/roadmap.md).

## The problem

Go is excellent for backend services and tells you nothing about how to organise
one. Every team rebuilds the same scaffolding, every codebase looks different,
and six months in the `domain` package imports `gorm` and nothing failed the
build.

Existing frameworks sit at the extremes: minimal routers that give structure to
nothing, or proto-first microservice platforms that treat architecture as a
folder convention. **Nobody owns "DDD-first, transport-agnostic,
architecture-enforcing" in Go.**

## What it will look like

```bash
warren new shop              # a running service, in under two minutes
warren g module orders       # a layered module, wired into main.go for you
```

Write the use case **once**:

```go
func (h *CreateOrder) Handle(ctx context.Context, req CreateOrderReq) (Order, error) {
    // no net/http, no grpc, no kafka — that is the whole point
}
```

Expose it wherever you need it:

```go
func (m *OrdersModule) Routes(r warren.Registrar) {
    r.HTTP.Post("/orders", m.CreateOrder)
    r.GRPC.Method("order.v1.OrderService/Create", m.CreateOrder)
    r.Events.On("billing.customer.created", m.CreateOrder)
}
```

And keep the structure honest:

```console
$ warren lint arch
internal/modules/orders/domain/order.go:8:2: domain must not import infrastructure
  rule: layers.domain.may_import = []
  fix:  define the port in domain/ and implement it in infrastructure/
```

## The three ideas

1. **Transport-agnostic use cases.** One `Handler[Req, Res]`. HTTP, gRPC, CLI,
   and message consumers are thin adapters over it, and the handler imports none
   of them.
2. **DDD as real types**, not folder names. Aggregate roots, domain events,
   repositories, unit of work, and the transactional outbox are framework
   primitives.
3. **Architecture enforced in CI.** Structure that isn't enforced decays. This is
   the moat — nothing else in the Go ecosystem does it.

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
| Echo · Gin | Supported | |
| Fiber | Community | fasthttp-based, so it cannot share `net/http` middleware |

## How this project is built

Three rules, in order:

1. **The architecture is agreed first.** [docs/architecture.md](docs/architecture.md) is
   the map, and it changes deliberately rather than by drift.
2. **Every feature gets a spec before it gets code** — the problem, the public
   API as Go, every error message, and a definition of done. It lives as
   `SPEC.md` in the package it describes, so that code and spec move in one
   diff. If the code ends up differing, the spec is corrected in the same pull
   request. [`errors/SPEC.md`](errors/SPEC.md) is the worked example.
3. **Decisions are researched and agreed, not prototyped.** No spikes, no
   throwaway branches. Read the evidence, weigh the options, decide, build once.

## Where things are

| File | What it holds |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Module map, what we write vs. what we wrap, the dependency rule, lifecycle, errors |
| [docs/assets/](docs/assets/) | `architecture.puml`, the source for both diagrams, and the generated [module map](docs/assets/architecture.png) and [lifecycle](docs/assets/lifecycle.png) |
| [docs/roadmap.md](docs/roadmap.md) | v0.1 → v1.0, as checkboxes, with v0.1's exit criteria |
| `<package>/SPEC.md` | One per package: the problem, the public API as Go, behaviour, and a definition of done — e.g. [errors/SPEC.md](errors/SPEC.md) |
| [AGENT.md](AGENT.md) | The rules — invariants, conventions, process. Written for AI agents, accurate for humans |
| [CLAUDE.md](CLAUDE.md) | Claude Code specifics; points at AGENT.md for everything else |
| [Makefile](Makefile) | `make ci` runs every gate CI runs |
| [.golangci.yml](.golangci.yml) | The quality gate: every linter on, each exception justified in place |
| [scripts/](scripts/) | The checks `make` runs — chiefly `check-module-rules.sh`, which enforces the invariants below |

## The invariants

Not conventions — build failures:

1. **The core module has zero third-party dependencies.** Permanently. A port
   goes in core; its implementation goes in a submodule.
2. **No driver type in a public signature** — no `*chi.Mux`, no `*pgx.Conn`, no
   `*kgo.Client` — and no third-party DI container anywhere: Warren writes its
   own.
3. **`domain` imports nothing** from the other layers.
4. **Handlers import no transport package.**
5. **No committed `replace` directive.**

```bash
make lint-modules     # checks 1, 2 and 5 today
make ci               # everything CI runs
```

## Licence

[Apache-2.0](LICENSE) © The Warren Authors
