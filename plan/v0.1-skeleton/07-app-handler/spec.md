# Spec: Handler and middleware

| | |
|---|---|
| **Module** | `warren/app` (core — standard library only) |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [01-errors](../01-errors/spec.md), [02-log](../02-log/spec.md) |
| **Blocks** | [08-transport-http](../08-transport-http/spec.md), and every transport thereafter |
| **PRD** | §3.3, §4.3, §6.1, §14.2 |
| **ADRs** | None — but this type *is* [docs/architecture.md §1](../../../docs/architecture.md), "the one idea" |
| **Date** | 2026-07-28 |

---

## 1. Problem

PRD §3.3 names transport-agnostic use cases as differentiator 1 and says
plainly: *"This is the feature nobody else has cleanly."* A use case written for
HTTP today is rewritten to be exposed over gRPC tomorrow, and copy-pasted again
to run as a Kafka consumer.

The fix is one interface, and it is four lines. The work is not in defining it —
it is in making every transport genuinely able to adapt to it, and in resisting
every subsequent pressure to leak a transport concern into it.

## 2. Goals

1. **One `Handler[Req, Res]`** that HTTP, gRPC, CLI, and consumers all adapt to.
2. **Middleware that is written once and applies to every transport** — logging,
   validation, transactions, retries, metrics (PRD §4.2).
3. **The type is small enough that a user could have written it.** If Warren's
   central abstraction needs a page of explanation, the abstraction is wrong.
4. **No transport concept reachable from a handler.** Not status codes, not
   headers, not acks, not partitions.

## 3. Non-goals

- **No command/query bus at v0.1.** PRD §6.1 lists buses under `warren/app`;
  they are not needed to build a service, and a bus designed before its
  decorators exist is a guess. v0.2.
- **No request-scoped DI.** [03-di](../03-di/spec.md) §11.3.
- **No validation implementation.** `warren/validate` wraps
  `go-playground/validator` in a submodule; the transport calls it before the
  handler. Core cannot depend on it (invariant 1).
- **No `Handler` registry.** Handlers are provided through the container.

## 4. Public API

```go
package app

// Handler is the unit every transport adapts to. This is the framework's
// central type: HTTP, gRPC, CLI, and message consumers are thin adapters over
// it, and it imports none of them.
type Handler[Req any, Res any] interface {
    Handle(ctx context.Context, req Req) (Res, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc[Req any, Res any] func(context.Context, Req) (Res, error)

func (f HandlerFunc[Req, Res]) Handle(ctx context.Context, req Req) (Res, error)

// Middleware wraps a Handler in a cross-cutting concern. The same middleware
// value runs under every transport, which is the point.
type Middleware[Req any, Res any] func(Handler[Req, Res]) Handler[Req, Res]

// Chain applies middleware so that the first argument is the outermost — the
// order they are read in.
func Chain[Req, Res any](h Handler[Req, Res], mw ...Middleware[Req, Res]) Handler[Req, Res]

// Built-in middleware. Each is a separate function so a user takes only what
// they want; none is applied unless asked for.

// Logging logs the handler name, duration, and outcome at Debug, and failures
// at Error with the error's semantic code and fields.
func Logging[Req, Res any](name string) Middleware[Req, Res]

// Recovery converts a panic into a CodeInternal error carrying the stack.
// A panic in one request must not take down a process serving others.
func Recovery[Req, Res any]() Middleware[Req, Res]

// Timeout bounds a handler's execution.
func Timeout[Req, Res any](d time.Duration) Middleware[Req, Res]
```

That is the entire package at v0.1. The restraint is deliberate: this type is
load-bearing for every later transport, and a small surface is what lets the
gRPC and consumer adapters in v0.3 arrive without changing it.

**Nothing here is `net/http`-shaped.** No `Request`, no `Response`, no headers,
no status. That absence is the feature.

## 5. Behaviour

- **`ctx` is always first and never stored.** Everything request-scoped —
  logger, correlation ID, transaction, deadline — travels in it.
