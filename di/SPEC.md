# Spec — `warren/di`

**Status:** approved 2026-07-29, implemented 2026-07-30 — all six decisions in §11 agreed as recommended
**Prior art audited:** 2026-07-29 — `dig` v1.19.0, `wire` v0.7.0; findings in §9.1
**Roadmap:** v0.1 · item 3 of 11
**Import path:** `github.com/MerseniBilel/warren/di`
**Depends on:** `reflect`, `runtime`, `context`, `slices`, `strings` · and
`warren/errors`

---

## 1. Problem

A Warren service is assembled from modules, and a module states what it needs
rather than how to build it. Something has to turn a set of constructors into a
built object graph, fail loudly when the set is incomplete, and expose the graph
as data so the CLI can answer questions about it.

Three failures this package exists to prevent:

- **Hand-written wiring in `main.go`.** It works, and it grows to four hundred
  lines that nobody re-orders correctly. Adding a dependency to a constructor
  means editing every call site that builds it. This is the thing `warren new`
  cannot generate honestly without a container.
- **A wiring mistake that surfaces as a 500.** A nil repository injected into a
  handler is a nil-pointer dereference on the first request that touches it, at
  11pm, in production. `docs/architecture.md §5` is explicit: nothing is
  listening until the graph is validated, so a wiring error is a startup crash.
- **A bad DI error message.** `docs/architecture.md §6` names this as the single
  most common reason people abandon a framework, and `docs/roadmap.md` makes it a
  v0.1 exit criterion: a missing provider prints the resolution chain, the
  requesting file, and a copy-pasteable fix. That is a requirement on this
  package specifically. Every message in §6 is written out for that reason.

`di` is built third because `lifecycle`, `app`, `warren`, and both generators all
sit on top of it, and because `warren.New` needs the container — which is why it
is ours rather than `dig`'s (`docs/architecture.md §3`).

## 2. Goals

1. **Registration is declarative and typed at the point of use.** A module says
   `di.Provide[OrderRepository](c, postgres.NewOrderRepository)` — the port it
   satisfies is stated, not inferred from a return type.
2. **The graph is validated without constructing anything.** Missing providers,
   duplicates, and cycles are found before a single constructor runs, so a
   failing boot has opened no connections.
3. **Every failure names a file and a fix.** Every message in §6 carries the
   registration site, and the resolution chain where one exists.
4. **The graph is available as data**, so `warren graph di` and
   `warren explain di` (v0.4) read it rather than re-deriving it. This is one of
   the two reasons the architecture rejected `dig`.
5. **Zero allocations on resolution after `Build`**, and no reflection in the
   request path.
6. Standard library only, permanently.

## 3. Non-goals

- **Decoration** — wrapping an already-provided `T`. It is the other hard half of
  a container (`docs/architecture.md §3` names it alongside value groups), and it
  has no consumer in v0.1: middleware composes through `app.Chain`, not through
  the container. The registry in §5.1 keeps a provider's identity separate from
  its type key so that decoration can be added without a breaking change.
- **Per-request construction.** See §11.2 — allowing it puts `reflect.Value.Call`
  in the request path, which `docs/architecture.md §7` forbids, and nothing in
  v0.1 needs it.
- **Named or tagged instances** — `di.Provide[*sql.DB](c, NewReplica,
  di.Named("replica"))`. Two of a type is expressed by having two types:
  `type ReplicaDB struct{ *sql.DB }`. That is idiomatic Go, it is checked by the
  compiler rather than by a string, and it keeps the key space at one provider
  per type — which is what makes the ambiguity error in §6.2 impossible to reach
  by accident. See §11.4.
- **Lazy singletons.** Everything registered is constructed at `Build`. A
  provider nobody uses is a `warren doctor` finding (v0.4, "dead providers"), not
  a runtime optimisation.
- **Optional dependencies**, `dig`'s `optional:"true"`. A constructor that works
  without a dependency takes it as a nil-able field set after construction, or is
  two constructors.
- **Lifecycle.** `OnStart`/`OnStop` belong to `warren/lifecycle`, which is built
  next and which imports this package. `di` knows nothing about starting or
  stopping — see §7 for the one property it must provide for that to work.
- **A container on the context.** `di` defines no context key. A handler receives
  its dependencies as struct fields, set at construction; a handler that reaches
  into a container at request time is service-locator, not injection.

## 4. Public API

### 4.1 The container

```go
// Package di is Warren's dependency-injection container: constructors are
// registered against the type they provide, the graph is validated before
// anything is built, and the result is available as data.
package di

// Container holds the registered providers and, after Build, the singletons
// constructed from them. Its zero value is not usable; construct one with New.
//
// Registration is not safe for concurrent use: a service registers from one
// goroutine at boot. Resolution after Build is read-only and is safe for
// concurrent use without a lock.
type Container struct{ /* unexported */ }

// New returns an empty Container.
func New() *Container
```

### 4.2 Registration

Each records the caller's file and line, so every message in §6 can name where a
provider came from. None returns an error: a registration failure is recorded and
reported by [Container.Validate], which keeps a module's wiring free of error
handling on every line. See §5.2.

