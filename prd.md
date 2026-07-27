# Warren — Product Requirements Document

**A DDD-first application framework and CLI for Go backends and microservices.**

| | |
|---|---|
| **Status** | Draft v0.1 — for discussion |
| **Working name** | Warren (see §2 for alternatives) |
| **Author** | — |
| **Date** | July 2026 |

---

## 1. Problem & Opportunity

### 1.1 The problem

Go is excellent for backend services and terrible at telling you how to organise one. Every team rebuilds the same scaffolding: wiring, config, graceful shutdown, HTTP + gRPC bootstrapping, message consumers, observability, folder conventions. The result is that:

- **Every Go codebase looks different.** Onboarding cost is high; there's no shared vocabulary.
- **DDD in Go is folklore.** "Clean architecture Go" blog posts and boilerplate repos are abundant, inconsistent, and unmaintained. Nothing enforces the layering once the repo is cloned.
- **Layer discipline erodes silently.** Six months in, the domain package imports `gorm`, and nothing failed the build.
- **Adding a transport is a rewrite.** A use case exposed over HTTP is rarely reusable over gRPC or as a Kafka consumer without copy-paste.
- **Messaging is always bespoke.** Outbox, retries, DLQ, idempotency, consumer lifecycle — hand-rolled per team, usually incompletely.
- **Developers arriving from NestJS/Spring hit a wall.** No modules, no DI, no generator. Productivity drops before it recovers.

### 1.2 Why now

Go microservice frameworks exist but sit at the extremes: minimal routers (Gin, Echo, Chi, Fiber) that give structure to nothing, or heavyweight microservice platforms (Kratos, go-zero, GoFrame) that are proto-first / API-DSL-first and treat architecture as a folder convention rather than an enforced contract. Sponge explicitly markets itself to NestJS refugees, which validates the demand. **Nobody owns "DDD-first, transport-agnostic, architecture-enforcing" in Go.**

### 1.3 Non-goals

Warren is explicitly **not**:

