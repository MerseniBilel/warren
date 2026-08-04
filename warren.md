# Warren — Package Manifest & Usage Guide

**Companion to the PRD. One entry per package: what it owns, what it wraps, what it exposes, and how it's used.**

| | |
|---|---|
| **Status** | Draft v0.1 |
| **Scope** | Runtime packages + CLI. Excludes docs site and examples repo. |
| **Date** | July 2026 |

---

## How to Read This Document

Each package entry has a fixed shape:

- **Path** — the Go import path
- **Module** — which `go.mod` it belongs to (multi-module repo)
- **Wraps** — third-party dependency, if any
- **Owns** — the responsibility, stated in one line
- **Surface** — the public API a user actually touches
- **Usage** — real code

**Mode** classifies each dependency decision:

| Mode | Meaning |
|---|---|
| **Build** | Warren owns it outright. No third-party equivalent is acceptable. |
| **Wrap** | Good library, but users must not import it directly. Port interface in front, raw handle available as escape hatch. |
| **Vendor** | Imported and used directly. Swapping it would be a breaking change we accept. |

**The wrap rule:** if changing a library would force edits across hundreds of user files, it must be behind a port.

---

## 1. Architecture

This section describes the internal design of the framework itself — not the layout of applications built with it (that is `warren new`'s concern, documented separately).

**The governing constraint: Warren obeys its own dependency rule.** If the kernel imported `net/http`, the architecture-linting pitch would be a lie. Every boundary below is enforced by the same `warren lint arch` that ships to users.

### 1.1 Four rings

```
┌─────────────────────────────────────────────────────────┐
│  TOOLING            warren/cli                          │  build-time only
│                     templates · AST editor · analyzer   │  never in a service go.mod
└─────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────┐
│  ADAPTERS   transport/http   transport/grpc             │  separate go modules
│             broker/kafka     broker/memory              │  never import each other
│             persistence/postgres    observability       │
└─────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────┐
│  CONTRACTS  app.Handler   broker.Publisher/Subscriber   │  ports & shared types
│             persistence.Repository/UnitOfWork           │  implementation-free
│             transport.Registrar   domain.*              │  (one exception: §3.5)
└─────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────┐
│  KERNEL     warren · di · lifecycle · config            │  stdlib + dig only
│             log · errors · validate · health            │
└─────────────────────────────────────────────────────────┘
```

Dependencies point downward only.

- **Kernel** has no knowledge that HTTP, SQL, or Kafka exist.
- **Contracts** are pure interfaces, so an adapter and a user's domain package can both depend on `broker.Publisher` without ever meeting. This is what makes §3 packages implementation-free. One deliberate exception: the three protocol registrars of §3.5 are **concrete structs with generic methods** — Go 1.27 permits type parameters on methods of concrete types but never on interface methods, so §3.5's API is only expressible this way. They remain driver-free.
- **Adapters** are leaves. `broker/kafka` and `persistence/postgres` are mutually invisible — which is precisely what makes them independently versionable and community-ownable.
- **Tooling** is a one-way street: the CLI imports the runtime to analyse it; the runtime never imports the CLI.

### 1.2 Module encapsulation — the hard part

This is where most Go DI frameworks stop short. `dig` and `fx` give you one global type-keyed container: register `*UserRepo` and every component in the process can inject it. Nest's module system is meaningfully different — a provider is private unless exported, and importing is explicit.

Warren gets this via **scoped child containers**:

```
                    root container
                (config, logger, tracer)
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
   platform scope    user scope        billing scope
   ├ postgres.DB     ├ *UserService    ├ *InvoiceService
   ├ broker.Conn     ├ UserRepository  ├ InvoiceRepo
   └ exports: both   └ exports:        └ imports: platform, user
                       UserRepository        ↳ sees UserRepository
                                             ↳ CANNOT see *UserService
```

A module declaration is a **value, not a side effect** — `warren.NewModule(...)` returns an inert data structure and registers nothing. The bootstrapper walks the whole graph first, then materialises containers. That ordering is what makes cycle detection and encapsulation *checkable* rather than emergent.

This design assumes runtime scoping. If the DI mechanism changes to compile-time wiring (`wire`-style codegen), this model needs redesigning — see the PRD's open question #1.

### 1.3 Boot sequence

The design rule: **every error the framework can detect surfaces at boot, never on request 1.**

```
 0  load config          layered: defaults → file → env → flags, validated
 1  flatten module graph resolve imports, detect cycles → fail
 2  build scopes         one child container per module, copy exported bindings
                          + an EMPTY *transport.Table in the root scope
 3  VALIDATE GRAPH       every dep resolvable? ambiguous? unused? → fail
 4  instantiate          controllers + consumers, topological order
 5  register             they build ONE route table, frozen into step 2's
 5b instantiate eager    adapters read the finished table and claim a protocol
                          → a route nobody serves fails HERE
 6  OnStart              dependency order: pool → repos → consumers → servers
 7  readiness opens      health endpoint flips green
 8  serve
──────────────────────── SIGTERM
 9  readiness closes     LB drains BEFORE anything stops
 9b drain delay          LB observes the 503 before the listener goes away
10  OnStop               reverse order, per-hook timeout, force-kill deadline
```

Two steps are worth defending in a design review:

- **Step 3** is why a missing provider is a startup crash with a full resolution chain (see §2.2), not a nil-pointer panic in production.
- **Step 9** is why rolling deploys don't drop requests. Closing readiness *before* stopping servers is the ordering most hand-rolled Go services get backwards.

### 1.4 The transport spine

How one handler serves three protocols:

```
HTTP request ─┐
gRPC call ────┼─▶ edge middleware ─▶ decode ─▶ validate ─┐
Kafka msg ────┘   (transport-specific)                   │
                                                         ▼
                                              core middleware chain
                                          (tracing, tx, retry, metrics)
                                                         │
                                                         ▼
                                              Handler[Req, Res].Handle
                                                         │
                              ┌──────────────────────────┤
                              ▼                          ▼
                        encode success            errors.Error
                              │                          │
              ┌───────────────┼──────────┐    NotFound → 404 / NotFound / ack
              ▼               ▼          ▼    Conflict → 409 / AlreadyExists / ack
           200 JSON      proto msg     ack    Internal → 500 / Internal / nack
```

**Two middleware rings, deliberately.** *Edge* middleware is transport-shaped — CORS, gRPC interceptors, consumer ack semantics — and cannot be shared. *Core* middleware wraps `Handler[Req,Res]` itself, so a transaction decorator or retry policy is written once and applies everywhere. Conflating the two is the mistake that makes "transport-agnostic" frameworks leak.

The error table (§2.6) is the load-bearing piece: domain code returns `errors.Conflict(...)` and each adapter owns the translation.

**No reflective dispatch on the hot path.** Worth stating explicitly, because Go teams will assume otherwise — and worth stating precisely, because the stronger claim is not true. `encoding/json`, `validate`'s compiled rule, and `transport`'s parameter binder all touch `reflect` per request; what none of them does is *decide* anything there.

Every reflective decision — which fields carry parameters, which setters convert them, which rules run, which codec encodes, which middleware wraps which handler — is made during steps 1–5 and frozen. By step 8 the route table holds pre-built closures with middleware already composed:

```go
type route struct {
    invoke func(ctx context.Context, raw []byte) ([]byte, error)
}
```

What is left per request is a match, a fixed walk over precomputed field indices, and direct calls: no type search, no tag re-parse, no method resolution by name. The DI container is not consulted at request time. Each request path carries an allocation test asserting an exact count, so the claim is a number rather than an adjective. (Amended 2026-08-02; see AGENT.md invariant 7.)

### 1.5 Messaging runtime

Consumers are lifecycle components, not goroutines someone forgot about:

```
Kafka/AMQP ─▶ Subscriber ─▶ recover ─▶ trace-extract ─▶ inbox dedupe
                                                            │
                                              ┌─────────────┴──────────┐
                                         already seen              new
                                              │                     │
                                            ack                 Handler
                                                                    │
                                              ┌─────────────────────┤
                                            ok                    error
                                              │                     │
                                            ack           retry w/ backoff
                                                                    │
                                                          exhausted → DLQ
```

Publishing runs the other way: `UnitOfWork` writes aggregate state and outbox rows in one transaction, and a separate **relay** — its own lifecycle participant, leader-elected — drains the outbox to the broker. Every box in that chain is driver-agnostic middleware, which is why swapping Kafka for RabbitMQ changes one line in `main.go` and nothing else.

### 1.6 Repository layout

```
warren/                                 MODULE: core        (stdlib + dig)
├── warren.go, di/, lifecycle/,
│   config/, log/, errors/, validate/, health/            ← kernel
├── inbox/                                                ← dedupe port + memory store
├── broker/memory/, broker/brokertest/                    ← in-process driver + contract suite
├── outbox/                                               ← writer port, relay, memory store
└── domain/, app/, persistence/, broker/, transport/      ← contracts (see §1.1's exception)

SHIPPED in v0.1
warren/transport/http/                  MODULE  net/http only
warren/broker/kafka/                    MODULE  twmb/franz-go
warren/broker/memory/                   (core)  in-process, passes brokertest
warren/persistence/postgres/            MODULE  jackc/pgx
warren/observability/                   MODULE  OpenTelemetry SDK
warren/validate/playground/             MODULE  go-playground/validator
warren/testing/                         (core)  stdlib only — no testify
warren/cli/                             MODULE  cobra                   (build-time only)

DEFERRED to v0.2 — each spec records why
warren/openapi/                         MODULE  —                       first for v0.2
warren/auth/                            MODULE  golang-jwt + go-oidc    identity type is a core decision
warren/transport/grpc/                  MODULE  google.golang.org/grpc  needs `warren g proto`
warren/jobs/                            MODULE  robfig/cron             amends the boot/shutdown orders
warren/resilience/                      MODULE  gobreaker + x/time/rate narrowed: breaker + limiter only
warren/persistence/mongo/               MODULE  mongo-driver            design round on the UnitOfWork port
warren/persistence/redis/               MODULE  redis/go-redis          entangled with jobs' elector
warren/broker/rabbitmq/                 MODULE  rabbitmq/amqp091-go     memory already proves the swap
warren/broker/nats/                     MODULE  nats-io/nats.go         §5.3 is seven words
warren/persistence/mysql/               —                               not in this table by design
warren/config/yaml/                     MODULE  yaml parser             audit first
```

**Rule:** adapters never import each other. `broker/kafka` and `persistence/postgres` are mutually invisible. Both depend only on the core module's contract packages.

### 1.7 Dependency budget

| Service profile | Direct deps in the user's `go.mod` |
|---|---|
| HTTP only | `warren`, `warren/transport/http` — two modules, zero third party |
| + Postgres | `+ warren/persistence/postgres`, `pgx` — two modules, one third party |
| + gRPC + Kafka + OTel | `+ grpc`, `franz-go`, OTel SDK + exporter |

If a hello-world service's `go.sum` looks like a Java project's, the multi-module split has failed.

---

## 2. Kernel

### 2.1 `warren`

- **Path** `github.com/<org>/warren` · **Module** core · **Mode** Build
- **Owns** — application bootstrap, the module system, the run loop.

**Surface**

```go
func New(modules ...Module) *App
func (a *App) Run() error              // blocks; handles SIGINT/SIGTERM
func (a *App) Start(ctx) error         // for tests
func (a *App) Stop(ctx) error
func (a *App) Invoke(module string, fn any) error // reach what boot built

func NewModule(name string, opts ...ModuleOption) Module

// ModuleOptions
func Imports(...Module) ModuleOption
func Providers(constructors ...any) ModuleOption
func Controllers(...any) ModuleOption
func Consumers(...any) ModuleOption
func Exports[T any]() ModuleOption
func Eager[T any]() ModuleOption       // materialise at boot even if unconsumed
func Optional[T any]() ModuleOption    // a nil T from a provider is MEANT, not a defect

// boot-time substitution — the seam warren/testing is built on
func Substitute[T any](v T) Substitution  // replace every provider of T
func Bind[T any](v T) Substitution        // add a root-scope binding
func (a *App) Substitute(subs ...Substitution) error

// the validator boot step 5 compiles into every route closure; default
// validate.Required(). Must be called before Start.
func (a *App) Validator(v validate.Validator) error

func (m Module) Name() string
func OnStart(fn func(context.Context) error) ModuleOption
func OnStop(fn func(context.Context) error) ModuleOption
```

`App.Invoke(module, fn)` resolves `fn`'s parameters from that module's scope
and calls it — the seam tests and pre-transport mains use to reach the
components boot built, with module encapsulation intact. `Eager[T]()`
materialises a provider nothing consumes: `config.Module` uses it so a bad
config fails the boot even when no constructor injects the struct.

A provider returning `nil` normally **fails the boot** — §1.3's rule is that
every detectable error surfaces at boot, and a nil interface otherwise booted
clean and became a 500 on the first request to touch it. `Optional[T]()`
declares that a nil `T` is *meant*. It is per type, not per module: one
declared absence does not disarm the check for anything else the module
provides, and consumers of an optional binding handle the nil themselves.
`warren/observability` is the case it exists for — a nil `app.Telemetry` is
"no collector configured", and `app.WithTelemetry` drops a nil so the
uninstrumented request path stays a pass-through rather than paying for a
no-op value on every request.

Two hooks patterns, deliberately different: `warren.OnStart`/`OnStop` take
plain closures fixed at declaration time; anything created *at* boot — a
consumer pipeline's drain func, a pool a constructor opened — registers the
other way, by injecting `lifecycle.Lifecycle` (provided in the root scope)
and appending its own `lifecycle.Hook`.

**Declare each module once.** Modules are deduplicated by identity, so
`func Module() warren.Module` called by two importers is two modules sharing
a name — a boot error. Use `var Module = sync.OnceValue(func() warren.Module
{ ... })`: it reads like a function and yields one identity.

**Key property:** `NewModule` returns an inert value. Nothing registers on construction. The bootstrapper walks the entire graph before materialising containers — that ordering is what makes cycle detection and encapsulation checkable rather than emergent.

Semantics fixed with the implementation: `Module` is an opaque struct value
(`ModuleOption` a function over it), and `NewModule` records its call site —
the "declared in module.go:14" line of §2.2's diagnostic. A module-import
*cycle is unrepresentable*: `Imports` carries values, so closing a cycle would
be infinite recursion in user code before `New` runs; provider cycles are
`warren/di`'s. Module names must be unique (they are scope names); exporting a
type no provider returns is a boot error; imported modules are flattened
transitively and deduplicated. Entry points are the controllers and consumers:
instantiating them (dependency order, per module topologically) is what
materialises the singleton graph — an unconsumed private provider is simply
never built. `lifecycle.Lifecycle` is provided in the root container, so any
constructor can inject it and adapters can register hooks. An `App` boots
once; `Run` returns the boot error or, after a signal, `Stop`'s joined errors,
and a second signal during shutdown cancels the drain — the force-exit
short-circuit.

**Step 5 registers.** Every controller and consumer is instantiated at step 4
and *kept*; step 5 hands each module's own `transport.Registrar` to them and
freezes the result into the `*transport.Table` the root scope has held, empty,
since step 2 — empty because an adapter injects it, so its constructor's input
must resolve at step 3, long before any route exists. Adapters are eager
singletons, so they build at step 5b: each reads a complete table and claims
its protocol, and a route no adapter claims fails the boot right there rather
than 404ing in production. Everything listed in `Controllers` or `Consumers`
must implement `transport.Controller`; one that does not fails the boot naming
the type, the option, and the module's declaration site, because a `Register`
with a typo'd signature compiles and registers nothing. A type that genuinely
registers nothing belongs in `Providers` with `Eager[T]()`.
`App.Validator(v)` sets the validator those route closures compile in.

Three rules the 2026-08-01 adversarial review of this package pinned down:
**modules are deduplicated by identity, not call site** — copies of one
`NewModule` value shared through several import paths are one module, while a
module factory called twice creates two, and sharing a name is then the
duplicate-name boot error (`config.Module[T]` names itself after `T` for the
same reason, so several config structs coexist); **constructors wire, OnStart
acquires** — a constructor that opens a connection owns a resource the boot
sequence cannot release if a later module fails to build, so acquisition
belongs in hooks, whose rollback the lifecycle guarantees; and a failure
relayed across an import edge renders **one** diagnostic block, attributed to
the module whose fix it is.

**Usage**

```go
// internal/modules/user/module.go
func Module() warren.Module {
    return warren.NewModule("user",
        warren.Imports(platform.DatabaseModule, platform.BrokerModule),
        warren.Providers(
            NewRegisterUserHandler,
            NewGetUserHandler,
            postgres.NewUserRepository,   // returns domain.UserRepository
        ),
        warren.Controllers(NewUserController),
        warren.Consumers(NewBillingConsumer),
        warren.Exports[domain.UserRepository](),   // only this leaves the module
    )
}
```

Anything not in `Exports` is private to the module. Another module importing `user` sees `domain.UserRepository` and **cannot** resolve `*RegisterUserHandler`.

---

### 2.2 `warren/di`

- **Path** `warren/di` · **Module** core · **Wraps** `go.uber.org/dig` · **Mode** Wrap
- **Owns** — the container, scoping, graph validation, diagnostics.

**Why dig, not fx:** dig is v1 with strict SemVer and is explicitly designed to power application frameworks. Fx would impose *its* lifecycle, and we need readiness gating and drain ordering that fx doesn't model. More decisively: "a missing provider prints a copy-pasteable fix" is a stated DX target, and we can't hit it while surfacing someone else's diagnostics.

**Surface**

```go
type Container interface {
    Provide(constructor any, opts ...ProvideOption) error
    Invoke(fn any) error
    Scope(name string) Container         // child container = module boundary
    Validate() error                     // all deps resolvable? ambiguous? unused?
    Explain(target any) Resolution       // powers `warren explain di`
}

func New() Container // the root container

// ProvideOption configures one Provide call. Two options exist:
func Exported() ProvideOption                    // visible to importing modules; read by the bootstrapper and the diagnostics
func DeclaredAt(file string, line int) ProvideOption // the module declaration site — the "declared in module.go:14" line

// Resolution is Explain's result: Target, Found, Provider, Scope, Site, and
// Inputs ([]Resolution, recursive). It renders itself as an indented tree.

func Resolve[T any](c Container) (T, error)
func MustResolve[T any](c Container) T // panics with the diagnostic — the kernel's one sanctioned panic (boot only)
```

`Validate` runs entirely off Warren's own provider records — dig is asked to
construct, never to explain — and constructs nothing: step 3 completes before
step 4 instantiates a single singleton. `Scope(name)` is idempotent: a repeat
call returns the same child. Whether an *unused* provider fails validation is
still open — it needs the root package's entry-point model to be meaningful —
so `Validate` currently checks resolvable and ambiguous.

**Diagnostics are the product here.** Raw dig error:

```
missing dependencies for function ...: missing type: *postgres.Pool
```

Warren's:

```
✗ cannot resolve dependency

    domain.UserRepository
      └─ required by *user.RegisterUserHandler
           └─ required by *user.UserController
                └─ declared in internal/modules/user/module.go:14

  No provider found in scope "user" or its imports.

  Did you mean:
    • postgres.NewUserRepository is registered in scope "billing" but not exported.
      Add to billing's module: warren.Exports[domain.UserRepository]()
    • Or provide it locally:  warren.Providers(postgres.NewUserRepository)
```

Users never import `go.uber.org/dig`. That is the wrap boundary.

---

### 2.3 `warren/lifecycle`

- **Path** `warren/lifecycle` · **Module** core · **Mode** Build
- **Owns** — ordered startup and shutdown, readiness gating, drain.

**Surface**

```go
type Hook struct {
    Name    string
    OnStart func(context.Context) error
    OnStop  func(context.Context) error
    Timeout time.Duration
}

type Lifecycle interface {
    Append(Hook)
    Start(context.Context) error
    Stop(context.Context) error
    Ready() bool // the readiness state warren/health serves — true between steps 7 and 9
}

func New(opts ...Option) Lifecycle
func ForceExitDeadline(d time.Duration) Option // bounds the whole shutdown; default 30s
```

Semantics, fixed with the implementation: a nil `OnStart` or `OnStop` is a
start-only or stop-only hook and is skipped; `Hook.Timeout` bounds each of
`OnStart` and `OnStop` individually, and zero means no per-hook timeout (the
force-exit deadline still bounds the whole of `Stop`); `Stop` continues past a
failing hook and returns every failure joined — shutdown never abandons the
remaining hooks because one flush failed; a failing `OnStart` stops the boot
and stops the already-started hooks in reverse before returning. `Ready()`
flips true when `Start` returns nil and false as `Stop`'s first action —
before the first `OnStop` runs.

A lifecycle runs once: `Start` a second time — or after `Stop` — is an error.
`Stop` is idempotent, and `Stop` arriving while `Start` is mid-boot wins:
readiness closes immediately, `Start` abandons after the in-flight hook and
returns "boot abandoned", and `Stop` unwinds what had started — a stopped
process never advertises ready. `Append` is safe from inside a running hook
(the adapter pattern); the appended hook starts in its turn.

**Shutdown order is the reason this is built, not borrowed:**

```
SIGTERM
  1. readiness probe → 503        ← load balancer drains BEFORE anything stops
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack
  4. outbox relay flushes
  5. DB pools, broker connections close
  6. force-exit deadline (default 30s)
```

Closing readiness *before* stopping servers is the ordering most hand-rolled Go services get backwards, and it's why rolling deploys drop requests.

**Usage** — most users never touch it directly; adapters register hooks. Direct use:

```go
warren.NewModule("cache",
    warren.Providers(NewRedisClient),
    warren.OnStart(func(ctx context.Context) error { return cache.Warm(ctx) }),
    warren.OnStop(func(ctx context.Context) error { return cache.Flush(ctx) }),
)
```

---

### 2.4 `warren/config`

- **Path** `warren/config` · **Module** core · **Mode** Build
- **Owns** — layered configuration resolution and validation.

**Why not Viper:** heavy transitive dependency tree, global state, stringly-typed access. This is ~600 lines to own and it's the first thing every user touches.

**Core parses no files.** Config is split by where a value comes from, not by
what parses it:

| Source | Needs a parser? | Where it lives |
|---|---|---|
| Struct defaults | No | core |
| Environment variables | No | core |
| CLI flags | No | core |
| Config files (YAML, TOML, …) | Yes | submodule (`config/yaml` first) |

**Resolution order** (later wins): struct defaults → file sources → environment
variables → command-line flags. Core merges all sources in order and binds the
result to the struct; it never knows YAML exists — it just sees a map. The same
hook later admits `config/toml`, `config/json`, or a Vault/AWS-Secrets source
with no change to core.

**Surface**

```go
func Load[T any](opts ...Option) (T, error)
func Module[T any](opts ...Option) warren.Module   // provides T to the graph

// Source is where config values come from. Defaults, env, and flags ship in
// core; anything needing a parser implements Source from its own submodule.
// A Source is itself an Option, so it slots straight into Module and Load.
type Source interface {
    Load() (map[string]any, error)
}

func WithEnvPrefix(prefix string) Option
func WithFlags(*flag.FlagSet) Option

// in warren/config/yaml (separate module — the only place a YAML library exists):
func File(path string) config.Source
```

Semantics fixed with the implementation: `Option` is dispatched at `Load`
time — the type system cannot express "a `Source` is itself an `Option`" for
foreign implementations, so a non-option argument is a boot error naming the
type. `WithFlags` requires an already-parsed set; flag names are the dotted
field path (`-postgres.dsn`), and only flags explicitly set on the command
line override earlier layers. The environment layer runs only under a
`WithEnvPrefix` prefix. Core enforces `validate:"required"` itself — it is
resolution completeness, not validation; the rest of the `validate:`
vocabulary is `warren/validate`'s once its seam exists.

Parked for `config/yaml`'s spec (carried from the retired core config spec):
how the submodule selects an environment-specific file (`config.<env>.yaml`),
and whether a missing file is an error or an empty map — in a container there
is often no file at all. Core just sees a `Source` succeed or fail.

**Usage**

```go
type Config struct {
    Env      string `config:"env" default:"development" validate:"oneof=development staging production"`
    HTTPPort int    `config:"http_port" default:"8080"`

    Postgres struct {
        DSN         string `config:"dsn" validate:"required"`   // → WARREN_POSTGRES_DSN
        MaxConns    int32  `config:"max_conns" default:"10"`
    } `config:"postgres"`

    Kafka struct {
        Brokers []string `config:"brokers" validate:"required,min=1"`
        Group   string   `config:"group" validate:"required"`
    } `config:"kafka"`
}

// main.go — no file: core only, zero extra deps. Most containerized services.
warren.New(
    config.Module[Config](config.WithEnvPrefix("WARREN")),
    ...
)

// main.go — with a YAML file: pulls in the config/yaml submodule.
warren.New(
    config.Module[Config](
        yaml.File("config.yaml"),
        config.WithEnvPrefix("WARREN"),
    ),
    ...
)

// Any provider now takes Config as a dependency:
func NewUserRepository(cfg Config, db postgres.DB) domain.UserRepository { ... }
```

Validation runs at boot. A missing `WARREN_POSTGRES_DSN` is a startup failure with the field path named, not a nil-pointer panic on the first query.

---

### 2.5 `warren/log`

- **Path** `warren/log` · **Module** core · **Uses** `log/slog` · **Mode** Vendor (context carrier)
- **Owns** — context-carried loggers and correlation-ID propagation. Not a
  logging abstraction: the logger *is* `*slog.Logger`, used directly — this
  package only carries it on the context, which is why the mode is Vendor
  (matching the §9 ledger), not Wrap.

**Surface**

```go
func FromContext(ctx context.Context) *slog.Logger // slog.Default() when unseeded
func With(ctx context.Context, args ...any) context.Context
func CorrelationID(ctx context.Context) string     // "" when none is carried

// The seeding side, called by transport adapters at the edge — adapters are
// separate modules, so the seam must be exported:
func WithLogger(ctx context.Context, l *slog.Logger) context.Context
func WithCorrelationID(ctx context.Context, id string) context.Context
```

The correlation ID is minted at the edge: an adapter reuses the one arriving on
the wire or mints a fresh one, then seeds it here and attaches it to the
logger. Core never generates identifiers.

**Usage**

```go
func (h *RegisterUserHandler) Handle(ctx context.Context, cmd RegisterUser) (UserDTO, error) {
    log.FromContext(ctx).Info("registering user", "email", cmd.Email)
    // trace_id, span_id, correlation_id, module, handler are already attached
}
```

The transport adapters seed the context. Handlers never construct a logger and never take one as a constructor dependency.

---

### 2.6 `warren/errors`

- **Path** `warren/errors` · **Module** core · **Mode** Build
- **Owns** — the semantic error vocabulary. **Load-bearing for the entire transport story.**

**Surface**

```go
type Code string

const (
    CodeInvalid          Code = "INVALID"
    CodeNotFound         Code = "NOT_FOUND"
    CodeConflict         Code = "CONFLICT"
    CodeUnauthenticated  Code = "UNAUTHENTICATED"
    CodePermissionDenied Code = "PERMISSION_DENIED"
    CodeUnavailable      Code = "UNAVAILABLE"   // retryable
    CodeInternal         Code = "INTERNAL"
)

func Invalid(field string, err error) *Error          // "field <field> is invalid", wraps err
func NotFound(resource string, id any) *Error         // "<resource> <id> not found"
func Conflict(msg string, args ...any) *Error         // printf-style; args are fmt operands
func Unauthenticated(reason string) *Error            // "<reason>" — about the CALLER's identity
func PermissionDenied(action string) *Error           // "not allowed to <action>"
func Unavailable(dependency string, err error) *Error // "<dependency> is unavailable", wraps err
func Internal(err error) *Error                       // "unexpected failure", wraps err

func (e *Error) Code() Code
func (e *Error) Message() string
func (e *Error) Details() map[string]any // a copy; mutating it does not touch e
func (e *Error) Error() string           // "CODE: message", ": cause" appended when one is wrapped
func (e *Error) Unwrap() error           // the wrapped cause, so stdlib errors.Is/As see through
func (e *Error) WithDetail(k string, v any) *Error
func Is(err error, code Code) bool
```

The type's home is `warren/errors`, so its qualified name is `errors.Error` —
everywhere. In generated code that also touches driver sentinels, the Warren
package is imported under the alias `werrors` so the bare `errors` identifier
stays the standard library (see §6.1).

**Why it matters** — one table, three transports:

| Code | HTTP | gRPC | Consumer |
|---|---|---|---|
| `INVALID` | 400 | `InvalidArgument` | → DLQ (never retry) |
| `NOT_FOUND` | 404 | `NotFound` | ack + log |
| `CONFLICT` | 409 | `AlreadyExists` | ack (idempotent replay) |
| `UNAUTHENTICATED` | 401 | `Unauthenticated` | → DLQ (never retry) |
| `PERMISSION_DENIED` | 403 | `PermissionDenied` | → DLQ (never retry) |
| `UNAVAILABLE` | 503 | `Unavailable` | nack + backoff retry |
| `INTERNAL` | 500 | `Internal` | nack + retry, then DLQ |

This table is why domain code can return `errors.Conflict(...)` and never import `net/http`.

A `Code` the table does not list is treated by every adapter as `INTERNAL` —
the safe default for the unknown: 500, `Internal`, nack + retry, then DLQ.

**Why the two auth codes dead-letter rather than retry or ack.** The message
won't get a better token by waiting — retrying an expired credential just burns
the backoff budget and delays the inevitable. And acking means "handled, delete
it" — but an auth failure on a queue is a bug in your own system (a service
published without proper identity, or with someone else's), and acking destroys
the evidence. The DLQ stops the message, keeps it for inspection, and fires the
DLQ alert. Which is correct, because this should wake someone up.

**`UNAUTHENTICATED` describes the caller's identity, not yours.** If your
service failed to authenticate to something downstream — Postgres, S3, another
API — that is not `UNAUTHENTICATED`; it is `UNAVAILABLE`, and it retries.
Adapter authors get this wrong constantly, which is why the rule sits next to
the table.

---

### 2.7 `warren/validate`

- **Path** `warren/validate` · **Module** core · **Mode** Build
- **Amended 2026-08-02.** This section used to say *Wraps
  `go-playground/validator/v10` · Mode Wrap*, in the core module. That
  cannot be: invariant 1 makes core stdlib + `dig`, permanently. Core holds
  the **port** and one stdlib implementation; the wrap is §2.7a.

Core owns `Validator` (a boot-time compiler), the compiled `Rule`, and
`Required()` — which enforces `required` and **refuses every other token at
boot**, naming it. Failures surface as `*errors.Error` with `CodeInvalid`
and per-field details, so no library's error type reaches a handler.
Transport adapters call the compiled rule automatically after decode;
handlers never invoke it.

```go
type Validator interface{ Plan(t reflect.Type) (Rule, error) }
type Rule func(v any) error

func PlanFor[T any](v Validator) (func(*T) error, error)  // boot step 5
func Required() Validator                                  // stdlib, core
func None() Validator                                      // the deliberate opt-out
```

**Detail keys are the JSON wire name, dotted for nesting** —
`address.postcode` — and **embedded structs flatten**, exactly as
`encoding/json` promotes their fields, so `Base.email` is never a key.
Nested pointers are descended; a slice or map of tagged elements is
**refused at boot**, because this implementation walks fields and not
elements, and silently passing a `[]LineItem` whose SKU is required is the
failure the package exists to prevent.

`None()` turns validation off for the whole application and is permanent —
it is the "I validate in the handler" answer, and the test-path validator. It
is no longer the escape hatch for a tag core cannot enforce: §2.7a ships, so
`validate:"required,email"` boots by installing that module rather than by
disabling validation everywhere.

```go
type RegisterUser struct {
    Email string `json:"email" validate:"required"`
    Name  string `json:"name"  validate:"required"`
}
// A bad request never reaches Handle().
```

---

### 2.7a `warren/validate/playground`

- **Path** `warren/validate/playground` · **Module** `warren/validate/playground` · **Wraps** `go-playground/validator/v10` · **Mode** Wrap
- **Shipped in v0.1 (2026-08-02).** Audited: v10.30.3, MIT, 20 091 stars,
  pushed 2026-07-29, not archived, 8 third-party modules — none of which a
  service keeping `validate.Required()` pays for.
- It also checks every tag AT BOOT: go-playground panics on an unknown
  constraint when it VALIDATES, so a typo in a field nobody has exercised
  would take the process down on the first request that touched it. `Plan`
  walks the type and probes each token, turning that into a boot diagnostic
  naming the field and the typo.

A `Validator` implementing the full tag vocabulary — `email`, `min`, `max`,
`oneof`, `dive` — compiled once per type at boot, with
`validator.ValidationErrors` normalised into `errors.Invalid` details before
anything leaves the module. Custom constraints, tag renaming and
translations are its business, not core's. Installed with
`transport.WithValidator(playground.New())`.

---

### 2.8 `warren/health`

- **Path** `warren/health` · **Module** core · **Mode** Build
- **Owns** — the registry of checks and the two probe verdicts. It serves
  nothing: `warren/transport/http` serves `/healthz` and `/readyz` from this
  registry, and `warren/transport/grpc` serves the gRPC health service from
  the same one, so the two can never disagree.

```go
type Check interface { Name() string; Check(ctx context.Context) error }
func NewCheck(name string, fn func(context.Context) error) Check

type Registry interface {
    Register(c Check, opts ...RegisterOption) error
    Live(ctx context.Context) Report   // no checks, always up
    Ready(ctx context.Context) Report  // the gate, then critical checks
}
func New(ready func() bool, opts ...Option) Registry
func DefaultTimeout(d time.Duration) Option    // 2s
func Timeout(d time.Duration) RegisterOption
func Informational() RegisterOption            // reported, does not gate
```

**Liveness runs no dependency checks, ever.** Restarting a process does not
fix a dead database, so a liveness probe wired to Postgres kills every
replica when Postgres blips. `/healthz` answers "this code ran" and stays up
through the drain. **Readiness** is `lifecycle.Ready()` and then every
critical check, run concurrently under their own timeouts — probe latency is
the slowest check, not the sum. With the gate closed no check runs and the
body says which red it is: `starting` before the gate first opened,
`draining` after it closed.

Checks run **on the probe**, not on an interval: freshness is the entire
product of a readiness probe, and a cached verdict keeps traffic flowing to a
pod whose dependency died. A `Cached` decorator is additive if a user ever
demonstrates the cost; the reverse is a behaviour change to everyone's probes.

The bootstrapper provides the registry in the root container, wired as
`health.New(lc.Ready)` — the gate is the lifecycle's, and health only reads
it, so the two can never drift. Adapters register their pings from their own
module constructors.


---

## 3. Contracts

Pure interfaces, zero implementations, in the core module. This is what lets an adapter and a user's domain package depend on the same type without meeting.

### 3.1 `warren/domain`

- **Mode** Build

```go
type ID interface { comparable; fmt.Stringer }

type Entity[T ID] struct{ id T }
func (e *Entity[T]) ID() T                        // identity accessor

// Root is the constraint repositories are generic over. AggregateRoot
// satisfies it. (A struct cannot serve as a Go type constraint — only this
// interface makes Repository[T Root[K], K ID] expressible.)
type Root[T ID] interface {
    ID() T
    PullEvents() []Event
}

type AggregateRoot[T ID] struct {
    Entity[T]
    events []Event
}
func NewAggregateRoot[T ID](id T) AggregateRoot[T]  // identity set at construction
func (a *AggregateRoot[T]) Raise(e Event)
func (a *AggregateRoot[T]) PullEvents() []Event   // drained by UnitOfWork

// Optimistic concurrency, OPT-IN. Embed VersionedRoot instead of
// AggregateRoot and Repository.Save becomes conditional on the version the
// aggregate was loaded at: a stale write is CodeConflict and changes nothing.
type Versioned interface {
    Version() int64        // 0 = never persisted, so the write is an insert
    SetVersion(v int64)    // drivers only: at reconstitution, and after a write
}

type VersionedRoot[T ID] struct {
    AggregateRoot[T]
    version int64
}
func NewVersionedRoot[T ID](id T) VersionedRoot[T]                        // version 0
func ReconstituteVersionedRoot[T ID](id T, version int64) VersionedRoot[T] // the load path

type Event interface {
    EventName() string          // "user.registered"
    OccurredAt() time.Time
    AggregateID() string        // deliberately string, not T: the outbox and
}                               // broker handle ONE shape — ID's fmt.Stringer
                                // is load-bearing, not conventional

type Specification[T any] interface {
    IsSatisfiedBy(T) bool   // in-memory evaluation; drivers translate their own way
}
```

**Usage**

```go
package domain

type User struct {
    domain.AggregateRoot[UserID]
    Email  Email
    Name   string
    Status Status
}

func NewUser(email Email, name string) *User {
    u := &User{
        AggregateRoot: domain.NewAggregateRoot(NewUserID()),
        Email:  email,
        Name:   name,
        Status: StatusPending,
    }
    u.Raise(UserRegistered{UserID: u.ID(), Email: email.String(), At: time.Now()})
    return u
}

func (u *User) Activate() error {
    if u.Status == StatusActive {
        return errors.Conflict("user already active")
    }
    u.Status = StatusActive
    u.Raise(UserActivated{UserID: u.ID(), At: time.Now()})
    return nil
}
```

Events accumulate on the aggregate. Nothing is published until the `UnitOfWork` commits — that's what makes state changes and event publication atomic.

---

### 3.2 `warren/app`

- **Mode** Build · **The central abstraction of the framework.**

```go
type Handler[Req, Res any] interface {
    Handle(ctx context.Context, req Req) (Res, error)
}

type Middleware[Req, Res any] func(Handler[Req, Res]) Handler[Req, Res]

// HandlerFunc adapts a bare function to Handler — how middleware wrap
// handlers without declaring a struct each time.
type HandlerFunc[Req, Res any] func(ctx context.Context, req Req) (Res, error)

func Chain[Req, Res any](h Handler[Req, Res], mw ...Middleware[Req, Res]) Handler[Req, Res]
```

`Chain(h, a, b, c)`: `a` is the **outermost** — first to see the request,
last to see the response — reading order matching execution order. Chain runs
at boot; the composed handler is the route table's pre-built closure, and
invoking it allocates nothing (benchmarked).

**Built-in core middleware** — transport-independent, applies everywhere:

| Middleware | Effect |
|---|---|
| `app.Transactional(uow)` | Wraps `Handle` in a transaction; commits state + outbox atomically. Its `uow` is `app.UnitOfWork` — one method, declared in `app` so it imports no sibling contract; `persistence.UnitOfWork` satisfies it |
| `app.Retrying(policy)` | Retries on `CodeUnavailable` |
| `app.Traced()` | Span per handler, named `<module>.<handler>` |
| `app.Metered()` | Duration histogram, error counter by code |
| `app.Authorized(policy)` | Policy check before invocation |

These are the *core* ring of the two-ring middleware model in §1.4 — they wrap `Handler[Req,Res]` and therefore apply identically to HTTP, gRPC, and consumers. Transport-shaped concerns belong in the edge ring, owned by the adapter.

**The port homes (settled 2026-08-01): the policies and the telemetry seam
live in `app` itself.** They exist for these middleware, and a one-interface
package would be ring bureaucracy. Four of the five are implemented:

```go
type RetryPolicy interface {           // warren/resilience implements it
    Next(attempt int) (delay time.Duration, retry bool)
}
type AuthorizationPolicy interface {   // warren/auth implements it
    Authorize(ctx context.Context) error // nil allows; deny with a §2.6 code
}
type Telemetry interface {             // warren/observability implements it
    Span(ctx context.Context, name string) (context.Context, func(err error))
    Record(name string, d time.Duration, err error) // counters keyed by errors.Code
}
func WithTelemetry(ctx context.Context, t Telemetry) context.Context
func TelemetryFromContext(ctx context.Context) Telemetry
func WithHandlerName(ctx context.Context, name string) context.Context
func HandlerName(ctx context.Context) string
```

`Traced()` and `Metered()` keep their no-argument signatures because the
telemetry **rides the context**, the same pattern as §2.5's logger:
observability's edge integration seeds `Telemetry`, the transport adapter
seeds the `"<module>.<handler>"` name in the route table's pre-built closure
(it is the one party that knows both names), and on a context carrying
neither the two middleware are exact pass-throughs — 0 allocs, benchmarked.
`Retrying` retries `CodeUnavailable` only and returns the handler's **last**
error on exhaustion or cancellation (freshest, code intact); `Authorized`
short-circuits on denial and returns the policy's error **unchanged**, so
policies speak the §2.6 vocabulary (`PermissionDenied` for a known caller,
`Unauthenticated` for absent identity). A nil policy panics at composition —
the same boot-time guard as `Chain`'s.

All five ship. The composition order is trace → meter → authorize → **retry →
transaction**: retry outside the transaction means a serialization failure
re-runs the whole transaction rather than retrying inside a doomed one, which
is the only ordering that makes `Serializable` usable.

---

### 3.3 `warren/persistence`

- **Mode** Build (ports) · **The deliberate omission is an ORM.**

```go
type Repository[T domain.Root[K], K domain.ID] interface {
    FindByID(ctx context.Context, id K) (T, error)   // CodeNotFound when absent
    Save(ctx context.Context, root T) error          // MUST call persistence.Track
    Delete(ctx context.Context, id K) error
}

type UnitOfWork interface {
    Do(ctx context.Context, fn func(context.Context) error, opts ...Option) error
}

type Transaction struct { ReadOnly bool; Isolation Level }
func ReadOnly() Option
func Isolation(l Level) Option        // ReadCommitted | RepeatableRead | Serializable
func Configure(opts ...Option) Transaction

// the enlistment seam
func Track(ctx context.Context, aggregates ...domain.Aggregate)
func Collect(ctx context.Context) (context.Context, func() []domain.Event)
func InTransaction(ctx context.Context) bool

// the in-process driver + the exported contract suite
func NewMemoryUnitOfWork() *MemoryUnitOfWork
func NewMemoryRepository[T domain.Root[K], K domain.ID](uow *MemoryUnitOfWork) *MemoryRepository[T, K]
func RunContract[T domain.Root[K], K domain.ID](t *testing.T, newDriver NewDriver[T, K], newAggregate func(K) T)
```

**How the unit of work drains events it never imported.** A `Repository` is
generic; a unit of work holds saved aggregates of many types at once and
cannot name their identifier types. `domain.Aggregate` — `PullEvents()`
alone — is the non-generic view that admits a heterogeneous collection, and
`Track`/`Collect` ride the same context the transaction already travels on.
`Save` calls `Track` as part of its contract, not as an implementation
detail: a driver whose `Save` does not `Track` loses events, and the contract
suite asserts it. Outside a transaction `Track` is a no-op that loses
nothing — the events stay pending for a later `Do`.

**A nested `Do` joins.** §10's handler calls `uow.Do` itself while §3.2 wraps
the same handler in `app.Transactional`, so erroring would break documented
code. Savepoints were rejected: rolling back to one would publish events for
state that was rolled back, and Mongo and Redis have none, so the port's
semantics would become driver-dependent. Options on a nested `Do` are an
error rather than a silent downgrade. A panic rolls back and **re-panics** —
a leaked transaction holds locks, and swallowing the panic would convert a
bug into a 503 and destroy the stack.

Error codes: absent → `NOT_FOUND`; unique or optimistic-concurrency conflict
→ `CONFLICT`; constraint violation → `INVALID`; connection lost, pool
exhausted, serialization failure, **commit failure** → `UNAVAILABLE`, so
`app.Retrying` composed OUTSIDE `app.Transactional` re-runs the whole
transaction. An unsupported `Option` is `INVALID`, never a silent downgrade.


### 3.4 `warren/broker`

- **Mode** Build (ports only) · **The least negotiable wrap in the framework.**

```go
type Message struct {
    ID          string            // idempotency key
    Type        string            // "user.registered"
    Key         string            // partition / routing key
    Payload     []byte
    Headers     map[string]string // trace context propagates here
    OccurredAt  time.Time
}

type Publisher interface {
    Publish(ctx context.Context, topic string, msgs ...Message) error
}

type Subscriber interface {
    Subscribe(ctx context.Context, topic string, h MessageHandler) error
}

type MessageHandler func(context.Context, Message) error
```

**Driver-agnostic middleware** — written once, applies to Kafka, Rabbit, NATS, and memory identically:

`Recover` → `TraceExtract` → `Deduplicate(inbox)` → `Retry(backoff)` → `DeadLetter` → `ConcurrencyLimit` → `Drain`

That property is the entire messaging pitch. It evaporates the moment a consumer touches `kgo.Record` directly — which is why the port is mandatory and the raw client is an explicit escape hatch, not the default path.

That list is the *outcome* order a failing message experiences. The wrapping
order `broker.Pipeline` composes (settled 2026-08-02 with the implementation)
is: `Recover` (safety net) → `Drain` → `TraceExtract` → `Deduplicate` →
`DeadLetter` (the disposition stage, mapping the §2.6 consumer column by the
error's outermost code) → `Retry` (`UNAVAILABLE` and `INTERNAL` only; waits
observe cancellation) → `ConcurrencyLimit` (semaphore held per attempt — a
message in backoff holds no slot) → `Recover` (innermost: a handler panic is
`INTERNAL`, retried, then dead-lettered) → handler. The chain lives in
`broker` itself — pure functions over the port's own types, the same shape
`app`'s middleware take one ring over:

```go
type Middleware func(MessageHandler) MessageHandler
func Chain(h MessageHandler, mw ...Middleware) MessageHandler // mw[0] outermost; nil panics at composition
func Pipeline(topic string, h MessageHandler, store inbox.Store, dlq Publisher,
    opts ...SubscribeOption) (MessageHandler, func(ctx context.Context) error)

type SubscribeOption struct{ /* opaque */ }
func WithRetry(p app.RetryPolicy) SubscribeOption   // default ExponentialBackoff(3)
func WithDeadLetter(topic string) SubscribeOption   // default "<topic>.dlq"
func WithConcurrency(n int) SubscribeOption         // default uncapped
func WithDedupeTTL(d time.Duration) SubscribeOption // default 24h
func WithoutDedupe() SubscribeOption                // the named opt-out
func ExponentialBackoff(attempts int) app.RetryPolicy // base 100ms, ×2, cap 30s, full jitter
```

Chain-produced codes: a publish failure is the driver's column (transient →
`Unavailable`, rejected → `Invalid`, else `Internal`); a failed DLQ publish
**nacks** — the message is neither acked nor dropped, and redelivery
re-attempts until the broker recovers; a pre-handler decode failure is
`Invalid` → DLQ; a non-Warren handler error is `INTERNAL`. Dead-lettered
messages carry the original envelope plus forensic headers
(`warren-origin-topic`, `warren-error-code`, `warren-error`,
`warren-attempts`) — which is also why `Message` needs no `Topic` field: the
topic is per-subscription boot-time state, and provenance travels on the one
kind of message where it is a question. Dedupe marks only on nil (disposed):
`Seen` before the handler, fail **closed** (`UNAVAILABLE` nack — duplicates
over loss, never silently).

