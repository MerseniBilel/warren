# `github.com/MerseniBilel/warren/observability` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — `Module` and the three options are binding; condition: the `app.Traced()`/`app.Metered()` mechanism (Open question 1, the same question as app's carve-out) settled before handler instrumentation is built |
| **Source** | [warren.md §7.1](../warren.md) |
| **Module** | own module (`github.com/MerseniBilel/warren/observability`) |
| **Mode** | Wrap (wiring, not abstraction) |
| **Wraps** | OpenTelemetry Go SDK |

## Problem

Telemetry has to be wired at five places — HTTP, gRPC, consumers, DB calls, and
handlers (warren.md §7.1) — and every one of them lives in a different module.
The adapters ring is made of separate go modules that **never import each other**
(§1.1, §1.6), so no adapter can be the place where exporter setup, sampling, and
propagation are configured for the others. Left to the application, that wiring
is re-derived per service and gets it subtly different each time.

warren.md §7.1 makes this one module whose import does the wiring: *"One import
instruments HTTP, gRPC, consumers, DB calls, and handlers."*

## Goals

- Provide `observability.Module(...)` configured by service name, OTLP endpoint,
  and sample ratio (§7.1).
- One import instruments the five sites §7.1 names.
- Propagate trace context into `broker.Message.Headers` so that, as §7.1 puts
  it, *a span survives the trip through Kafka into the consumer*. §3.4 reserves
  `Headers` for exactly this, and §1.5's consumer chain has a `TraceExtract`
  stage that reads it.
- Keep `trace.Tracer` directly accessible — this module wraps **setup, not the
  API** (§7.1).

## Non-goals

- **Not an abstraction over OpenTelemetry.** §7.1 and the §9 ledger row
  (Telemetry · OpenTelemetry Go · Wrap · "wiring only") both say wiring. Warren
  defines no `Tracer`, `Span`, or `Meter` port of its own.

  This is consistent with the wrap rule rather than an exception to it. The rule
  (AGENT.md § Modes) is: *if changing a library would force edits across hundreds
  of user files, it must be behind a port.* Every instrumented boundary here is
  inside Warren — the adapters, the handler middleware of §3.2, the broker
  headers of §3.4 — so swapping the SDK edits Warren's files, not the user's.
  A user who calls `trace.Tracer` holds the OTel API **by choice**, one call site
  at a time; a user who does not touch it is untouched by a swap. Compare `pgx`
  or `kgo`, which a user is *forced* through on every query or message: those are
  the ones §1.1 puts behind ports.
- **Not a logging package.** §2.5 owns the context-carried logger. §2.5 says
  `trace_id` and `span_id` "are already attached" to it — this module is the
  source of those values, but warren.md does not state the mechanism (see Open
  questions).
- **Not a metrics or dashboards product.** §7.1 names an OTLP endpoint and a
  sample ratio and nothing further.
- **Imports no other adapter** (invariant 4).

## Dependency audit

**Chosen:** the OpenTelemetry Go SDK. warren.md's stated reason is scope, not
comparison: §7.1 wants one import that instruments five boundaries and a
propagation format that survives a broker hop, and §9 classes it Wrap with the
note "wiring only". No alternative is evaluated anywhere in warren.md.

**Outstanding.** warren.md records **no observation date, no archived check, no
last-release date, no transitive footprint, and no licence check** for the OTel
Go SDK — nor for the exporter §1.7 implies. AGENT.md § Adding a dependency makes
that audit mandatory *before* the dependency lands in a `go.mod`, and the same
section notes two widely-recommended packages that turned out to be archived. The
audit must be run, recorded here, and added to the §9 ledger before any code.

One placement note that is already settled: §1.7's dependency budget puts "OTel
SDK + exporter" in the **user's** direct dependency list for the
`+ gRPC + Kafka + OTel` profile. That is deliberate for a wiring-mode package and
should stay true after implementation.

## Public API

warren.md §7.1 gives usage, not signatures:

```go
observability.Module(
    observability.ServiceName("user-service"),
    observability.OTLPEndpoint(cfg.OTel.Endpoint),
    observability.SampleRatio(0.1),
)
```

§10 passes `observability.Module(observability.ServiceName("user-service"))`
directly to `warren.New`, whose signature is `New(modules ...Module) *App`
(§2.1) — so `Module` returns a `warren.Module`. The argument types are the ones
visible in the calls: a string name, a string endpoint, a float ratio. **The
option type's name is not fixed by warren.md**, nor is whether these are the only
options.

Provisional, to be approved before implementation:

```go
// Package observability wires OpenTelemetry into a Warren application: HTTP,
// gRPC, consumers, DB calls, and handlers are instrumented by importing this
// module. It wraps setup, not the API — trace.Tracer stays directly accessible.
package observability

// Module returns a Warren module that installs tracing and metrics across
// every instrumented boundary of the application.
func Module(opts ...Option) warren.Module

// ServiceName sets the service name reported on every span and metric.
func ServiceName(name string) Option

// OTLPEndpoint sets the OTLP collector endpoint telemetry is exported to.
func OTLPEndpoint(endpoint string) Option

// SampleRatio sets the trace sampling ratio, between 0 and 1.
func SampleRatio(ratio float64) Option
```

## Behaviour

- **Boot, not request time.** `Module` is a module value; per §2.1 and AGENT.md,
  a module declaration is inert and registers nothing on construction. Wiring
  happens when the bootstrapper materialises the graph (§1.3 steps 2–6).
