# `warren/transport/grpc` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2 (decided 2026-08-02)** — architect round complete and every open question below is now RULED. It is not blocked on a decision; it is blocked on `warren g proto`, which does not exist and is the harder of the two artifacts. Building the adapter first means building it twice, and the version shipped in between would have to lie about reflection. |
| **Source** | [warren.md §4.2](../../warren.md) |
| **Module** | own module (`warren/transport/grpc`) |
| **Mode** | Wrap |
| **Wraps** | `google.golang.org/grpc` |

## Problem

The same `app.Handler[Req, Res]` that serves `POST /users` must also serve
`user.v1.UserService/Register` (§3.5, §10) — with no change to the handler, and
no `google.golang.org/grpc` import anywhere near it.

Two reasons this is a Wrap rather than a direct dependency, both stated in §4.2:

1. **Interceptors must register through the same middleware chain as HTTP.** If
   gRPC had its own parallel chain, a transaction or authorization decorator
   would have to be written twice, and the two copies would drift.
2. **`Error` must map to `codes.Code` without handler involvement.** The moment
   a handler translates a code itself, ring 2 is broken (AGENT.md § The error
   table is load-bearing).

The package is a leaf in the ADAPTERS ring (§1.1): its own Go module, importing
only the core module's contract packages, never another adapter — including
never `warren/transport/http`.

## Goals

- Implement `transport.GRPCRegistrar` (§3.5) so
  `r.GRPC().Method("user.v1.UserService/Register", c.register)` exposes an
  existing handler.
- Own the gRPC column of the error table (§2.6).
- Own the gRPC edge ring: interceptors, decode from proto, encode to proto
  (§1.4).
- Serve **reflection and the health service on by default** (§4.2), the latter
  from the same `warren/health` registry the HTTP adapter reads (§2.8).
- Participate in lifecycle with the ordering fixed in §1.3 and §2.3: servers
  start last, stop after readiness closes, in-flight requests finish.

## Non-goals

- **No core middleware.** Transactions, retry, tracing, metrics, and
  authorization wrap `Handler[Req, Res]` and live in `app` (§3.2). Interceptors
  are the *edge* ring and belong here (§1.4).
- **No reflection on the request path** (invariant 7) — the gRPC *reflection
  service* is a different thing and is on by default; Go reflection runs during
  boot steps 1–5 only.
- **No import of another adapter** (invariant 4).
- **Does not spec `warren g proto`.** §4.2 mentions
  `warren g proto user --service UserService`, which generates the `.proto`,
  runs `buf generate`, and wires the generated service to existing handlers.
  That command belongs to `warren/cli` (§8, tooling ring, build-time only) and
  is specced there. What this package owes it is a stable registration surface;
  what it does *not* do is generate code. `buf` is Vendor, tooling only (§9).

## Public API

Taken from the §4.2 usage block, which is the only surface warren.md fixes. Doc
comments added. §4.2 gives call forms, not signatures — the `Option` return
types below are inferred from the `Server(opts ...Option)` pattern and are not
stated in warren.md.

```go
// Package grpc exposes Warren handlers over gRPC. It is the only package in a
// service that imports google.golang.org/grpc.
package grpc

// Server returns a warren.Module that serves the application's registered gRPC
// methods. Reflection and the health service are enabled by default. It
// registers a lifecycle hook: the listener opens after dependencies are healthy
// and closes before pools close, letting in-flight calls finish.
func Server(opts ...Option) warren.Module

// Port sets the TCP port the server listens on.
func Port(int) Option

// Interceptors appends edge interceptors to the gRPC ring. They register
// through the same middleware chain as HTTP; core middleware — the kind that
// must also apply to HTTP and consumers — belongs in app, not here.
func Interceptors(...) Option

// Recovery is named in §4.2 as an argument to Interceptors. Its behaviour and
// return type are unspecified — see Open question 1.
func Recovery() // ...

// Tracing is named in §4.2 as an argument to Interceptors. Its behaviour and
// return type are unspecified — see Open question 1.
func Tracing() // ...

// TLS enables transport security using the certificate and key at the given
// paths.
func TLS(certFile, keyFile string) Option

// Raw is the escape hatch: it hands the underlying *grpc.Server to fn so that a
// hand-written or legacy service can be registered directly. It is an explicit
// opt-out, not the default path.
func Raw(fn func(s *grpc.Server)) Option
```

