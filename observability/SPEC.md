# `github.com/MerseniBilel/warren/observability` — SPEC

| | |
|---|---|
| **Status** | **APPROVED AND IMPLEMENTED (2026-08-02)** — the approval condition is met: `app.Telemetry` shipped as the core seam, so open question 1 is answered by code. The architect round ruled on questions 2–8, the dependency audit is done, and the module is built and tested. |
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

**Done, 2026-08-02.**

| Package | Version | Licence | Stars | Last push | Archived |
|---|---|---|---|---|---|
| `open-telemetry/opentelemetry-go` | v1.44.0 (2026-05-27) | Apache-2.0 | 6 500 | 2026-08-02 | no |

**Transitive footprint: 16 third-party modules**, measured with `go list -deps`
on this module as built — not read off a README. Among them
`google.golang.org/grpc`, `google.golang.org/protobuf`,
`google.golang.org/genproto` and `grpc-ecosystem/grpc-gateway`.

**There is no lighter exporter.** OTLP-over-HTTP was measured too, on the
assumption it would avoid gRPC, and it does not: `otlptracehttp` reaches
`google.golang.org/grpc` through its own `otlpconfig` package, compiling 65
gRPC packages either way. Both exporters cost the same, so shipping two would
buy a second code path to test for nothing. **OTLP over gRPC ships; the HTTP
exporter is not offered.**

For scale, in this repository: core costs 1 third-party module (`dig`),
`transport/http` 0, `persistence/postgres` 6, and this module 16. That gap is
why it is opt-in, in its own module, and why `scripts/invariants.sh` now
refuses `go.opentelemetry.io` in any other `go.mod` — a service that does not
import it must pay nothing, and one convenient import elsewhere would put gRPC
in every user's `go.sum`.

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

**As implemented** (the full surface is in the package doc comments; this is
the shape):

```go
func Module(opts ...Option) warren.Module

func ServiceName(name string) Option        // required when exporting
func ServiceVersion(version string) Option
func ResourceAttr(key, value string) Option
func OTLPEndpoint(addr string) Option       // "host:port"; empty disables export
func Insecure() Option
func OTLPHeader(key, value string) Option
func SampleRatio(ratio float64) Option      // default 1; parent-based
func MetricInterval(d time.Duration) Option // default 60s
func ExportTimeout(d time.Duration) Option  // default 10s

func WithoutMetrics() Option
func WithoutHandlerInstrumentation() Option
func WithoutLogCorrelation() Option

func LogAttrs() log.ContextAttrs
```

Twelve options, not three. Three cannot configure an OTLP exporter: auth
headers, TLS and the metric interval are all required in practice. warren.md
§7.1 is amended in the same change.

**No OpenTelemetry type appears in any signature.** `LogAttrs` returns
`log.ContextAttrs`, a core function type. Open question 3 is ruled that way:
`trace.Tracer` stays accessible through OTel's own global provider —
`otel.Tracer("billing").Start(ctx, …)` — so a user holds the SDK by choice,
one call site at a time, and invariant 3 needs no exemption.

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
- **`app.Traced()` and `app.Metered()` become real through `app.Telemetry`**,
  the core-ring seam that shipped with `app`: a two-method port core declares
  and this module implements. Core holds the interface, never an
  implementation, so invariants 1 and 5 both hold. Answered — open question 1
  is closed.
- **Instrumentation is automatic, composed at BOOT.** `transport.WithTelemetry`
  binds it, and `buildInvoker` — the one place every typed route and every
  event route passes through, and generic, so `Traced[Req,Res]` is
  instantiable — wraps each handler once at step 5. The request path decides
  nothing and consults nothing, which is invariant 7 exactly. With no
  telemetry bound nothing is composed and the closure is byte-identical to a
  service that never heard of this package; `transport/http`'s allocation test
  still measures 17. Opt out with `WithoutHandlerInstrumentation()`.
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

## Rulings — architect round, 2026-08-02

**2. `trace_id` and `span_id` reach the logger through a `slog.Handler`, not a
derived logger.** Core gains `log.ContextAttrs` (a function type) and
`log.Handler(h, extra...)`, which resolves context fields in `Handle` — where
`slog` is designed to do it. This module supplies `LogAttrs()`, and `Module`
installs the wrapper unless told not to.