---

### 3.5 `warren/transport`

- **Mode** Build (port) · **One `Register`, three protocols.**

**v0.1 (Go 1.26).** Generic methods on concrete types are a Go 1.27 feature,
so registration is generic **free functions** — warren.md §9's recorded
"Fix A" — with the names and argument order the 1.27 methods will have:

```go
type Registrar interface{ /* sealed: transport holds the only implementation */ }
type Controller interface{ Register(r Registrar) }

func Get[Req, Res any](r Registrar, pattern string, h app.Handler[Req, Res], opts ...RouteOption)
func Post[Req, Res any](...)   // default success 201; Delete 204; the rest 200
func Method[Req, Res any](r Registrar, fullMethod string, h app.Handler[Req, Res], opts ...RouteOption)
func OnEvent[Req, Res any](r Registrar, topic string, h app.Handler[Req, Res], opts ...broker.SubscribeOption)

// The escape hatch: a protocol-native handler for what byte-in/byte-out
// deliberately does not model — upload, download, SSE, WebSocket upgrade.
// h is opaque to core (the kernel never imports net/http); the adapter
// serving p type-asserts it and fails the boot, naming the route and the
// type, if it does not fit. It travels through the sealed Registrar so the
// MODULE's container builds the handler, with the module's private providers.
//
// Unlike Get/Post/… — which name a verb and take a bare path — Raw names no
// verb, so its PATTERN CARRIES ONE: "POST /uploads".
func Raw(r Registrar, p Protocol, pattern string, h any, opts ...RouteOption)

type RawRoute struct {
    Protocol Protocol
    Pattern  string // the adapter's own syntax — "POST /uploads" for net/http
    Name     string
    Guards   []app.AuthorizationPolicy
    Handler  any
}

func (t *Table) Raw() []RawRoute
func (b *Builder) Fill(t *Table) error   // freeze into the Table boot provided at step 2

func Status(code int) RouteOption
func Guard(p app.AuthorizationPolicy) RouteOption   // runs BEFORE decode
func Named(name string) RouteOption

type Invoker func(ctx context.Context, raw []byte) ([]byte, error)
type Codec interface{ Name() string; Decode([]byte, any) error; Encode(any) ([]byte, error) }
func JSON() Codec

type HTTPRoute struct { Verb, Pattern, Name string; Success int
    Guards []app.AuthorizationPolicy; Request, Response reflect.Type
    Bind func(Codec) Invoker }
type GRPCRoute struct{ ... }
type EventRoute struct { Topic, Name string; Options []broker.SubscribeOption
    Request reflect.Type; Bind func(Codec) broker.MessageHandler }

type Builder struct{ ... }
func NewBuilder(opts ...BuilderOption) *Builder
func (b *Builder) For(module string) Registrar
func (b *Builder) Table() (*Table, error)
func (t *Table) HTTP() []HTTPRoute; GRPC() []GRPCRoute; Events() []EventRoute
func (t *Table) Claim(p Protocol, by string); Unserved() error
```