- **A handler returns a value and an error, not a status.** Mapping to a
  transport's vocabulary is the adapter's job, driven by
  [`errors.Code`](../01-errors/spec.md).
- **Middleware order is outermost-first**, matching reading order. `Chain(h, A,
  B)` runs A, then B, then h. The alternative is defensible and constantly
  confuses people.
- **`Recovery` is not applied by default.** PRD §4.1 principle 1: no hidden
  control flow. The generated code applies it visibly, where the user can see
  and remove it.
- **Handlers must be safe for concurrent use.** One instance serves many
  requests; per-request state lives in the arguments or the context. Documented
  on the interface, because a user putting a field on their handler struct is
  the most likely subtle bug in a Warren service.
- **Zero framework allocation per invocation.** A middleware chain is composed
  once at wiring time, not rebuilt per request.

## 6. Errors

The package defines no errors of its own beyond `Recovery`'s conversion:

| Condition | Code | Message |
|---|---|---|
| Handler panicked | `CodeInternal` | The handler name, the panic value, and the stack — captured here because this is the one place a stack genuinely helps |
| `Timeout` exceeded | `CodeDeadlineExceeded` | The handler name and the configured timeout |

Everything else is the handler's own error, passed through unchanged. A
middleware that rewrites an error it did not create is a middleware that makes
failures unexplainable.

## 7. Configuration

None. Middleware is applied explicitly at wiring.

## 8. Testing

Unit only, and this package's tests are mostly about what *cannot* be expressed.

- **PRD §14.2's handler compiles and runs unchanged** against this API, with
  fakes for its repository and unit of work. The PRD's own example is the
  acceptance test.
- **A handler under three transports**: a table test invoking the same handler
  value through the HTTP adapter, a direct call, and a stub consumer adapter,
  asserting identical results. This is differentiator 1, so it is tested as a
  behaviour rather than asserted in a README.
- **Middleware order**: `Chain(h, A, B)` records `A→B→h`.
- **`Recovery`** turns a panic into `CodeInternal` and the process survives —
  including a panic in a middleware, not only in the handler.
- **`Timeout`** cancels the context the handler observes, verified without
  `time.Sleep`.
- **Concurrency**: 100 parallel invocations of a chained handler under `-race`.
- **Import check**: a test asserting the package's import list is exactly the
  standard library plus `warren/errors` and `warren/log`. Cheap, and it catches
  the one regression that would matter most.

## 9. Invariants touched

- **Invariant 1** — core, standard library only.
- **Invariant 4 (handlers import no transport)** — this package is the invariant.
  Everything in §3 and §4 exists to hold it.

## 10. Definition of done

- [ ] Public API matches §4
- [ ] PRD §14.2's example compiles and passes as a test
- [ ] The three-transport equivalence test passes
- [ ] Unit tests per §8, `-race -shuffle=on`
- [ ] `make ci` green
- [ ] `docs/` concept page: handlers, and the one-use-case-three-exposures example
- [ ] Runnable example in `examples/handler/`
- [ ] Changelog fragment

## 11. Open questions

1. **Does `Middleware[Req, Res]` being generic hurt?** Go generics do not allow a
   single value to wrap handlers of differing types, so cross-cutting concerns
   must be instantiated per handler type — meaning the generated `module.go`
   repeats a middleware line per handler. The alternatives (an `any`-typed
   middleware with runtime assertions, or transport-level middleware only) trade
   type safety or the "written once, applies everywhere" claim. **This is the
   most consequential open question in v0.1 after §13.1**, and it should be
   answered by writing the generated output three ways and reading them.
2. **Should `Handler` have a `Name()` for observability?** It would make logging
   and tracing self-describing, and it would put a framework concern on the
   user's type. Leaning: name is supplied at registration, not by the handler.
3. **Where do the transaction and outbox decorators live?** PRD §6.1 lists them
   under `warren/app`, but they need `UnitOfWork`, which is v0.2 persistence. The
   decorator belongs wherever it can be written without core taking a dependency
   — probably `warren/persistence`.
