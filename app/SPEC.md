# `github.com/MerseniBilel/warren/app` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01) for `Handler`, `Middleware`, and `Chain` only** — the five built-in middleware are carved out, blocked on Open questions 1–2 (their parameter types cannot exist in core as warren.md describes them) |
| **Source** | [warren.md §3.2](../warren.md) |
| **Module** | core |
| **Mode** | Build — **the central abstraction of the framework** |
| **Wraps** | — |

## Problem

Warren's first claim is "**transport-agnostic use cases.** One
`app.Handler[Req, Res]`; HTTP, gRPC, and message consumers are thin adapters
over it" (AGENT.md § What Warren is). Everything else in the framework is
arranged to make that one sentence true, and this package is where it is
expressed.

The failure mode it exists to prevent: a "transport-agnostic" framework whose
use cases still take an `*http.Request`, or whose cross-cutting concerns —
transactions, retries, tracing, authorization — are written three times, once
per protocol, and drift. §1.4 names the mistake precisely: "Conflating the two
[middleware rings] is the mistake that makes 'transport-agnostic' frameworks
leak."

So `app` defines two things and only two things: a use case
(`Handler[Req, Res]`), and a decorator over a use case
(`Middleware[Req, Res]`). Because the decorator wraps the handler and not the
protocol, "a transaction decorator or retry policy is written once and applies
everywhere" (§1.4). The §3.5 payoff — one `c.register` served over HTTP, gRPC,
and Kafka — is a direct consequence of the handler being this shape and no other.

## Goals

1. Define the single use-case type the whole framework composes against:
   `Handler[Req, Res]`, generic in both directions, `context.Context` first.
2. Define **core-ring** middleware as a function over that handler, and a
   composition helper, so a cross-cutting concern is written once.
3. Ship the five built-in core middleware §3.2 tabulates — `Transactional`,
   `Retrying`, `Traced`, `Metered`, `Authorized` — so that transactions,
   retries, tracing, metrics, and authorization "apply identically to HTTP,
   gRPC, and consumers" (§3.2, AGENT.md § Two middleware rings).
4. Keep the handler free of every transport: invariant 6, and §10's closing
   comment on the use case — "No net/http. No pgx. No kgo. That is the entire
   point."
5. Compose at boot, invoke at request time. The composed chain is what the route
   table's pre-built closure calls; the DI container is not consulted per
   request (§1.4, invariant 7).

## Non-goals

- **Not the edge ring.** CORS, gRPC interceptors, consumer ack semantics,
  decode, encode, and status mapping are transport-shaped, "cannot be shared",
  and belong to the adapter (§1.4, AGENT.md § Two middleware rings). Nothing in
  this package knows a protocol exists.
- **Not a CQRS bus or mediator.** There is no dispatcher, no
  `Send(command any)`, no type-keyed registry, no reflection-based routing.
  Handlers are resolved by the container at boot and called directly.
- **Not validation.** `warren/validate` runs in the adapter after decode, and "a
  bad request never reaches `Handle()`" (§2.7).
- **Not error translation.** Handlers return `warren/errors` codes; each adapter
  owns its column of the §2.6 table.
- **No implementation of the concerns it decorates.** `Transactional` does not
  own a transaction, `Traced` does not own a tracer. The port lives elsewhere
  and the driver lives in an adapter module — invariant 1's standing move:
  "define the port in core, implement it in a submodule."
- **No reflection.** Invariant 7. Composition is generic function application.

## Public API

Taken from warren.md §3.2 verbatim; doc comments added.

```go
// Package app defines Warren's central abstraction: a transport-agnostic use
// case, and the core-ring middleware that decorates it.
//
// A Handler is written once and exposed over HTTP, gRPC, and message consumers
// by adapters that this package knows nothing about.
package app

// Handler is a use case: one request in, one response out, plus an error drawn
// from the warren/errors vocabulary. It is the unit every transport adapter
// wraps and every core middleware decorates.
//
// A Handler imports no transport package. That is the framework's whole point.
type Handler[Req, Res any] interface {
	Handle(ctx context.Context, req Req) (Res, error)
}

// Middleware decorates a Handler with a cross-cutting concern. Because it wraps
// the handler rather than the protocol, one middleware applies identically to
// HTTP, gRPC, and consumers — this is the core ring of the two-ring model in
// warren.md §1.4. Transport-shaped concerns belong to the edge ring, owned by
// the adapter.
type Middleware[Req, Res any] func(Handler[Req, Res]) Handler[Req, Res]

// Chain composes middleware around a handler and returns the composed handler.
// It runs at boot, not per request: the result is stored in the route table as
// a pre-built closure.
func Chain[Req, Res any](h Handler[Req, Res], mw ...Middleware[Req, Res]) Handler[Req, Res]
```

