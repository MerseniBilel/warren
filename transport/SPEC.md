# `github.com/MerseniBilel/warren/transport` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §3.5](../warren.md), §1.4, §5.1 |
| **Module** | core |
| **Mode** | Build (ports only) |
| **Wraps** | — |

## Problem

§3.5 states the payoff in four lines of user code:

```go
func (c *UserController) Register(r transport.Registrar) {
	r.HTTP().Post("/users", c.register)
	r.GRPC().Method("user.v1.UserService/Register", c.register)
	r.Events().On("billing.customer.created", c.register)

	r.HTTP().Get("/users/{id}", c.get)
}
```

"`c.register` is an `app.Handler[RegisterUser, UserDTO]`. It imports no
transport package. Adapters own decode, encode, status mapping, and ack
semantics." One use case, three protocols, and the file that exposes it names no
protocol library.

`warren/transport` is the port that makes those four lines possible. It is
deliberately tiny: two interfaces, one of which is a factory for three
protocol-shaped registrars. It exists so that a controller — the only place in a
user's application where a route path and a handler meet — depends on a
core-module interface rather than on `chi`, `grpc`, or `franz-go`. Invariant 6:
"If a change makes a use case import `net/http`, `grpc`, or a broker client, the
change is wrong. This is the framework's whole point."

The second thing it fixes is *when* registration happens. Boot step 5 is
"register — controllers + consumers build route tables in memory" (§1.3), and by
step 8 "the route table holds pre-built closures with middleware already
composed" (§1.4). `Registrar` is a **boot-time** interface. Nothing in this
package is touched on the request path.

## Goals

1. Give a controller one dependency — `transport.Registrar` — through which it
   can expose a handler over HTTP, gRPC, and events.
2. Keep every protocol type out of the port: no `*chi.Mux`, no `*grpc.Server`,
   no `http.HandlerFunc` (invariant 3).
3. Make registration a boot-time act that produces a route table, so the request
   path is a map lookup and direct calls with the container never consulted
   (§1.4, invariant 7).
4. Draw the ring boundary explicitly: this package defines *where a handler is
   exposed*; the **edge** middleware that decodes, validates, encodes, maps
   status codes, and acks is owned by the adapter.

## Non-goals

- **Not a router, a server, or a serving loop.** `transport/http` (§4.1) and
  `transport/grpc` (§4.2) are separate modules; each provides its own
  `Server(opts...)` module and its own lifecycle hooks.
- **Not the edge ring.** §1.4: "*Edge* middleware is transport-shaped — CORS,
  gRPC interceptors, consumer ack semantics — and cannot be shared." It belongs
  to the adapter. §3.5 assigns the whole edge job to adapters: "Adapters own
  decode, encode, status mapping, and ack semantics." The **core** ring —
  transactions, retry, tracing, metrics, authorization — is `app` (§3.2), wraps
  `Handler[Req, Res]`, and is invisible from here.
- **No decode, encode, or serialisation.** Adapter concern (§3.5). Validation
  runs there too: "Transport adapters call it automatically after decode;
  handlers never invoke it" (§2.7).
- **No error-to-status mapping.** §2.6's table has one column per adapter, and
  "each adapter owns its column" (AGENT.md).
- **No OpenAPI.** §4.3 reads route registrations from the outside and emits
  OpenAPI 3.1; it is its own module and this package does not know it exists.
- **No message broker semantics.** `EventRegistrar` names a topic and takes
  `broker` options (§5.1); ack, retry, and DLQ behaviour is `broker`'s (§3.4).
- **Zero implementations** — invariant 5.

## Public API

Taken from warren.md §3.5 verbatim; doc comments added.

```go
// Package transport defines the port through which a controller exposes a use
// case over one or more protocols.
//
// It is a boot-time interface. Registration happens at boot step 5 and produces
// a route table of pre-built closures; nothing here is on the request path.
package transport

// Registrar hands a controller the protocol-specific registrars it needs. A
// controller depends on this interface and on no protocol library.
type Registrar interface {
	// HTTP returns the HTTP route registrar.
	HTTP() HTTPRegistrar
	// GRPC returns the gRPC method registrar.
	GRPC() GRPCRegistrar
	// Events returns the message-subscription registrar.
	Events() EventRegistrar
}

// Controller exposes handlers over transports. Register is called once, at boot
// step 5, while the route table is built — never per request.
type Controller interface {
	Register(Registrar)
}
```

**`HTTPRegistrar`, `GRPCRegistrar`, and `EventRegistrar` are named by §3.5 but
never defined anywhere in warren.md.** What follows is only what §3.5, §5.1, and
§7.2 *demonstrate* they must support. Parameter and option types marked **`?`**
are unspecified — see open questions 1 and 2. These are not decisions this spec
is making.

