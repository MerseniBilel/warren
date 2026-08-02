# `warren/transport/http` — SPEC

| | |
|---|---|
| **Status** | **RESHAPED, not yet approved (architect round 2026-08-02).** The router is decided — `net/http.ServeMux`, no chi — and the rulings below are binding on the rewrite. Three foundations of the previous draft were wrong: it described types core does not have (`HTTPRegistrar`, `RouterAdapter`), it picked the worst of five candidate routers on the project's own stated priority, and it depends on a boot step that is unimplemented and unmentioned. **Blocked on the human for two decisions, marked ⚠ below.** |
| **Source** | [warren.md §4.1](../../warren.md) |
| **Module** | own module (`warren/transport/http`) |
| **Mode** | Vendor (standard library only) |
| **Wraps** | `net/http` — and nothing else. Its `go.mod` is the core module and no third party. |

## Decisions — architect round, 2026-08-02

Every measurement below was taken on this machine (go1.26.3, darwin/arm64,
Apple M3 Pro) and is reproducible.

### The router: `net/http.ServeMux`. Vendor. No chi, gin, echo, httprouter or fiber.

The argument is not that chi is unhealthy — audited 2026-08-02 it is the
healthiest candidate (MIT, **zero dependencies**, 22.6k stars, released
2026-07-06). The argument is that **the sealed `Registrar` already threw away
everything a router is bought for.** `transport.Registrar` is sealed, adapters
consume `[]HTTPRoute`, and core owns decode, validate, param binding and status
defaults. The adapter's entire use of a router is two operations: match a
method+pattern to one pre-built closure, and hand back the path segments. Route
groups, `Mount`, per-group middleware, binding, rendering, `gin.Context` — none
of it is reachable through the port, by construction.

What changed since §4.1 was written: **Go 1.22 gave `ServeMux` method and
wildcard patterns, and Go 1.23 gave `Request.Pattern`** — precisely the two
features the port needs, the second being the low-cardinality route label
`app.Metered()` wants.

Routing plus two path parameters, no body:

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| `net/http.ServeMux` | 155.6 | 48 | **2** |
| `go-chi/chi/v5` | 237.7 | 704 | **4** |
| `julienschmidt/httprouter` | 43.0 | 64 | 1 |
| `gin` | 34.9 | 0 | 0 |
| `echo` | 49.9 | 0 | 0 |

chi's own source explains its number, in `Mux.ServeHTTP`: `r.WithContext(...)`
shallow-copies the whole `http.Request` — that is the 704 B. `ServeMux` sets
`r.Pattern` in place and never clones. **Choosing the slowest and heaviest
option and calling this the performance-first framework does not survive a
benchmark blog post.**

gin's and echo's zeroes are unreachable behind this port: both reach 0 by
pooling their own `Context` and owning the handler signature, and §4.1's
`Middleware(func(http.Handler) http.Handler)` — the reason chi was chosen —
forces `*http.Request` back in, and the clone with it. gin also costs **29
indirect requires**, including a mongo driver, quic-go, a JIT JSON encoder and
an assembler, in every service's `go.sum`. `httprouter` is the rule-9 case in
everything but the flag: **v1.3.0, September 2019; no commit since 2024-07.**
fasthttp/fiber cannot honour `func(http.Handler) http.Handler`, `Flusher`,
`Hijacker` or HTTP/2 at all.

**The ecosystem argument survives.** `chi/v5/middleware` — `RealIP`,
`RequestID`, `Recoverer`, `Compress`, `Timeout` — is `net/http`-shaped and runs
unmodified on a `ServeMux`. §4.1's own usage example compiles unchanged. A user
who wants it adds chi to *their* go.mod; Warren does not put it in everyone's.

**Boot diagnostics get better:** `ServeMux` refuses overlapping patterns at
registration and names both call sites; chi accepts `{id}` and `{uid}` on the
same path silently. And invariant 3 stops being strained — with no driver type
in the package, `Raw(func(*http.ServeMux))` is not an escape from an invariant.