**Built-in core middleware** (§3.2). warren.md gives these as a table of call
forms and effects, not as signatures. The call forms are reproduced exactly; the
parameter types marked **`?`** are *not stated anywhere in warren.md* and are
open questions 1 and 2, not decisions made by this spec.

| Call form (verbatim from §3.2) | Effect (verbatim from §3.2) |
|---|---|
| `app.Transactional(uow)` | Wraps `Handle` in a transaction; commits state + outbox atomically |
| `app.Retrying(policy)` | Retries on `CodeUnavailable` |
| `app.Traced()` | Span per handler, named `<module>.<handler>` |
| `app.Metered()` | Duration histogram, error counter by code |
| `app.Authorized(policy)` | Policy check before invocation |

```go
// Transactional wraps Handle in a unit of work, so the aggregate state written
// by the handler and the outbox rows for the events it raised commit in one
// transaction. See warren.md §3.3 for the six-step Do sequence it delegates to.
func Transactional[Req, Res any](uow persistence.UnitOfWork) Middleware[Req, Res]

// Retrying re-invokes the handler on errors carrying CodeUnavailable, under the
// given policy. No other code is retried.
func Retrying[Req, Res any](policy ?) Middleware[Req, Res]

// Traced opens one span per handler invocation, named "<module>.<handler>".
func Traced[Req, Res any]() Middleware[Req, Res]

// Metered records a duration histogram per handler and an error counter keyed
// by the warren/errors code.
func Metered[Req, Res any]() Middleware[Req, Res]

// Authorized runs a policy check against the identity on the context before
// invoking the handler. Because it is core-ring, authorization applies to gRPC
// and consumers too, not only to HTTP (warren.md §7.2).
func Authorized[Req, Res any](policy ?) Middleware[Req, Res]
```

Usage — a handler, from §10, unchanged:

```go
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
```

## Behaviour

**Where the core ring sits.** §1.4's spine is explicit about the order:

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
```

Edge middleware, decode, and validate all run **before** the core chain is
entered, and encode and status mapping run after it returns. By the time
`Handle` is called the request is a typed `Req` that has already been
validated. §10's request walkthrough repeats the same order for `POST /users`.

**Composition happens at boot.** Boot step 5 is "register — controllers +
consumers build route tables in memory" (§1.3). `Chain` runs there. At step 8
the route table holds `invoke func(ctx context.Context, raw []byte) ([]byte,
error)` closures "with middleware already composed" (§1.4), and "per-request
cost is a map lookup and direct calls. The DI container is not consulted at
request time." A `Chain` call on the request path, or a container lookup inside
a middleware, breaks invariant 7.

**Ordering.** `Chain(h, a, b, c)` composes a defined order. warren.md lists the
chain contents twice — "(tracing, tx, retry, metrics)" in §1.4's diagram and
"(trace, transaction, metrics)" in §10's walkthrough — but never fixes the
argument-order convention (outermost-first or innermost-first), and the two
lists are not in the same order as each other. See open question 3.

**Retrying.** "Retries on `CodeUnavailable`" (§3.2), which is the only code
§2.6's constant block annotates `// retryable`. (The §2.6 *table* also retries
`INTERNAL` in its consumer column — that is the broker chain's retry, not this
middleware's.) `errors.Is(err, errors.CodeUnavailable)` is the
predicate. Every other code is returned to the caller unretried — notably
`CodeInvalid`, which §2.6 marks "never retry".

**Transactional and the outbox.** The middleware does not implement
transactions; it delegates to `persistence.UnitOfWork.Do`, whose six-step
sequence (§3.3) is what makes "commits state + outbox atomically" true. Note
that a handler may *also* call `uow.Do` itself — §10 does exactly that — so the
interaction of an outer `Transactional` with an inner explicit `Do` is a real
case; see open question 4.