A controller registers once and is exposed three ways:

```go
func (c *UserController) Register(r transport.Registrar) {
    transport.Post(r, "/users", c.register)
    transport.Method(r, "user.v1.UserService/Register", c.register)
    transport.OnEvent(r, "billing.customer.created", c.register)
}
```

**The registrar is sealed** — an adapter cannot reimplement registration and
drift from it, so every router decodes, validates, binds parameters, and
defaults statuses identically. Adapters consume `[]HTTPRoute` instead.

**`Bind(Codec) Invoker`, not a finished closure**: the gRPC codec is protobuf
and lives in an adapter module, so a core-built closure could never serve
gRPC. Handing out a boot-time factory keeps the erasure — the part that needs
`Req`/`Res` — in core where the type parameters exist, and the serialisation
in the adapter where the driver is.

**Guards travel as data**, not composed into the closure, so a denial
precedes the decoder: an unauthorized caller's malformed body is a 403, not a
400, and unauthenticated input never reaches the JSON decoder.

Registration errors accumulate and surface together from `Table()`; duplicate
routes and empty patterns fail the boot; `Claim`/`Unserved` fail the boot when
routes are registered for a protocol nothing serves — a route nobody serves is
a route that silently 404s in production.

**Honest note on the 1.27 migration:** explicit type arguments are mandatory
in *both* shapes — Go cannot infer `[Req, Res]` from a concrete handler passed
where an interface is expected — so §3.5's inference-free call sites are a
risk to verify when 1.27 ships, not a certainty.