```go
// HTTPRegistrar registers a handler at an HTTP method and path. Path syntax is
// the adapter's ("/users/{id}" in warren.md §3.5).
type HTTPRegistrar interface {
	// Post registers a handler for POST at path.
	Post(path string, h ?, opts ...?)
	// Get registers a handler for GET at path.
	Get(path string, h ?, opts ...?)
}

// GRPCRegistrar registers a handler against a fully qualified gRPC method name.
type GRPCRegistrar interface {
	// Method registers a handler for "package.Service/Method".
	Method(name string, h ?, opts ...?)
}

// EventRegistrar subscribes a handler to a topic, with the per-subscription
// options defined by warren/broker.
type EventRegistrar interface {
	// On subscribes a handler to a topic.
	On(topic string, h ?, opts ...?)
}
```

Every call site warren.md contains, verbatim — these are the acceptance criteria
for whatever shape is chosen:

```go
// §3.5 and §10
r.HTTP().Post("/users", c.register)
r.GRPC().Method("user.v1.UserService/Register", c.register)
r.Events().On("billing.customer.created", c.register)
r.HTTP().Get("/users/{id}", c.get)

// §7.2 — a per-route edge option
r.HTTP().Get("/users/{id}", c.get, auth.RequireScope("users:read"))

// §5.1 — a consumer, with broker options
r.Events().On("billing.subscription.created", c.activate,
	broker.WithRetry(broker.ExponentialBackoff(5)),
	broker.WithDeadLetter("billing.subscription.created.dlq"),
	broker.WithConcurrency(10),
)
```

Four things those call sites *constrain*, and they are the closest thing to a
contract warren.md gives:

1. **The call sites discard any return value.** Whether `Post`, `Get`, `Method`
   and `On` return an error or a route-builder at all is open question 4 — a
   discarded return is not the same as no return.
2. **`Get` and `On` are demonstrably variadic in options** (§7.2 and §5.1), and
   those options come from *other* packages — `auth` for HTTP, `broker` for
   events. `Post` and `Method` are never shown with options.
3. **The same handler value goes to all three registrars** (§3.5 registers
   `c.register` three ways), so whatever the handler parameter is, it is one
   type across HTTP, gRPC, and events.
4. **`Register` returns nothing**, so a registration conflict is not reported
   through a return value.

## Behaviour

**Registration is boot step 5, and only boot step 5.** The boot sequence
(§1.3, AGENT.md § Two orderings) puts `register — controllers + consumers build
route tables in memory` after instantiation and before `OnStart`. So:

- `Register` is called exactly once per controller, on an already-instantiated
  controller whose dependencies the container resolved at step 4.
- Reflection is permitted here. "Reflection runs during steps 1–5 only"
  (§1.4, invariant 7). It is forbidden after.
- What registration *produces* is the route table, whose entries §1.4 gives
  concretely:

  ```go
  type route struct {
      invoke func(ctx context.Context, raw []byte) ([]byte, error)
  }
  ```

  "Per-request cost is a map lookup and direct calls. **The DI container is not
  consulted at request time.**" A `Registrar` implementation that resolves from
  the container inside `invoke`, or that defers composition to first request,
  breaks invariant 7.

**Where the two rings meet.** §1.4's spine, with this package's boundary marked:

```
HTTP request ─┐
gRPC call ────┼─▶ edge middleware ─▶ decode ─▶ validate ─┐   ← adapter (edge ring)
Kafka msg ────┘   (transport-specific)                   │
                                                         ▼
                                              core middleware chain      ← app (core ring)
                                          (tracing, tx, retry, metrics)
                                                         │
                                                         ▼
                                              Handler[Req, Res].Handle   ← the use case
```

`transport` defines only the act of associating a route with a handler. Both
rings are built *around* that association by the adapter and by `app`
respectively — the edge ring outside, the core ring inside. Conflating them "is
the mistake that makes 'transport-agnostic' frameworks leak" (§1.4), so a
`Registrar` method that took an `func(http.Handler) http.Handler` would be a
category error: that is `transport/http`'s `Middleware(...)` option (§4.1),
which is correctly on the adapter's module, not on this port.

**Three exposures, one handler, one error table.** Because the same handler is
registered three ways, the *only* thing that differs per protocol is the
translation, and §2.6 is the translation:

| Code | HTTP | gRPC | Consumer |
|---|---|---|---|
| `INVALID` | 400 | `InvalidArgument` | → DLQ (never retry) |
| `NOT_FOUND` | 404 | `NotFound` | ack + log |
| `CONFLICT` | 409 | `AlreadyExists` | ack (idempotent replay) |
| `UNAVAILABLE` | 503 | `Unavailable` | nack + backoff retry |
| `INTERNAL` | 500 | `Internal` | nack + retry, then DLQ |