```go
// Provide registers ctor as the constructor for T.
//
// ctor is a function returning either (R) or (R, error), where R is assignable
// to T. Its parameters are its dependencies, resolved by type. Naming T
// explicitly is what lets a concrete constructor register against the port it
// satisfies, which is Warren's central pattern:
//
//	di.Provide[domain.OrderRepository](c, postgres.NewOrderRepository)
//
// A constructor is called at most once. A second Provide for the same T is an
// error reported by Validate, not a silent replacement.
func Provide[T any](c *Container, ctor any, opts ...Option)

// Supply registers an already-constructed value as the instance for T, for
// something built before the container exists — configuration, most often:
//
//	di.Supply[*Config](c, cfg)
func Supply[T any](c *Container, value T, opts ...Option)

// Contribute registers ctor as one member of the group of T, resolved together
// by Group. Many providers may contribute the same type; that is the difference
// between Contribute and Provide.
//
//	di.Contribute[app.Middleware](c, NewAuthMiddleware)
//
// A type is provided singly or contributed to a group, never both; mixing them
// is an error reported by Validate.
func Contribute[T any](c *Container, ctor any, opts ...Option)
```

```go
// Option configures one registration.
type Option func(*provider)

// At overrides the registration site recorded for a provider. It is what makes a
// re-export possible without every message in §6 naming the framework instead of
// the user's module — see §7.
func At(site Site) Option

// Caller returns the Site of the function skip frames above the caller of Caller.
// Caller(0) is the caller itself, Caller(1) its caller.
func Caller(skip int) Site
```

`Option` exists for exactly one purpose today and is deliberately not a general
extension point: `di.Named` was declined in §11.4, and nothing else is planned.

### 4.3 Validation and construction

```go
// Validate reports every problem in the registered graph without constructing
// anything: a bad constructor signature, a duplicate, a missing provider, a
// cycle. Errors are joined, so one call reports all of them rather than the
// first.
//
// It is what warren doctor and warren lint call, and what Build runs first.
func (c *Container) Validate() error

// Build validates the graph and then constructs every singleton, in dependency
// order. It is idempotent: a second call returns the first call's error and
// constructs nothing.
//
// ctx is the boot context. A constructor may take a context.Context parameter to
// receive it — for a driver whose own constructor requires one — and must not
// retain it: it is cancelled when boot finishes, not when the process stops.
func (c *Container) Build(ctx context.Context) error
```

### 4.4 Resolution

```go
// Resolve returns the instance of T. It reports an error if T has no provider,
// if the container is not built, or if T is a group.
//
// After Build this is a map lookup and a type assertion: no construction, no
// reflection, no allocation.
func Resolve[T any](c *Container, target *T) error

// Group returns every contributed instance of T, in registration order.
//
// The order is the order the Contribute calls ran, which is module registration
// order. It is deliberate and tested: a middleware chain or a route table built
// from a group whose order varies between runs is a bug that reproduces once a
// week.
func Group[T any](c *Container, target *[]T) error
```

### 4.5 The graph

```go
// Graph returns the registered graph as data, for warren graph di and
// warren explain di. It is available after registration; Build is not required.
//
// The returned value shares nothing with the container and is safe to hold.
func (c *Container) Graph() Graph

// Graph is the registered object graph. Nodes are ordered by type name, so a
// rendering of it is stable between runs.
type Graph struct {
	Nodes []Node
}

// Node is one registered provider.
type Node struct {
	// Type is the type the provider satisfies — the T of Provide[T].
	Type reflect.Type
	// Deps are the provider's parameter types, in declaration order.
	Deps []reflect.Type
	// Ctor is where the constructor itself is defined, empty for a Supply.
	Ctor Site
	// Registered is where Provide, Supply, or Contribute was called.
	Registered Site
	// Kind distinguishes a single provider from a group member and a value.
	Kind Kind
}

// Site is a position in the source, used to name a file in an error message.
type Site struct {
	Func string
	File string
	Line int
}

// String returns "file:line".
func (s Site) String() string

// Kind classifies how a Node was registered.
type Kind uint8

const (
	KindProvided    Kind = iota // registered by Provide
	KindContributed             // registered by Contribute, one member of a group
	KindSupplied                // registered by Supply, already constructed
)

// String returns the kind's name, for example "Provided".
func (k Kind) String() string
```

That is the whole surface: three registration functions, two entry points, two
resolvers, and the graph.

**On `Resolve` taking a pointer.** Go permits no type parameters on methods, so
the generic functions take the container as their first argument — that part is
forced. The out-parameter is not: `Resolve[T](c) (T, error)` reads better. It is
rejected because a failed resolution then has to return a zero `T`, and for a
`T` that is a value type the compiler materialises one on every call. The
out-parameter form lets the error path touch nothing. §11 records this as the
one place where the API is shaped by cost rather than by taste; if a benchmark
does not support it, the two-return form is nicer and should win.

## 5. Behaviour

### 5.1 Keys and the registry

The key is `reflect.TypeFor[T]()`. One type, one provider — with groups as the
one deliberate exception, keyed by the same type and holding a list.