**Usage** (§4.2, §10):

```go
grpc.Server(
	grpc.Port(9090),
	grpc.Interceptors(grpc.Recovery(), grpc.Tracing()),
	grpc.TLS(certFile, keyFile),
)
```

Two signatures above are deliberately incomplete because warren.md leaves them
incomplete, and guessing them would be inventing public API:

- `Interceptors(...)` — warren.md shows the call, never the parameter type.
  Invariant 3 forbids `grpc.UnaryServerInterceptor` in a public signature, so
  the type cannot simply be the driver's. See Open questions 1.
- `Recovery()` and `Tracing()` — named in §4.2 as arguments to `Interceptors`,
  so they return whatever that type is. Same question.

## Behaviour

**Boot** (§1.3). Methods are registered at step 5, into an in-memory table of
pre-composed closures. The lifecycle hook runs at step 6 in dependency order —
pool → repos → consumers → **servers**. Readiness flips green at step 7.

**Call path** (§1.4). Edge interceptors → decode from proto → validate →
`app.Handler` → core middleware → `Handle` → encode to proto, or map the error.

The adapter owns, and the handler never sees:

- **decode** — the proto request message into `Req`.
- **validate** — `warren/validate` runs automatically after decode; a bad request
  never reaches `Handle` (§2.7).
- **encode** — `Res` into the proto response message.
- **status mapping** — the gRPC column of §2.6, below. This is the reason the
  package is wrapped at all.
- **edge middleware** — interceptors, including `Recovery` and `Tracing`.
- **context seeding** — the transport adapters seed the logging context, so
  handlers never construct a logger (§2.5).
- **the health service** — served from the same registry that backs `/healthz`
  and `/readyz` (§2.8), so a Postgres ping check reports through both transports
  without being registered twice.
- **reflection** — on by default (§4.2).

**Shutdown** (§1.3 step 9–10, §2.3). Readiness closes first so the load balancer
drains; then HTTP/gRPC servers stop accepting and in-flight calls finish;
consumers, relay, and pools follow; a force-exit deadline (default 30s, owned by
`lifecycle`) bounds the sequence.

**Must never**: consult the DI container per call (invariant 7); let a handler
see `codes.Code`; import `transport/http` or any other adapter (invariant 4);
leak `*grpc.Server`, `grpc.ServerOption`, or `codes.Code` through any signature
other than `Raw`.

## Error mapping

The gRPC column of §2.6. Domain code returns `errors.Conflict(...)`; this table
is the only place it becomes a `codes.Code`.

| Code | gRPC |
|---|---|
| `INVALID` | `InvalidArgument` |
| `NOT_FOUND` | `NotFound` |
| `CONFLICT` | `AlreadyExists` |
| `UNAUTHENTICATED` | `Unauthenticated` |
| `PERMISSION_DENIED` | `PermissionDenied` |
| `UNAVAILABLE` | `Unavailable` |
| `INTERNAL` | `Internal` |

§2.6 now carries rows for `UNAUTHENTICATED` (`Unauthenticated`) and
`PERMISSION_DENIED` (`PermissionDenied`), with the rule that `UNAUTHENTICATED`
describes the caller's identity, not the service's — downstream auth failure is
`UNAVAILABLE`. See Open question 6, resolved.

## Escape hatch

```go
grpc.Raw(func(s *grpc.Server) { pb.RegisterLegacyServer(s, impl) })
```

`*grpc.Server` is named in invariant 3 as a driver type that may not appear in a
public signature. `Raw` is the single named opt-out, and §4.2's own example says
what it is for: registering a **legacy** service that predates the port. Making
it the default path would put `google.golang.org/grpc` back into user code, and
with it the per-transport error mapping and the second middleware chain that the
Wrap exists to prevent.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing). Serve
  over an in-process listener (`bufconn`-style, no port bound); test drain
  against a synchronised handler, never a `time.Sleep`.