**Traced and Metered are core-ring on purpose.** A span named
`<module>.<handler>` is the same span whether the request arrived over HTTP,
gRPC, or Kafka, which is what makes §7.1's claim work: "trace context
propagates into `Message.Headers`, so a span survives the trip through Kafka
into the consumer." The edge ring adds transport-shaped spans around it.

**Authorized is core-ring on purpose.** §7.2: "Guards run as edge middleware;
identity lands on the context; `app.Authorized(policy)` runs as core middleware
so authorization applies to gRPC and consumers too." The edge ring
authenticates and puts identity on the context; the core ring authorizes.

**Ring position.** `app` is CONTRACTS, core module, and may import KERNEL
packages (`errors`, `log`) and — as §3.3 already does for `domain` — sibling
contract packages. `Transactional`'s parameter is `persistence.UnitOfWork`,
which is a contract in the same ring and the same module; that import is
consistent with `persistence` importing `domain`. The parameters of `Retrying`
and `Authorized` are **not** in this position, and that is open question 1.

## Errors

This package defines no error values. It handles two kinds:

- **Errors from the wrapped handler** — always the `warren/errors` vocabulary
  (§2.6). Middleware inspects them by code (`errors.Is(err, code)`) and must
  return them unchanged or wrapped with `%w`, never with `%v` (AGENT.md § Errors).
  A code must survive the whole chain, because the adapter downstream reads it
  to pick a status. A middleware that flattens `CodeConflict` into
  `CodeInternal` silently turns a 409 into a 500 and a DLQ message into a nack.
- **Errors from the middleware's own concern** — a commit failure in
  `Transactional`, a denied policy in `Authorized`. warren.md does not state
  which codes these carry. See open question 5.

`Retrying` returns the last error when retries are exhausted. warren.md does not
say whether the intermediate errors are preserved; also open question 5.

## Testing

**Contract suite.** `app` is where every port's decorator meets every port's
driver, so it owns a **core-middleware contract suite** — a reusable suite that
any core middleware, built-in or user-written, must pass, and that any driver
supplying a middleware's dependency (a `UnitOfWork` implementation, a policy)
must survive. Per AGENT.md it is written and updated **before** the drivers. It
must cover:

- **Transparency.** For a handler that succeeds, the decorated handler returns
  the identical response value. For a handler that fails, the error's
  `warren/errors` code survives the chain unchanged, for every code in §2.6.
- **Ordering.** `Chain(h, a, b, c)` invokes the middleware in the documented
  order on the way in and the reverse on the way out, asserted by an append-only
  trace. Locked by a golden test, because a silent reordering moves the
  transaction boundary relative to the retry boundary.
- **Context discipline.** The context reaching `Handle` is derived from the one
  passed to the outermost middleware; cancellation propagates; no middleware
  stores a context in a struct (AGENT.md § General).
- **`Transactional`.** Handler error rolls back; handler success commits;
  events raised by aggregates saved in the scope reach the outbox in the same
  transaction; the transaction on the context is the one repositories pick up
  (§3.3 steps 1–5). Runs against the `persistence` contract-suite fake, not a
  database — no Docker, no network.
- **`Retrying`.** Retries on `CodeUnavailable` and on nothing else — a table
  over all seven codes in §2.6. Retry count is honoured; exhaustion returns an
  error; context cancellation aborts the retry loop. **No sleeps**: the clock is
  injected in tests (AGENT.md § Testing).
- **`Traced` / `Metered`.** Span name is `<module>.<handler>`; the error counter
  is keyed by code; the histogram records once per invocation including on the
  error path.
- **`Authorized`.** Denied policy short-circuits — the handler is never invoked
  — and the middleware is exercised through a consumer-shaped call path as well
  as an HTTP-shaped one, since applying to consumers is its stated reason to be
  core-ring (§7.2).
- **Transport independence.** The whole suite runs with no transport adapter
  imported at all. A compile-time check that the `app` package's dependency
  graph contains no adapter is part of the suite.

**Benchmarks.** This is the request path, so allocation benchmarks are required
by AGENT.md and invariant 7: `Handle` through a bare handler, and through a
chain of five middleware, reporting `allocs/op`. The target property to hold the
line on is that composition allocates at boot and not per request.

## Definition of done

