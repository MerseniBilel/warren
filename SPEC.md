# `github.com/MerseniBilel/warren` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — the §2.1 surface stands; open questions (step-0 vs module, step-5 owner, `Run`'s error rule) are warren.md amendments to settle before those seams are built |
| **Source** | [warren.md §2.1](warren.md) |
| **Module** | core |
| **Mode** | Build |
| **Wraps** | — |

## Problem

Without this package there is no place to declare a module and no place to boot
one. Go's DI containers give you a single global type-keyed container, so every
provider in the process is visible to every other provider, and wiring mistakes
surface as a nil-pointer panic on request 1 rather than at startup. Warren needs
a module declaration that is a **value, not a side effect**, so that the whole
graph can be walked and checked before anything is constructed.

## Goals

- Own application bootstrap, the module system, and the run loop (§2.1).
- `NewModule` returns an **inert value**. Nothing registers on construction; the
  bootstrapper walks the entire graph before materialising containers. That
  ordering is what makes cycle detection and encapsulation *checkable* rather
  than emergent (§1.2, §2.1).
- Give real module encapsulation: anything not named in `Exports` is private to
  its module. A module importing `user` sees `domain.UserRepository` and
  **cannot** resolve `*RegisterUserHandler` (§1.2, §2.1).
- Drive the boot sequence of §1.3 so that **every error the framework can detect
  surfaces at boot, never on request 1**.
- `Run` blocks and handles SIGINT/SIGTERM; `Start`/`Stop` exist so tests can
  drive the same sequence without signals (§2.1).

## Non-goals

- **Not a container.** Scoping, graph validation, resolution and diagnostics
  belong to `warren/di` (§2.2). This package holds the module *declaration* and
  the sequencing; `di` holds the machinery.
- **Not a lifecycle runner.** Ordered start/stop, readiness gating and drain
  belong to `warren/lifecycle` (§2.3). `warren.OnStart` / `warren.OnStop` are
  declaration-side sugar that end up as lifecycle hooks.
- **Not config.** Layered resolution and validation belong to `warren/config`
  (§2.4), which contributes itself as a module via `config.Module[T]`.
- **No knowledge that HTTP, SQL or Kafka exist.** The kernel ring is stdlib +
  dig only (§1.1, invariant 1).
