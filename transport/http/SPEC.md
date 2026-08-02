# `warren/transport/http` — SPEC

| | |
|---|---|
| **Status** | **APPROVED AND IMPLEMENTED (2026-08-02)** — architect round complete, both ⚠ blocking questions ruled on, boot step 5 built in core, the adapter shipped with golden files and an allocation test. See **Divergences** below for what the implementation changed. |
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

**Boot diagnostics get better** — with a caveat the previous draft got wrong,
recorded under "Corrections to the previous draft" below. And invariant 3 stops
being strained: with no driver type in the package, handing out an
`*http.ServeMux` would not even be an escape from an invariant.

**Given up, and the spec says so:** regex path constraints (`validate:` covers
the intent), `Mount`/sub-router composition, replaceable `NotFound` handler
objects, and chi's ordering helpers.

**Consequences:** `RouterAdapter` and `Router(...)` are deleted — a swap
mechanism protecting against a cost the sealed port already eliminated. The
Gin/Echo/Fiber adapters become a non-goal. warren.md §9 loses its
`HTTP | go-chi/chi/v5 | Wrap` row and §4.1's Surface block loses `Router`.

### ⚠1 — RULED: boot step 5 is built, in core, with an empty Table provided at step 2

**The problem.** `app.go` instantiates controllers and consumers and never calls
`Register` on any of them; `resolveDynamic` (`app.go:399`) discards the resolved
value, so no instance is even available to register. warren.md §2.1 admits it.
This package's definition of done is therefore unreachable without a public-API
change to the core `warren` package.

**The ruling.** Build it, with three corrections to the proposal as first
drafted:

1. **The Table is provided EMPTY at boot step 2, and filled at step 5.** Boot
   step 3 (`root.Validate()`, `app.go:200`) walks every provider's inputs before
   anything is instantiated. An adapter constructor that injects
   `*transport.Table` must resolve *then* — long before any controller has
   registered. Providing the Table after registration would fail every HTTP
   application at step 3. The pointer is the seam: provided empty, filled in
   place.

2. **`Builder.Table()` cannot be reused, because it allocates.** Core
   `transport` gains `Builder.Fill(*Table) error`, and `Table()` becomes
   `Fill` into a fresh one.

3. **Step 4 and step 5 become separate passes over `ordered`.** Today one loop
   resolves controllers, resolves eager types, and appends hooks per module. An
   adapter's server is an eager singleton in its own module; resolved in the
   same pass it would see a table that later modules have not registered into
   yet. Passes: instantiate controllers/consumers (keeping the values) →
   register them all into one Builder → `Fill` → instantiate eager singletons
   and append hooks → `Unserved()`.

   A free win: adapter hooks now append after every domain hook, so
   `pool → repos → consumers → servers` (§1.3) holds *by construction* on the
   way up and reverses correctly on the way down, instead of by accident of the
   order the user listed modules in `warren.New`.

**`Controllers(...any)` takes `any`, so the bootstrapper type-asserts and fails
the boot naming the type and its declaration site.** Today a controller with a
typo'd `Register` signature registers nothing, silently, and every one of its
routes 404s in production — the exact failure §1.3 exists to prevent. The
assertion is mandatory, not opt-in: a `Controllers` entry that is not a
controller is meaningless, and `warren.Eager[T]()` already covers
"instantiate this at boot, it registers nothing".

**The ring direction is a deliberate carve-out, not an oversight.** `warren` →
`transport` is KERNEL → CONTRACTS, which §1.1 forbids. The root `warren`
package is the kernel's **composition root** — the one package whose job is to
see everything and wire it — and step 5 is not expressible without it. AGENT.md
gains the carve-out in the same change, worded to bind: *the root `warren`
package may import the contracts ring; no other kernel package may.*
`scripts/invariants.sh` gains the grep that enforces the second half. No import
cycle results: `transport` imports `app`, `broker`, `errors`, `validate`, none
of which import `warren`.

**Core surface added by this ruling** (also recorded in warren.md §2.1 and §3.5):