**Given up, and the spec must say so:** regex path constraints (`validate:`
covers the intent), `Mount`/sub-router composition, replaceable `NotFound`
handler objects, and chi's ordering helpers. `ServeMux`'s implicit
trailing-slash 301 needs explicit golden tests.

**Consequences:** `RouterAdapter` and `Router(...)` are deleted — a swap
mechanism protecting against a cost the sealed port already eliminated. The
Gin/Echo/Fiber adapters become a non-goal. warren.md §9 loses its
`HTTP | go-chi/chi/v5 | Wrap` row.

### Blocking, and ⚠ for the human

**⚠ 1. Boot step 5 does not exist.** `app.go` instantiates controllers and
consumers and **never calls `Register` on any of them**; warren.md §2.1 admits
it. So this package's definition of done is unreachable without a change to the
core `warren` package — a public-API change, and therefore not the
implementer's call. Proposed: boot builds one `*transport.Table` at step 5,
provides it in the root scope, adapters inject it and call `Claim`, and
`Unserved()` is checked after every module constructor and before
`lifecycle.Start`. And `Controllers(...any)` takes `any`, so the bootstrapper
must type-assert and **fail the boot naming the type and its declaration
site** — today a controller with a typo'd `Register` signature registers
nothing, silently, which is the exact failure §1.3 exists to prevent.

**⚠ 2. Streaming, uploads, SSE and WebSockets have no shape, and the port
cannot grow one.** `Invoker` is `func(ctx, []byte) ([]byte, error)`: whole body
in memory, whole response in memory. Right for commands, wrong for multipart
upload, file download, SSE, WebSocket upgrade and NDJSON export — the same hole
as `transport/grpc` open question 2. Widening `Invoker` is the wrong fix;
modelling streams behind a three-protocol port is a v2 design that has to
satisfy gRPC streaming and broker batching too. Ruling: **the typed
byte-in/byte-out route is the only typed route in v0.1**, plus one named
escape hatch, `Handle(pattern string, h http.Handler)`, which gets the edge
ring, the correlation ID and the drain, and no decode/validate/encode. An
honest documented gap beats a guessed abstraction — but shipping a backend
framework with no answer for file upload is the human's call.

### Rulings on the rest

**Error body.** `{"error":{"code","message","details","correlation_id"}}`,
`application/json; charset=utf-8`. `code` is `errors.Code` verbatim — the
seven-value closed set, so clients can switch on it. **`INTERNAL` never renders
`Message()` and never renders the wrapped cause** — a fixed `"internal error"`
plus the correlation ID, with the real thing logged at ERROR; anything else
leaks DSNs and SQL to the internet. Validation failures are `INVALID` with one
`details` entry per field in declaration order, so golden files are
deterministic. Not RFC 9457 `problem+json`: `errors.Code` is not a URI.

**Success status.** Already decided in core (`transport.Status`, defaults 201
for `Post`, 204 for `Delete`, 200 otherwise); the adapter only reads it. Two
rules to add: `Success` is ignored on the error path, and a 204 route writes
**no body and no `Content-Type`**.

**Health probes are not routes.** `/healthz` and `/readyz` register directly on
the mux and **bypass the edge ring entirely** — no auth, no rate limit, no
tracing; a span every two seconds is a telemetry bill, not a signal. `/readyz`
returns 503 with a body naming the failing check, and whether the gate is
`starting` or `draining`.

**Defaults.** `Port` 8080 · `ReadHeaderTimeout` **10s** (non-negotiable, the
Slowloris fix) · `ReadTimeout` 30s · `WriteTimeout` **0, deliberately** (a
global write timeout kills SSE and large downloads) · `IdleTimeout` 120s ·
`MaxHeaderBytes` 1 MiB · **`MaxBodyBytes` 1 MiB, new** (`Invoker` is
`[]byte`-in; an unbounded body is a one-request OOM) · **`DrainDelay` 5s,
new** · stop-hook timeout 15s, which must sit inside `lifecycle`'s 30s
force-exit budget.