- **No compile-time wiring.** The scoped-child-container model assumes runtime
  scoping. If the DI mechanism ever changes to `wire`-style codegen this model
  needs redesigning (§1.2, PRD open question #1) — it is out of scope here.

## Public API

`warren.md` §2.1 fixes the following surface. Doc comments are added here; no
signature is changed.

```go
// Module is an inert declaration of one module: its name, its imports, its
// providers, controllers, consumers, exports, and its lifecycle hooks.
// Constructing a Module registers nothing and performs no work — the
// bootstrapper walks the whole graph before any container is materialised.
type Module // representation not fixed by warren.md — see Open questions

// ModuleOption configures a Module during NewModule.
type ModuleOption // representation not fixed by warren.md — see Open questions

// App is a bootstrapped application: the flattened module graph, its scoped
// containers, and its run loop.
type App // representation not fixed by warren.md — see Open questions

// New builds an App from the given module declarations. It does not start the
// application; the boot sequence runs in Run or Start.
func New(modules ...Module) *App

// Run boots the application and blocks, handling SIGINT and SIGTERM. On signal
// it runs the shutdown sequence and returns.
func (a *App) Run() error

// Start runs the boot sequence and returns once the application is serving.
// It exists so tests can drive boot without signals.
func (a *App) Start(ctx context.Context) error

// Stop runs the shutdown sequence: readiness closes first, then hooks stop in
// reverse order.
func (a *App) Stop(ctx context.Context) error

// NewModule returns an inert Module value named name, configured by opts.
// Nothing is registered and no container is touched.
func NewModule(name string, opts ...ModuleOption) Module

// Imports declares the modules this module depends on. Only the imported
// modules' exported bindings become visible.
func Imports(modules ...Module) ModuleOption

// Providers declares constructors owned by this module. A provider is private
// to its module unless it is also named in Exports.
func Providers(constructors ...any) ModuleOption

// Controllers declares the controllers this module registers at boot step 5.
func Controllers(controllers ...any) ModuleOption

// Consumers declares the message consumers this module registers at boot step 5.
func Consumers(consumers ...any) ModuleOption

// Exports makes T resolvable by modules that import this module. Anything not
// exported stays private to the module.
func Exports[T any]() ModuleOption

// OnStart registers a startup hook for this module, run in dependency order.
func OnStart(fn func(context.Context) error) ModuleOption

// OnStop registers a shutdown hook for this module, run in reverse order.
func OnStop(fn func(context.Context) error) ModuleOption
```

Declaration site, from §2.1:

```go
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

## Behaviour

### The boot sequence (§1.3) — this package owns the ordering

```
 0  load config          layered: defaults → file → env → flags, validated
 1  flatten module graph resolve imports, detect cycles → fail
 2  build scopes         one child container per module, copy exported bindings
 3  VALIDATE GRAPH       every dep resolvable? ambiguous? unused? → fail
 4  instantiate          singletons, topological order
 5  register             controllers + consumers build route tables in memory
 6  OnStart              dependency order: pool → repos → consumers → servers
 7  readiness opens      health endpoint flips green
 8  serve
──────────────────────── SIGTERM
 9  readiness closes     LB drains BEFORE anything stops
10  OnStop               reverse order, per-hook timeout, force-kill deadline
```

Ownership: step 0 is `warren/config`; steps 1–4 are `warren/di`; steps 6–7 and
9–10 are `warren/lifecycle`. This package sequences them and owns the signal
handling around step 8. Step 5's owner is not fixed by `warren.md` — §1.4 and
§3.5 put route-table construction on the adapter side — see Open questions. The order may not be rearranged
(AGENT.md § Two orderings you may not rearrange).

`warren.md` does not draw the internal line inside steps 1–2 between this
package's graph walk (§1.2: "the bootstrapper walks the whole graph first") and
`di`'s container work — see Open questions.

- `New` returns no error, so no fallible boot work can happen there — it
  collects the module values. (An inference from the §2.1 signature; warren.md
  states no behaviour for `New`.)
- `Start(ctx)` runs steps 0–7 and returns once the application is serving.
- `Run` calls `Start`, blocks on SIGINT/SIGTERM, then runs steps 9–10.
- `Stop(ctx)` runs steps 9–10 without waiting for a signal.
- Failure at any of steps 1, 3 or 4 is a startup failure, returned as an error;
  nothing is left half-started.

### Module encapsulation (§1.2)

The bootstrapper builds one child container per module (step 2) and copies into
each scope only the bindings its imports **export**. In §1.2's worked example,
`billing` imports `platform` and `user`; it therefore sees `UserRepository` and
**cannot** see `*UserService`.

Because a module declaration is a value and not a side effect, the whole graph
exists in memory before step 2. This is what makes cycle detection (step 1) and
encapsulation (step 2) checkable properties rather than emergent ones. A module
declaration that registers on construction breaks steps 1–3.

### No reflection on the request path (§1.4)

Reflection runs during steps 1–5 only. By step 8 the route table holds pre-built
closures with middleware already composed, and the DI container is not consulted
at request time.

## Errors

`warren.md` does not fix error text for this package. Recorded here is what must
be produced; the wording is open and must be pinned before implementation.

| Path | Condition | Text |
|---|---|---|
| Step 1 | Import cycle between modules | **Open.** Must name the modules on the cycle and where each was declared. Which package emits it depends on the step 1–2 boundary — see Open questions. |
| Step 3 | Graph validation failure | Produced by `warren/di`; this package surfaces it unchanged. The golden target is in [di/SPEC.md](di/SPEC.md). |
| Step 4 | A singleton constructor returns an error | **Open.** Must name the constructor and the module that declared it. |
| Steps 6, 10 | Hook failure or timeout | Produced by `warren/lifecycle` — see [lifecycle/SPEC.md](lifecycle/SPEC.md). |

Per AGENT.md § Errors, every message must name what was missing, who requested
it, where it was declared, and a copy-pasteable fix.

## Testing

- **Golden-file test for every error message** this package emits, once the text
  is agreed. Diagnostics are the product (invariant 2).
- **Encapsulation contract suite.** §1.2's graph as a table test: `billing`
  imports `platform` and `user`; resolving `UserRepository` from `billing`
  succeeds, resolving `*UserService` from `billing` fails.
- **Inertness test.** `NewModule` with providers, controllers, consumers and
  hooks constructs nothing and touches no container — assert no constructor ran.
- **Boot-order test.** Assert steps 0–8 and 9–10 execute in the documented order,
  and that a failure at step 1, 3 or 4 stops boot before step 6.
- **Allocation benchmark on the request path.** Owned jointly with the transport
  adapters: the assertion this package must support is that by step 8 the route
  table holds pre-built closures and the container is not consulted per request
  (invariant 7).
- Unit tests: no Docker, no network, no sleeps. `Start`/`Stop` exist precisely so
  boot can be driven synchronously in a test.

## Definition of done

- [ ] Spec approved.
- [ ] `Module` / `ModuleOption` representation agreed (Open questions).
- [ ] Public API implemented exactly as in Public API above, with doc comments.
- [ ] `NewModule` proven inert by test.
- [ ] Boot steps 0–8 and shutdown steps 9–10 sequenced and tested in order.
- [ ] Encapsulation contract suite passes on §1.2's graph.
- [ ] Every emitted error has agreed text and a golden-file test.
- [ ] Core module `go.mod` still lists stdlib + `go.uber.org/dig` only.
- [ ] No dig type appears in any exported signature here.
- [ ] `warren.md` amended in the same change if any signature diverged.

## Open questions

1. **Is `Module` a struct or an interface, and is `ModuleOption` a func type or
   an interface?** §2.1 uses both as opaque named types and fixes neither.
2. **Step 0 versus step 1.** §1.3 puts "load config" at step 0, *before* the
   module graph is flattened — but §2.4 delivers config as `config.Module[T]`,
   an ordinary module in the `warren.New(...)` list, which would make it a
   step-4 instantiation. Is config special-cased ahead of the graph walk, or is
   step 0 really "instantiate the config module first"?
3. **Related: §10's `main.go` reads `cfg.Postgres.DSN` and `cfg.Kafka.Brokers`
   at composition time**, inside the `warren.New(...)` call, while §2.4 says
   providers receive `Config` as an injected dependency. Where does that `cfg`
   value come from before `New` is called? These two usages cannot both be the
   intended pattern.
4. **What is the type of an entry in `Providers(...any)`, `Controllers(...any)`
   and `Consumers(...any)`?** They are `any` in the surface; is anything checked
   at step 1, or only at step 3?
5. **Can `Exports[T]()` export a type this module does not provide?** §2.1 says
   only that anything not in `Exports` is private. Is exporting a
   non-provided type a boot error?
6. **Are module names required to be unique across the graph?** The diagnostics
   in §2.2 address scopes by name (`scope "user"`, `scope "billing"`), which
   implies yes, but §2.1 does not say so.
7. **Does `Run` return the error from boot, from a hook, or both?** §2.1 gives
   `Run() error` with no aggregation rule.
8. **Where is the line between this package and `di` in boot steps 1–2?** §1.2
   says "the bootstrapper walks the whole graph first, then materialises
   containers", which puts the flatten-and-detect-cycles work here; §2.2 says
   `di` owns "the container, scoping, graph validation". Does the root package
   flatten and hand `di` a resolved graph, or does `di` own step 1 outright?
9. **What signal does `Run` treat as force-exit?** §2.3 gives a force-exit
   deadline (default 30s); §2.1 says `Run` handles SIGINT/SIGTERM. Whether a
   second signal short-circuits the deadline is unstated.