```go
// package transport

// Fill freezes the accumulated registrations into t — the Table the
// bootstrapper provided in the root scope at boot step 2, before the graph was
// validated, so that an adapter could inject it. Every registration problem is
// reported together, so one boot names them all.
func (b *Builder) Fill(t *Table) error

// package warren

// Validator sets the validator whose rules are compiled into every route
// closure at boot step 5. The default is validate.Required(). It must be
// called before Start.
//
// It is the reachable form of the fix transport's own diagnostic already
// promises: "transport.WithValidator(validate.None())" names a Builder that,
// until now, only the bootstrapper held.
func (a *App) Validator(v validate.Validator) error
```

### ⚠2 — RULED: byte-in/byte-out is the only typed route in v0.1, with TWO escape doors

`Invoker` is `func(ctx, []byte) ([]byte, error)`: whole body in memory, whole
response in memory. Right for commands, wrong for multipart upload, file
download, SSE, WebSocket upgrade and NDJSON export — the same hole as
`transport/grpc` open question 2. **Widening `Invoker` is the wrong fix**: it
taxes the 99% path for the 1%, and it cannot be designed without answering gRPC
client/server streaming and broker batching at the same time. That is a v2 port.
Deferring is right.

**But `Handle(pattern, h)` as an `Option` alone does not close the hole**, and
this is what the previous draft missed. An `Option` is evaluated in `main.go`,
so the handler must be constructed *there* — outside every module scope. A file
upload handler needs the module's storage port and its repository, and those are
private to the module by design (§1.2). The escape hatch as drafted could not
reach the dependencies of the only use case it exists for. That, not the absence
of typed multipart, is what would sink adoption.

**Two doors, and the controller-side one is primary.** Core `transport` gains
one untyped registration; the adapter serves it.

```go
// package transport

// Raw registers a protocol-native handler — the escape hatch for what the
// typed byte-in/byte-out port deliberately does not model: multipart upload,
// file download, SSE, WebSocket upgrade, NDJSON export.
//
// h is opaque to core: the kernel and the contracts ring never import
// net/http, so the adapter serving p type-asserts it (warren/transport/http
// asserts http.Handler) and fails the boot — naming the route and the concrete
// type — when the assertion fails. It travels through the sealed Registrar so
// that the handler is built by the MODULE's own container, with the module's
// own private providers. That is the whole reason it is not an adapter option.
func Raw(r Registrar, p Protocol, pattern string, h any, opts ...RouteOption)

// RawRoute is one escape-hatch registration.
type RawRoute struct {
	Protocol Protocol
	Pattern  string // the adapter's own syntax — "POST /uploads" for ServeMux
	Name     string
	Guards   []app.AuthorizationPolicy
	Handler  any
}

// Raw returns the escape-hatch registrations.
func (t *Table) Raw() []RawRoute
```

`Unserved()` counts `RawRoute`s toward each protocol's total: a raw HTTP route
in an application with no HTTP server is the same production 404 as a typed one.

**What a raw route gets:** the edge ring in full — recover, correlation-ID and
logger seeding, telemetry, user `Middleware(...)` in argument order — plus its
`transport.Guard` policies (they are `app.AuthorizationPolicy`, driver-free, and
run before the handler), the drain, and `DrainDelay`.

**What it does not get, stated so nobody is surprised:** decode, param binding,
`validate`, encode, `transport.Status` defaults, the error table (the handler
writes its own status and body), the `MaxBodyBytes` limit (uploads are the
point — the handler owns its own `http.MaxBytesReader`), the allocation budget,
and any future `warren/openapi` entry.

**The second door, `http.Handle(pattern, h) Option`, stays** for the genuinely
dependency-free cases that belong in `main`: `net/http/pprof`, static assets, a
vendor SDK's webhook receiver.

**And `http.Raw(func(mux *http.ServeMux))` is deleted, not renamed.** The
previous draft carried it over from the chi design, where `*chi.Mux` had a large
API the port did not model. `*http.ServeMux`'s entire API is `Handle`,
`HandleFunc`, `Handler` and `ServeHTTP` — so a mux escape hatch would buy
exactly what `Handle` already gives, at the cost of a third door and a
registration order nobody can reason about. One concept fewer.

### Corrections to the previous draft's `net/http` claims

Verified on go1.26.3, darwin/arm64. Each of these changes a golden file, so they
are load-bearing.