- **Integration tests behind `//go:build integration`** — anything binding a real
  port, using real TLS material, or requiring `buf`/generated code.
- **Golden-file tests for error text.** Every code/status pair produced by the
  mapping table, including the message and details carried on the status.
- **Allocation benchmark on the call path.** Invariant 7 is a performance claim
  and needs a number: a fixed allocation count per call, and no container lookup.
- **Contract parity with HTTP.** The same handler, driven through both adapters,
  must produce the same `Code` for the same input — that is the claim §1.4 makes
  and it should be a test, not a promise.
- `t.Parallel()`, table-driven subtests named for behaviour.

## Definition of done

1. `Server`, `Port`, `Interceptors`, `Recovery`, `Tracing`, `TLS`, and `Raw`
   implemented with the signatures above (once Open question 1 is answered),
   each with a doc comment starting with its identifier.
2. `transport.GRPCRegistrar` implemented; `r.GRPC().Method(...)` serves an
   existing `app.Handler` end to end.
3. Interceptors register through the same middleware chain as HTTP, demonstrated
   by the parity test.
4. Every row of the gRPC column mapped, with golden files.
5. Reflection and the health service on by default, the latter reading the
   `warren/health` registry.
6. Lifecycle hook registered with the §2.3 ordering; a drain test proves
   in-flight calls finish after readiness closes.
7. Allocation benchmark committed and passing.
8. `go.mod` contains the core module and `google.golang.org/grpc` and nothing
   else that is not stdlib.
9. No driver type in an exported signature except inside `Raw`.
10. This spec corrected in the same pull request wherever the implementation
    diverged.

## The ruling that defers it — architect round, 2026-08-02

**The crux was open question 3: how a proto message becomes the handler's
`Req`.** Three candidates were on the table; the answer is a fourth, and it is
what makes the adapter wait.

**(a) The handler's `Req` IS the generated proto type — DISQUALIFIED, and not
on purity.** The same handler serves `transport.Post(r, "/users", c.register)`,
and the HTTP adapter would then JSON-encode a generated struct with
`encoding/json`, which is not protojson: wrong field names, wrong enums, broken
oneofs, `nil`-versus-empty confusion. **(a) breaks the HTTP adapter for the very
handler the gRPC adapter exists to share.** It destroys the claim it was meant
to serve.

**(c) A proto codec over PLAIN Go structs — investigated, measured, and
rejected on contract grounds.** It is mechanically possible:
protobuf-go's aberrant-message path marshals a struct carrying
`protobuf:"bytes,1,opt,name=email,proto3"` tags given three stub methods, and
`grpc.ForceServerCodecV2` plus `grpc.UnknownServiceHandler` will serve method
names with no generated `ServiceDesc` at all. It is also **faster than JSON** —
measured on an M3 Pro, a three-field message: 108 ns/32 B/2 allocs to marshal
against `encoding/json`'s 122/48/1, and 158/117/5 to unmarshal against 603/336/9.

Those numbers are recorded here precisely so nobody re-derives them and mistakes
them for a case. It is rejected because what it produces is not a protobuf
service:

- the message name is the **Go type name** (`main.RegisterUser`, not
  `user.v1.RegisterRequest`);
- the descriptor never enters `protoregistry`, so **the reflection service —
  which §4.2 has on by default — has nothing to report**, and grpcurl,
  `buf curl`, Postman, Envoy transcoding and grpc-gateway are all dead;
- **field numbers would live in Go struct tags**, so a contributor reordering a
  struct or renumbering a tag silently breaks every deployed client — which is
  the exact failure protobuf exists to make impossible;
- the mechanism is `internal/impl`, documented as a best-effort compatibility
  shim for pre-APIv2 messages, not an authoring path.

It ships something that speaks gRPC *framing* and is not a service any other
language can consume. That is a worse lie than shipping no gRPC. **Rejected
permanently.**

**(d) is the answer: generated proto types at the wire, plain structs at the
handler, and a GENERATED conversion shim in the wiring layer.** The `.proto` is
the source of truth, `buf` generates the messages, and a generated binding
converts `*pb.X ↔ Req` per method. This is legal against the shipped port
today: `GRPCRoute.Bind(Codec) Invoker` is called once per route at boot, so each
route can bind a codec that knows its own pb type. It costs one message
allocation and one struct copy per call, no reflection on the request path, and
it buys full proto fidelity, a working reflection service, and a `.proto` file
as the cross-language contract.