**Shutdown: warren.md's claim that step 9 is why rolling deploys do not drop
requests is not true as specified.** Readiness closing and the listener
stopping happen in immediate succession, but a load balancer polls `/readyz`
on an interval and keeps routing for up to one full poll period afterwards.
`DrainDelay` makes the stop hook wait, interruptibly, between the two, so the
balancer observes the 503 before the listener goes away. warren.md §1.3 gains
the step. Also to state: **`Server.Shutdown` does not cancel in-flight request
contexts**, so a handler that ignores `ctx` blocks the drain until the
force-exit deadline.

**Middleware order, stated as fixed:** recover (outermost, not removable) →
correlation-ID and logger seeding → telemetry → user `Middleware(...)` in
argument order → mux dispatch → guards → decode → validate → core chain →
handler. User middleware cannot precede correlation-ID seeding or their log
lines have none. **Limitation to state plainly: `Middleware` is global-only in
v0.1**; "auth on `/admin/*` only" is answered by `transport.Guard(policy)` per
route, which runs before decode — which is also why guards travel as data.

**TLS: add `TLS(*tls.Config)` and `TLSFiles(cert, key)`.** "Terminate at the
ingress" is the user's decision, not the framework's; mTLS meshes and
single-binary deployments both need it. `*tls.Config` is stdlib, so invariant 3
is untouched, and it matches Kafka's option rather than gRPC's file pair.

**H2C: add it.** Since Go 1.24 `http.Protocols`/`SetUnencryptedHTTP2` needs no
`x/net`; verified on go1.26.3. Meshes speak it. TLS implies h2 already.

**404/405 cost nothing.** A boot-time method-less pattern per path with a
precomputed `Allow` header, plus a `/` catch-all, gives JSON 404 and 405
envelopes with zero per-request work. Verified.

**Allocation budget, to be committed in the spec:** ≤ 16 allocations and
≤ 640 B for a POST with a JSON body and two path parameters — of which
**seven are `encoding/json`'s decoder** and ten are core's `Invoker`
(measured). The reference point the benchmark must also print is the same
handler called directly: **0 allocations.** `TestAllocations` asserts an exact
count. Two rules with numbers behind them, to be written down before someone
"simplifies" them back: read the body into a **pooled `bytes.Buffer`**, never
`json.NewDecoder(r.Body)` (9 allocs/968 B vs 7/272) and never `io.ReadAll`
(2/544); and read query parameters by **scanning `r.URL.RawQuery`** (0 allocs),
never `r.URL.Query()` (4 allocs/432 B).

**Already applied from this round:** the `app.WithHandlerName` boxing (one
allocation per request on all three protocols) is fixed in
`app.StampHandlerName`; invariant 7 is restated in AGENT.md, warren.md §1.4 and
CLAUDE.md, because `bindParams`, `validate`'s rule and `encoding/json` all
reflect per request and the old wording was false.

**Also to correct:** §1.7's dependency budget claims an HTTP-only service
depends on `dig` and `validator` — `dig` is a direct dependency of nobody's
service (invariant 2) and `validator` is not in core at all. The true row is
**`warren`, `warren/transport/http` — two modules, zero third party**, which is
a headline the manifest is currently understating.

---

## Problem

A use case is an `app.Handler[Req, Res]` (§3.2). It must be reachable over HTTP
without importing `net/http`, without knowing that 404 exists, and without
choosing a router. Something has to turn a request into `Req`, run the handler,
turn `Res` into a response, and turn a `*Error` into a status code. That
something is this package, and it is the only place in an HTTP service where
`net/http` and the router appear.

The package is a leaf in the ADAPTERS ring (§1.1): its own Go module, importing
only the core module's contract packages, never another adapter.

## Goals

- Implement `transport.HTTPRegistrar` (§3.5) so `r.HTTP().Post("/users", c.register)`
  serves a handler that imports no transport package.