**The trailing-slash redirect is 307, not 301.** `net/http/server.go` uses
`StatusTemporaryRedirect` in all three redirect paths. Measured: `GET /dir`
against pattern `/dir/` → **307**, `Location: /dir/`; path cleaning (`//exact`,
`/a/../exact`) → **307**. And the redirect is narrower than "trailing slash"
suggests: `GET /exact/` against pattern `GET /exact` is a plain **404**, no
redirect. Golden files assert 307 and assert the 404 on the suffix case.

**404 and 405 still cost nothing, but the previous draft's mechanism was
backwards.** `ServeMux` **already** returns 405 with a correct `Allow`, computed
for free:

```
DELETE /users/42  ->  405  Allow: GET, HEAD   Pattern: ""
GET    /users     ->  405  Allow: POST        Pattern: ""
```

Registering a method-less shim pattern per path **destroys** that: `GET /users`
then yields `405` with an empty `Allow` and `Pattern: "/users"`. So the shim is
needed only to render the JSON envelope, and **it must compute and write `Allow`
itself** — including the implicit `HEAD` that every `GET` pattern provides.
`r.Pattern` being the method-less pattern is how the shim knows it is the shim.
Also verified: `OPTIONS /users` is a 405, not an automatic 204 — `ServeMux` does
no preflight handling, so the shim must not swallow a CORS middleware's OPTIONS
response.

**`ServeMux` does not *refuse* a conflicting pattern — it panics.** The message
is excellent (both patterns, both registration `file:line`), but AGENT.md
§ General forbids panics in library code outside two named boot-time exceptions,
and this is not one. Registration is wrapped in a `recover()` and re-rendered as
a Warren boot diagnostic. Note the division of labour: the `Builder` already
catches duplicate `verb + pattern` at step 5; what `ServeMux` adds is `{id}` vs
`{uid}` overlap detection, which surfaces one step later, at adapter
construction.

**Confirmed correct, and measured:** method + wildcard patterns and `{$}`;
`GET` also serving `HEAD`; `Request.Pattern` set in place with no clone;
`Request.PathValue` at **0 allocations**; mux dispatch with two path parameters
at **2 allocations**; `http.Protocols` + `SetUnencryptedHTTP2` present with no
`golang.org/x/net`; and **`Server.Shutdown` does not cancel in-flight request
contexts** (measured: handler reported `NOT cancelled`, `Shutdown` blocked until
it returned). That last one belongs in the `ShutdownTimeout` doc comment, not
just here.

### Rulings on the rest

**Error body.** `{"error":{"code","message","details","correlation_id"}}`,
`application/json; charset=utf-8`. `code` is `errors.Code` verbatim — the
seven-value closed set, so clients can switch on it. **`INTERNAL` never renders
`Message()` and never renders the wrapped cause** — a fixed `"internal error"`
plus the correlation ID, with the real thing logged at ERROR; anything else
leaks DSNs and SQL to the internet. Validation failures are `INVALID` with one
`details` entry per field.

**`details` is a JSON object, ordered by `encoding/json`'s sorted keys — not
declaration order.** The previous draft promised declaration order; that is not
achievable. `errors.Error.Details()` returns a `map[string]any` and `validate`
fills it one `WithDetail(field, ...)` per field. A map has no order. Golden files
are deterministic because `encoding/json` sorts map keys — alphabetically.

Not RFC 9457 `problem+json`: `errors.Code` is not a URI.

**Success status.** Already decided in core (`transport.Status`, defaults 201 for
`Post`, **204 for `Delete`**, 200 otherwise); the adapter only reads it. Two
rules to add: `Success` is ignored on the error path, and a 204 route writes
**no body and no `Content-Type`**. (`transport.Status`'s own doc comment says
"200 for every other verb" and is stale against its own code; fixed in this
change.)

**204 still pays for an encode, and the spec says so rather than hiding it.**
`buildInvoker` always ends `return c.Encode(res)`, so a `Delete` marshals a body
the adapter discards. Fixing it means a "no body" flag on `HTTPRoute` and a
branch in a generic function — a core change with a cost on every other protocol.
Deferred, and carried in the allocation budget below as a named line item rather
than a surprise.