---

## 4. Transport Adapters

### 4.1 `warren/transport/http`

- **Wraps** `net/http` · **Mode** Vendor (standard library only) — *amended 2026-08-02, see below*

**Amended 2026-08-02: the router is `net/http.ServeMux`, and there is no swappable router.** Go 1.22 gave `ServeMux` method and wildcard patterns and Go 1.23 gave `Request.Pattern` — the two features this port actually needs — and the sealed `Registrar` (§3.5) already discards everything else a router is bought for: it consumes `[]HTTPRoute`, and core owns decode, validate, param binding and status defaults. Measured, chi is the *worst* of five candidates on this project's stated first priority — 4 allocations and 704 B per request against `ServeMux`'s 2 and 48 B, because `Mux.ServeHTTP` shallow-copies the whole `*http.Request` — and gin's and echo's zeroes are unreachable behind a `func(http.Handler) http.Handler` middleware model. `RouterAdapter` and the Gin/Echo/Fiber adapters are dropped: a swap mechanism protecting against a cost the port already eliminated.

The ecosystem argument survives intact — `chi/v5/middleware` is `net/http`-shaped and runs unmodified on a `ServeMux`, so a user who wants it adds chi to *their* go.mod rather than everyone's. Full evidence, and what this gives up, in `transport/http/SPEC.md`.

