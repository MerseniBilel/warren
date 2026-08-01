# `github.com/MerseniBilel/warren/log` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — the three-function surface is binding; conditions: the mode label (the API is Vendor-shaped; §2.5 needs amending to match §9), the unseeded-context fallback, and the seeding surface (Open questions 1, 2, 6) settled before adapters consume it |
| **Source** | [warren.md §2.5](../warren.md) |
| **Module** | core |
| **Mode** | Wrap (thin) |
| **Wraps** | `log/slog` |

## Problem

A handler needs to log, and everything worth putting in the record — trace ID,
span ID, correlation ID, module, handler — is request-scoped. The two usual
answers both fail Warren's rules:

- **Inject a logger.** Then every constructor grows a logger parameter, and the
  logger is process-scoped, so the request-scoped fields have to be re-attached
  by hand at every call site.
- **Use a package-level logger.** That is package-level mutable state, which
  [AGENT.md § General](../AGENT.md) forbids, and it still carries nothing
  request-scoped.

warren.md §2.5 takes the third answer: the logger rides on the `context.Context`
that every `app.Handler` already receives, and the transport adapters seed it.
Handlers never construct a logger and never take one as a constructor
dependency.

## Goals

- Carry a `*slog.Logger` on the context and hand it back to any code that holds
  the context.
- Propagate a correlation ID so the records emitted for one request can be tied
  together.
- Stay thin. `log/slog` is the standard library, and this package is a
  context-carrier in front of it, not a logging API of its own.

## Non-goals

- **Not a logging abstraction.** warren.md §9 records `log/slog` as the library;
  Warren does not define its own `Logger` interface, levels, or handlers.
- **Not a logging library.** [CLAUDE.md](../CLAUDE.md) names "a logging library"
  among the things not to add to the core.
- **Not the thing that populates the fields.** warren.md §2.5 says `trace_id`,
  `span_id`, `correlation_id`, `module`, and `handler` "are already attached" —
  the transport adapters and `warren/observability` put them there. This package
  transports the logger; it does not decide what is on it.
- **No knowledge of HTTP, gRPC, or brokers.** This is kernel (warren.md §1.1);
  the kernel has no knowledge that HTTP, SQL, or Kafka exist. Which header a
  correlation ID arrives in is an adapter's business.

## Public API

The surface is exactly warren.md §2.5, with doc comments added.

```go
// Package log carries a *slog.Logger on the context and propagates the
// correlation ID that ties one request's records together.
//
// Transport adapters seed the context. Handlers read the logger back out with
// FromContext; they never construct a logger and never take one as a
// constructor dependency.
package log

import (
	"context"
	"log/slog"
)

// FromContext returns the logger carried by ctx. The trace ID, span ID,
// correlation ID, module, and handler are already attached to it by the
// transport adapter that seeded the context.
func FromContext(ctx context.Context) *slog.Logger

// With returns a copy of ctx carrying a logger with args appended to the
// logger already on ctx. args are slog key/value pairs. Use it to add
// request-scoped fields that every subsequent record in this call tree should
// carry.
func With(ctx context.Context, args ...any) context.Context

// CorrelationID returns the correlation ID carried by ctx — the identifier
// that ties together every record emitted while handling one request, across
// the transports and consumers it passes through.
func CorrelationID(ctx context.Context) string
```

`With` is a function, not a type, so it is the standard Go options idiom and is
permitted by [AGENT.md § Naming](../AGENT.md).

## Behaviour

- **The context is the carrier.** `context.Context` is the first parameter of
  every function here, and no logger is stored in a struct (AGENT.md
  § General).
- **Adapters seed, handlers read.** warren.md §2.5: "The transport adapters seed
  the context." A handler's only interaction is `log.FromContext(ctx)`.
- **`With` returns a context, not a logger.** The signature in warren.md §2.5 is
  `With(ctx, args...) context.Context`, so added fields propagate to everything
  downstream that receives the derived context, not just to one call.