**Health probes are not routes.** `/healthz` and `/readyz` register directly on
the mux and **bypass the edge ring — except `recover`**, which stays, because a
panicking check must not kill the process that is answering the probe. No auth,
no rate limit, no tracing; a span every two seconds is a telemetry bill, not a
signal. They are served from the injected `health.Registry`, which boot already
provides in the root scope — no new core API is needed. `/readyz` returns 503
with a body naming the failing check and whether the gate is `starting` or
`draining`, both of which `health` already produces.

**Defaults.** `Port` 8080 · `ReadHeaderTimeout` **10s** (non-negotiable, the
Slowloris fix) · `ReadTimeout` 30s · `WriteTimeout` **0, deliberately** (a global
write timeout kills SSE and large downloads) · `IdleTimeout` 120s ·
`MaxHeaderBytes` 1 MiB · **`MaxBodyBytes` 1 MiB** (`Invoker` is `[]byte`-in; an
unbounded body is a one-request OOM) · **`DrainDelay` 5s** · `ShutdownTimeout`
15s, which must sit inside `lifecycle`'s 30s force-exit budget.

**Shutdown: warren.md's claim that readiness-closes-first is why rolling deploys
do not drop requests is not true as specified.** Readiness closing and the
listener stopping happen in immediate succession, but a load balancer polls
`/readyz` on an interval and keeps routing for up to one full poll period
afterwards. `DrainDelay` makes the stop hook wait, interruptibly, between the
two, so the balancer observes the 503 before the listener goes away. warren.md
§1.3 gains the step.

**Middleware order, fixed:** recover (outermost, not removable) → correlation-ID
and logger seeding → telemetry → user `Middleware(...)` in argument order → mux
dispatch → guards → decode → validate → core chain → handler. User middleware
cannot precede correlation-ID seeding or their log lines have none.
**Limitation, stated plainly: `Middleware` is global-only in v0.1**; "auth on
`/admin/*` only" is answered by `transport.Guard(policy)` per route, which runs
before decode — which is also why guards travel as data.

**TLS: `TLS(*tls.Config)` and `TLSFiles(cert, key)`.** "Terminate at the
ingress" is the user's decision, not the framework's; mTLS meshes and
single-binary deployments both need it. `*tls.Config` is stdlib, so invariant 3
is untouched, and it matches Kafka's option rather than gRPC's file pair.

**H2C: yes.** Since Go 1.24 `http.Protocols`/`SetUnencryptedHTTP2` needs no
`x/net`; verified on go1.26.3. Meshes speak it. TLS implies h2 already.

**`Listener(net.Listener)`, added.** The drain test in the definition of done
needs a real listener whose address the test knows, and `Port(0)` gives no way
to discover the bound port. `net.Listener` is standard library, it is one
option instead of an address-discovery API, and production never touches it.
`Addr(string)` is also added: binding `127.0.0.1` rather than `0.0.0.0` is a
real deployment decision that `Port(int)` alone cannot express. `Addr` amends
warren.md §4.1's surface.

**Allocation budget, committed:** ≤ 16 allocations and ≤ 640 B for a POST with a
JSON body and two path parameters — of which **seven are `encoding/json`'s
decoder**, ten are core's `Invoker` (measured), and one is the `Delete`-path
encode noted above. The reference point the benchmark also prints is the same
handler called directly: **0 allocations.** `TestAllocations` asserts an exact
count. Two rules with numbers behind them, written down before someone
"simplifies" them back: read the body into a **pooled `bytes.Buffer`**, never
`json.NewDecoder(r.Body)` and never `io.ReadAll`; and read query parameters by
**scanning `r.URL.RawQuery`**, never `r.URL.Query()` (measured: 4 allocations).

**Also corrected in this change:** §1.7's dependency budget claims an HTTP-only
service depends on `dig` and `validator` — `dig` is a direct dependency of
nobody's service (invariant 2) and `validator` is not in core at all. The true
row is **`warren`, `warren/transport/http` — two modules, zero third party**,
which is a headline the manifest was understating.

---

## Problem

A use case is an `app.Handler[Req, Res]` (§3.2). It must be reachable over HTTP
without importing `net/http`, without knowing that 404 exists, and without
choosing a router. Something has to turn a request into `Req`, run the handler,
turn `Res` into a response, and turn a `*Error` into a status code. That
something is this package, and it is the only place in an HTTP service where
`net/http` appears.