A provider's identity is kept separate from its type key in the registry, so a
future `Decorate[T]` can insert itself between the two without changing what
`Resolve` does. That is the only concession made to a non-goal.

### 5.2 Registration errors are deferred, not returned

`Provide`, `Supply`, and `Contribute` return nothing. A malformed registration —
a non-function, a wrong return arity, a type that does not satisfy `T` — is
recorded against its call site and reported by `Validate`.

The alternative is `if err := di.Provide[...](c, ...); err != nil` on every line
of every module's wiring, and generated code that is three times the size for a
class of error that always fails at boot anyway. The cost is real and worth
naming: **a mistake in a `Provide` call is caught at `Validate`, not at the
`Provide` itself.** What makes that acceptable is that the container has nothing
between the two — `warren.Run` validates immediately after registration, so the
gap is microseconds and the message names the exact line.

`Validate` joins its errors with `errors.Join`, so a service with four wiring
mistakes learns all four. Ordering is by registration site, so the output is
stable.

Two consequences of that, both discovered in implementation and both load-bearing:

- **One problem is returned unwrapped, not joined.** `errors.Join` holds several
  unrelated errors, so `errors.Detail` reports no fields and no fix for one — and
  the fields are where the chain, the requesting file, and the fix live. Joining a
  single cause would therefore have silently emptied §6.1, the exit criterion.
  Since almost every failed boot has exactly one cause, it is returned as itself;
  a caller facing several renders each branch.
- **Completeness and acyclicity are skipped when a registration was rejected.** A
  provider that failed §5.3 is not in the registry, so every type it would have
  provided would also be reported missing, and the real mistake would be buried
  under its own fallout.

### 5.3 What a constructor may look like

Accepted:

```go
func NewOrderRepository(db *sql.DB) *postgres.OrderRepository
func NewOrderRepository(db *sql.DB) (*postgres.OrderRepository, error)
func NewPool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error)
func NewClock() Clock
```

Rejected at `Validate`, each with its own message in §6:

| Shape | Why |
|---|---|
| `nil` | `di.Provide[T](c, nil)` — reflecting on it panics, so it is rejected before anything else |
| not a function | almost always `di.Provide[T](c, NewThing())` — the constructor was called |
| returns nothing, or three values | the contract is `(R)` or `(R, error)` |
| second return is not `error` | ditto |
| `R` not assignable to `T` | the registration is a lie; §6.5 names the missing method |
| `T` is `error` | `func() error` cannot be read: is the return the value or the failure? |
| variadic | its dependency count is unknowable, so the graph cannot be validated |
| two parameters of the same type | `func(a, b *sql.DB)` resolves one instance twice; it is a typo, not a design |
| returns `nil` with no error | §6.9 — a nil dependency is a 500 later; it fails boot instead |

The last four rows come from the prior-art audit in §9.1 rather than from
first principles, which is the point of doing it.

Dependencies are the parameter types, resolved by type. `context.Context` is
pre-registered and resolves to `Build`'s argument.

### 5.4 Validation, then construction

`Validate` does three passes over the registry, none of which calls a
constructor:

1. **Shape.** Every registration is a usable constructor for the type it claims.
2. **Completeness.** Every dependency of every provider has a provider. A missing
   one is reported with the chain that reached it (§6.1).
3. **Acyclicity.** Depth-first with a colour per node; a back edge is a cycle,
   reported as the path (§6.3).

`Build` then constructs, depth-first from each registered provider. A constructor
returning an error stops the build immediately — the graph is already known good,
so the failure is the constructor's own and there is nothing to be learned by
continuing to open connections.

**Memoisation is by constructor identity, not by type key.** This distinction is
the one the prior-art audit corrected (§9.1), and it matters because Warren's
central pattern makes it easy to register one constructor against two ports:

```go
di.Provide[domain.OrderReader](c, postgres.NewOrderRepository)
di.Provide[domain.OrderWriter](c, postgres.NewOrderRepository)
```

Keyed by type, that is two instances of the same repository — wasteful for a
stateless one and a genuine bug for anything holding a cache, a pool, or a
counter. Keyed by the constructor's code pointer, it is one instance reachable
under two types, which is what the author obviously meant. A constructor runs
exactly once no matter how many types it satisfies or how many dependents it has.

**A closure is never deduplicated, and that limit is deliberate.** `reflect`'s own
documentation warns that a func value's pointer is "not necessarily enough to
identify a single function uniquely": every closure created from one source
location shares a code pointer while capturing different variables, so merging
them would hand one caller another caller's instance. A named function or method
has no `.funcN` segment in its runtime name and is safe to memoise, which is the
case the pattern above relies on and the case generated code produces. Two
closures written at one source location are two constructors.

**Everything registered is constructed.** A provider nobody depends on is still
built, because the alternative is a container that decides for you which of your
providers matter, and a service whose Kafka connection opens on the first request
instead of at boot. Dead providers are a `warren doctor` finding.

Two consequences of that, both stated because they are the cost of eager
construction and neither is obvious:

- **An unused provider's dependencies must still be satisfiable.** `dig` tolerates
  an incomplete graph until something is actually invoked; Warren does not, because
  it builds everything. Registering a provider you do not use makes its
  dependencies your problem.
