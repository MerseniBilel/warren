# Spec: Modules and bootstrap

| | |
|---|---|
| **Module** | `warren` (the root package) |
| **Milestone** | v0.1 |
| **Status** | Draft — **carries an unresolved structural question, see §11.1** |
| **Depends on** | [03-di](../03-di/spec.md), [04-lifecycle](../04-lifecycle/spec.md), [05-config](../05-config/spec.md), [02-log](../02-log/spec.md) |
| **Blocks** | [08-transport-http](../08-transport-http/spec.md), [10-cli-new](../10-cli-new/spec.md), [11-cli-generate-module](../11-cli-generate-module/spec.md) |
| **PRD** | §4.2, §5.3, §14.1, §14.3 |
| **ADRs** | [ADR-0001](../../../docs/adr/0001-dependency-injection.md), [ADR-0003](../../../docs/adr/0003-repo-layout.md) — **and one still to be written** |
| **Date** | 2026-07-28 |

---

## 1. Problem

This is the feature the whole positioning rests on. PRD §3.1: *"Warren is what
you'd get if NestJS had been designed by Go developers: modules, DI, and a
generator."* A module is the unit that groups providers, controllers, and
consumers; declares what it imports; and publishes what it exports. Without it,
Warren is a DI container with extra steps.

It is also the unit of the modular-monolith story (PRD §5.3): a module has no
compile-time dependency on another module's internals, which is what makes
`warren extract module billing` conceivable at all.

## 2. Goals

1. **`warren.Module` as a value**, composed of providers, controllers,
   consumers, imports, and exports — exactly the shape in PRD §14.1.
2. **`warren.New(...).Run()`** boots the whole application in the shape of PRD
   §14.3, with no hidden control flow.
3. **A module's private providers are invisible to its siblings.** Enforced by
   the container's scopes, not by convention.
4. **Every wiring error surfaces at boot**, before a listener binds (PRD §4.1).
5. **A user can read the generated `main.go` and `module.go` and understand what
   happens, in five minutes** (PRD §1.3). If a reader has to ask "but what
   actually calls this," the design is wrong.

## 3. Non-goals

- **No dynamic module loading.** Modules are values composed in `main.go`.
- **No cross-module direct calls.** Modules communicate through exported ports
  and, from v0.3, published events. This restriction is what makes extraction
  possible and it is not a v0.1 convenience to be relaxed.
- **No `extract module` here.** That is v0.5; this spec only avoids making it
  impossible.
- **No auto-discovery of modules by directory scanning.** Explicit registration
  in `main.go` is the point (PRD §4.1 principle 1).

## 4. Public API

```go
package warren

// Module is a bounded context's wiring: what it provides, what it exposes,
// and what it needs from elsewhere. It is a value, not an interface — a module
// is data, and the framework does the work.
type Module struct { /* unexported */ }

func NewModule(name string, opts ...ModuleOption) Module

type ModuleOption func(*moduleConfig)

// Imports declares the modules this one depends on. Their exported types
// become resolvable here; their private ones do not.
func Imports(mods ...Module) ModuleOption

// Providers registers constructors, scoped to this module.
func Providers(ctors ...any) ModuleOption

// Controllers registers transport entry points. They are collected as a group
// so a transport adapter can bind every controller without knowing the modules.
func Controllers(ctors ...any) ModuleOption

// Consumers registers message consumers. Collected the same way. Accepted at
// v0.1 so generated modules have a stable shape; brokers arrive in v0.3.
func Consumers(ctors ...any) ModuleOption

// Exports makes type T resolvable by modules that import this one.
func Exports[T any]() ModuleOption

// App is a composed, not-yet-running application.
type App struct { /* unexported */ }

func New(items ...any) *App   // modules and top-level options

// Run boots the application: builds and validates the graph, runs start hooks
// in order, then blocks until a termination signal, then shuts down. It
// returns the first error that ended the run; nil on a clean shutdown.
func (a *App) Run() error

// RunContext is Run with a caller-supplied context, for tests and for
// embedding Warren in a larger program.
func (a *App) RunContext(ctx context.Context) error

// Start and Stop expose the halves separately, for integration tests that need
// a running app they control.
func (a *App) Start(ctx context.Context) error
func (a *App) Stop(ctx context.Context) error

// Options for New.
func Logger(l *slog.Logger) Option
func Config(loader config.Loader) Option
func ShutdownTimeout(d time.Duration) Option

// Registrar is what a controller receives to declare its routes. Each
// transport contributes its own section; the sections a build does not include
// are nil, and calling into one is a boot error naming the missing module.
type Registrar interface {
    HTTP() HTTPRegistrar   // nil unless warren/transport/http is present
    // GRPC and Events arrive in v0.3, additively.
}
```