The package is a leaf in the ADAPTERS ring (§1.1): its own Go module, importing
only the core module, never another adapter.

## Goals

- Serve `transport.Table.HTTP()` — the routes a controller registered with
  `transport.Post(r, "/users", c.register)` — so that a handler which imports no
  transport package is reachable over HTTP.
- Own decode, validate-dispatch, encode, status mapping, and the edge middleware
  ring for HTTP (§1.4).
- Own the HTTP column of the error table (§2.6) so no handler ever maps a code to
  a status.
- Serve `transport.Table.Raw()`'s HTTP entries — the escape hatch for uploads,
  downloads, SSE and WebSocket upgrade.
- Participate in lifecycle with the ordering fixed in §1.3 and §2.3: start after
  dependencies are healthy, stop before pools close, drain in-flight requests
  after `DrainDelay`.

## Non-goals

- **Not a web framework.** Warren serves handlers over `net/http`; it does not
  compete with routers.
- **A swappable router.** Deleted by the ServeMux decision. Gin, Echo and Fiber
  adapters are a non-goal, and `RouterAdapter` does not exist.
- **No core middleware.** Transactions, retry, tracing, metrics, and
  authorization wrap `Handler[Req, Res]` and live in `app` (§3.2). This package
  owns the *edge* ring only — CORS, real-IP, correlation ID, auth guards (§1.4,
  §7.2). Conflating the two rings is the mistake that makes
  "transport-agnostic" frameworks leak.
- **No reflective dispatch and no container lookup on the request path**
  (invariant 7, as amended 2026-08-02). Route tables are built during boot
  step 5; by step 8 they hold pre-built closures. `encoding/json` and core's
  precomputed-index param binding do reflect per request, and invariant 7 says
  so — the claim is that no *decision* is made then.
- **No import of another adapter** (invariant 4).
- **Not the OpenAPI generator.** `warren/openapi` is a separate module (§4.3).
- **No typed streaming.** Ruled in ⚠2: v2, behind a port that satisfies gRPC
  streaming and broker batching too.

## Public API

