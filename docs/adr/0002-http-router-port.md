# ADR-0002: HTTP router port shaped on `net/http`, chi as the default

- **Status:** Accepted
- **Date:** 2026-07-27
- **Relates to:** PRD §6.2, §6.6, §4.1 principle 4

## Context

Warren must let users choose their HTTP router, and must recommend one. That
requires a port every candidate router can satisfy.

Audited 2026-07-27 (see [dependencies.md §3.3](../dependencies.md)):

| Router | Engine | `http.Handler` compatible |
|---|---|---|
| `net/http.ServeMux` | `net/http` | yes |
| `go-chi/chi` v5.3.1 | `net/http` | yes |
| `labstack/echo` v5.3.1 | `net/http` | yes |
| `gin-gonic/gin` v1.12.0 | `net/http` | yes |
| `gofiber/fiber` v3.4.0 | **fasthttp** | **no** |

**The constraint.** Fiber is built on `valyala/fasthttp`, which deliberately
does not implement `net/http` interfaces. A Fiber handler is not an
`http.Handler`. No `net/http` middleware works with it.

So there are two possible port shapes:

1. Shape the port on `net/http`. Four of five routers satisfy it natively, and
   the entire Go middleware ecosystem — OpenTelemetry instrumentation, auth
   libraries, whatever the user already owns — works unchanged. Fiber needs a
   separate adapter.
2. Invent a neutral request/response abstraction that both engines can satisfy.
   Every router then needs a shim, users lose access to `*http.Request`, and
   third-party middleware stops working for everyone.

Option 2 breaks PRD §4.1 principle 4: `*http.Request` must always be reachable.

## Decision

**The HTTP port is shaped on `net/http`.** A Warren HTTP adapter registers
`http.Handler` values and receives `*http.Request`. `net/http` is the contract.

**chi is the default adapter**, used by `warren new` unless told otherwise.

**Stdlib `ServeMux` is a first-class adapter** for services that want zero HTTP
dependencies.

**Echo and Gin are supported adapters**, each its own module.

**Fiber is a community-owned adapter** that re-implements the chain against
fasthttp. It is documented as not sharing the `net/http` middleware ecosystem
and is not in the v0.x support matrix.

Warren owns middleware chaining, route groups, and mounting **above** the
router, not through it — because those must behave identically for HTTP, gRPC,
and message consumers (PRD §4.2). The router is asked to do one thing: match a
method and path to a handler.

## Consequences

### What this buys

- Any `net/http` middleware written in the last decade works with Warren, from
  any adapter, with no shim.
- The stdlib adapter is genuinely viable. Go 1.22 gave `ServeMux` method
  matching, `{wildcard}` and `{path...}` patterns, `r.PathValue`, and defined
  precedence. Its documented gaps — middleware chaining, groups, sub-routers —
  are exactly the three things Warren supplies itself. A Warren service can ship
  with no HTTP dependency at all.
- Swapping routers is a `go.mod` line and one bootstrap argument. Handlers do
  not change, because handlers never saw the router.

### What this costs

- Fiber users are second-class. Their adapter is separate, their middleware is
  separate, and it is community-maintained. This is stated plainly in the docs
  rather than discovered.
- We give up fasthttp's allocation profile as a default. For the services Warren
  targets (PRD §3.4), router allocation is not the bottleneck; a database round
  trip is.

### What we now cannot do

- We cannot claim uniform middleware across all five routers. Four, plus a
  caveated fifth.

## Alternatives considered

**Neutral request abstraction covering fasthttp** — rejected above. It taxes the
95% case to serve the 5%, and it violates the escape-hatch principle.

**Gin as the default** — 88,980 stars against chi's 22,585, and the name most
newcomers know. Rejected: it carries a real dependency tree where chi carries
none, and it introduces `*gin.Context` as a parallel context type, which is
precisely the coupling Warren exists to prevent. Its 733 open issues against
chi's 112 also suggests a different maintenance posture.

**Stdlib `ServeMux` as the default** — seriously considered, and it is a
supported adapter for this reason. Rejected as *default* because route groups
and sub-router mounting are ergonomics that generated code benefits from
immediately, and chi costs nothing to add: zero dependencies, ~1,000 lines,
100% `net/http`.

**Echo as the default** — a close second, actively released, 28 open issues.
chi wins on dependency weight and on being a router rather than a framework;
Warren already is the framework.

## Revisit when

- chi is archived or stops releasing for 12 months.
- Stdlib `ServeMux` gains group/mount support, at which point the default should
  become stdlib and chi becomes optional.
- Users report that Fiber's second-class status is blocking real adoption.