Target usage, from PRD §14.1 and §14.3 — this is the acceptance criterion for
the API, and it must compile as written:

```go
func Module() warren.Module {
    return warren.NewModule("user",
        warren.Imports(shared.DatabaseModule, shared.BrokerModule),
        warren.Providers(NewUserService, postgres.NewUserRepository),
        warren.Controllers(NewUserController),
        warren.Exports[domain.UserRepository](),
    )
}

func main() {
    if err := warren.New(
        config.Module,
        user.Module(),
        http.Server(http.Port(8080)),
    ).Run(); err != nil {
        os.Exit(1)
    }
}
```

**Note the deviation from PRD §14.3**, which shows `.Run()` with no error
handling. Discarding the boot error contradicts PRD §4.1 principle 2 and
`main.go` is the one file where a user must see the failure path. `Run` returns
an error and the generated `main.go` handles it.

## 5. Behaviour

- **Boot sequence, in this order and no other:**
  1. Compose modules into scoped containers.
  2. Load configuration.
  3. `Build()` the graph — validation only, nothing constructed.
  4. Construct controllers and lifecycle participants.
  5. Run start hooks in registration order.
  6. Mark ready; bind listeners.
  7. Block until signal or context cancellation.
  8. Mark not-ready, run stop hooks in reverse, exit.

  **Nothing binds a port before step 6.** A service that accepts a request while
  its graph is still resolving is the failure mode PRD §4.1 principle 2 exists
  to prevent.
- **A module appearing twice is registered once.** Diamond imports are normal and
  must not double-construct; identity is the module's name, and two different
  modules sharing a name is an error.
- **Import cycles between modules are detected at compose time** and reported as
  the full chain.
- **Each module gets a container scope named for it.** Its providers are private
  to that scope; `Exports[T]()` promotes exactly T to the parent.
- **Controllers and consumers are group-registered**, so a transport binds every
  controller in the app without any module importing a transport package
  (invariant 4).
- **`Run` returns nil on a clean signal-initiated shutdown.** A signal is not an
  error.

## 6. Errors

| Condition | Code | Message |
|---|---|---|
| Two modules with the same name | `CodeInvalid` | Both names with the file that declared each, and that names must be unique because they are container scope identifiers |
| Module import cycle | `CodeInvalid` | The full cycle `user → billing → user`, with the `Imports` line of each edge |
| Provider not found | via `di` | See [03-di §6](../03-di/spec.md) — the resolution chain, requesting file, and copy-pasteable fix |
| Type used from a non-imported module | `CodeInvalid` | Which module exports it, the `warren.Imports(...)` line to add, and where to add it |
| Type used but not exported | `CodeInvalid` | The owning module and the `warren.Exports[T]()` line to add |
| Controller registered with no matching transport | `CodeFailedPrecondition` | The controller, the transport it wants, and the `go get` plus `warren.New` line that adds it |
| `Registrar.HTTP()` used without the HTTP module | `CodeFailedPrecondition` | Same, phrased for the call site |
| Start failed | wrapped | Delegated to lifecycle; the hook name and the underlying error |

**The two module-visibility errors are the ones that teach the module system.**
A user meets them the first time they reach across a boundary, and the message
is the entire documentation they will read at that moment.

## 7. Configuration

`ShutdownTimeout`, `Logger`, and `Config` are the only top-level options at
v0.1. Everything else is contributed by a module.

## 8. Testing

- **PRD §14.1 and §14.3 compile as written**, as a test. If the illustrative
  code in the PRD does not compile against the real API, one of the two is wrong
  and this is how that gets noticed.