Each adapter owns its column. This package owns none of them; it is listed here
because it is the reason the port can stay this small.

**Not every controller registers everything.** §10 registers only HTTP and gRPC;
§5.1's consumer registers only events. `Registrar` exposes all three
unconditionally, which raises what happens when a controller calls `r.GRPC()` in
an application with no gRPC adapter installed — open question 3.

**Ring position.** CONTRACTS, core module. Imports KERNEL packages and — as the
`On` call sites require — sibling contract packages `app` and `broker`. It
imports no adapter and no protocol library. Note that §7.2's
`auth.RequireScope(...)` is passed *through* an `HTTPRegistrar` method while
`warren/auth` is an adapter module, so the option parameter's type cannot be an
`auth` type; that is part of open question 2.

## Errors

The port's methods return nothing, so no error crosses it at registration time.
Registration problems must therefore surface some other way, and the boot rule
is absolute: "**every error the framework can detect surfaces at boot, never on
request 1**" (§1.3). The errors a registrar can detect — a duplicate route, a
malformed path pattern, an event registration with no broker configured, a gRPC
method registered with no gRPC server — must fail the boot, with a message that
"names what was missing, who requested it, where it was declared, and a
copy-pasteable fix" (AGENT.md § Errors, invariant 2's standard).

warren.md specifies no such message and no mechanism for reporting one from a
void method. That is open question 4. Every message eventually specified gets a
golden-file test (AGENT.md § Testing).

At **request** time the errors that matter are the handler's, and the table
above is their contract. The adapter owns the translation; a handler that maps a
code to a status has broken ring 2.

## Testing

**Contract suite.** AGENT.md requires one per port, updated before the drivers —
here that means before `transport/http` (§4.1), `transport/grpc` (§4.2), the
Gin/Echo/Fiber router adapters (§4.1), and every broker's event registrar. One
exported suite that any `Registrar` implementation runs. It must cover:

- **Every call site in warren.md compiles and registers.** The six forms
  quoted above are the acceptance criteria and go in the suite verbatim; if the
  chosen handler-parameter shape cannot express them, it is the wrong shape.
- **Registration is boot-time and produces a table.** After `Register` returns,
  the route table contains one entry per call, keyed as the adapter documents.
- **The container is not consulted at request time.** Invoke a registered route
  against a container instrumented to fail on any resolution, and assert it is
  never touched. Invariant 7 is a claim, and AGENT.md says claims get tests.
- **No reflection after step 5.** The composed `invoke` closure performs no
  reflective call — asserted structurally where possible and by the allocation
  benchmark otherwise.
- **One handler, three exposures.** The same handler value registered through
  `HTTP`, `GRPC`, and `Events` produces three routes that all reach the same
  handler and return the same response for the same request. This is §3.5's
  headline claim and the suite's centre of gravity.
- **The §2.6 table, per adapter.** For each code, the HTTP adapter yields the
  tabulated status, the gRPC adapter the tabulated code, and the event adapter
  the tabulated ack/nack/DLQ outcome. The table is shared, so the assertions
  are shared and only the column changes.
- **Edge/core separation.** A core middleware from `app` observes the invocation
  identically over all three transports; an edge middleware registered on one
  adapter is invisible to the others. This is the property §1.4 says frameworks
  leak on.
- **Duplicate registration** fails at boot rather than at first request, per
  §1.3 — pinned by a golden-file test of the message once open question 4 is
  answered.
- **No protocol type in any exported signature** — invariant 3, checked in the
  suite.

**Constraints.** Unit tests only: no Docker, no network, no sleeps (AGENT.md
§ Testing). A registrar is a boot-time object, so nothing here needs a server;
the adapters' own suites run behind `//go:build integration` where they need
real ports.

**Benchmarks.** This package sits at the entrance to the request path, so
allocation benchmarks are required by AGENT.md and invariant 7: a registered
route invoked end to end through its `invoke` closure, reporting `allocs/op`,
and a comparison against the same handler called directly, which quantifies the
framework's per-request overhead. That number is the evidence behind "per-request
cost is a map lookup and direct calls".

## Definition of done

- [ ] `Registrar` and `Controller` compile as written.
- [ ] `HTTPRegistrar`, `GRPCRegistrar`, and `EventRegistrar` are defined — they
      cannot be written until open questions 1 and 2 are answered, and answering
      them amends `warren.md` §3.5 in the same change.
- [ ] The contract suite exists and passes, with all six warren.md call sites
      compiling verbatim inside it.
- [ ] Boot-time registration failures produce messages meeting invariant 2's
      standard, each with a golden-file test.