- Own decode, validate-dispatch, encode, status mapping, and the edge middleware
  ring for HTTP (§1.4).
- Own the HTTP column of the error table (§2.6) so no handler ever maps a code to
  a status.
- Keep the router swappable. chi is the default **because it is
  `net/http`-compatible**, so every stdlib-shaped middleware in the Go ecosystem
  works unmodified (§4.1, §9). Gin and Echo adapters implement the same
  `HTTPRegistrar`.
- Participate in lifecycle with the ordering fixed in §1.3 and §2.3: start after
  dependencies are healthy, stop before pools close, drain in-flight requests.

## Non-goals

- **Not a web framework.** Warren composes routers behind a stable port; it does
  not compete with them.
- **No core middleware.** Transactions, retry, tracing, metrics, and
  authorization wrap `Handler[Req, Res]` and live in `app` (§3.2). This package
  owns the *edge* ring only — CORS, real-IP, correlation ID, auth guards (§1.4,
  §7.2). Conflating the two rings is the mistake that makes
  "transport-agnostic" frameworks leak.
- **No reflection on the request path** (§1.4, invariant 7). Route tables are
  built during boot step 5; by step 8 they hold pre-built closures.
- **No import of another adapter** (invariant 4).
- **Not the OpenAPI generator.** `warren/openapi` is a separate module (§4.3).

## Public API

Taken from §4.1. Doc comments added. The four `Option` constructors are verbatim
from §4.1; `Raw`'s `Option` return type is inferred from the same pattern and is
not stated there — §4.1 shows it only as a bare call.

```go
// Package http exposes Warren handlers over HTTP. It is the only package in an
// HTTP service that imports net/http or a router.
package http

// Server returns a warren.Module that serves the application's registered HTTP
// routes. It registers a lifecycle hook: the listener opens after dependencies
// are healthy and closes before pools close, draining in-flight requests.
func Server(opts ...Option) warren.Module

// Port sets the TCP port the server listens on.
func Port(int) Option

// Middleware appends edge middleware to the HTTP ring, in the stdlib
// func(http.Handler) http.Handler shape. Anything written for net/http works
// unmodified; this is why chi is the default router. Core middleware — the kind
// that must also apply to gRPC and consumers — belongs in app, not here.
func Middleware(...func(http.Handler) http.Handler) Option

// Router replaces the default chi-backed router with another RouterAdapter,
// such as gin.Adapter() or echo.Adapter().
func Router(RouterAdapter) Option

// ReadTimeout sets the maximum duration for reading an entire request.
func ReadTimeout(time.Duration) Option

// Raw is the escape hatch: it hands the underlying *chi.Mux to fn so that code
// the port does not model can register directly. It is an explicit opt-out, not
// the default path.
func Raw(fn func(mux *chi.Mux)) Option

// RouterAdapter is the swappable router implementation behind the
// HTTPRegistrar. chi is the default; gin.Adapter(), echo.Adapter(), and the
// lossier Fiber adapter return implementations of this type.
type RouterAdapter interface {
	// warren.md names this type but does not fix its method set.
	// See Open questions.
}
```

**Usage** (§4.1, §10):

```go
http.Server(
	http.Port(cfg.HTTPPort),
	http.Middleware(cors.Default().Handler, middleware.RealIP),
	http.ReadTimeout(10*time.Second),
)
```

### Invariant 3 and the `net/http` signature

Invariant 3 forbids a driver type in a public signature and names `*chi.Mux`
explicitly. `Middleware` takes `func(http.Handler) http.Handler` anyway, and
this is not a violation: `net/http` is the standard library, not a driver, and
§4.1 fixes that signature precisely so that ecosystem middleware works
unmodified. The driver type is `*chi.Mux`, and it appears in exactly one place —
inside the named escape hatch `http.Raw`.