- **A failed `Build` leaves what it already constructed.** Half the singletons may
  exist, and some may hold a connection. Nothing is released, and that is
  deliberate: the caller is `warren.Run`, which is about to exit the process, and a
  teardown path that runs against a graph known to be half-built is a second,
  less-tested failure mode. `Build` is idempotent, so a test that builds a failing
  container repeatedly constructs nothing the second time.

### 5.5 Reflection, and where it stops

Reflection is used at registration and at `Build`: reading a constructor's
signature, and calling it. `docs/architecture.md §7` permits exactly this —
"reflection belongs to container construction at boot" — and forbids it in the
request path.

The line is held by there being **one lifetime in v0.1: the singleton.** Every
`reflect.Value.Call` happens inside `Build`. After `Build` returns, the instance
map is never written again, so `Resolve` is a lookup and a type assertion, and no
lock is needed for concurrent readers. That is the property §11.2 is protecting.

### 5.6 Cost

Startup has a 50 ms budget in `docs/roadmap.md`, and the container's share of it
must be a rounding error, since the rest belongs to the constructors themselves.

| Operation | Budget | Measured — Apple M3 Pro, Go 1.26.3 |
|---|---|---|
| `Validate`, 100 providers | < 1 ms, zero constructors called | **8.6 µs**, 21 allocations |
| `Validate`, 1000 providers | — | **96 µs** |
| `Build`, 100 providers | validation plus one `reflect.Value.Call` each | **125 µs**, registration included |
| `Build`, 1000 providers | — | **1.22 ms** |
| `Resolve[T]` after `Build` | 0 allocations | **12.3 ns, 0 allocations** |
| `Group[T]` after `Build` | 0 allocations | **11.9 ns, 0 allocations** |
| `Graph()` | allocates; it is tooling, called once | not benchmarked |

A hundred providers is a large service, and the container's share of the 50 ms
startup budget is 0.13 ms of it — a quarter of one percent. The rest of boot is the
constructors' own cost, which is where it belongs.

`Resolve` reaching zero depends on `reflect.TypeFor[T]()` not allocating, which
holds for pointer and interface types — what a service actually registers — and
not necessarily for a large value type. The benchmark states both, and the
allocation test asserts the pointer and interface cases, in the manner of
`errors/SPEC.md §5.6`.

### 5.7 A constructor may consume a group

A constructor whose parameter is `[]T` receives the group of `T`, in registration
order. That is how `warren` collects routes and middleware from modules that know
nothing about each other:

```go
func NewRouter(mw []app.Middleware) *app.Router
```

A directly provided `[]T` wins over the group of `T`, so a constructor that wanted
the whole slice is never surprised by one somebody registered as a unit. An empty
group is not an error: a service with no middleware still boots, and `Group`
yields an empty slice.

This was missing from the approved spec and is recorded here because §6.7's fix
line already assumed it.

## 6. Every error message this package emits

Every message is `errors.Invalid` when the graph is at fault and the developer
can fix it, and `errors.Internal` when a constructor itself failed and the graph
was correct. Each carries `Op("di.Validate")`, `Op("di.Build")`, or
`Op("di.Resolve")` — the public entry point, not the internal walk — and each has
a `Fix`. Rendered here through `errors.Detail`, which is how `warren` prints a
boot failure.

Every message here is a golden file: eighteen of them, since §6.9 has a `Supply`
variant and the two group mixups are separate renderings.

### 6.1 No provider — the exit criterion

```
di.Build: no provider for *sql.DB

  requested by  internal/modules/orders/module.go:14
  chain         *OrdersHandler → *OrderRepository → *sql.DB

  fix: add di.Provide[*sql.DB](c, NewDB) to internal/modules/orders/module.go
```

`requested by` is where the *requesting* provider was registered, which is a real
file the developer can open. The chain is outermost first, and is the reason this
package needs no stack trace.

> **Divergence resolved.** `errors/testdata/di_missing_provider.golden` was written
> as a fixture before this package existed, and named a `warren.Provide` that does
> not exist. It now holds exactly the text above with `warren.Run` prepended, which
> is the op `warren.Run` will add once it exists — so the two files agree, and both
> changed in one diff. The type names are package-qualified because that is what
> `reflect` renders; the earlier `*OrdersHandler` was hand-written and unreachable.

### 6.2 Provided twice

```
di.Validate: *sql.DB is provided twice

  first   internal/platform/module.go:9
  second  internal/modules/orders/module.go:14

  fix: remove one of the two di.Provide[*sql.DB] calls, or give one its own type, as in: type ReplicaDB struct{ *sql.DB }
```

### 6.3 Cycle

```
di.Validate: dependency cycle through *orders.Service

  *orders.Service     orders.NewService (internal/modules/orders/service.go:18)
  *orders.Repository  postgres.NewRepository (internal/adapters/postgres/repository.go:22)
  *orders.Service     orders.NewService (internal/modules/orders/service.go:18)

  fix: depend on an interface declared in domain/ and provide it from infrastructure/, or split *orders.Service
```