- **Scope isolation**: module A's private provider is unresolvable from B;
  A's exported one is resolvable only after B imports A. Both directions.
- **Diamond imports** construct a shared provider exactly once.
- **Import cycle** produces the full chain.
- **Boot ordering**: a recording harness asserts nothing binds before step 6, and
  that a failed provider means no listener ever bound.
- **Golden files for every §6 message.**
- **`Start`/`Stop` are usable from a test** without signals — this is what every
  integration test in every user's project will depend on, so it is tested here.
- **Benchmark**: boot of a 10-module, 100-provider app against the 50 ms budget
  (PRD §8).

## 9. Invariants touched

All six, but two are decisive:

- **Invariant 1.** See §11.1. Unresolved.
- **Invariant 4 (handlers import no transport).** The `Registrar` and the
  controller group exist so that a module declares routes without importing a
  transport package. If this design forces the import, the design is wrong.

## 10. Definition of done

- [ ] §11.1 resolved and recorded in an ADR **before implementation starts**
- [ ] Public API matches §4, and PRD §14.1/§14.3 compile
- [ ] Golden files for every §6 message
- [ ] Unit tests per §8, `-race -shuffle=on`
- [ ] Committed benchmark against the 50 ms budget
- [ ] `make lint-modules` green, whatever §11.1 resolves to
- [ ] `make ci` green
- [ ] `docs/` concept pages: modules, and the bootstrap sequence
- [ ] Runnable example in `examples/minimal/` — a two-module app
- [ ] Changelog fragment

## 11. Open questions

### 11.1 What does the core module actually contain? — blocking

**Two repository documents currently contradict each other.**

[docs/architecture.md §2](../../../docs/architecture.md) draws the core module as
containing `di · lifecycle · log · errors · domain · app · health · config-port`.
[dependencies.md §4](../../../docs/dependencies.md) places
`warren/di → go.uber.org/dig`.

Both cannot be true. If the root `warren` package provides `New` and `Module`,
it must depend on the container. If the container is `warren/di` and `warren/di`
requires `dig`, then **the core module transitively requires `dig`** and
[AGENT.md invariant 1](../../../AGENT.md) — "a minimal Warren service has a
`go.mod` an auditor can read in one screen" — stops being demonstrable.

Three candidate resolutions:

| | Approach | Cost |
|---|---|---|
| **A** | Core holds ports only (`errors`, `log`, `lifecycle`, `domain`, `app`, config port). `warren.New` moves to a `warren/app` module that depends on `warren/di`. | The root import path is no longer where `New` lives, which is surprising and affects every generated `main.go` and every doc page. |
| **B** | Core defines a `di.Container` **interface**; the dig-backed implementation stays in `warren/di`; `warren.New` accepts a container. | Every service imports `warren/di` anyway, so the auditor sees `dig` regardless — invariant 1 is satisfied on paper and not in substance. |
| **C** | The spike ([00](../00-di-approach-spike/spec.md)) chooses the hand-written generics container, no third-party dependency, and the whole question disappears. | Only available if the spike goes that way. Cannot be assumed. |

**This must be settled before any code in this spec is written**, because it
determines the import path in every generated `main.go` — the single hardest
thing to change later. It needs an ADR amending 0001 or 0003.

Note that option C makes the problem vanish. That is not a reason to prefer it,
but the spike should report which options it leaves open.

### 11.2 Other questions

2. **Is `Consumers` accepted at v0.1 with no broker?** Argued yes in §4: it keeps
   generated `module.go` stable so v0.3 does not rewrite every user's file. The
   counter is an option that does nothing, which is a small lie in the API.
3. **How does a module contribute lifecycle hooks?** Through a provider that
   takes `*lifecycle.Lifecycle`, or a dedicated `warren.Hooks(...)` option? The
   former is fewer concepts; the latter is more discoverable. Decide against real
   generated output.
4. **Does `Registrar` returning `nil` for an absent transport read well?** A nil
   return that must be checked is a poor Go API. The alternative is a
   compile-time split where the HTTP module contributes the registration
   function. Revisit while building [08](../08-transport-http/spec.md) — that is
   the first real user of it.