- [ ] `Handler`, `Middleware`, and `Chain` compile exactly as written above.
- [ ] The five built-in middleware exist with agreed signatures (open questions
      1 and 2 answered first — they cannot be written until then).
- [ ] The core-middleware contract suite exists, passes, and is exported for
      reuse by adapters and by `warren/testing`.
- [ ] Allocation benchmarks committed with recorded numbers; the "container is
      not consulted at request time" claim has a test behind it, per invariant 7.
- [ ] `go list -deps` shows no transport, broker driver, or persistence driver
      in this package's graph.
- [ ] The §10 handler compiles verbatim as a test.
- [ ] Golden test for middleware ordering.
- [ ] Open questions answered and this spec corrected in the same change.

## Open questions

1. **`Retrying(policy)` and `Authorized(policy)` need types from packages `app`
   cannot import.** §7.3 puts the retry/circuit-breaker `Policy` in
   `warren/resilience`, which wraps `gobreaker` and `backoff` and is an ADAPTER
   module. §7.2 puts the authorization policy behind `warren/auth`, which wraps
   `golang-jwt` and `go-oidc` — also an adapter module. Dependencies point
   downward only (§1.1), and invariant 1 forbids the core module any dependency
   beyond stdlib and dig. So `app` cannot name either type as warren.md
   describes it. The standing move is invariant 1's — "define the port in core,
   implement it in a submodule" — which would mean `app` (or another core
   package) declares minimal `RetryPolicy` and `AuthorizationPolicy` interfaces
   that `resilience` and `auth` implement. **warren.md does not say this
   anywhere**, and adding a port is a public-API decision. Where do these two
   interfaces live, and what are their methods?

   Note the contrast: `Transactional(uow)` has no such problem —
   `persistence.UnitOfWork` is a contract in the same ring and the same module.

2. **`Traced()` and `Metered()` take no arguments, yet need a tracer and a
   meter.** OpenTelemetry lives in `warren/observability`, an adapter module
   (§7.1), and the core module is stdlib + dig only. A no-argument constructor
   implies the tracer arrives some other way — resolved from the container at
   boot, read off the context, or a package-level global. Package-level mutable
   state and `init()` are both forbidden (AGENT.md § General), and reading a
   tracer off the context per request needs stating explicitly. Which is it?

3. **Chain order is unspecified, and warren.md lists it two different ways.**
   §1.4 says "(tracing, tx, retry, metrics)"; §10 says "(trace, transaction,
   metrics)". Neither is a decision about `Chain`'s argument-order convention
   (is `mw[0]` outermost or innermost?), and the difference matters: retry
   outside the transaction retries the whole transaction, retry inside it
   retries within one. What is the convention, and what is the default order for
   the five built-ins?

4. **Nested transactions.** §10's handler calls `h.uow.Do(...)` itself while
   §3.2 offers `Transactional(uow)` as middleware around the same handler. If
   both are present, does the inner `Do` join the ambient transaction, open a
   savepoint, or fail? warren.md shows both patterns and reconciles neither.
   This is really a `persistence` question, recorded there too.

5. **Error codes produced by the middleware themselves.** What code does a
   commit failure in `Transactional` carry — `CodeUnavailable` (retryable) or
   `CodeInternal`? What does a denied `Authorized` return —
   `CodePermissionDenied`, presumably, but §2.6 lists the constant without
   saying who raises it. And does `Retrying` return only the last error, or join
   the attempts? Each of these has a visible consequence in the §2.6 table, so
   they are contract, not detail.

6. **Who calls `Chain`, and with what?** Boot step 5 builds the route tables,
   and §3.5's `Controller.Register` shows a handler being registered with no
   middleware argument. So the core chain is assembled by the framework, not by
   the controller — but warren.md never says where the middleware list comes
   from (module-level? application-level? a `warren.NewModule` option?) or how a
   module adds one. `warren.NewModule`'s option list (§2.1) has no middleware
   option.

7. **Does `Middleware` have a non-generic counterpart?** `Handler[Req, Res]` is
   generic, but boot step 5's route table stores
   `invoke func(ctx, raw []byte) ([]byte, error)` (§1.4) — a non-generic
   closure. Something erases the type parameters between the two. warren.md
   never names that bridge, and `transport.Registrar` has the same problem from
   the other side (see `transport/SPEC.md`, open question 1). It is one decision
   and should be answered once for both packages.