The cycle is one line per hop, each naming the constructor and its file, rather
than a single arrow chain. That shape is taken from `dig/cycle_error.go` (§9.1):
a cycle is navigated by opening files, and a chain of type names gives a reader
nothing to open. `errors.Detail` aligns the type column; the constructor's module
path is trimmed to its last segment, because the full import path pushes the file
off the edge of a terminal and adds nothing a reader needs.

### 6.4 Constructor is not a function

```
di.Validate: the constructor for domain.OrderRepository is not a function

  registered  internal/modules/orders/module.go:14
  got         *postgres.OrderRepository

  fix: pass the constructor, not its result: di.Provide[domain.OrderRepository](c, NewOrderRepository)
```

### 6.5 Constructor does not satisfy the type

```
di.Validate: func(*sql.DB) *postgres.OrderRepository does not provide domain.OrderRepository

  registered  internal/modules/orders/module.go:14
  missing     Save(context.Context, *domain.Order) error

  fix: implement Save on *postgres.OrderRepository, or register the concrete type
```

Naming the missing method is a method-set comparison through `reflect`, and it is
the difference between a developer reading the message and a developer reading
the interface.

### 6.6 Wrong return shape

```
di.Validate: the constructor for domain.OrderRepository must return
(domain.OrderRepository) or (domain.OrderRepository, error)

  registered  internal/modules/orders/module.go:14
  signature   func(*sql.DB) (*postgres.OrderRepository, bool)

  fix: return an error as the second value, or nothing as the second value
```

### 6.7 Variadic constructor

```
di.Validate: the constructor for app.Router must not be variadic

  registered  internal/platform/module.go:22
  signature   func(...app.Middleware) *app.Router

  fix: take a slice and resolve it with di.Group, or contribute the members with di.Contribute
```

### 6.8 A type both provided and contributed

```
di.Validate: app.Middleware is both provided and contributed to a group

  provided     internal/platform/module.go:18
  contributed  internal/modules/orders/module.go:9

  fix: a type is resolved by di.Resolve or by di.Group, never both — pick one
```

### 6.9 Constructor returned nil, or a nil value was supplied

```
di.Build: the constructor for domain.OrderRepository returned nil

  constructor  internal/adapters/postgres/repository.go:31

  fix: return an error describing why, so that boot fails with a cause
```

`Supply` of a nil has the same cause and its own wording, reported at
`di.Validate` since no construction is involved:

```
di.Validate: the value supplied for *sql.DB is nil

  registered  internal/platform/module.go:9

  fix: supply a value, or provide a constructor that can report why there is none
```

### 6.10 Constructor failed

```
di.Build: constructing *sql.DB failed: dial tcp 127.0.0.1:5432: connection refused

  constructor  internal/platform/db.go:22

  fix: read the cause above: it is the constructor's own failure, not a wiring mistake
```

`errors.Internal`, wrapping the constructor's own error so `errors.Is` reaches
it. The fix points at the cause rather than guessing, because this is the one
failure where the container knows nothing useful — it is the constructor's message
that matters, and it is already on the first line.

### 6.11 Resolved before build

```
di.Resolve: the container is not built

  fix: call Build before resolving. warren.Run does this for you — a service should not need to
```

### 6.12 A near miss — the type is nearly provided

```
di.Build: no provider for sql.DB

  requested by  internal/modules/orders/module.go:14
  chain         *OrdersHandler → *OrderRepository → sql.DB
  provided      *sql.DB, at internal/platform/module.go:9

  fix: depend on *sql.DB, or register sql.DB with di.Provide[sql.DB](c, NewDB)
```

§6.1 with one extra field, emitted when the missing type has a near miss in the
registry: the pointer when the value was asked for and the reverse, an
implementation when an interface was asked for, or an interface the requested
concrete type satisfies. `dig` has four separate test groups for exactly these
confusions (§9.1), which is evidence they are the common failure and not an
exotic one. Suppressing the hint is never right — a missing provider that is
sitting in the container under one pointer's difference is the single most
frustrating version of this error.

### 6.13 Constructor panicked

```
di.Build: the constructor for *OrderService panicked: runtime error: index out of range [3] with length 0

  constructor  internal/modules/orders/service.go:18

  fix: this is a bug in the constructor, not in the wiring
```

`errors.Internal`. The recovered value is rendered into the message rather than
attached as a field — it is the one thing a reader needs, and a field would repeat
it — and the panic is re-raised nowhere: `warren.Run` prints this and exits, which is
what a raw panic would have achieved without naming the constructor. See §11.5.

### 6.14 Two parameters of the same type

```
di.Validate: the constructor for *app.Router takes two parameters of type *sql.DB

  registered  internal/platform/module.go:22
  signature   func(*sql.DB, *sql.DB) *app.Router

  fix: one parameter is enough — the container resolves one instance per type
```

From `wire`'s *provider has multiple parameters of type %s* (§9.1). The container
would resolve the same instance twice and nothing would break, which is exactly why
it is worth reporting: it is a typo, and a silent one.

### 6.15 The provided type is `error`