- **Core ring for handlers.** §3.2 lists `app.Traced()` (span per handler, named
  `<module>.<handler>`) and `app.Metered()` (duration histogram, error counter by
  code) as **core** middleware — they wrap `app.Handler[Req, Res]`, so one
  decoration covers HTTP, gRPC, and consumers identically (§1.4).
- **Edge ring for transports.** Transport-shaped instrumentation is the
  adapter's: §4.2 shows `grpc.Tracing()` registered as an interceptor, which is
  an edge-ring concern (§1.4).
- **The `app.Traced()` / `app.Metered()` mechanism is not specified.** `app` is a
  contract package in the core module: stdlib-and-dig only (invariant 1), zero
  implementations (invariant 5). A span and a histogram are implementations, and
  they need the OTel SDK, which core may never import. So `app.Traced()` cannot
  be made real inside `app` — something in this module must supply it, and
  **warren.md never says how**. This is the single largest gap in §7.1 and is
  raised in Open questions.
- **Trace context crosses the broker.** §3.4: `Message.Headers` is
  `map[string]string` and "trace context propagates here". §1.5 and §3.4 place
  `TraceExtract` immediately after `Recover` in the consumer chain. The injection
  side (publish) is implied by §7.1's claim but not drawn in any diagram.
- **Lifecycle.** An exporter has a connection to open and a buffer to flush, and
  §2.3 says adapters register lifecycle hooks rather than users touching
  `lifecycle` directly. Where the flush sits in the §2.3 shutdown list is not
  stated — see Open questions.

## Testing

- **No Docker, no network, no sleeps in unit tests** (AGENT.md § Testing).
  Exporting to a real collector is network I/O: every such test goes behind
  `//go:build integration`, as do any testcontainers-based collector fixtures.
- Unit tests assert against an in-process span recorder: a handler wrapped by the
  core middleware produces one span named `<module>.<handler>` (§3.2), and the
  error counter is keyed by `errors.Code` (§2.6).
- **Propagation round-trip, in memory.** Publish through `broker/memory` (§5.4,
  in-process, so no Docker) and assert the consumer's span is a child of the
  publisher's — the §7.1 claim, tested without Kafka.
- Sampling: `SampleRatio` is honoured; a config value outside 0–1 fails at boot,
  not on request 1 (§1.3).
- **Golden-file tests for error text**: none apply yet — warren.md fixes no error
  message for this package. Any message added during implementation gets one
  (AGENT.md § Testing).
- `t.Parallel()`, table-driven subtests named for behaviour.

## Definition of done

- [ ] Dependency audit for the OTel SDK and exporter run, recorded above with its
      observation date, and added to warren.md §9.
- [ ] `Module` and the three options exist, each with a doc comment starting with
      the identifier's name, and `Module` returns `warren.Module`.
- [ ] All five sites in §7.1 — HTTP, gRPC, consumers, DB calls, handlers — are
      instrumented by importing this module alone.
- [ ] Trace context is injected into and extracted from `Message.Headers`; the
      in-memory round-trip test passes.
- [ ] No OTel type appears in a Warren exported signature unless the human
      resolves Open question 3 the other way.
- [ ] This module imports no other adapter module (invariant 4).
- [ ] Open questions answered by the human and folded into this spec in the same
      change that implements them.
- [ ] `make ci` passes (once the Makefile exists — AGENT.md § Repository state).

## Open questions

1. **How do `app.Traced()` and `app.Metered()` become real?** They are declared
   in `app` (§3.2), a core contract package that may not import OTel (invariant
   1) and may hold no implementations (invariant 5). Is `app.Traced()` a
   constructor over a core-defined port that this module provides? Is the
   middleware actually exported from `observability` and the §3.2 table
   aspirational? warren.md does not say, and nothing can be built until it does.
2. **How do `trace_id` and `span_id` reach the logger?** §2.5 says they are
   "already attached" to the context-carried logger. `log` is core and cannot
   import OTel; this module is an adapter. Which side owns the seam?
3. **What does "`trace.Tracer` stays directly accessible" mean concretely?** That
   users reach it through OTel's own API (a global provider), or that Warren
   exports something returning a `trace.Tracer`? The second puts a third-party
   type in a Warren exported signature, which invariant 3 forbids for driver
   types. Which is intended, and is OTel exempt from invariant 3 by virtue of
   being wiring-mode?
4. **Does this module register lifecycle hooks, and where in the §2.3 shutdown
   order does the exporter flush?** After consumers stop (step 3) and before
   connections close (step 5) would keep the last spans, but warren.md's shutdown
   list does not mention telemetry at all.
5. **Metrics and logs, or traces only?** §3.2's `app.Metered()` implies a meter
   provider, but §7.1's surface names only `OTLPEndpoint` and `SampleRatio`
   (a trace concern). Is there one endpoint for both signals? Are OTel logs in
   scope at all, given §2.5 owns logging?
6. **Which propagator and which header keys?** §3.4 says trace context lives in
   `Message.Headers` but fixes no format. W3C `traceparent`? Is the choice
   configurable, and who owns the key names — this module or `broker`?
7. **Is instrumentation opt-out?** If a service imports this module, are
   `app.Traced()` and `app.Metered()` applied to every handler automatically, or
   does the user still decorate handlers explicitly? §7.1's "one import
   instruments … handlers" suggests automatic; §3.2 presents them as middleware
   one chains.
8. **Which exporter ships?** §1.7 says "OTel SDK + exporter" enters the user's
   `go.mod`. OTLP over gRPC and OTLP over HTTP are different modules with
   different transitive trees.