**Surface**

```go
func Server(opts ...Option) warren.Module

func Port(int) Option
func Addr(string) Option                                     // "127.0.0.1:8080"; overrides Port
func Listener(net.Listener) Option                           // a test binding port 0; overrides both
func Middleware(...func(http.Handler) http.Handler) Option   // stdlib signature
func Handle(string, http.Handler) Option                     // pprof, static assets, webhooks

func ReadHeaderTimeout(time.Duration) Option                 // 10s — the Slowloris fix
func ReadTimeout(time.Duration) Option                       // 30s
func WriteTimeout(time.Duration) Option                      // 0, deliberately: SSE, downloads
func IdleTimeout(time.Duration) Option                       // 120s
func MaxHeaderBytes(int) Option                              // 1 MiB
func MaxBodyBytes(int64) Option                              // 1 MiB — Invoker is []byte-in
func DrainDelay(time.Duration) Option                        // 5s — step 9b
func ShutdownTimeout(time.Duration) Option                   // 15s, inside lifecycle's 30s

func TLS(*tls.Config) Option
func TLSFiles(certFile, keyFile string) Option
func H2C() Option                                            // no golang.org/x/net since Go 1.24

const CorrelationHeader = "X-Correlation-Id"
```

Implemented 2026-08-02. `Router` and `RouterAdapter` are gone with the
ServeMux decision, and there is no `Raw(*http.ServeMux)`: a `ServeMux`'s entire
API is `Handle`, so a mux escape hatch would buy exactly what `Handle` gives.
The escape hatch that matters is `transport.Raw` (§3.5), registered from a
controller so the module's own container builds the handler.

Measured on go1.26.3/darwin-arm64: **17 allocations** for a POST with a JSON
body and a path and query parameter, asserted at a budget of 18 — 2 for
`ServeMux` dispatch, 6 for the edge ring, the rest for the typed path of which
~7 are `encoding/json`. The same handler
called directly allocates **0**. `TestAllocations` asserts the exact number.

**Usage**

```go
http.Server(
    http.Port(cfg.HTTPPort),
    http.Middleware(cors.Default().Handler, middleware.RealIP),
    http.ReadTimeout(10*time.Second),
)
```

Registers a lifecycle hook: starts after all dependencies are healthy, stops before pools close, waits `DrainDelay` so the load balancer observes the 503, then drains in-flight requests.

**Two escape hatches for what the typed port deliberately does not model** — uploads, downloads, SSE, WebSocket upgrades:

- `transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", h)` — registered from a **controller**, so the module's own container builds the handler with the module's own private providers. This is the one to reach for: an upload handler needs the repository, and a repository is private to its module. Note the pattern carries the method, unlike `Get`/`Post`, which name one already.
- `http.Handle("GET /debug/pprof/", h)` — an adapter option, evaluated in `main`, for handlers that need no module-scoped dependency at all: `net/http/pprof`, static assets, a vendor SDK's webhook receiver.