```
di.Validate: error cannot be provided

  registered  internal/modules/orders/module.go:14

  fix: a constructor's error is its second return value, not the type it provides
```

From dig's `TestCantProvideErrorLikeType` (§9.1). `func() error` cannot be read:
nothing says whether the return is the value or the failure.

Plus one variant each for the two group mixups, sharing §6.8's wording:
`di.Resolve` on a grouped type points at `di.Group`, and `di.Group` on a provided
type points at `di.Resolve`.

## 7. Interoperability

- **`lifecycle` gets its ordering for free, and must not ask for it.** Singletons
  are constructed in dependency order, so hooks a constructor registers as it is
  built are already in dependency order — which is what makes `lifecycle`'s
  reverse-order stop correct. `di` therefore exposes no ordering API, and
  `lifecycle` must not reconstruct one: the pool that started before the broker
  stops after it because it was *built* first. This is the whole coupling between
  the two packages, and it is one sentence.
- **`warren` owns the boot sequence** and calls `Validate` then `Build`. It is the
  only caller that should print these errors.
- **The graph is data, not a rendering.** `Graph()` returns `reflect.Type` values
  and source sites; `warren graph di` decides what a DOT file looks like.
- **A test builds a real container.** No mock container, no interface over
  `*Container`: registering three constructors is cheaper than faking one, and
  `warren/testing` (v0.4) boots a module in isolation using exactly this.
- **`reflect.Type` appears in `Graph`.** It is standard library, so no invariant
  is touched, and it is the only honest key for a type.
- **Call-site attribution must survive a re-export.** Every message in §6 names a
  file, captured with `runtime.Caller` inside `Provide`. If `warren` re-exports
  registration — `warren.Provide` calling `di.Provide` — then that capture names a
  file inside `warren`, and every error message in this package silently starts
  pointing at the framework instead of at the user's module. `dig` shipped a
  `LocationForPC` option because it hit precisely this. Warren will hit it the
  moment `warren.Provide` exists, so the registration functions take the skip
  depth from an unexported variant (`provideAt(site, …)`) that the re-export
  supplies, and a test asserts a two-hop registration still names the caller's
  file. Cheap now; a rewrite of fifteen golden files later.

## 8. Enforcement

- `depguard` already bans `go.uber.org/dig`, `go.uber.org/fx`, and `samber/do`
  repository-wide, with the reason pointing at `docs/architecture.md §3`. This
  package is why those bans exist; no change is needed.
- `exhaustive` already applies to every enum, so `Kind` is covered the day it
  exists.
- `govet`'s printf list in `.golangci.yml` needs no addition: `di` formats through
  `warren/errors`, whose constructors are already registered there.
- `funlen` (80 lines) and `cyclop` (15) will bind on the validation walk. If a
  pass genuinely needs more, it is two functions — the config is not to be
  relaxed for this package.

## 9. Testing

### 9.1 Prior-art audit — cases harvested from `dig` and `wire`

Audited 2026-07-29. `go.uber.org/dig` v1.19.0 (MIT, not archived, last pushed
2025-05-13) and `google/wire` v0.7.0 (Apache-2.0, **archived** 2025-08-22) were
read for their *behaviour and test names*, which is what nine years of bug
reports look like once they have been turned into assertions.

**No code was copied.** Both licences permit it with attribution, and neither
obligation was incurred: what is taken here is a list of failures to handle, which
is not copyrightable and is worth more than the implementation anyway.

| Case | Source | Warren's behaviour |
|---|---|---|
| Constructor called once despite many dependents | dig · *constructor is called at most once* | §5.4 |
| **One constructor registered under two types** | dig · *multiple-type constructor is called once*, *provide the same implementation with as interface* | **§5.4 — one instance, keyed by constructor identity. This audit is why.** |
| Untyped `nil` passed as the constructor | dig · `TestCantProvideUntypedNil` | §5.3, rejected first |
| Provided type is `error` | dig · `TestCantProvideErrorLikeType` | §5.3, rejected |
| Function with no return values | dig · `TestProvideFuncsWithoutReturnsFails` | §5.3, §6.6 |
| Second return is not `error` | wire · *second return type is %s; must be error* | §5.3, §6.6 |
| Two parameters of the same type | wire · *provider has multiple parameters of type %s* | §5.3, rejected |
| Return type does not satisfy the port; name the missing method | wire · *%s does not implement %s* | §6.5 |
| Same type provided twice | dig · *duplicate provide* | §6.2, both sites named |
| Value asked for, pointer provided (and the reverse) | dig · *requesting a value or pointer when other is present* | **§6.12, new** |
| Interface asked for, implementation provided (and the reverse) | dig · *requesting an interface when an implementation is available* | **§6.12, new** |
| Several unmet dependencies reported together, not just the first | dig · *multiple unmet constructor dependencies* | `Validate` joins, §5.2 |
| Transitive unmet dependency reports the chain | dig · *transitive dependency error* | §6.1 |
| A group member's unmet dependency fails the whole build | dig · *unmet dependency of a group value*, *failure to build a grouped value fails everything* | §6.1, chain names the contributor |
| The same type contributed twice to one group is legal | dig · *duplicate values are supported* | allowed, §4.2 |
| Validating without constructing | dig · `TestDryModeSuccess` (dry mode) | `Validate`, §4.3 — independent confirmation of the design |
| A failed constructor leaves no *invalid* state | dig · `TestFailingFunctionDoesNotCreateInvalidState` | §5.4 — partial, but idempotent and never re-entered |
| Registration site reported correctly through a wrapper | dig · `LocationForPC` option | **§7, new — this would have broken every message once `warren.Provide` existed** |
| Cycle output names the constructor and file per hop | dig · `cycle_error.go` | **§6.3, rewritten** |
| A constructor that panics | dig · `TestRecoverFromPanic`, opt-in there | **open — §11.5** |
| Constructor returns its own teardown, `(T, func(), error)` | wire · *provider for %s returns cleanup…* | **open — §11.6** |
| Variadic constructor | dig · *variadic arguments dependency* — dig **allows** it, passing nothing | rejected, §5.3. Informed divergence: silently passing nothing hides a missing provider |
| Incomplete graph tolerated until use | dig · `TestIncompleteGraphIsOkay` | rejected, §5.4. Informed divergence: eager construction demands completeness |
| An unused provider is an error | wire · *unused provider %q* | constructed anyway; a `warren doctor` finding, §5.4 |