**And (d) does not work without `warren g proto`.** Without the generator every
method needs a hand-written binding and a hand-maintained `.proto` — the exact
drift §4.3 refuses to tolerate for OpenAPI. Generating the `.proto` is the
*hard* half: name mapping, **stable field numbering across regenerations**, and
evolution when a field is deleted. That is a spec of its own, it is a CLI spec,
and it does not exist.

**So: defer.** The port is not the problem — this round found **zero required
changes to core `transport`**, which is a strong result for §3.5 and the
strongest argument that deferring costs nothing structurally. Measured, a gRPC
server with reflection and health costs **6 third-party modules**
(`grpc`, `protobuf`, `genproto`, `x/net`, `x/sys`, `x/text`) plus `buf` as
tooling — the largest footprint of any transport, bought for an adapter that
cannot yet honour its headline feature.

**This would be reversed only for a named design partner with a hard v0.1 gRPC
requirement.** The minimum honest shape then is (d) with a hand-written
`grpc.Bind[Req, Res](fullMethod, from, to)` per method and a hand-authored
`.proto` — real protobuf, real descriptors, real reflection, just no generator,
and `warren g proto` later emits exactly that call. **Option (c) is not on the
table under any circumstances.**

## The other rulings, so v0.2 resumes from a decided position

**1. `Interceptors` takes a Warren-owned type, and `Recovery()`/`Tracing()` are
deleted.**

```go
type Call struct{ FullMethod, Peer string }
type Handler func(ctx context.Context, c Call, req any) (any, error)
type Interceptor func(next Handler) Handler
```

Driver-free, so invariant 3 holds with no carve-out. §4.2 is wrong to make
recovery and tracing user-supplied options: `transport/http` makes recover
**outermost and not removable** and composes telemetry off `Table.Telemetry()`,
and an application must not be able to disable panic recovery by forgetting an
argument.

The driver carve-out is the **`Raw*` prefix**, and it needs a second member:
`Raw(func(*grpc.Server))` runs after `NewServer`, so it cannot install
`ChainUnaryInterceptor` or a keepalive policy. `RawServerOptions(...grpc.ServerOption)`
covers that, and `scripts/invariants.sh` scopes the carve-out to the prefix.

**2. Streaming is out, and `transport.Raw` already covers it — no core change.**
Same ruling as HTTP: typed byte-in/byte-out is the only typed shape.
`transport.Raw(r, transport.ProtocolGRPC, "user.v1.UserService/Watch", h)`,
where the pattern is the full method name and carries no verb because gRPC has
none. The adapter asserts `grpc.StreamHandler` and fails the boot naming the
route and the type, exactly as the HTTP adapter asserts `http.Handler`. That
the user's module then imports grpc is the identical precedent to a raw HTTP
route importing `net/http`.

**4. `WithoutReflection()` and `WithoutHealthService()`**, both on by default.
Reflection publishes the entire API surface and operations teams will want it
off in production; health stays on unless disabled, because Kubernetes `grpc`
probes require it.

**5. TLS matches `transport/http` exactly: `TLS(*tls.Config)` and
`TLSFiles(certFile, keyFile)`.** `*tls.Config` is standard library, not a
driver type. §4.2's `TLS(certFile, keyFile)` becomes `TLSFiles`; the path-pair
form alone cannot reach mTLS, client CAs, or in-memory certificates. One shape
across every adapter.

**7. The default port is `:50051`, not 9090.** 50051 is the gRPC ecosystem's
own default; **9090 is Prometheus's**, and it will collide the day
`observability` exposes a scrape endpoint. `Addr(string)` and
`Listener(net.Listener)` come too, for parity with HTTP — the second is what
this spec's own bufconn testing requirement needs.

**8. The `grpc.Server` collision is accepted and documented, and §4.2's example
does not compile as written.** Warren keeps the package name `grpc`, for
symmetry with `http.Server(...)`. A user reaching the escape hatch aliases:

```go
import (
    "google.golang.org/grpc"
    wgrpc "github.com/MerseniBilel/warren/transport/grpc"
)
wgrpc.Raw(func(s *grpc.Server) { pb.RegisterLegacyServer(s, impl) })
```

**9. `warren g proto` is in the design and out of v0.1.** It is what the adapter
is blocked on, so it gets specced *with* the adapter, in the CLI spec, and §8's
command surface gains it in the same change. Until then §4.2 must stop promising
a command §8 does not list.

**The health service does NOT use grpc-go's `health.Server`.** That keeps its
own status map and would need polling into sync, giving two sources of truth.
`grpc_health_v1.HealthServer` is implemented directly over the injected
`health.Registry` — `Check` with an empty service maps `Ready(ctx)` to
`SERVING`/`NOT_SERVING`, an unknown service name is `codes.NotFound` per the
health spec, and `Watch` polls and emits on change because Envoy uses it. Both
bypass the edge ring except recover, mirroring `/healthz` and `/readyz`.

**Telemetry comes off `tbl.Telemetry()`, never injected** — injecting
`app.Telemetry` makes it a required dependency and every uninstrumented service
fails to resolve it. Handler instrumentation is already composed in core by
`buildInvoker`, so this adapter owns only the SERVER span and trace
continuation from incoming metadata. HTTP/gRPC parity is free, not a promise.

## Open questions

For the human. warren.md does not answer these, and none should be guessed.

**All nine are now ruled — kept for the record of what was asked.**

1. **RULED (see above).** What type does `Interceptors` accept? Invariant 3 rules out
   `grpc.UnaryServerInterceptor`, but §4.2 gives no Warren-owned alternative,
   and there is no stdlib-shaped signature to fall back on the way HTTP falls
   back on `func(http.Handler) http.Handler`. This is a port shape and blocks the
   Public API section. What do `Recovery()` and `Tracing()` return?
2. **Streaming.** `transport.Registrar` models one shape —
   `Method(name, handler)` over `Handler[Req, Res]` (§3.5). Server-streaming,
   client-streaming, and bidirectional RPCs are not mentioned anywhere in
   warren.md. Out of scope for v1, or a gap in the port?
3. **Proto message ↔ `Req`/`Res`.** §4.2 says the CLI "wires the generated
   service to existing handlers" but never states the runtime contract: does the
   adapter unmarshal into a generated struct and convert, or does the handler's
   `Req` have to be the generated type? The answer decides whether an
   application layer stays proto-free.
4. **Can reflection or the health service be turned off?** "On by default"
   implies an opt-out; no option is named. Production users will ask for one.
5. **`TLS` option shape.** §4.2 gives `TLS(certFile, keyFile)` while §5.1 gives
   Kafka `TLS(*tls.Config)`. Inconsistent across adapters; mTLS, client CAs, and
   in-memory certificates are unreachable through the path-pair form.
6. **RESOLVED (2026-08-01):** warren.md §2.6 gained the rows: `UNAUTHENTICATED`
   → `Unauthenticated` and `PERMISSION_DENIED` → `PermissionDenied`, plus the
   rule that `UNAUTHENTICATED` describes the caller's identity, not the
   service's (downstream auth failure is `UNAVAILABLE`). **`UNAUTHENTICATED` and `PERMISSION_DENIED` have no row in §2.6** yet exist in
   `warren/errors` and are reachable through `auth` (§7.2). `codes.Unauthenticated`
   and `codes.PermissionDenied` are the obvious answers but are not written down;
   warren.md needs the rows added.
7. **Default port.** §10 uses `9090`; no default is stated for when `Port` is
   omitted.
8. **The name collision is real and needs a decision.** In this package
   `grpc.Server(...)` is Warren's module constructor, while inside
   `grpc.Raw(func(s *grpc.Server){...})` the very same selector is Google's
   server type. Both appear in §4.2, four lines apart. Rename one, or accept it
   and document it?
9. **`warren g proto` is not in §8's command surface,** which lists
   `g module|entity|command|repository|consumer` only. §4.2 introduces it. The
   manifest should be reconciled before the CLI spec commits to it.