There is no `http.Raw(func(mux *http.ServeMux))`: a `ServeMux`'s entire API is `Handle`, so a mux handle would buy exactly what `http.Handle` already gives, at the cost of a third door and a registration order nobody can reason about.

---

### 4.2 `warren/transport/grpc`

- **Wraps** `google.golang.org/grpc` · **Mode** Wrap

**DEFERRED TO v0.2 (decided 2026-08-02).** Every design question is ruled — see
`transport/grpc/SPEC.md` — and the adapter is blocked not on a decision but on
`warren g proto`, which does not exist and is the harder of the two artifacts.

The reason, in one paragraph: a handler's `Req` must stay a plain Go struct, or
the HTTP adapter would `encoding/json`-encode a generated proto type for the
same handler and get field names, enums and oneofs wrong. So the wire needs
generated proto messages and the handler needs plain structs, with a
**generated** shim converting between them — which is legal against the shipped
port today, because `GRPCRoute.Bind(Codec)` is per-route, and impossible to
maintain by hand at scale. A proto codec over plain structs *was* prototyped and
measured (faster than JSON) and is rejected permanently: the descriptor never
reaches `protoregistry`, so the reflection service this section turns on by
default would have nothing to report, and field numbers would live in Go struct
tags where a reordering silently breaks every deployed client.

The round found **zero required changes to core `transport`**, which is the
strongest evidence that §3.5's `Codec`/`Bind` design holds. Measured, the
adapter costs **6 third-party modules** — `grpc`, `protobuf`, `genproto`,
`x/net`, `x/sys`, `x/text` — plus `buf` as tooling.

The v0.2 landing zone, decided rather than guessed:

```go
wgrpc.Server(
    wgrpc.Port(50051),              // 50051, not 9090 — that is Prometheus's
    wgrpc.Interceptors(...),        // Warren-owned Interceptor, driver-free
    wgrpc.TLS(cfg),                 // matches transport/http; TLSFiles for paths
)
```

`Recovery()` and `Tracing()` are NOT options: recover is outermost and not
removable, and telemetry composes off `Table.Telemetry()` — an application must
not be able to disable panic recovery by forgetting an argument.

Escape hatch, aliased because the package name collides with Google's:

```go
import (
    "google.golang.org/grpc"
    wgrpc "github.com/MerseniBilel/warren/transport/grpc"
)
wgrpc.Raw(func(s *grpc.Server) { pb.RegisterLegacyServer(s, impl) })
```

Streaming is out of both v0.1 and v0.2's typed surface; `transport.Raw` with
`ProtocolGRPC` already carries it, exactly as it carries HTTP upgrades.

`warren g proto` is specced WITH the adapter, in the CLI spec, and §8's command
surface gains it in the same change — not before.

---

### 4.3 `warren/openapi`

- **Mode** Build

Reads route registrations plus DTO struct tags (including `validate:` constraints) and emits OpenAPI 3.1. No annotations, no separate spec file to drift. Serves Scalar/Swagger UI at `/docs`, or `warren openapi export > openapi.yaml` for CI.

---

## 5. Messaging

### 5.1 `warren/broker/kafka`

- **Wraps** `twmb/franz-go` · **Mode** Wrap

**Why franz-go.** Feature-complete pure Go covering Kafka 0.8.0 through 4.2+, and it targets every client KIP. **Audited 2026-08-02**: v1.21.5, BSD-3-Clause, 2 971 stars, pushed 2026-07-31, not archived, and **4 third-party modules compiled in** (`klauspost/compress`, `pierrec/lz4`, `twmb/franz-go`, `golang.org/x/crypto`) — the second-smallest adapter footprint in the framework, after `transport/http`'s zero.

**Corrected 2026-08-02: this section used to say franz-go was chosen partly for "transactions — which the outbox relay needs for exactly-once publishing". That contradicted §5.5, which is right and this was wrong.** The outbox ack is in Postgres and the publish is in Kafka; no Kafka transaction spans two systems, and `Relay.DrainOnce` publishes and then marks published, so a crash between them republishes. There is no `Transactional` option, and the guarantee is **at-least-once plus inbox dedupe**. What the driver does ship is the idempotent producer, which removes duplicates from producer RETRIES within a session — the one duplicate source inside its control.

The alternatives each disqualify themselves for framework use:

| Client | Blocker |
|---|---|
| `segmentio/kafka-go` | Tested to Kafka 2.7.1; newer protocol features unimplemented. A framework can't ship a lagging driver. |
| `confluent-kafka-go` | cgo wrapper around librdkafka — breaks static builds and scratch containers for **every** user touching Kafka. Non-starter. |
| `IBM/sarama` | Low-level protocol surface, poor docs, pointer-passing causes heavy allocation. |

**Surface**

```go
func Broker(opts ...Option) warren.Module

func Brokers(...string) Option
func ConsumerGroup(string) Option
func TLS(*tls.Config) Option
func SASL(sasl.Mechanism) Option
func Transactional(bool) Option
```

**Usage**

```go
// main.go — swapping to another driver changes only this block.
// broker/memory ships and passes the same brokertest suite, so the claim
// is demonstrated rather than asserted.
kafka.Broker(
    kafka.Brokers(cfg.Kafka.Brokers...),
    kafka.ConsumerGroup(cfg.Kafka.Group),
    kafka.TLS(cfg.Kafka.TLS),
    kafka.SASL(kafka.SCRAM512(cfg.Kafka.User, cfg.Kafka.Password)),
)
```

**Consumer**

```go
type BillingConsumer struct{ activate *ActivateUserHandler }

func (c *BillingConsumer) Register(r transport.Registrar) {
    transport.OnEvent(r, "billing.subscription.created", c.activate,
        broker.WithRetry(broker.ExponentialBackoff(5)),
        broker.WithDeadLetter("billing.subscription.created.dlq"),
        broker.WithConcurrency(10),
    )
}
```

Kafka-specific concerns (partition assignment, offset commit strategy) are `kafka.*` options on the module; the consumer code stays driver-neutral. Escape hatch: inject `*kgo.Client` directly.

---

### 5.2 `warren/broker/rabbitmq`

- **Wraps** `rabbitmq/amqp091-go` · **Mode** Wrap

Same `Publisher`/`Subscriber` implementation. Topic → exchange + routing key. Quorum queues by default, DLQ via dead-letter exchange, publisher confirms on.

```go
rabbitmq.Broker(
    rabbitmq.URL(cfg.Rabbit.URL),
    rabbitmq.Exchange("events", rabbitmq.Topic),
    rabbitmq.PrefetchCount(20),
)
```

### 5.3 `warren/broker/nats` — JetStream. Same ports.

### 5.4 `warren/broker/memory`

- **Mode** Build · **Default in tests and in modular monoliths.**

In-process pub/sub with the same interface. This is what makes `warren extract module` viable: modules communicate through the broker port from day one, so extraction swaps the driver rather than rewriting call sites.

---

### 5.5 `warren/outbox`

- **Path** `warren/outbox` · **Module** core · **Mode** Build

The transactional outbox: a unit of work writes aggregate state and outbox
rows in ONE transaction, and a separate relay — a lifecycle participant,
leader-elected — drains them to the broker afterwards. If the transaction
rolls back the rows roll back with it; if the process dies between commit and
publish the rows are still there. The honest guarantee is **at-least-once
publication**, which is why the inbox dedupes on `Message.ID`.

```go
type Record struct { Topic string; Message broker.Message }

type Store interface {          // implemented by the persistence adapters
    Append(ctx, recs ...Record) error          // the writer — runs in the caller's transaction
    Pending(ctx, limit int) ([]Record, error)  // insertion order, parked excluded
    MarkPublished(ctx, ids ...string) error
    MarkFailed(ctx, id string, cause error) error
}
type Waiter interface { Wait(ctx context.Context) }  // optional: appending is the signal

func JSONEncoder(opts ...EncodeOption) Encoder       // Key = AggregateID
type Elector interface { Lead(ctx, fn func(context.Context) error) error }
func Standalone() Elector                            // always leads; warns with replicas
func NewRelay(store Store, pub broker.Publisher, opts ...RelayOption) *Relay
func (r *Relay) DrainOnce(ctx context.Context) (int, error)
func NewMemoryStore(opts ...MemoryOption) Store      // test / modular-monolith only
```

**One port, not two.** §5.5's "writer" and §6.1's "outbox table + writer" are
the same thing: `Store.Append`. The rows live in the database, so the
implementation is necessarily the persistence adapter's, and a separate
one-method `Writer` would be ring bureaucracy. `persistence` and `outbox` do
not import each other — the driver imports both.

**Ordering and failure.** The relay publishes in insertion order, batching
consecutive same-topic records into one call, and stops at the first failure
without publishing anything behind it: global order is stronger than
per-aggregate order and makes the promise one sentence long. Dispositions by
the publish error's outermost §2.6 code: `UNAVAILABLE` leaves the record for
the next poll and **never parks** (a broker outage is not the record's
fault); `INVALID` parks immediately (retrying a deterministic rejection would
stall the queue forever); anything else retries under `Backoff` and then
parks. Parking is loud — it breaks ordering for that key permanently.

**Leader election** is a port. `outbox.LeaderElection(postgres.AdvisoryLock("outbox"))`
in production; `Standalone()` by default, which is correct for one instance
and for the modular monolith.

**Kafka transactions do not make this exactly-once.** The outbox ack is in
Postgres and the publish is in Kafka — two systems. At-least-once plus inbox
dedupe is the guarantee; §5.1's stronger wording is corrected here.


### 5.6 `warren/inbox`

- **Path** `warren/inbox` · **Module** core · **Mode** Build

The dedupe-store port, plus the stdlib memory store that makes
dedupe-by-default cost neither Docker nor a database. Enabled by default —
at-least-once delivery means duplicates are certain, not hypothetical.
Durable stores ship with the persistence adapters (Postgres joining the unit
of work, Redis with native TTLs).

**What it does and does not promise.** It **suppresses redelivery of a
message that has already been disposed of**. It is not exactly-once, and it
does not close the concurrent window: the same ID delivered to two workers
at the same time runs the handler twice, because `Seen` and `MarkSeen` are
separate calls and nothing claims. That is deliberate — an atomic
claim-and-release trades a rare duplicate for a rare *loss*, since a crash
between claim and release suppresses the redelivery and the message is gone,
and every other disposition rule here chooses duplicates over loss.
**At-least-once remains the guarantee.** The Postgres store closes the
crash-after-success window by writing its row inside the handler's own
`UnitOfWork` transaction, which is why `ctx` is on both methods.

```go
type Store interface {
    Seen(ctx context.Context, key string) (bool, error)
    MarkSeen(ctx context.Context, key string, ttl time.Duration) error
}
func NewMemoryStore(opts ...MemoryOption) Store // map + mutex + injectable clock
func WithClock(now func() time.Time) MemoryOption
func WithMaxRecords(n int) MemoryOption
```

**The key is scoped by subscription**, `"<subscription>\x00<Message.ID>"`,
built by `broker.Pipeline` from the name it is given. Keyed on the message ID
alone, two features consuming one topic through one store would suppress each
other: whichever handler ran first marks it seen and the second never sees the
message at all. `Message.ID` is therefore **required** — `broker`'s
`RequireMessageID` stage refuses a message without one as `INVALID`, so it is
dead-lettered rather than filed under the empty key and destroyed.

