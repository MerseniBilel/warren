# `warren/transport/http` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §4.1](../../warren.md) |
| **Module** | own module (`warren/transport/http`) |
| **Mode** | Wrap (router swappable) |
| **Wraps** | `net/http` + `go-chi/chi/v5` |

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