- **Correlation ID crosses the messaging boundary.** warren.md §3.4 puts trace
  context in `Message.Headers`, and §7.1 says a span survives the trip through
  Kafka into the consumer. The correlation ID is the same kind of value; how it
  is carried on a `Message` is not fixed by warren.md — see Open questions.

## Errors

**This package returns no errors.** No function in the §2.5 surface returns an
`error`, and warren.md fixes no message text for it.

The one behaviour that would normally produce an error — asking for a logger
from a context that was never seeded — has no defined outcome in warren.md.
`panic` in library code is forbidden by AGENT.md § General, so the choice is
between a usable fallback logger and a no-op; warren.md does not say which. See
Open questions.

## Testing

- **Golden-file tests for error messages** — none apply: this package produces
  no error messages. If the fallback behaviour of `FromContext` ends up emitting
  a diagnostic, that text gets a golden file like any other.
- **Allocation benchmark on the request path.** `FromContext` and
  `CorrelationID` run per request, and `With` runs per request in every adapter
  that seeds fields. AGENT.md § Testing requires a number behind the request
  path, and invariant 7 forbids reflection there. Benchmark: allocations for
  `FromContext` on a seeded context, and for `With` adding two fields.
- **No Docker, no network, no sleeps** in unit tests (AGENT.md § Testing). This
  package needs none of the three: assertions run against an in-memory
  `slog.Handler` that records the attributes it was given.
- `t.Parallel()` and table-driven subtests named for behaviour.
- Round-trip tests: `With` then `FromContext` observes the added attributes;
  a context derived twice accumulates attributes in order; the parent context is
  unmodified.

## Definition of done

- [ ] The three functions in Public API exist with those exact signatures, each
      with a doc comment starting with the identifier's name.
- [ ] Package compiles under the core module with no import outside the standard
      library.
- [ ] Round-trip and unseeded-context tests pass, `t.Parallel()`, table-driven.
- [ ] Allocation benchmarks exist for `FromContext`, `With`, and
      `CorrelationID`.
- [ ] The Open questions below are answered by the human and folded into this
      spec, in the same change that implements them.
- [ ] `make ci` passes (once the Makefile exists — see AGENT.md § Repository
      state).

## Open questions

1. **warren.md contradicts itself on this package's mode.** §2.5 says
   **Wrap (thin)**; the §9 ledger row for Logging says **Vendor**. The two modes
   mean different things (AGENT.md § Modes): Wrap means users must not import
   `log/slog` directly and a port sits in front; Vendor means it is imported and
   used directly. The §2.5 surface returns `*slog.Logger`, which is a Vendor
   shape — a Wrap would return a Warren port. Which is it? The API as written
   only makes sense as Vendor.
2. **What does `FromContext` return when the context was never seeded?** A
   usable default logger writing to stderr, a no-op logger that discards, or
   `slog.Default()`? warren.md is silent, and `nil` is not viable because the
   §2.5 usage calls `.Info` on the result unconditionally.
3. **Does `With` also work when no logger is on the context yet** — i.e. does it
   create one — or does it require a seeded context?
4. **Who mints the correlation ID, and what does `CorrelationID` return when
   there is none?** Empty string, or is one always generated at the edge?
   warren.md never says which component generates it.
5. **How does a correlation ID reach a consumer?** §3.4 reserves
   `Message.Headers` for trace context; is the correlation ID carried there
   under a fixed key, and is that key owned by this package or by `broker`?
6. **Is there a way to seed the context at all in the public surface?** The
   three functions read and derive; adapters have to put the first logger on the
   context somehow. Is that a fourth exported function, or an internal one the
   adapters reach through another route? Adapters are separate modules, so they
   cannot use an unexported one.
7. **Does anything need to configure the root logger** — format, level, output —
   or is that entirely the application's business before `warren.New`? warren.md
   §2.5 shows no configuration surface.