The protocol is `broker.Deduplicate`'s (§3.4): `Seen` before the handler,
`MarkSeen` only after a nil disposition — a nacked message must not count as
its own duplicate — and store failures fail closed. TTL is per call because
the window is per *subscription* (it must exceed that subscription's
redelivery window), while one `Store` serves them all.

**`warren/inbox/inboxtest` is the contract suite**, written before the second
store as `broker/brokertest` was for the broker port, so Postgres and Redis do
not each re-derive the semantics. A driver runs it in one line
(`inboxtest.Run(t, newStore)`); ten checks bind, and the two that catch people
are silent ones: **re-marking refreshes the TTL in both directions** (Redis
`SET … EX` does it natively, Postgres needs `ON CONFLICT … DO UPDATE` — a
`DO NOTHING` expires a hot id mid-window, and a `GREATEST(…)` cannot shorten
one), and **`Seen` must not record** — a store that upserts on read reports
every first delivery as its own duplicate and the handler never runs at all.
The suite's own tests point nine deliberately broken stores at it and assert
each check is the one that catches its violation.

**Keys are opaque and hold no NUL.** A store must not truncate, case-fold or
normalise a key — a `VARCHAR(n)`, a MySQL `*_ci` collation, and a 255-byte
index prefix each collide distinct keys and silently discard live messages —
but it MAY assume no NUL byte, because Postgres `text` rejects `0x00` outright.
Warren upholds that end: the scope separator is U+001F, not NUL, and
`RequireMessageID` refuses an id carrying one as INVALID, so it is
dead-lettered rather than becoming a store error in production and nothing at
all on the memory store.

**The memory store is bounded** (200k records by default) and evicts the
earliest deadline first: the sweep can only reclaim what has already expired,
so an unbounded map under a default-on middleware is throughput × TTL — tens
of millions of entries at a modest rate. Eviction trades certainty for
memory, and per-process state means a redelivery landing on another replica
after a rebalance is not deduplicated at all. Both are why a real deployment
wants a durable store.

---

## 6. Persistence

### 6.1 `warren/persistence/postgres`

- **Wraps** `jackc/pgx/v5` · **Mode** Wrap

**Provides** (implemented 2026-08-02): `postgres.DB` — a driver-free query handle repositories inject — a `UnitOfWork` implementation, transaction-context propagation, the outbox store with `LISTEN`/`NOTIFY`, an advisory-lock elector, a durable inbox store, and a ping health check.

**It does NOT provide `*pgxpool.Pool`, and it never migrates at boot.** The pool reaches user code only through the named escape hatch `postgres.Raw(func(ctx, *pgxpool.Pool) error) Option`; a raw handle as the default path is what Wrap mode exists to prevent. Migrating from a lifecycle hook races every replica of a rolling deploy, applies DDL the still-serving old replicas were not written against, and turns one bad file into a simultaneous crash-loop — so `postgres.Schema` (plain SQL, goose file format) and `postgres.Migrate` are a deploy step, and there is no option to make them a boot step.

The outbox, inbox and elector are OPTIONS of `Module` — `WithOutbox()`, `WithInbox()`, `WithAdvisoryLock()` — not sibling modules: they need the pool, and a sibling module cannot see another module's providers.

```go
postgres.Module(
    postgres.DSN(cfg.Postgres.DSN),
    postgres.MaxConns(cfg.Postgres.MaxConns),
    postgres.WithOutbox(),
    postgres.WithAdvisoryLock(),
)
```

**Applying the schema is a deploy step, and it is two calls** — Warren's own
tables and yours. Both record into `warren_schema_migrations`, keyed by bare
filename, so give your files names that cannot collide with Warren's
(`00001_orders.sql`, not `00001_warren_outbox.sql`):

```go
// cmd/migrate/main.go — run by your deploy job, never by the service.
if err := postgres.Migrate(ctx, dsn, postgres.Schema); err != nil { ... }
if err := postgres.Migrate(ctx, dsn, schema.FS);      err != nil { ... }
```

**A repository, by hand or generated.** Three rules, and the first two are
what stop events being silently lost:

```go
type UserRepository struct{ db postgres.DB }   // DB resolves tx-from-context or pool

func (r *UserRepository) Save(ctx context.Context, u *domain.User) error {
    // 1. RequireTx FIRST. Outside a unit of work the row would autocommit
    //    while the aggregate's events stayed pending on an object about to
    //    go out of scope — lost silently, with no outbox row.
    if err := postgres.RequireTx(ctx, "save user"); err != nil {
        return err
    }
    if _, err := r.db(ctx).Exec(ctx,
        `INSERT INTO users (id, email) VALUES ($1, $2)
         ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email`,
        u.ID(), u.Email); err != nil {
        return err
    }
    // 2. Track enlists the aggregate, so its events reach the outbox at
    //    commit. persistence.Repository's own doc: a Save that does not
    //    Track loses them, and the contract suite asserts it.
    persistence.Track(ctx, u)
    return nil
}

func (r *UserRepository) FindByID(ctx context.Context, id domain.UserID) (*domain.User, error) {
    row := r.db(ctx).QueryRow(ctx, `SELECT id, email FROM users WHERE id = $1`, id)
    var u domain.User
    if err := row.Scan(&u.ID, &u.Email); err != nil {
        if errors.Is(err, postgres.ErrNoRows) {   // re-exported: no pgx import
            return nil, werrors.NotFound("user", id)
        }
        return nil, err
    }
    return &u, nil
}

func (r *UserRepository) Delete(ctx context.Context, id domain.UserID) error {
    if err := postgres.RequireTx(ctx, "delete user"); err != nil {
        return err
    }
    // 3. Check rows-affected: a DELETE that matched nothing is NOT_FOUND.
    //    The contract suite deletes twice and requires the second to fail.
    n, err := r.db(ctx).Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
    if err != nil {
        return err
    }
    if n == 0 {
        return werrors.NotFound("user", id)
    }
    return nil
}
```

Plain pgx-backed SQL, fully readable, yours to edit. `r.db(ctx)` is the only
piece of framework machinery and it does one thing: return the ambient
transaction if one exists, else the pool.

Repositories import the standard library's `errors` bare and Warren's under an
alias — `werrors "github.com/MerseniBilel/warren/errors"` — so
`errors.Is(err, postgres.ErrNoRows)` and `werrors.NotFound("user", id)` coexist
in one file. `postgres.ErrNoRows` is re-exported precisely so a repository
never imports `pgx` itself.

**`warren g repository --driver postgres` does not exist yet.** The three
rules above are unenforced by any compiler, which makes that generator the
highest-value item left in the CLI.

### 6.2–6.4 `mysql` / `mongo` / `redis`

Same `Repository` and `UnitOfWork` ports. Mongo's UoW uses sessions; Redis provides cache + distributed lock rather than repositories.

---

## 7. Cross-Cutting

### 7.1 `warren/observability`

- **Wraps** OpenTelemetry Go SDK · **Mode** Wrap (wiring, not abstraction)

**Amended 2026-08-02, on implementation.** One import instruments handlers, HTTP requests and broker propagation — three of the boundaries automatically, plus gRPC when that adapter ships. **Database query spans need one explicit line**, because the pgx tracer seam is a pgx type and an adapter may not import another adapter (invariant 4):

```go
postgres.Configure(func(c *pgxpool.Config) error {
    c.ConnConfig.Tracer = otelpgx.NewTracer()
    return nil
})
```

Handler instrumentation is composed at BOOT — `buildInvoker` wraps `app.Traced` and `app.Metered` around every route once, at step 5 — so the request path decides nothing, and a service with no telemetry bound has byte-identical route closures. `trace.Tracer` stays directly accessible through OTel's own global provider: this wraps setup, not the API, and **no OpenTelemetry type appears in any Warren signature**.

**List `observability.Module` anywhere in `warren.New`.** The exporter flushes LAST — after servers stop, after consumers drain, after pools close — because the bootstrapper resolves the telemetry before it instantiates anything else, so the flush hook is appended first and unwinds last. That is a property of the boot phase, not of argument order, which is the point: it cannot be got wrong.

**Log correlation is one line in `main`, and this module does not install it:**

```go
slog.SetDefault(slog.New(log.Handler(
    slog.NewJSONHandler(os.Stdout, nil), observability.LogAttrs())))
```

`log.Handler` is core, so a service with no telemetry still gets `correlation_id` on every record; `LogAttrs` adds `trace_id` and `span_id` when this module is present. Both resolve at emit time, so a request that logs nothing pays nothing. **Use the `*Context` methods** — `log.FromContext(ctx).InfoContext(ctx, …)`; slog's plain `Info` passes `context.Background()` and silently drops every correlation field.

**Known gaps, carried from the implementation round:** a handler span opens inside the route closure, so `app.Metered`'s histogram covers the handler only — the transport-level `SERVER` span around decode, validation and encode is what carries route and status. `WithoutMetrics()` disables the meter provider entirely. There is no exporter-connectivity health check: `/readyz` stays green while export is failing, and export failures surface as WARN log lines with a `module` field.

```go
observability.Module(
    observability.ServiceName("user-service"),
    observability.OTLPEndpoint(cfg.OTel.Endpoint),
    observability.SampleRatio(0.1),
)
```

Trace context propagates into `Message.Headers`, so a span survives the trip through Kafka into the consumer.

### 7.2 `warren/auth`

**Wraps** `golang-jwt/jwt/v5`, `coreos/go-oidc`. Guards run as edge middleware; identity lands on the context; `app.Authorized(policy)` runs as core middleware so authorization applies to gRPC and consumers too.

```go
r.HTTP().Get("/users/{id}", c.get, auth.RequireScope("users:read"))
```

### 7.3 `warren/resilience`

**Wraps** `sony/gobreaker`, `cenkalti/backoff/v4`, `x/time/rate` behind one `Policy` interface. Nobody should configure gobreaker in a handler.

```go
resilience.Policy(
    resilience.Timeout(3*time.Second),
    resilience.Retry(3, resilience.Exponential(100*time.Millisecond)),
    resilience.CircuitBreaker(resilience.FailureRatio(0.5)),
)
```

### 7.4 `warren/jobs`

**Wraps** `robfig/cron/v3`. Cron and background workers as lifecycle participants — they start after dependencies are ready and drain on shutdown, unlike loose goroutines.

```go
jobs.Cron("0 2 * * *", cleanupHandler, jobs.LeaderOnly())
jobs.Worker(reconcileHandler, jobs.Interval(30*time.Second))
```

### 7.5 `warren/testing`

**Vendors nothing.** Standard library plus core — no testify, no
testcontainers, matching §1.6. Container fixtures, when they exist, land in
`warren/testing/containers`, its own module, so Docker never enters this
one's graph.

```go
func TestRegisterUser(t *testing.T) {
    a := warrentest.NewModuleTest(t, user.Module(),
        warrentest.Replace[domain.UserRepository](fakes.NewUserRepo()),
        warrentest.WithMemoryBroker(),
    )
    // NewModuleTest registers Close with t.Cleanup; calling it is optional.

    res, err := warrentest.Invoke[RegisterUser, UserDTO](t.Context(), a,
        RegisterUser{Email: "a@b.com", Name: "Ada"})

    if err != nil {
        t.Fatalf("Invoke: %v", err)
    }
    warrentest.AssertPublished[domain.UserRegistered](t, a)
}
```