- A web framework. It composes existing routers/servers; it does not write an HTTP stack.
- An ORM. It ships repository patterns, not a query builder.
- A PaaS or deployment platform (that is Encore's game).
- A service mesh or service-discovery product.
- A magic system. **If a developer cannot read the generated code and understand it in five minutes, the feature is wrong.**

---

## 2. Naming

### 2.1 Recommendation: **Warren**

A warren is a network of connected burrows. Gophers dig burrows. The metaphor maps to modular monoliths that split into microservices, it nods to RabbitMQ, and it fits Go's naming culture (Gin, Echo, Chi, Cobra, Viper, Buffalo — short, concrete, faintly playful).

- Module path: `github.com/<org>/warren` → `go get warren.dev/warren` if the domain is acquired
- CLI binary: `warren`, short alias `wr`
- Concept naming falls out naturally: a **burrow** = a module/bounded context; the **den** = the app root. *(Optional — do not force cutesy vocabulary into the API surface. See §2.3.)*

### 2.2 Shortlist

| Name | Metaphor | Notes |
|---|---|---|
| **Warren** ★ | Connected burrows | Best cultural fit; memorable; RabbitMQ pun; low collision |
| **Cairn** | Stacked stones marking a path | Distinctive, structural, essentially unclaimed in Go |
| **Keel** | The spine a ship is built on | Very short; strong "foundation" meaning; **collides with keel.so** |
| **Girder** | Structural steel | Serious, engineering-flavoured; less warm |
| **Trellis** | A structure that guides growth | Great metaphor; collides with Roots Trellis (WordPress) |
| **Lattice** | Regular repeating structure | Clean; several dead projects hold the name |

**Avoided:** anything of the form `gonest` / `nestgo` — it's brand piggybacking, it caps the project's ceiling at "the knockoff," and those names are already taken by small existing repos.

### 2.3 Naming discipline

Cute metaphor names are fine for the *project*; they're a tax on the *API*. Public identifiers should read plainly: `warren.Module`, `warren.Provide`, `http.Controller`, `broker.Subscriber`. No `den.NewBurrow()`.

### 2.4 Pre-launch checklist

- [ ] GitHub org/repo available
- [ ] `pkg.go.dev` collision check
- [ ] Domain (`.dev` / `.io`) available
- [ ] USPTO/EUIPO word-mark search in software class
- [ ] Homebrew formula name, `npm`-adjacent squatting check for the docs site

---

## 3. Positioning

### 3.1 The one-liner

> **Warren is what you'd get if NestJS had been designed by Go developers: modules, DI, and a generator — but explicit, compile-checked, and DDD-first.**

### 3.2 Competitive landscape

| | Structure | HTTP+gRPC | DDD primitives | CLI | Messaging | Arch enforcement |
|---|---|---|---|---|---|---|
| Gin / Echo / Chi | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |
| Kratos | Layout convention | ✓ (proto-first) | Partial | ✓ | Adapters | ✗ |
| go-zero | `.api` DSL | Separate services | ✗ | ✓ (goctl) | Partial | ✗ |
| Sponge | Templates | ✓ | ✗ | ✓ | ✓ | ✗ |
| Encore | Opinionated | ✓ | ✗ | ✓ | ✓ | Partial (static analysis) |
| Uber Fx / Wire | DI only | ✗ | ✗ | ✗ | ✗ | ✗ |
| **Warren** | **Bounded contexts** | **✓ (handler-first)** | **✓ first-class** | **✓** | **✓ first-class** | **✓ enforced in CI** |

### 3.3 The three differentiators

1. **Transport-agnostic use cases.** You write a use case once. HTTP, gRPC, CLI, and message consumers are thin adapters over the same handler. This is the feature nobody else has cleanly.
2. **DDD as code, not as a folder naming convention.** Aggregate roots, domain events, repositories, unit-of-work, and the transactional outbox are framework primitives with real types.
3. **Architecture linting.** `warren lint arch` fails the build when `domain/` imports `infrastructure/`. Structure that isn't enforced decays; this is the moat.

### 3.4 Target users

- **Primary:** teams of 3–30 Go engineers building service-oriented backends who have felt structural drift.
- **Secondary:** NestJS / Spring Boot / .NET developers adopting Go, who expect a framework to exist.
- **Tertiary:** consultancies and platform teams standardising service templates across many repos.

---

## 4. Core Concepts

### 4.1 Design principles

1. **Explicit over magic.** Go developers reject frameworks that hide control flow. Every generated file is readable, editable, and yours.
2. **Errors at compile time or startup — never at request time.** The DI graph is validated during bootstrap; a missing provider fails the process on boot, not on the first 500.
3. **Codegen over reflection where feasible.** Reflection is for the container; text/template codegen is for everything the developer will read.
4. **Escape hatches everywhere.** `*http.Request`, `*grpc.Server`, and the raw broker client are always reachable. No abstraction is a prison.
5. **Optional by default.** Kafka, Postgres, gRPC, and tracing are separate Go modules. A minimal HTTP service pulls almost nothing.

### 4.2 Building blocks

| Concept | NestJS equivalent | Warren form |
|---|---|---|
| Module | `@Module` | `warren.Module` value composing providers, controllers, consumers, imports/exports |
| Provider | `@Injectable` | Any constructor function `func(deps...) (T, error)` |
| Controller | `@Controller` | Struct implementing `Routes(r Registrar)` — or generated from annotations |
| Use case | Service method | `app.Handler[Req, Res]` — the transport-agnostic unit |
| Lifecycle hooks | `OnModuleInit` | `OnStart(ctx)` / `OnStop(ctx)` registered on the lifecycle manager |
| Middleware / Guard / Interceptor | same | `Middleware`, unified across HTTP, gRPC, and consumers |
| Pipes / Validation | `class-validator` | Struct-tag validation on the DTO, run by the transport adapter |

### 4.3 The central abstraction

```go
// The unit every transport adapts to.
type Handler[Req any, Res any] interface {
    Handle(ctx context.Context, req Req) (Res, error)
}
```

One use case, three exposures:

```go
func (m *UserModule) Routes(r warren.Registrar) {
    r.HTTP.Post("/users", m.CreateUser)              // POST /users
    r.GRPC.Method("user.v1.UserService/Create", m.CreateUser)
    r.Events.On("billing.customer.created", m.CreateUser)
}
```

Adapters own their concerns: HTTP owns status-code mapping and content negotiation; gRPC owns proto marshalling and status codes; the consumer owns acks, retries, and DLQ routing. The handler owns none of it and imports none of them.

### 4.4 DDD primitives

```go
type AggregateRoot interface {
    ID() ID
    PullEvents() []DomainEvent   // drained and published by the UnitOfWork
}
```

- `domain.Entity`, `domain.ValueObject`, `domain.AggregateRoot` embeddable bases
- `domain.Event` + in-process event bus
- `Repository[T AggregateRoot, ID comparable]` port with generated adapters
- `UnitOfWork` — transaction boundary that commits state **and** the outbox atomically
- `Specification[T]` for composable query predicates
- Optional `EventStore` for event-sourced aggregates (post-1.0)

### 4.5 Error model

A single `warren.Error` with a semantic code (`NotFound`, `Conflict`, `PermissionDenied`, `Invalid`, `Internal`) that each transport maps to its own vocabulary — HTTP status, gRPC code, or nack/DLQ decision. Domain code returns semantic errors and knows nothing about 404.

---

## 5. Architecture & Project Layout

### 5.1 Generated project layout

```
myapp/
├── cmd/
│   ├── api/main.go                 # HTTP + gRPC entrypoint
│   └── worker/main.go              # consumer entrypoint
├── internal/
│   ├── modules/
│   │   └── user/
│   │       ├── domain/             # entities, VOs, events, repo INTERFACES, domain services
│   │       ├── application/        # commands, queries, handlers, DTOs, ports
│   │       ├── infrastructure/     # postgres repo, kafka publisher, external clients
│   │       ├── interfaces/         # http controller, grpc service, event consumers
│   │       └── module.go           # wiring — the only file that sees all four layers
│   ├── shared/
│   │   ├── kernel/                 # shared VOs, base types, common domain events
│   │   └── platform/               # cross-cutting infra (db pool, broker conn, cache)
│   └── config/
├── api/proto/                      # protobuf definitions (if gRPC enabled)
├── migrations/
├── deployments/                    # Dockerfile, compose, k8s manifests
├── test/                           # integration + e2e (testcontainers)
├── warren.yaml                     # framework config: layout, generators, arch rules
└── go.mod
```

### 5.2 Dependency rule (enforced)

```
interfaces ──▶ application ──▶ domain
     │              │             ▲
     └──────────────┴─────────────┘
infrastructure ──────────────────┘  (implements domain ports)

domain imports NOTHING from the other three layers.
```

`warren lint arch` reads the rules from `warren.yaml` and fails CI on violation. Rules are configurable — a team that wants a looser layout can relax them, deliberately and visibly.

### 5.3 Modular monolith → microservices

The layout is identical either way. A module has no compile-time dependency on another module's internals — only on published integration events and (optionally) a generated client. Extraction is therefore:

```bash
warren extract module billing --into ../billing-service
```

which lifts the module into a new repo, replaces in-process event subscriptions with broker subscriptions, and generates a gRPC client for the calls that crossed the boundary. **Start as a monolith, split when the pain is real** — this is the framework's opinion, and this command is how it earns it.

---

## 6. Package Inventory

Multi-module repo. Core is small; every heavy dependency is its own Go module so `go.mod` stays honest.

### 6.1 Core — `github.com/<org>/warren`

| Package | Responsibility |
|---|---|
| `warren` | App bootstrap, `Module`, `Provide`, `Invoke`, run loop |
| `warren/di` | Container (thin wrapper over `uber-go/dig`), graph validation, scopes |
| `warren/lifecycle` | Ordered start/stop, graceful shutdown, readiness gating |
| `warren/config` | Layered config: defaults → file → env → flags, struct-tag binding, validation |
| `warren/log` | `log/slog` wrapper, context-aware, correlation-ID propagation |
| `warren/errors` | Semantic errors, wrapping, stack capture, transport mapping |
| `warren/domain` | Entity, ValueObject, AggregateRoot, DomainEvent, Specification |
| `warren/app` | `Handler[Req,Res]`, command/query buses, decorators (logging/tx/retry/metrics) |
| `warren/validate` | Struct-tag validation over `go-playground/validator` |
| `warren/health` | Liveness/readiness probes, dependency checks |

### 6.2 Transports

| Module | Notes |
|---|---|
| `warren/transport/http` | Router-agnostic; **`net/http` + `chi` default**, Gin/Echo/Fiber adapters |
| `warren/transport/grpc` | Server + client, interceptor chain, reflection, health service |
| `warren/transport/gateway` | Optional grpc-gateway bridging for proto-first teams |
| `warren/openapi` | OpenAPI 3.1 generation from handler + DTO metadata |

### 6.3 Messaging

| Module | Notes |
|---|---|
| `warren/broker` | Ports: `Publisher`, `Subscriber`, `Message` envelope (CloudEvents-compatible), middleware |
| `warren/broker/kafka` | `franz-go` or `segmentio/kafka-go`; consumer groups, partition-aware handling |
| `warren/broker/rabbitmq` | `amqp091-go`; exchanges, routing keys, quorum queues |
| `warren/broker/nats` | JetStream |
| `warren/broker/memory` | In-process — the default for tests and modular monoliths |
| `warren/outbox` | Transactional outbox + relay (poller and CDC modes) |
| `warren/inbox` | Idempotent consumption / dedupe store |

Cross-cutting messaging concerns live in `warren/broker` middleware and apply to every driver: retry with backoff, DLQ routing, idempotency, tracing propagation, panic recovery, concurrency limits, graceful drain on shutdown.

### 6.4 Persistence

| Module | Notes |
|---|---|
| `warren/persistence` | `Repository[T,ID]` port, `UnitOfWork`, transaction context |
| `warren/persistence/postgres` | `pgx`, migrations via `goose`/`atlas`, outbox table |
| `warren/persistence/mysql` | |
| `warren/persistence/mongo` | |
| `warren/persistence/redis` | Cache + distributed lock |

### 6.5 Cross-cutting

| Module | Notes |
|---|---|
| `warren/observability` | OpenTelemetry traces/metrics/logs, wired across all transports and brokers by default |
| `warren/auth` | JWT/OIDC guards, RBAC policy hooks |
| `warren/resilience` | Circuit breaker, retry, rate limit, bulkhead, timeout |
| `warren/jobs` | Cron + background workers on the same lifecycle |
| `warren/testing` | Module test harness, fakes, testcontainers helpers, event assertions |
| `warren/cli` | Cobra-based generator |

### 6.6 Key dependency decisions

| Decision | Choice | Rationale |
|---|---|---|
| DI mechanism | `uber-go/dig` wrapped, **not** raw `fx` | Lifecycle semantics stay ours; fx's error messages are a known DX liability |
| Codegen vs reflection for routes | Explicit registration **primary**, annotation codegen **optional** | Explicitness is the price of Go community adoption |
| HTTP router | `chi` on `net/http` | stdlib-compatible, minimal, swappable |
| Config | Own layered loader, not Viper | Viper's dependency weight and global state are a poor fit |
| Logging | `log/slog` | stdlib; no opinion imposed on users |
| Repo layout | Multi-module | Keeps core `go.mod` clean; matches OTel's proven approach |

---

## 7. CLI Specification

Cobra-based. Every generator is a `text/template` embedded via `embed.FS` and overridable per project via `warren.yaml`.

### 7.1 Scaffolding

```bash
warren new myapp \
  --module github.com/acme/myapp \
  --layout modular-monolith \        # | microservice | library
  --transport http,grpc \
  --db postgres \
  --broker kafka \
  --with observability,auth,docker

warren new myapp --interactive       # guided prompts
warren new myapp --preset acme/backend-standard   # org preset from a git repo
```

### 7.2 Generators

```bash
warren generate module      user                                  # alias: warren g mo
warren g entity             user/User --fields "email:Email,name:string,status:Status"
warren g value-object       user/Email --validate
warren g event              user/UserRegistered --publish
warren g command            user/RegisterUser --transport http,grpc
warren g query              user/GetUserByID --transport http
warren g repository         user/User --driver postgres
warren g controller         user --transport http
warren g consumer           user --event billing.customer.created --broker kafka
warren g migration          add_users_table
warren g proto              user --service UserService
warren g client             billing                               # typed cross-service client
```

Every generator supports `--dry-run` (print the diff) and `--force`.

### 7.3 Development workflow

```bash
warren dev                  # hot reload, watches templates + protos + migrations
warren build                # multi-target build
warren test --module user   # scoped tests
warren run worker           # run a specific entrypoint
```

### 7.4 Governance — the differentiating commands

```bash
warren lint arch            # dependency-rule violations → non-zero exit
warren doctor               # convention drift, missing wiring, dead providers
warren graph modules        # module dependency graph (DOT/Mermaid/SVG)
warren graph di             # DI graph, unused and ambiguous providers
warren graph events         # who publishes / who consumes each event
warren explain di UserRepo  # trace how a dependency resolves
```

### 7.5 Evolution

```bash
warren add kafka            # add a driver to an existing project
warren extract module billing --into ../billing-service
warren upgrade              # migrate templates & config across framework versions
```

### 7.6 Generator behaviour rules

1. **Idempotent.** Re-running a generator over existing files is a no-op or a clearly-marked diff.
2. **Surgical wiring.** Registering into `module.go` is an AST edit, not a marker-comment hack.
3. **Never overwrite silently.** Conflicts prompt or fail.
4. **Generated code is committed and owned.** No `.gen.go` files the developer isn't allowed to touch, except protobuf output.
5. **Every generator is a template the user can fork.** `warren templates eject` copies the built-ins into the repo.

---

## 8. Developer Experience Targets

| Metric | Target |
|---|---|
| Zero → running service with HTTP + gRPC + Postgres | < 2 minutes |
| Zero → first endpoint with a passing test | < 10 minutes |
| Add a full CRUD module | 1 command + editing business logic only |
| Cold `go build` on a 20-module project | < 30 s |
| Framework startup overhead | < 50 ms |
| Docs: every concept has a runnable example | 100 % |

**Error message standard.** A missing provider prints the resolution chain, the file that requested it, and a copy-pasteable fix. This is a first-class feature, not polish — it is the single most common reason DI frameworks are abandoned.

---

## 9. Testing Story

- `warren/testing` boots a module in isolation with fakes substituted by interface.
- In-memory broker is the default in tests; the same consumer code runs against it.
- `testcontainers-go` helpers for Postgres/Kafka/Rabbit integration tests, opt-in by build tag.
- Domain-event assertions: `assert.EventPublished[UserRegistered](t, aggregate)`.
- Golden-file tests for every CLI generator — templates break silently otherwise.
- Contract tests between modules generated from published event schemas.

---

## 10. Roadmap

### v0.1 — Skeleton (weeks 1–6)
DI container, module system, lifecycle, config, logging, errors, HTTP transport, `warren new`, `warren g module`. **Goal:** the author can build a real service with it.

### v0.2 — DDD core (weeks 7–12)
Domain primitives, repository + UnitOfWork, Postgres driver, migrations, validation, entity/command/query generators.

### v0.3 — gRPC & messaging (weeks 13–20)
gRPC transport, unified middleware, broker ports + Kafka + in-memory drivers, outbox, consumer generator.

### v0.4 — Governance (weeks 21–26)
`lint arch`, `doctor`, `graph`, OpenAPI generation, OTel wiring, testing harness. **The differentiators land here — do not defer them.**

### v0.5 — Ecosystem (weeks 27–36)
RabbitMQ + NATS, Mongo, auth, resilience, jobs, `extract module`, presets, docs site.

### v1.0 — Stability
API freeze, semantic-versioning commitment, migration tooling, 3+ production adopters, benchmark suite.

**Sequencing note:** v0.1 must be dogfooded on a real service before v0.2 starts. A framework designed in the abstract will be wrong in ways only usage reveals.

---

## 11. Success Metrics

| Horizon | Metric |
|---|---|
| 3 months | Framework runs one real production service; core API stable enough to write docs against |
| 6 months | 500 GitHub stars; 10 external projects; 5 external contributors |
| 12 months | 2,500 stars; 3 public case studies; a conference talk; measurable pkg.go.dev import count |
| Qualitative | Someone writes "we chose Warren over Kratos because…" without prompting |

---

## 12. Risks

| Risk | Severity | Mitigation |
|---|---|---|
| **Go community rejects "frameworks" on principle** | High | Lead with the CLI and arch-linting as standalone value; make every layer optional; never hide control flow |
| **Scope explosion** — this PRD describes ~5 products | High | v0.1 must be shippable and useful alone. Ruthless deferral. Drivers can be community-owned |
| **Kratos/go-zero/Sponge already "good enough"** | Medium | Differentiate on DDD primitives + arch enforcement + transport-agnostic handlers, not on feature count |
| **Codegen-driven frameworks age badly** | Medium | Generated code is owned by the user, not regenerated; `warren upgrade` migrates rather than overwrites |
| **Maintenance burden of N broker/DB drivers** | Medium | Stable port interfaces + contract test suite; community drivers with a certification checklist |
| **DDD is a hard sell to teams that want CRUD** | Medium | `--layout simple` mode that skips the domain layer; let teams grow into it |
| **Bus factor of 1** | High | Docs and contribution guide from day one; design for external driver contributions early |

---

## 13. Open Questions

1. **DI: `dig` wrapper vs. generics-based explicit registration vs. `wire` codegen?** Leaning `dig` + generic sugar. Prototype all three in week 1 — this decision is structural and hard to reverse.
2. **Proto-first or handler-first for gRPC?** Recommendation: handler-first with optional proto generation. Proto-first teams are already served by Kratos.
3. **Annotation-based route codegen — ship it or resist it?** It's the closest thing to decorators, and it's also the thing most likely to make Go developers close the tab.
4. **Is `extract module` realistic or a demo?** It's the most compelling feature in the pitch and the most likely to be half-true. Prototype early or cut from marketing.
5. **Modular monolith vs. microservice as the default `new` layout?** Monolith is the better default; verify against user interviews.
6. **Licence and governance model** — MIT vs. Apache-2.0; solo maintainer vs. org from day one.
7. **Does event sourcing belong in scope at all,** or is it a v2 module that risks defining the project as niche?

---

## 14. Appendix — Illustrative Code

### 14.1 Module definition

```go
package user

func Module() warren.Module {
    return warren.NewModule("user",
        warren.Imports(shared.DatabaseModule, shared.BrokerModule),
        warren.Providers(
            NewUserService,
            postgres.NewUserRepository,   // provides domain.UserRepository
        ),
        warren.Controllers(NewUserController),
        warren.Consumers(NewBillingConsumer),
        warren.Exports[domain.UserRepository](),
    )
}
```

### 14.2 Use case

```go
type RegisterUser struct {
    Email string `json:"email" validate:"required,email"`
    Name  string `json:"name"  validate:"required,min=2"`
}

type RegisterUserHandler struct {
    users domain.UserRepository
    uow   persistence.UnitOfWork
}

func (h *RegisterUserHandler) Handle(ctx context.Context, cmd RegisterUser) (UserDTO, error) {
    email, err := domain.NewEmail(cmd.Email)
    if err != nil {
        return UserDTO{}, errors.Invalid("email", err)
    }
    if taken, _ := h.users.ExistsByEmail(ctx, email); taken {
        return UserDTO{}, errors.Conflict("user already exists")
    }

    u := domain.NewUser(email, cmd.Name)   // raises UserRegistered internally

    // Commits the aggregate and the outbox row in one transaction.
    if err := h.uow.Do(ctx, func(ctx context.Context) error {
        return h.users.Save(ctx, u)
    }); err != nil {
        return UserDTO{}, err
    }
    return toDTO(u), nil
}
```

No `http`, no `grpc`, no `kafka` import anywhere in that file. That's the whole point.

### 14.3 Bootstrap

```go
func main() {
    warren.New(
        config.Module,
        observability.Module,
        user.Module(),
        billing.Module(),
        http.Server(http.Port(8080)),
        grpc.Server(grpc.Port(9090)),
        kafka.Broker(kafka.Brokers("localhost:9092")),
    ).Run()
}
```

---

*Draft for discussion. Sections 2, 6, and 7 are the ones most likely to change after prototyping.*
