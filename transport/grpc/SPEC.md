# `warren/transport/grpc` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
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

## Open questions

For the human. warren.md does not answer these, and none should be guessed.

1. **What type does `Interceptors` accept?** Invariant 3 rules out
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
