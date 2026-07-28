# Spec: Dependency injection container

| | |
|---|---|
| **Module** | `warren/di` |
| **Milestone** | v0.1 |
| **Status** | Draft — **blocked on [00-di-approach-spike](../00-di-approach-spike/spec.md)** |
| **Depends on** | [00-di-approach-spike](../00-di-approach-spike/spec.md), [01-errors](../01-errors/spec.md) |
| **Blocks** | [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md), and every generator |
| **PRD** | §4.1, §4.2, §6.1, §6.6, §7.4, §8 |
| **ADRs** | [ADR-0001](../../../docs/adr/0001-dependency-injection.md) |
| **Date** | 2026-07-28 |

---

## 1. Problem

A module system needs a container: something that takes constructor functions,
works out the order to call them in, and hands each one its dependencies. Go has
no such thing built in, and the two available answers are a code generator that
is now archived and a reflection container whose error messages are the reason
people abandon DI frameworks.

The hard part is not resolution. It is **failing well**: PRD §8 calls DI error
quality "the single most common reason DI frameworks are abandoned," and PRD
§4.1 principle 2 requires every graph error to surface at boot rather than on
the first request.

## 2. Goals

1. **Register constructors of the form `func(deps...) (T, error)`** (PRD §4.2)
   and resolve them by type.
2. **Validate the entire graph at boot** — before any listener binds — and exit
   non-zero with a usable message (PRD §4.1).
3. **Scope providers to a module.** A provider registered in one module is not
   resolvable from another unless exported (PRD §14.1).
4. **Collect groups**: every `Controller`, every `Consumer`, resolvable as a
   slice without each module knowing about the others.
5. **Expose the graph as plain data**, so `warren graph di` and
   `warren explain di UserRepo` (PRD §7.4, v0.4) are a rendering problem rather
   than a re-derivation.
6. **Resolve a 200-provider graph in under 20 ms**, holding the 50 ms total
   startup budget (PRD §8) with room for everything else.

## 3. Non-goals

- **No runtime resolution.** Everything resolves at boot. Resolving during a
  request is what makes reflection cost show up in a latency graph, and it is
  what turns a wiring bug into a 500 (PRD §4.1: reflection belongs at boot).
- **No lifecycle.** Ordered start/stop is [04-lifecycle](../04-lifecycle/spec.md).
  This separation is precisely why `fx` was rejected (ADR-0001).
- **No lazy or optional providers at v0.1** beyond what the fixture needs.
- No user-facing `dig` concepts. See §9.

## 4. Public API

**This section is provisional until the spike settles.** The surface below is
written against ADR-0001's decision (wrap `dig`) and is deliberately shaped so
that the generics prototype could implement it unchanged — which is itself a
requirement: if the chosen engine forces a different public API, the wrapper is
not doing its job.

```go
package di

// Container holds providers and resolves them. Not safe for concurrent
// registration; resolution after Build is safe for concurrent use.
type Container struct { /* unexported */ }

func New(opts ...Option) *Container

// Provide registers a constructor. ctor must be a function returning
// (T, error) or (T). Registering the same type twice is an error at Build.
func Provide[T any](c *Container, ctor any, opts ...ProvideOption) error

// ProvideValue registers an already-constructed value.
func ProvideValue[T any](c *Container, v T) error

// Resolve returns the T from the container, constructing it if needed.
func Resolve[T any](c *Container) (T, error)

// Group collects every provider registered into the named group.
func Group[T any](c *Container, name string) ([]T, error)

// Scope returns a child container whose providers are invisible to its parent
// and to its siblings. This is what makes a module boundary real.
func (c *Container) Scope(name string) *Container

// Export makes a type resolvable from the parent scope.
func Export[T any](c *Container) error

// Build validates the whole graph without constructing anything: missing
// providers, ambiguity, and cycles all surface here. Call it before binding a
// listener.
func (c *Container) Build() error

// Graph returns the resolved dependency graph as plain data. No driver type
// appears in it; `warren graph di` renders it to DOT or Mermaid.
func (c *Container) Graph() Graph

type Graph struct {
    Nodes []Node
    Edges []Edge
}

type Node struct {
    Type     string // fully qualified, e.g. "user/domain.UserRepository"
    Scope    string // owning module
    Provider string // constructor name
    File     string // source location of the constructor
    Line     int
    Group    string // "" unless group-registered
}

type Edge struct{ From, To string } // From depends on To

type Option func(*config)
type ProvideOption func(*provideConfig)

func InGroup(name string) ProvideOption
func As[T any]() ProvideOption // register the concrete type as interface T
```

**No `dig` type appears anywhere above** — not in a parameter, a return, a
struct field, or an error. That is ADR-0001 rule 2, and `Graph` is the test of
it: it would have been far cheaper to return `dig`'s visualisation output
directly.