Four rows changed this spec: constructor-identity memoisation, the near-miss
message, the cycle format, and call-site attribution through a re-export. Two
more became open decisions. That is the return on an afternoon of reading, and it
is the reason `AGENT.md` forbids prototyping in favour of research.

### 9.2 The suite

Unit tests only — no Docker, no network, no sleeps.

- **A golden file for each of the fifteen messages in §6**, rendered through
  `errors.Detail`. §6.1 is the v0.1 exit criterion and is asserted byte for byte.
- **Every row of §9.1 is a named subtest**, so the audit is executable rather
  than a paragraph someone read once.
- Shape validation: one subtest per row of §5.3's rejection table.
- Completeness: a missing provider three deep reports the full chain, outermost
  first.
- Cycles: direct (A→A), two-node, five-node, and a diamond that is *not* a cycle
  and must pass.
- Memoisation: a constructor with ten dependents runs once, and a constructor
  registered against two ports runs once and yields the same pointer under both.
  Asserted with a counter, not with timing.
- Group order is registration order, over 1000 shuffled-seed runs — the same
  determinism test `errors` has, for the same reason.
- `Validate` reports all four of four independent mistakes, in registration
  order.
- `Build` is idempotent: a second call constructs nothing and returns the same
  error.
- `context.Context` resolves to `Build`'s argument, and a constructor that
  retains it sees it cancelled — asserted, so the doc comment is not merely
  advice.
- Concurrency: 100 goroutines calling `Resolve` after `Build` under `-race`.
- Benchmarks with `-benchmem`: `Validate` and `Build` at 10 / 100 / 1000
  providers, `Resolve`, `Group`.
- Allocation test against §5.6 under `//go:build !race`, for the reason in
  `errors/SPEC.md §9`.
- `Example` functions for `Provide`, `Supply`, `Contribute`, `Resolve`, `Group`,
  `Build`, and `Graph`.

## 10. Definition of done

- [x] `di/` implements §4, with the divergences in §4.2 and §5.7 corrected here, standard library plus `warren/errors`
- [x] Every exported identifier has a doc comment starting with its name
- [x] Eighteen golden files committed under `di/testdata/`, §6.1 byte-identical
      to this spec
- [x] Every row of §9.1 present as a named subtest
- [x] Benchmarks committed, §5.6 budget met and quoted there and in §11.3
- [x] `.golangci.yml` unchanged — no new exception was needed, and no `//nolint`
      was added outside the two the test flag and the golden regexp already carry
- [x] `Example` functions for all seven entry points
- [x] `docs/roadmap.md` v0.1 item 3 ticked
- [x] This spec corrected wherever the code diverged — §4.2, §5.2, §5.4, §5.6, §5.7, §6.2–§6.15, §11.3
- [x] The §6.1 divergence resolved in one diff with
      `errors/testdata/di_missing_provider.golden`, `errors/detail_test.go`, and
      `errors/SPEC.md` §5.4–§5.5
- [x] `make ci` green on macOS: 0 lint issues, all modules' tests passing, no vulnerabilities

## 11. Decisions taken

Agreed 2026-07-29, all six as recommended. Recorded rather than settled quietly,
because they shape every package that sits on this one. The last two came out of
the §9.1 audit.

Two carry an obligation into the implementation: §11.2 removes `Scope` from
`docs/roadmap.md` v0.1 item 3, and §11.3 stands only until a benchmark says
otherwise.

### 11.1 Reflection over plain constructors, rather than typed closures

**Agreed: reflection.** The alternative that would remove it is a constructor
that resolves its own dependencies:

```go
di.Provide[*OrderService](c, func(s *di.Scope) (*OrderService, error) {
	var repo domain.OrderRepository
	if err := di.Resolve(s, &repo); err != nil { return nil, err }
	return NewOrderService(repo), nil
})
```