```go
// Package http exposes Warren handlers over HTTP. It is the only package in an
// HTTP service that imports net/http, and it imports nothing else that is not
// the core module.
package http

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/MerseniBilel/warren"
)

// Server returns a warren.Module that serves the application's registered HTTP
// routes — transport.Table.HTTP() and the ProtocolHTTP entries of Table.Raw(),
// both frozen at boot step 5 — over a net/http.ServeMux.
//
// Its constructor claims transport.ProtocolHTTP, so an application that
// registers HTTP routes without this module fails the boot rather than 404ing
// in production. It registers a lifecycle hook: the listener opens after every
// dependency has started, and on shutdown it waits DrainDelay after readiness
// closed — so the load balancer observes the 503 before the listener goes
// away — and then calls Server.Shutdown.
func Server(opts ...Option) warren.Module

// Option configures the server.
type Option struct{ /* unexported */ }

// Port sets the TCP port the server listens on. The default is 8080.
func Port(n int) Option

// Addr sets the full listen address, "host:port", and overrides Port. The
// default host is empty — every interface. Bind "127.0.0.1:8080" to serve
// loopback only.
func Addr(addr string) Option

// Listener serves l instead of binding an address, and overrides Addr and
// Port. It exists so a test can bind port 0 and know the address; production
// code has no reason to reach for it.
func Listener(l net.Listener) Option

// Middleware appends edge middleware, in the standard library's
// func(http.Handler) http.Handler shape, so anything written for net/http —
// chi/v5/middleware included — runs unmodified. It applies to every typed and
// raw route, and to nothing else: the health probes bypass it.
//
// It runs after recover, correlation-ID seeding and telemetry, in argument
// order, and before mux dispatch. It is global in v0.1; per-route
// authorization is transport.Guard, which runs before decode.
//
// Core middleware — the kind that must also apply to gRPC and consumers —
// belongs in app, not here.
func Middleware(mw ...func(http.Handler) http.Handler) Option

// ReadHeaderTimeout bounds reading the request headers. The default is 10s.
// An unbounded header read is Slowloris.
func ReadHeaderTimeout(d time.Duration) Option

// ReadTimeout bounds reading the entire request. The default is 30s.
func ReadTimeout(d time.Duration) Option

// WriteTimeout bounds writing the response. The default is 0 — deliberately
// off, because a global write timeout kills SSE and large downloads. Set it
// per deployment when every route is a short command.
func WriteTimeout(d time.Duration) Option

// IdleTimeout bounds a keep-alive connection between requests. The default is
// 120s.
func IdleTimeout(d time.Duration) Option

// MaxHeaderBytes bounds the request headers. The default is 1 MiB.
func MaxHeaderBytes(n int) Option

// MaxBodyBytes bounds the request body of a typed route. The default is 1 MiB:
// transport.Invoker takes the whole body as a []byte, so an unbounded body is
// a one-request OOM. Raw routes are exempt and own their own limit.
func MaxBodyBytes(n int64) Option

// DrainDelay is how long the stop hook waits, interruptibly, between readiness
// closing and the listener stopping. The default is 5s: a load balancer polls
// /readyz on an interval and keeps routing for up to one full poll period
// after the 503.
func DrainDelay(d time.Duration) Option

// ShutdownTimeout bounds the whole stop hook — DrainDelay plus the drain. The
// default is 15s, which must stay inside lifecycle's 30s force-exit deadline.
//
// Server.Shutdown does not cancel in-flight request contexts, so a handler
// that ignores ctx blocks the drain until this expires.
func ShutdownTimeout(d time.Duration) Option

// TLS serves HTTPS with cfg. *tls.Config is standard library, not a driver
// type, and it is the option an mTLS mesh needs; TLSFiles is the two-file
// shorthand. TLS implies HTTP/2.
func TLS(cfg *tls.Config) Option

// TLSFiles serves HTTPS from a certificate and a key file.
func TLSFiles(certFile, keyFile string) Option

// H2C accepts unencrypted HTTP/2 alongside HTTP/1.1, through
// http.Protocols.SetUnencryptedHTTP2. Service meshes speak it, and since Go
// 1.24 it needs no golang.org/x/net.
func H2C() Option

// Handle registers h at pattern directly on the mux, for handlers that need no
// module-scoped dependency: net/http/pprof, static assets, a vendor SDK's
// webhook receiver. It gets the edge ring, the correlation ID and the drain,
// and no decode, validate or encode.
//
// A raw handler that needs a repository or a storage port is registered from
// the controller instead — transport.Raw(r, transport.ProtocolHTTP,
// "POST /uploads", h) — so that the module's own container builds it.
func Handle(pattern string, h http.Handler) Option
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

Invariant 3 forbids a driver type in a public signature. Under the ServeMux
decision this package has no driver type at all: `http.Handler`,
`*tls.Config` and `net.Listener` are the standard library, and §4.1 fixes
`Middleware`'s signature precisely so that ecosystem middleware works
unmodified. The strain the chi draft carried — a `*chi.Mux` escaping through
`Raw` — is gone with chi.

## Behaviour

**Boot** (§1.3). Registration happens at step 5: controllers build the route
table in memory, with core middleware already composed into closures. This
module's constructor injects the `*transport.Table` boot provided at step 2,
claims `ProtocolHTTP`, and builds the mux — every route a pre-built closure,
every pattern registered once. The lifecycle hook runs at step 6, in dependency
order — pool → repos → consumers → **servers** — so the listener opens only
after everything it depends on is started. Readiness flips green at step 7; the
process serves at step 8.

**Request path** (§1.4, §10):

```
recover → correlation ID + logger → telemetry → user Middleware
    → ServeMux dispatch
        → guards → decode JSON → bind params → validate → app.Handler
            → core middleware (trace, transaction, metrics)
                → Handle()
        ← Res   → encode, transport.Status
        ← *Error → the status table below
