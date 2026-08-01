# `github.com/MerseniBilel/warren/log` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — implemented; the three conditions (mode label, unseeded fallback, seeding surface — Open questions 1, 2, 6) were settled the same day and warren.md §2.5 amended to match |
| **Source** | [warren.md §2.5](../warren.md) |
| **Module** | core |
| **Mode** | Vendor (context carrier) |
| **Uses** | `log/slog`, directly — no port in front |

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
// the transports and consumers it passes through. It returns "" when ctx
// carries none.
func CorrelationID(ctx context.Context) string

// WithLogger returns a copy of ctx carrying l. It is the seeding side of
// FromContext, called by transport adapters at the edge of a request; handler
// code has no reason to call it.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context

// WithCorrelationID returns a copy of ctx carrying id. The correlation ID is
// minted at the edge: an adapter reuses the identifier arriving on the wire
// or mints a fresh one, seeds it here, and attaches it to the logger.
func WithCorrelationID(ctx context.Context, id string) context.Context
```

The seeding pair was added on 2026-08-01 (Open question 6): adapters are
separate modules, so the seam they seed through must be exported, and
warren.md §2.5 now lists all five functions. `With`, `WithLogger`, and
`WithCorrelationID` are functions, not types, so they are the standard Go
idiom and are permitted by [AGENT.md § Naming](../AGENT.md).

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
from a context that was never seeded — is defined (Open question 2, resolved):
`FromContext` returns `slog.Default()`. The result is always usable and never
nil, a unit-tested handler logs without any adapter in scope, and an
application that configured its root logger with `slog.SetDefault` sees that
configuration respected even on a detached context.

The 2026-08-01 adversarial review closed the one hole in that guarantee:
`WithLogger(ctx, nil)` no longer carries the nil — the context behaves as
unseeded, so an adapter seeding an uninitialized logger field cannot make
`FromContext` return nil and panic the first `.Info` downstream.

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

- [x] The five functions in Public API exist with those exact signatures, each
      with a doc comment starting with the identifier's name — `log/log.go`,
      2026-08-01.
- [x] Package compiles under the core module with no import outside the
      standard library (`context`, `log/slog`).
- [x] Round-trip and unseeded-context tests pass, `t.Parallel()`, table-driven,
      asserted against an in-memory `slog.Handler`.
- [x] Allocation benchmarks exist — recorded 2026-08-01, Apple M-series:
      `FromContext` on a seeded context 4.6 ns/op, **0 allocs**;
      `CorrelationID` 4.2 ns/op, **0 allocs**; `With` adding two fields
      175 ns/op, 6 allocs (per-request in adapters — it derives a logger and
      a context, both allocations inherent to the design).
- [x] The Open questions below are answered and folded into this spec in the
      same change that implements them (question 5 deferred to `broker` /
      `observability`, where the header key is owned).
- [x] `make ci` passes — fmt, vet, lint, invariants, `go test -race`, green
      2026-08-01.

## Open questions

1. **RESOLVED (2026-08-01) — the mode is Vendor.** The §2.5 surface returns
   `*slog.Logger`, which only makes sense as Vendor: users hold slog directly,
   and this package is a context carrier in front of it, not a port. warren.md
   §2.5 was amended from "Wrap (thin)" to "Vendor (context carrier)", removing
   the contradiction with the §9 ledger.
2. **RESOLVED (2026-08-01) — `FromContext` returns `slog.Default()` when
   unseeded.** Never nil (the §2.5 usage calls `.Info` unconditionally), and
   an application that configures its root logger via `slog.SetDefault` sees
   that configuration respected on detached contexts too. A no-op logger was
   rejected: silently discarding records is the worst failure mode a logging
   seam can have.
3. **RESOLVED (2026-08-01) — `With` works on an unseeded context** by deriving
   from `FromContext`'s fallback. One rule, no special case. With no args it
   returns ctx unchanged, allocating nothing.
4. **RESOLVED (2026-08-01) — the edge mints; core returns what is carried.**
   A transport adapter reuses the correlation ID arriving on the wire or mints
   a fresh one, then seeds it with `WithCorrelationID`. `CorrelationID`
   returns `""` when none is carried — core never generates identifiers.
   Recorded in warren.md §2.5.
5. **How does a correlation ID reach a consumer?** §3.4 reserves
   `Message.Headers` for trace context; is the correlation ID carried there
   under a fixed key, and is that key owned by this package or by `broker`?
   **Deferred:** the header key belongs to the messaging seam — decide in
   `broker`'s (or `observability`'s) spec before the first consumer chain is
   built. Nothing in this package blocks on it.
6. **RESOLVED (2026-08-01) — the seeding surface is exported.**
   `WithLogger(ctx, l)` and `WithCorrelationID(ctx, id)` were added to the
   public API and to warren.md §2.5: adapters are separate modules and cannot
   reach an unexported seam. This was the one surface addition; it was flagged
   as a manifest amendment in the same change.
7. **RESOLVED (2026-08-01) — root-logger configuration is the application's
   business.** Format, level, and output are set by the user (typically
   `slog.SetDefault`) before `warren.New`; this package adds no configuration
   surface, and the `slog.Default()` fallback is what makes that arrangement
   coherent.