- [ ] `go list -deps` shows no protocol library and no adapter in this package's
      graph.
- [ ] Allocation benchmark committed with its number, plus the
      container-not-consulted test.
- [ ] Open questions answered and this spec corrected in the same change.

## Open questions

1. **The three registrar interfaces are undefined, and the obvious definition
   does not compile.** `HTTPRegistrar`, `GRPCRegistrar`, and `EventRegistrar`
   are named in §3.5's `Registrar` and never declared. The blocker is not
   authorship, it is Go: the handler being registered is
   `app.Handler[Req, Res]` (§3.5 says so explicitly), **Go interfaces cannot
   have generic methods**, and `Registrar` is a non-generic interface. So
   `Post(path string, h app.Handler[Req, Res])` cannot be written as an
   interface method at all.

   warren.md gives two hints and resolves nothing. §1.4's route table stores
   `invoke func(ctx context.Context, raw []byte) ([]byte, error)` — type-erased
   bytes in, bytes out. §2.1's `warren.Controllers(...any)` takes `any`. So some
   bridge erases `Handler[Req, Res]` to a non-generic form, and it is presumably
   a generic *free function* (interfaces cannot do it, free functions can).
   warren.md never names it, never shows it at a call site, and §3.5's example
   passes `c.register` directly with no wrapping.

   This is the single largest gap across the five contract packages. It is the
   same question as `app/SPEC.md` open question 7 and `broker/SPEC.md` open
   question 6, and it needs one answer for all three. It is a public-API decision
   and belongs to the human.

2. **The option parameters are undefined, and they come from packages this one
   cannot import.** §7.2 passes `auth.RequireScope("users:read")` to
   `r.HTTP().Get(...)`; §5.1 passes `broker.WithRetry(...)`,
   `broker.WithDeadLetter(...)`, `broker.WithConcurrency(...)` to
   `r.Events().On(...)`. `warren/auth` is an ADAPTER module (§1.6, wrapping
   `golang-jwt` and `go-oidc`) and the core module is stdlib + dig only, so an
   `auth` type cannot appear in a core signature. `broker` is a sibling
   contract, so its options are importable. What are the two option types, where
   do they live, and does `HTTPRegistrar` take an opaque `any`?

3. **What happens when a registrar's adapter is absent?** `Registrar` offers all
   three accessors unconditionally, but §10's application installs
   `http.Server`, `grpc.Server`, and `kafka.Broker` while §5.4's modular
   monolith has no HTTP at all. Does `r.GRPC()` in a service with no gRPC module
   return nil, a no-op registrar, or fail the boot? Per §1.3 a detectable error
   must surface at boot; nothing states which of these it is.

4. **Registration reports nothing.** `Post`, `Get`, `Method`, `On`, and
   `Register` all return void. A duplicate route, a bad path pattern, or an event
   registration with no broker are all detectable at step 5 and must fail the
   boot — but a void method cannot report them. Panic (forbidden in library code
   by AGENT.md), an error accumulated on the registrar and checked by the
   bootstrapper, or a signature change? The error-message text is also
   unwritten, and invariant 2 sets a high bar for it.

5. **`Controller` and consumers are the same interface but different module
   options.** §5.1's `BillingConsumer` has method `Register(r
   transport.Registrar)` — it satisfies `transport.Controller` — yet §2.1
   registers it with `warren.Consumers(NewBillingConsumer)` and the HTTP
   controller with `warren.Controllers(NewUserController)`. If both satisfy the
   same interface, what does the distinction change: boot ordering, which
   registrars are available, lifecycle placement? And is a type named `Controller`
   the right name for a Kafka consumer?

6. **HTTP path syntax is unspecified at the port.** §3.5 and §7.2 use
   `"/users/{id}"`, which is chi's and `net/http` 1.22's syntax. Is that syntax
   part of the port's contract — binding the Gin, Echo, and Fiber adapters
   (§4.1) to translate it — or is it adapter-defined and therefore not portable?
   §4.1 already concedes "Fiber gets a lossier adapter and a documented caveat".

7. **Only `Post` and `Get` are ever demonstrated.** PUT, PATCH, DELETE, HEAD,
   route groups, path prefixes, and middleware scoped to a group all appear
   nowhere in warren.md. Presumably the full method set exists; it is not
   written down, and this spec will not invent it.

8. **Nothing states how a `Controller` reaches its `Register` call.**
   `warren.Controllers(...any)` (§2.1) takes constructors, not `Controller`
   values, and takes `any` rather than the interface. Does the bootstrapper
   type-assert to `transport.Controller` after instantiation at step 4? If so,
   a controller with a typo'd method signature fails silently — exactly the
   class of error §1.3 says must surface at boot.