It also closes `transport/http/SPEC.md` open question 6 and vindicates that
adapter's refusal to derive a per-request logger: `log.With` costs **8
allocations on every request**, measured, whether or not the request logs
anything. Resolving at emit time means a request that logs nothing pays
nothing, and one that logs ten lines pays no derivation either.

**3. Through OTel's own global API.** See Public API above.

**4. The exporter flushes LAST.** `Module` registers declared hooks, which
unwind in reverse module order — so listing it FIRST in `warren.New` makes it
stop last: after consumers, after the outbox relay, after the pool closes. The
spec's earlier guess (between consumers and connections) is strictly worse,
because it would drop the spans emitted *during* teardown, which are exactly
the ones nobody can reproduce. `ExportTimeout` bounds the flush and a timeout
logs rather than failing shutdown: a saturated collector must not hold a pod
in Terminating.

**5. Traces and metrics; OTel logs are out.** `app.Metered` ships in core
today and would be permanently inert with traces alone. One `OTLPEndpoint`
serves both signals, which is what every collector expects.
`WithoutMetrics()` opts out. Logs stay out because §2.5 owns logging and
ruling 2 already puts the trace ID on every record — which is what a
log-to-trace jump actually needs.

**6. W3C `traceparent`/`tracestate` plus `baggage`, not configurable.** The
key names are the W3C spec's, not Warren's, and every collector and hosted
backend already speaks them. `broker.Message.Headers` stays an opaque
`map[string]string` and `broker` never learns a key name — which is what its
field comment already promised. B3 and Jaeger are deferred; a shop that needs
them has a collector that translates.

**7. Automatic and opt-out.** See Behaviour above.

**8. OTLP over gRPC, hard-wired.** See the audit: the HTTP exporter is not
lighter. Taking an exporter from the user was rejected — the parameter type
would be `sdktrace.SpanExporter`, the invariant 3 breach ruling 3 refused.
`OTLPEndpoint("")` is the escape hatch for tests and local runs, and it costs
no API.

## Divergences — what the implementation changed

**1. `app.Telemetry` grew from two methods to four.** `Inject` and `Extract`
were added, because propagation cannot be expressed without a core-shaped
seam: the adapters carry trace context, and an adapter may not import this
module (invariant 4). The carrier is a **function**, not a map, so an HTTP
header, a `broker.Message.Headers` and gRPC metadata are all reachable with no
intermediate allocation — and so core never names a header key.

**2. `app.Stamp(name, tel)` fuses two context values into one.** Handler name
and telemetry are both boot-fixed, so carrying them separately would cost an
instrumented request two `context.WithValue` allocations for a pair that never
varies. `StampHandlerName` remains for the telemetry-absent path.

**3. Telemetry travels on `transport.Table`, not through the container.**
Injecting `app.Telemetry` into an adapter's constructor would make it a
REQUIRED dependency, and every uninstrumented service would fail to resolve
it. The Table is already the boot-to-adapter channel — provided empty at step
2, filled at step 5, read by adapters at step 5b — so it carries this too.
Found by building it the other way first and watching a telemetry-free app
fail to boot.

**4. `postgres.Configure(func(*pgxpool.Config) error) Option` is new, and
`postgres.Raw`'s doc comment was wrong.** `Raw` runs at `OnStart`, *after*
`pgxpool.NewWithConfig`, so it cannot install a query tracer —
`ConnConfig.Tracer` is read at construction. The comment said it could.
`Configure` runs before the pool exists, which is the only moment that seam is
open.

**5. Database spans need one explicit line**, so warren.md §7.1's "one import
instruments HTTP, gRPC, consumers, DB calls, and handlers" is amended: it is
four of five automatically, and the fifth is one `postgres.Configure` call in
`main`. The pgx tracer seam is a pgx type and this module may not import that
adapter; the alternative would break invariant 4.

**6. `broker.InjectTrace(ctx, msgs)` is new** — the publish side, which
nothing implemented. It is a no-op when no telemetry is bound. The outbox must
call it when the row is WRITTEN, not when it is relayed: the relay runs long
after the request's span ended, and a span parented to the relay's own context
is a trace nobody can follow back to the request that caused it.

**7. `scripts/invariants.sh` refuses `go.opentelemetry.io` in any `go.mod` but
this one.** With 16 modules behind it, one convenient import elsewhere would
put gRPC and protobuf in every user's `go.sum`.

## Open questions