The consequence is stated rather than hidden: **`Middleware` is
`net/http`-shaped, so a router that is not `net/http`-compatible cannot honour
it.** Fiber therefore gets a lossier adapter and a documented caveat, because
fasthttp is not `net/http`-compatible (§4.1).

## Behaviour

**Boot** (§1.3). Registration happens at step 5: controllers build the route
table in memory, with edge and core middleware already composed into closures.
The lifecycle hook runs at step 6, in dependency order — pool → repos →
consumers → **servers** — so the listener opens only after everything it depends
on is started. Readiness flips green at step 7; the process serves at step 8.

**Request path** (§1.4, §10):

```
chi → edge middleware (CORS, auth, correlation ID)
    → decode JSON → validate → app.Handler
        → core middleware (trace, transaction, metrics)
            → Handle()
    ← Res  → encode
    ← *Error → status from the table below
```

The adapter owns, and the handler never sees:

- **decode** — request body, path parameters, and query into `Req`.
- **validate** — `warren/validate` runs automatically after decode; a bad
  request never reaches `Handle` (§2.7). Failures surface as `CodeInvalid` with
  per-field detail and are encoded by the mapping below.
- **encode** — `Res` to JSON.
- **status mapping** — the HTTP column of §2.6, below.
- **edge middleware** — CORS, correlation ID, auth guards (§1.4, §7.2).
- **context seeding** — the transport adapters seed the logging context, so
  `log.FromContext(ctx)` already carries trace, span, and correlation IDs and
  handlers never construct a logger (§2.5).

**Health endpoints** (§2.8). `/healthz` (liveness) and `/readyz` (readiness,
gated by lifecycle state) are HTTP endpoints served from the `warren/health`
registry, into which adapters self-register their checks. The kernel cannot know
HTTP exists (§1.1), so this package serves them; warren.md does not say through
what API it reads the registry — see Open questions.

**Shutdown** (§1.3 step 9–10, §2.3). Readiness closes *first* so the load
balancer drains before anything stops. Only then does this server stop
accepting; in-flight requests finish. Pools and broker connections close after
that. A force-exit deadline (default 30s, owned by `lifecycle`) bounds the whole
sequence.

**Must never**: consult the DI container per request (invariant 7); run
reflection per request; map an error itself in handler code; import
`transport/grpc`, `broker/*`, or `persistence/*` (invariant 4); leak `*chi.Mux`
through any signature other than `Raw`.

## Error mapping

The HTTP column of §2.6. Domain code returns `errors.Conflict(...)`; this table
is the only place it becomes a status code.

| Code | HTTP |
|---|---|
| `INVALID` | 400 |
| `NOT_FOUND` | 404 |
| `CONFLICT` | 409 |
| `UNAUTHENTICATED` | 401 |
| `PERMISSION_DENIED` | 403 |
| `UNAVAILABLE` | 503 |
| `INTERNAL` | 500 |

§2.6 now carries rows for `UNAUTHENTICATED` (401) and `PERMISSION_DENIED` (403),
with the rule that `UNAUTHENTICATED` describes the caller's identity, not the
service's — downstream auth failure is `UNAVAILABLE`. See Open question 6,
resolved.

## Escape hatch

```go
http.Raw(func(mux *chi.Mux) { /* ... */ })
```

`*chi.Mux` is a driver type, so invariant 3 keeps it out of every other
signature. `Raw` is the single named opt-out: a user who reaches for it has
chosen chi deliberately and accepts that `http.Router(gin.Adapter())` will not
work alongside it. Making raw access the default path would make the router
un-swappable, which is the property the Wrap mode exists to protect.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing). The
  server is exercised through `httptest` with an in-process listener; the drain
  path is tested against a synchronised handler, never a `time.Sleep`.
- **Integration tests behind `//go:build integration`** — anything binding a real
  port or crossing a process boundary.
- **Golden-file tests for error text.** Every status/body pair produced by the
  mapping table, and every validation-failure body, gets a golden file. Untested
  error text rots immediately.