## 5. Behaviour

- **Registration order does not matter.** The graph is topologically sorted at
  `Build`.
- **`Build` constructs nothing.** It proves every requested type is reachable.
  A constructor with a side effect must not run during validation.
- **Each provider is called at most once per scope.** Singleton within a scope
  is the only lifetime at v0.1; request scoping is deferred until a transport
  asks for it.
- **A child scope sees its parent's exported types; a parent sees nothing of its
  child.** Siblings see nothing of each other. This is the enforcement behind
  PRD §14.1's `Imports`/`Exports`.
- **Constructor errors abort the boot** and are wrapped with the constructor's
  name and file.
- **Cycles are detected at `Build`** and reported as the full chain.
- **Concurrency**: `Provide` is single-goroutine (it happens during module
  definition); `Resolve` after `Build` is safe for concurrent use.

## 6. Errors

This table is the feature. Every message is covered by a golden test.

| Condition | Code | Message |
|---|---|---|
| No provider for a requested type | `CodeInternal` | The missing type; the resolution chain that led to it; the file and line of the requesting constructor; and a copy-pasteable `warren.Provide(NewX)` line naming where to add it |
| Two providers for one type | `CodeInternal` | Both candidates with source locations, and how to disambiguate |
| Cycle | `CodeInternal` | The complete cycle rendered `A → B → C → A`, with each edge's source location |
| Constructor returned an error | wrapped | Constructor name, file, and the underlying error via `%w` |
| Not a function, or wrong signature | `CodeInternal` | What was passed, what was expected, and the calling file |
| Type resolved from a sibling scope | `CodeInternal` | Which module owns it, and that it must be exported — with the `warren.Exports[T]()` line to add |
| Empty group requested | — | Not an error. An empty slice; a module with no controllers is normal |

**The scope error is the one that will be hit most.** A user hits it the first
time they try to use another module's repository, and the message either teaches
them the module system in one line or sends them to the issue tracker.

## 7. Configuration

None user-facing. `Option` exists for internal test seams (a deterministic
iteration order for golden tests, for instance) rather than for tuning.

## 8. Testing

- **The §4 fixture from the spike becomes the permanent test suite** for this
  package.
- **Golden files for every message in §6.** These are the deliverable; a
  regression in an error message is a regression in the headline feature.
- **Scope isolation**: sibling scopes cannot resolve each other's private types
  — asserted for both directions.
- **Cycle detection** on a 3-cycle, a self-cycle, and a cycle reachable only
  through a group.
- **`Build` constructs nothing**: providers with a side effect that records a
  call, asserted zero after `Build` and non-zero after `Resolve`.
- **Concurrency**: 100 parallel `Resolve` calls after `Build` under `-race`,
  asserting each provider ran exactly once.
- **Benchmark**: a synthetic 200-provider graph, asserting the 20 ms goal from
  §2. Committed, so a regression is visible rather than remembered.

## 9. Invariants touched

- **Invariant 1 (core is stdlib-only).** If ADR-0001 stands, this module depends
  on `dig` and therefore **cannot** be part of the core module. See
  [06-module-and-bootstrap §11](../06-module-and-bootstrap/spec.md) — the
  question of what `warren.New` then imports is unresolved and needs an ADR.
- **Invariant 2 (no driver type in a public signature).** The whole of §4 is
  this invariant. `depguard`'s `dig-containment` rule in
  [`.golangci.yml`](../../../.golangci.yml) enforces the import side;
  `Graph` enforces the API side by construction.

## 10. Definition of done

- [ ] Spike decision recorded and this spec updated to match it
- [ ] Public API matches §4
- [ ] Golden files for every §6 message
- [ ] Unit tests per §8, `-race -shuffle=on`
- [ ] Committed benchmark meeting the §2 target, with the number in the doc page
- [ ] `make lint-modules` confirms `dig` is imported only here
- [ ] `make ci` green
- [ ] `docs/` concept page: providers and DI
- [ ] Runnable example in `examples/di/`
- [ ] Changelog fragment

## 11. Open questions

1. **Is `Build()` separate from `Run()`, or folded into it?** Separate lets a
   test validate a graph without starting anything, and lets `warren doctor`
   (v0.4) reuse it. Leaning separate.
2. **Does `As[T]()` cover interface binding well enough**, or is a distinct
   `ProvideAs` clearer at the call site? Decide against real generated
   `module.go` output, not in the abstract.
3. **Request-scoped providers.** Not needed by v0.1 and genuinely useful later
   (per-request tenant, transaction). Design the scope model so it can be added
   without breaking `Scope`, but do not build it.