Context is FIRST, as everywhere else in Warren. `InvokeIn` takes a module
name for a test spanning several features, and `WithValidator` compiles the
routes against another validator — how a module whose requests carry tags
core refuses is tested at all.

Every CLI generator has golden-file tests — templates break silently
otherwise.

---

## 8. CLI — `warren/cli`

**Vendors** `spf13/cobra`, and nothing else. The tooling ring from §1.1 — build-time only, never in a service's `go.mod`, and it imports the runtime rather than the reverse.

`dave/dst` and `x/tools/go/packages` were both budgeted for and neither was needed: the AST editor splices bytes located through `go/parser` (§9), and `lint arch` reads imports syntactically so that it still works on a project that does not compile — which is when a layer violation is most likely.

### Three subsystems

| Subsystem | Purpose |
|---|---|
| **Templates** | `embed.FS`, ejectable via `warren templates eject` for per-org forks |
| **AST editor** | Stdlib: the insertion point is located with `go/parser` and the new text is spliced into the original bytes, so comments survive by construction. `dave/dst` was rejected — see the paragraph above and §9. |
| **Analyzer** | Syntactic — `go/parser` in `ImportsOnly` mode. `go/packages` was budgeted and dropped, which is what lets `lint arch` run on a project that does not compile. |

That analyzer is why the governance commands are cheap once the first exists — and it works on any Go project, Warren or not. If the framework stalls, that piece stands alone.

### Command surface

```bash
# scaffold
warren new myapp --module github.com/acme/myapp \
  --layout modular-monolith --transport http,grpc --db postgres --broker kafka

# generate
warren g module     user
warren g entity     user/User --fields "email:Email,name:string"
warren g command    user/RegisterUser --transport http,grpc
warren g repository user/User --driver postgres
warren g consumer   user --event billing.customer.created

# govern  ← the differentiators
warren lint arch                # dependency-rule violations, non-zero exit
warren doctor                   # drift, dead providers, missing wiring
warren graph modules|di|events
warren explain di UserRepository

# evolve
warren add rabbitmq
warren migrate layout --module task --to modular
warren extract module billing --into ../billing-service
```

### Generator rules

1. **Idempotent** — re-running is a no-op or a marked diff
2. **Surgical wiring** — AST edits, never regex
3. **Never overwrite silently** — conflicts prompt or fail
4. **Generated code is committed and owned** — no untouchable `.gen.go` except protobuf output
5. **Every template is forkable**

All generators support `--dry-run` and `--force`.

---

## 9. Third-Party Dependency Ledger

| Area | Library | Mode | Note |
|---|---|---|---|
| DI | `uber-go/dig` | Wrap | v1, strict SemVer, built to power frameworks · audited 2026-08-01: v1.19.0 (2025-05-13), MIT, not archived, 4.5k stars, 33 open issues |
| Inbox dedupe | — | Build | port + stdlib memory store in core; durable stores ship with persistence adapters |
| CLI | `spf13/cobra` | Vendor | build-time only, own module · audited 2026-08-02: cobra v1.10.2 (2025-12-04), Apache-2.0, 2 transitive. **`golang.org/x/tools` dropped 2026-08-02**: budgeted for `go/packages`, never imported — `lint arch` reads imports syntactically, which also makes it work on a project that does not compile |
| CLI AST editing | — (stdlib `go/parser` + `go/format`) | Build | **`dave/dst` rejected 2026-08-02**: no published releases, untouched 2022-12→2026-04, pins x/tools 2022, `go 1.18` — the wrong dependency under a subsystem that edits every user's `module.go` on a project adopting Go 1.27. Splicing bytes located by the AST preserves comments *by construction*, and is the model `x/tools/go/analysis` itself uses |
| Config | — | Build | Viper rejected: weight + global state |
| Lifecycle | — | Build | fx rejected: imposes its own lifecycle |
| Logging | `log/slog` | Vendor | stdlib |
| Validation (core) | — (stdlib `reflect`) | Build | port + `Required()` + `None()` in core; every unenforceable token is **refused at boot**, never ignored. Invariant 1 makes any library here impossible |
| Validation (full tags) | `go-playground/validator/v10` | Wrap | own module `warren/validate/playground` (§2.7a) · errors normalised to `errors.Error` before leaving it · **Audited 2026-08-02**: v10.30.3 (2026-05-29), MIT, 20 091 stars, pushed 2026-07-29, not archived, **8 third-party modules** compiled in (mimetype, locales, universal-translator, validator, leodido/go-urn, x/crypto, x/sys, x/text). Shipped as `warren/validate/playground`; a service that keeps `validate.Required()` pays none of them |
| HTTP | `net/http.ServeMux` | Vendor | stdlib. **`go-chi/chi/v5` rejected 2026-08-02** — healthy (MIT, zero deps, released 2026-07-06) and still wrong here: measured 4 allocs / 704 B per request vs ServeMux's 2 / 48, because `Mux.ServeHTTP` clones the request; and behind the sealed Registrar none of what chi is bought for is reachable. gin rejected (29 indirect requires, incl. a mongo driver and a JIT JSON encoder); httprouter rejected (v1.3.0, 2019-09; no commit since 2024-07 — the rule-9 case in all but the flag); fasthttp/fiber rejected (no `func(http.Handler) http.Handler`, no Flusher/Hijacker, no HTTP/2) |
| gRPC | `google.golang.org/grpc` | Wrap | shared middleware chain |
| Proto | `buf` | Vendor | tooling only |
| Kafka | `twmb/franz-go` | Wrap | pure Go, Kafka ≤4.2+, transactions |
| RabbitMQ | `rabbitmq/amqp091-go` | Wrap | official successor to streadway/amqp |
| NATS | `nats-io/nats.go` | Wrap | JetStream |
| Postgres | `jackc/pgx/v5` | Wrap | no ORM, by design. **Audited 2026-08-02**: v5.10.0, MIT, 14 087 stars, pushed 2026-08-01, not archived. Six modules compiled in — `pgpassfile`, `pgservicefile`, `pgx`, `puddle`, `x/sync`, `x/text` — measured with `go list -deps`, not read off a README. One pgx type in one exported signature: `postgres.Raw` |
| Migrations | **none — Build** | Build | **`pressly/goose` REJECTED 2026-08-02.** Healthy (MIT, v3.27.3 2026-07-22, 11.3k stars) and only 5 modules as a library import — but once migrating at boot is banned it buys nothing an ordered applier and a version table do not, and against goal 2 a contributor debugging a migration reads 100 lines of ours instead of goose's dialect/locker/provider layering. `postgres.Schema` ships in goose's FILE format, so a project already running goose, atlas or dbmate applies it with one line |
| Redis | `redis/go-redis/v9` | Wrap | cache + lock |
| Telemetry | OpenTelemetry Go | Wrap | **Audited 2026-08-02**: v1.44.0 (2026-05-27), Apache-2.0, 6 500 stars, pushed 2026-08-02, not archived. **24 third-party modules in the build graph**, including grpc, protobuf and genproto — an order of magnitude above anything else here (core 1, transport/http 0, postgres 6), which is why it is opt-in in its own module and `scripts/invariants.sh` refuses `go.opentelemetry.io` in any other go.mod. OTLP over gRPC only: the HTTP exporter reaches grpc through its own config package and is not lighter. Wiring only — no OTel type in any Warren signature |
| Auth | `golang-jwt/jwt/v5`, `coreos/go-oidc` | Wrap | |
| Resilience | `sony/gobreaker`, `cenkalti/backoff/v4` | Wrap | one `Policy` interface |
| Cron | `robfig/cron/v3` | Wrap | lifecycle-aware |
| Testing | none — stdlib + core | Build | `testify` and `testcontainers-go` were both budgeted and neither adopted: assertions are `if got != want`, and Docker fixtures wait for `warren/testing/containers`, its own module. |
| CLI | `cobra` | Vendor | build-time only |

---

## 10. End-to-End: One Feature Through Every Package

```go
// ── domain (imports: warren/domain, warren/errors) ───────────────────
func NewUser(email Email, name string) *User {
    u := &User{
        AggregateRoot: domain.NewAggregateRoot(NewUserID()),
        Email: email, Name: name, Status: StatusPending,
    }
    u.Raise(UserRegistered{UserID: u.ID(), Email: email.String()})
    return u
}

// ── application (imports: warren/app, warren/persistence, domain) ─────
func (h *RegisterUserHandler) Handle(ctx context.Context, cmd RegisterUser) (UserDTO, error) {
    email, err := domain.NewEmail(cmd.Email)
    if err != nil {
        return UserDTO{}, errors.Invalid("email", err)
    }
    if taken, _ := h.users.ExistsByEmail(ctx, email); taken {
        return UserDTO{}, errors.Conflict("user already exists")
    }

    u := domain.NewUser(email, cmd.Name)

    // Aggregate state + outbox row commit in ONE transaction.
    if err := h.uow.Do(ctx, func(ctx context.Context) error {
        return h.users.Save(ctx, u)
    }); err != nil {
        return UserDTO{}, err
    }
    return toDTO(u), nil
}
// No net/http. No pgx. No kgo. That is the entire point.

// ── interfaces (imports: warren/transport) ───────────────────────────
func (c *UserController) Register(r transport.Registrar) {
    // v0.1 (Go 1.26): free functions. On 1.27 these become methods with the
    // same names and argument order — see §3.5.
    transport.Post(r, "/users", c.register)
    transport.Method(r, "user.v1.UserService/Register", c.register)
}

// ── bootstrap ────────────────────────────────────────────────────────
func main() {
    // main reads config values at composition time (adapter options), so it
    // loads once and provides the value — config.Module[T] is the form for
    // services that don't (§2.4).
    cfg, err := config.Load[Config](config.WithEnvPrefix("WARREN"))
    if err != nil {
        log.Fatal(err)
    }
    warren.New(
        warren.NewModule("config",
            warren.Providers(func() Config { return cfg }),
            warren.Exports[Config](),
        ),
        observability.Module(observability.ServiceName("user-service")),
        postgres.Module(postgres.DSN(cfg.Postgres.DSN)),
        kafka.Broker(kafka.Brokers(cfg.Kafka.Brokers...)),
        outbox.Relay(),
        user.Module(),
        billing.Module(),
        http.Server(http.Port(8080)),
        grpc.Server(grpc.Port(9090)),
    ).Run()
}
```

**What happens on `POST /users`:**

```
ServeMux → edge middleware (recover, correlation ID, telemetry, yours)
    → decode JSON → validate → app.Handler
        → core middleware (trace, transaction, metrics)
            → Handle()
                → domain.NewUser raises UserRegistered
                → uow.Do: INSERT users + INSERT outbox, one COMMIT
            ← UserDTO
    → encode 201
                                    ⋮  (asynchronously)
outbox relay polls → publishes to Kafka via franz-go
    → billing module's consumer receives it
        → dedupe (inbox) → retry policy → its own handler
```

Every arrow crosses a package boundary defined in this document. No arrow points inward past a contract.

---

*Section 8 (CLI) and the outbox relay design are the parts most likely to change once prototyped.*