This is fully compile-checked and needs no `reflect`. It is **ruled out by the
architecture**, not merely disliked: the dependencies are inside the closure
body, so they cannot be known without running it, and `docs/architecture.md §5`
requires the graph to be validated *without constructing anything*. Goal 2 and
half the error messages in §6 disappear with it.

The second alternative is arity-numbered helpers — `di.Provide2[T, A, B]` — which
declare their dependencies as data *and* keep full type checking. It breaks on
Warren's central pattern: registering a concrete constructor against a port needs
the provided type and the returned type to differ, and Go's partial type argument
lists cannot express that without a second family of `ProvideAs2`, `ProvideAs3`,
… The API doubles, and it caps arity.

So: `ctor any`, validated at the moment of registration, with the file and line
in the message. The cost is that §6.4, §6.5, §6.6, and §6.7 are boot failures
where a different design would have made them compile failures. That is the real
price of this decision and it should be agreed with eyes open.

### 11.2 One lifetime in v0.1 — and `Scope` deferred

**Agreed: `Scope` is cut from v0.1**, and revisited in v0.2 alongside transaction
propagation. `docs/roadmap.md` v0.1 item 3 is corrected in the same change as this
approval. The argument for cutting:

- **Nothing in v0.1 uses it.** Request-scoped attributes are `log.With`'s job.
  Handlers get their dependencies at construction.
- **The one real consumer is v0.2's `UnitOfWork`**, and a transaction most
  naturally propagates on the `context.Context` that already crosses every layer,
  not through a second container. If that is where it lands, `Scope` is API with
  no user — and `docs/roadmap.md` already warns that v0.2's primitives are wrong
  if designed before a real service runs.
- **Per-request construction is what makes it expensive**, and it puts
  `reflect.Value.Call` in the request path, against `docs/architecture.md §7`.
  Singleton-only is what buys the lock-free `Resolve` in §5.5.

If you would rather keep it, the minimal shape is a child scope holding values
placed into it by the transport, with resolution falling through to the root and
no construction of its own — roughly 50 lines, and it changes nothing else in
this spec.

### 11.3 `Resolve` takes an out-parameter

**Agreed, and the benchmark settled it.** `Resolve[T](c, &target)` reads worse than
`Resolve[T](c) (T, error)`, and was accepted only on the claim that returning a
value has to materialise a zero `T` on the error path.

`BenchmarkResolveShape` implements both shapes against the same container
(`resolve_internal_test.go` exists for no other reason). Apple M3 Pro, Go 1.26.3:

| Shape | Value type (`struct{[32]string}`) | Pointer type |
|---|---|---|
| out-parameter | **31.1 ns** | 12.3 ns |
| returned `(T, error)` | 54.5 ns | **11.5 ns** |

So the claim holds where it mattered and not where it did not: the out-parameter is
1.75× faster for a value type, and a fraction slower than noise for a pointer. Since
a service registers pointers and interfaces almost exclusively, this decision buys
very little in practice — it is kept because the value-type case is real and the
cost of the uglier signature is one `&`. Worth reversing if it ever confuses
someone more than it saves.

### 11.4 One provider per type, no names

**Agreed: no names.** Two of a type is two types
(`type ReplicaDB struct{ *sql.DB }`), which the compiler checks and a string
does not. It also makes §6.2 unreachable except by genuine mistake. The cost is a
wrapper type at the one or two places a service really does have two of
something.

### 11.5 A constructor that panics — from §9.1

**Agreed: recover, once, in `Build`, and convert it to an error naming the
constructor** — specified as §6.13. `dig` made this opt-in
(`RecoverFromPanics`); the default there is to let it fly.

Letting it fly is defensible — it is boot, the process is about to die, and the
stack trace is genuinely the most useful artifact. The argument for recovering is
that `warren.Run` is the only caller, and a raw `panic: runtime error: invalid
memory address` with a stack full of `reflect.Value.Call` frames tells a user
nothing about *which of their constructors* did it. Recovering lets the message
say:

```
di.Build: the constructor for *OrderService panicked: runtime error: index out of range [3]

  constructor  internal/modules/orders/service.go:18

  fix: this is a bug in the constructor, not in the wiring
```

`AGENT.md` forbids `panic` in library code; it says nothing about recovering from
a user's. If we recover, the original panic value and stack are attached so
nothing is lost. If you prefer it to fly, that is one fewer path to test and I
will not argue hard.

### 11.6 Constructors that return their own teardown — from §9.1

**Agreed: no, and the reason is recorded.** `wire` accepts `(T, func(), error)`, where the
`func()` is a cleanup. It is elegant: the thing that opened the pool is the thing
that closes it, in one function, with no second registration.

It is declined because **`lifecycle` is the next package and owns this**, and two
mechanisms for shutdown is exactly how ordering bugs get written. A cleanup
returned to `di` has no place in the ordered reverse-stop sequence unless `di`
also models ordering — which §7 deliberately keeps out of this package.

Worth re-reading when `lifecycle/SPEC.md` is written: if registering a stop hook
turns out to be verbose enough that people skip it, this is the ergonomic answer
and the decision should be revisited there rather than here.