```

The adapter owns, and the handler never sees:

- **decode** — request body into `Req`, through a pooled `bytes.Buffer`.
- **params** — path values from `Request.PathValue`, query by scanning
  `RawQuery`; bound into `Req` by core's precomputed setters.
- **validate** — `warren/validate` runs automatically after decode; a bad
  request never reaches `Handle` (§2.7). Failures surface as `CodeInvalid` with
  per-field detail.
- **encode** — `Res` to JSON.
- **status mapping** — the HTTP column of §2.6, below.
- **edge middleware** — CORS, correlation ID, auth guards (§1.4, §7.2).
- **context seeding** — the transport adapters seed the logging context, so
  `log.FromContext(ctx)` already carries trace, span, and correlation IDs and
  handlers never construct a logger (§2.5).

**Health endpoints** (§2.8). `/healthz` (liveness — runs no checks) and
`/readyz` (readiness, gated by lifecycle state then critical checks) are served
from the injected `health.Registry`. They bypass every edge middleware except
recover.

**Shutdown** (§1.3, §2.3). Readiness closes first. `DrainDelay` then elapses,
interruptibly, so the load balancer observes the 503. Only then does this server
stop accepting; in-flight requests finish. Pools and broker connections close
after that. `ShutdownTimeout` bounds the hook; `lifecycle`'s 30s force-exit
deadline bounds the whole sequence.

**Must never**: consult the DI container per request (invariant 7); make a
reflective *decision* per request; map an error itself in handler code; import
`transport/grpc`, `broker/*`, or `persistence/*` (invariant 4).

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

A non-`*errors.Error` returned by a handler is an `INTERNAL` 500 with its text
logged, never rendered. `UNAUTHENTICATED` describes the caller's identity, not
the service's — a downstream auth failure is `UNAVAILABLE`.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing). The
  server is exercised through `httptest`; the drain path is tested against a
  synchronised handler and a `Listener(...)` on port 0, never a `time.Sleep`.
- **Integration tests behind `//go:build integration`** — anything crossing a
  process boundary.
- **Golden-file tests for error text.** Every status/body pair produced by the
  mapping table, every validation-failure body, the 404 and 405 envelopes, the
  307 redirect, and every boot diagnostic this package can produce.
- **Allocation test on the request path.** `TestAllocations` asserts an exact
  count for a POST with a JSON body and two path parameters, and the benchmark
  prints the direct-call reference of 0.
- `t.Parallel()`, table-driven subtests named for behaviour.

## Definition of done

1. Every function in the Public API section implemented with those signatures,
   each with a doc comment starting with its identifier.
2. Boot step 5 lands in core first: `Builder.Fill`, `transport.Raw`/`RawRoute`/
   `Table.Raw()`, `Unserved()` counting raw routes, the empty Table at step 2,
   the three-pass step 4/5/6, the `transport.Controller` assertion with its
   golden diagnostic, and `App.Validator`.
3. A controller registering `Post`/`Get` serves requests end to end, decode →
   bind → validate → handler → encode.
4. Every row of the HTTP column mapped, with golden files; plus the 404, 405
   (with a computed `Allow`), and 307 envelopes.
5. `/healthz` and `/readyz` served from the health registry, `/readyz` gated by
   lifecycle state, both bypassing the edge ring except recover.
6. `transport.Raw` HTTP routes served, with the edge ring and guards and without
   decode/validate/encode; a non-`http.Handler` fails the boot naming the route
   and the type.
7. Lifecycle hook registered with the §2.3 ordering; a drain test proves
   in-flight requests finish after readiness closes, and that `DrainDelay`
   elapses between the two.
8. `TestAllocations` committed and passing; the benchmark prints both numbers.
9. `go.mod` contains the core module and nothing else that is not stdlib.
10. `transport/http` added to the Makefile's `MODULES` list.
11. AGENT.md's composition-root carve-out and the `scripts/invariants.sh` grep
    landed with the core change.
12. warren.md corrected in the same change: §1.3 gains `DrainDelay`, §2.1 loses
    "step 5 arrives with the transport adapters", §4.1's Surface block loses
    `Router` and gains the real option list, §9 loses the chi row, §1.7's
    dependency budget corrected.
13. This spec corrected wherever the implementation diverged, and **retired**
    once the package is implemented and reviewed.

## Divergences — what the implementation changed, and why

Rule 4 of the spec process: where the code had to differ, the spec is
corrected in the same change rather than later.

**1. The allocation budget is 17, asserted at 18 — not the 16 the spec estimated.** The 16 was an estimate made before
the code existed. Measured on go1.26.3/darwin-arm64, with the breakdown
committed in `TestAllocations`:

| | allocs |
|---|---|
| `net/http.ServeMux` dispatch, one path wildcard | 2 |
| edge ring — ID string, response-header slice, correlation context value, and the `http.Request` clone `r.WithContext` makes so user middleware sees the ID | 6 |
| typed path — of which ~7 are `encoding/json`'s decoder | 10 |
| **total** | **17** (budget asserted at 18) |

The same handler called directly allocates **0**, and `BenchmarkHandlerDirect`
prints it beside `BenchmarkRequest` so the gap is never an adjective.

**2. The edge ring does NOT derive a per-request logger, and warren.md §2.5's
promise is not yet free.** The first implementation seeded
`log.With(ctx, "correlation_id", id)`. Measured, that one call costs **8
allocations** — more than the entire decode-validate-handle-encode path — and
it charges them to every request, including the ones that never log. It was
removed: the adapter seeds `log.WithCorrelationID` (2 allocations) and the
error and panic paths add the ID explicitly. §2.5 wants `log.FromContext(ctx)`
to carry it; the way to have that for free is a `slog.Handler` in `warren/log`
that reads the ID off the context at `Handle` time, which is where `slog` is
designed to do it. **Open question 6 below.**

Two smaller measured wins, recorded so nobody "simplifies" them back:
`CorrelationHeader` is spelled `X-Correlation-Id`, net/http's canonical form,
so `Get` and `Set` skip re-canonicalising the key on every request; and
`Content-Type` is written by assigning a package-level `[]string` straight
into the header map, which skips both the canonicalisation and the
one-element slice `Header.Set` allocates. `Content-Length` is left to
net/http, which computes it for any response it can buffer.

**3. `Addr(string)` and `Listener(net.Listener)` were both added** — the spec
anticipated `Listener`; `Addr` is the deployment decision (`127.0.0.1` versus
every interface) that `Port(int)` alone cannot express. Both amend
warren.md §4.1, which is corrected in the same change.

**4. A 405 renders `"code":"NOT_FOUND"`.** The code set is closed at seven
values so clients can switch on it, and none of them is
"method not allowed". `NOT_FOUND` is the honest member — the route as
addressed does not exist — and the `message` says which method was refused,
with `Allow` naming the ones that are not. Golden-tested rather than argued
about.

**5. `ErrorLog` is routed into `slog`**, which answers what was open question 5:
left unset, net/http writes connection-level errors (TLS handshake failures,
malformed requests) straight to stderr, bypassing the service's own log
stream.

**6. `make workspace` generates a git-ignored `go.work`.** A submodule cannot
resolve an untagged core module and invariant 8 forbids a committed `replace`,
so the Makefile generates the workspace and every compiling target depends on
it. `.gitignore` is new in this change and exists for exactly this.

## Open questions

Everything the previous draft asked has been answered — questions 1–3 dissolved
with `RouterAdapter`, 4 by `transport.Status`, 5 and 8–10 by the rulings above,
6 by warren.md §2.6's rows, and 7 by `health.Registry` already being in the root
scope. What genuinely remains:

1. **Adapter hook ordering is documented, not enforced.** §1.3 promises
   `consumers → servers`; the three-pass step 5 makes it true for adapters
   registered as eager singletons, but nothing stops a user appending a
   `lifecycle.Hook` later. A `lifecycle.Hook` phase field would enforce it —
   deferred, because it changes a kernel port.
2. **Who chooses the HTTP `Codec`.** `HTTPRoute.Bind(Codec)` is a seam with
   exactly one caller and no way for a user to supply a faster JSON codec per
   server. Deferred to whenever someone asks.
3. **Per-prefix middleware.** Global-only in v0.1; `Guard` covers authorization
   but not a rate limit or a CORS policy scoped to `/admin/*`. Not in v0.1.
4. **Do raw routes appear in `warren/openapi` (§4.3)?** For that spec to answer.
5. **RESOLVED** — `ErrorLog` goes to `slog`. See Divergences 5.
6. **`warren/log` should carry the correlation ID through a `slog.Handler`,
   not through a per-request derived logger.** warren.md §2.5 promises
   `log.FromContext(ctx)` already carries it; deriving it at the edge costs 8
   allocations on every request, including the ones that never log. A
   `slog.Handler` that reads the ID off the `ctx` it is already handed at
   `Handle` time makes the promise free. It is a kernel change, so it is the
   human's, and it is the last thing between this adapter and §2.5. See
   Divergences 2.