- **Allocation benchmark on the request path.** Invariant 7 is a performance
  claim and needs a number: a benchmark asserts a fixed allocation count per
  request and that the DI container is not consulted.
- `t.Parallel()`, table-driven subtests named for behaviour.

## Definition of done

1. `Server`, `Port`, `Middleware`, `Router`, `ReadTimeout`, and `Raw` implemented
   with the signatures above, each with a doc comment starting with its
   identifier.
2. `transport.HTTPRegistrar` implemented over chi; a controller registering
   `Post`/`Get` serves requests end to end.
3. Decode → validate → handler → encode wired, with `warren/validate` invoked
   automatically after decode.
4. Every row of the HTTP column mapped, with golden files.
5. `/healthz` and `/readyz` served from the health registry, `/readyz` gated by
   lifecycle state.
6. Lifecycle hook registered with the §2.3 ordering; a drain test proves
   in-flight requests finish after readiness closes.
7. Allocation benchmark committed and passing.
8. `go.mod` for the module contains the core module and `go-chi/chi/v5` and
   nothing else that is not stdlib.
9. No `*chi.Mux` in any exported signature except `Raw`.
10. This spec corrected in the same pull request wherever the implementation
    diverged.

## Open questions

For the human. warren.md does not answer these, and none should be guessed.

1. **`RouterAdapter`'s method set.** §4.1 names the type and says
   `gin.Adapter()` / `echo.Adapter()` return one, but gives no interface. What
   must a router implement? This is a port shape, so it is the human's call.
2. **Where do the Gin, Echo, and Fiber adapters live?** §1.6 lists only
   `warren/transport/http` as a module. Sub-packages of this module, or separate
   modules? Separate modules keep the chi dependency out of a Gin user's
   `go.sum`; sub-packages do not.
3. **What is the Fiber caveat, exactly?** §4.1 says the adapter is "lossier" and
   the caveat is "documented", without saying what is lost. Presumably
   `Middleware` — but that must be stated, not inferred.
4. **Success status code.** §1.4 shows `200 JSON`; §10 shows `encode 201` for
   `POST /users`. What rule chooses, and is it configurable per route?
5. **Error response body.** The status mapping is fixed; the JSON body is not
   specified anywhere — neither the envelope, nor how `WithDetail` details and
   per-field validation errors are rendered. Every golden file depends on this.
6. **RESOLVED (2026-08-01):** warren.md §2.6 gained the rows: `UNAUTHENTICATED`
   → 401 and `PERMISSION_DENIED` → 403, plus the rule that `UNAUTHENTICATED`
   describes the caller's identity, not the service's (downstream auth failure
   is `UNAVAILABLE`). **`UNAUTHENTICATED` and `PERMISSION_DENIED` have no row in §2.6** yet exist in
   `warren/errors` and are reachable through `auth.RequireScope` (§7.2). 401 and
   403 are the obvious answers but are not written down; warren.md needs the rows
   added.
7. **How does this package read the health registry?** §2.8 puts `/healthz` and
   `/readyz` under kernel `warren/health`, but the kernel has no knowledge that
   HTTP exists (§1.1). The exposure API is unstated.
8. **Defaults.** No default is given for `Port`, `ReadTimeout`, write/idle
   timeouts, max header bytes, or the per-hook drain timeout. §10 uses `8080`
   and `config.Config` defaults `http_port` to `8080`; that is an example, not a
   stated default.
9. **TLS.** §4.2 gives gRPC `grpc.TLS(certFile, keyFile)`; §4.1 gives HTTP no TLS
   option at all. Deliberate (terminate at the ingress) or an omission?
10. **`§1.7` lists `chi` as a *direct* dependency of an HTTP-only service's
    `go.mod`,** alongside `dig` and `validator`. Both contradict the Wrap
    boundary — invariant 2 says users never import dig, and §4.1 says the router
    is swappable behind a port. Are these meant as "appears in `go.sum`" /
    indirect entries?
