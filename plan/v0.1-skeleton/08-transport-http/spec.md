# Spec: HTTP transport

| | |
|---|---|
| **Module** | Port in `warren/transport/http`; adapters in `warren/transport/http/{chi,stdlib}` |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [07-app-handler](../07-app-handler/spec.md), [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md), [04-lifecycle](../04-lifecycle/spec.md), [01-errors](../01-errors/spec.md) |
| **Blocks** | [10-cli-new](../10-cli-new/spec.md) |
| **PRD** | §4.3, §4.5, §6.2, §6.6 |
| **ADRs** | [ADR-0002](../../../docs/adr/0002-http-router-port.md) |
| **Date** | 2026-07-28 |

---

## 1. Problem

This is the first adapter, and it either proves
[`app.Handler`](../07-app-handler/spec.md) or disproves it. A use case has to
become an HTTP endpoint without the use case knowing what HTTP is — including
status codes, content negotiation, and request decoding.

The user's constraint (their words): they must be able to choose their HTTP
framework. ADR-0002 settles how: **the port is shaped on `net/http`**, chi is
the default, stdlib `ServeMux` is first-class, Echo and Gin are supported, and
Fiber is community-owned because `fasthttp` cannot satisfy a `net/http` port.

## 2. Goals

1. **Bind a `Handler[Req, Res]` to a method and path**, with decoding,
   validation, invocation, and encoding handled by the adapter.
2. **Map every `errors.Code` to a status**, exhaustively — a new code that an
   adapter forgets fails the build, not the request.
3. **Swap routers with a `go.mod` line and one bootstrap argument.** No handler
   changes, because no handler ever saw the router.
4. **`*http.Request` and `http.ResponseWriter` are always reachable** (PRD §4.1
   principle 4).
5. **Graceful shutdown**: stop accepting, drain in-flight requests, then close —
   as a lifecycle stop hook.
6. **Any `net/http` middleware from the last decade works**, from any adapter,
   with no shim.

## 3. Non-goals

- **Warren does not write an HTTP stack** (PRD §1.3). It composes routers.
- **No Echo or Gin adapter at v0.1.** chi and stdlib prove the port is
  router-agnostic; a third adapter only re-proves it. v0.2.
- **No Fiber.** `fasthttp` is not `net/http`; ADR-0002 makes it community-owned
  and out of the v0.x support matrix.
- **No OpenAPI generation.** v0.4.
- **No annotation-based route codegen.** PRD §13.3 — explicit registration
  ships, and stays primary regardless of what is decided later.

## 4. Public API

The **port**, in `warren/transport/http` — depends on core only:

```go
package http

// Registrar is what a controller uses to declare routes. Warren owns chaining,
// groups, and mounting above the router (ADR-0002); the router is asked only
// to match a method and path.
type Registrar interface {
    Method(method, pattern string, h stdhttp.Handler)
    Group(prefix string, fn func(Registrar))
    Use(mw ...func(stdhttp.Handler) stdhttp.Handler)
}

// Convenience wrappers over Method.
func Get(r Registrar, pattern string, h stdhttp.Handler)
func Post(r Registrar, pattern string, h stdhttp.Handler)
func Put(r Registrar, pattern string, h stdhttp.Handler)
func Patch(r Registrar, pattern string, h stdhttp.Handler)
func Delete(r Registrar, pattern string, h stdhttp.Handler)

// Handle adapts an app.Handler to an http.Handler: decode, validate, invoke,
// encode. This is the bridge, and it is the only place a transport concern
// meets a use case.
func Handle[Req, Res any](h app.Handler[Req, Res], opts ...HandleOption) stdhttp.Handler

// Router is what an adapter implements. Warren ships chi and stdlib; a user can
// implement this for any net/http-compatible router.
type Router interface {
    Registrar
    ServeHTTP(w stdhttp.ResponseWriter, r *stdhttp.Request)
}

// Server is the module contributed to warren.New.
func Server(opts ...ServerOption) warren.Option

func Port(n int) ServerOption
func Addr(s string) ServerOption
func WithRouter(r Router) ServerOption     // default: chi
func ReadTimeout(d time.Duration) ServerOption
func WriteTimeout(d time.Duration) ServerOption
func ShutdownTimeout(d time.Duration) ServerOption

// StatusFor maps a semantic error code to an HTTP status. The switch is
// exhaustive with no default, so a new code fails the build here.
func StatusFor(c errors.Code) int

// Escape hatches. PRD §4.1 principle 4: no abstraction is a prison.
func RequestFrom(ctx context.Context) *stdhttp.Request
```

The **adapters**, one module each:

```go
package chi    // warren/transport/http/chi   — depends on go-chi/chi/v5
func New() http.Router

package stdlib // warren/transport/http/stdlib — standard library only
func New() http.Router
```

**No `chi` type appears in the port** (invariant 2). `chi.Mux` is reachable
only by importing the chi adapter directly, which is the deliberate escape
hatch, not the default path.

## 5. Behaviour

- **Request flow**: decode `Req` from body, path, and query → validate → invoke
  the handler → encode `Res`. Each step is replaceable through `HandleOption`,
  because a framework that cannot serve a non-JSON endpoint is not usable.
- **Decoding** is JSON by default, with path parameters bound by struct tag and
  query parameters likewise. `Req` of type `struct{}` skips decoding entirely.
- **Validation** runs before the handler, through `warren/validate` when it is
  present. A validation failure returns `CodeInvalid` and never reaches the
  handler.
- **Status mapping** via `StatusFor`, from the error's semantic code
  ([01-errors](../01-errors/spec.md)). The switch has no `default`; the
  `exhaustive` linter is enabled in [`.golangci.yml`](../../../.golangci.yml)
  precisely for this switch.
- **Success statuses**: 200, or 201 when the route is a `Post` and the handler
  returns a non-zero `Res`. Overridable per route — implicit is convenient and
  occasionally wrong.
- **Error responses** are a stable JSON body: `code`, `message`, and `fields`.
  Internal errors log the full detail and return a generic message with the
  correlation ID; leaking an internal error's text to a client is an information
  disclosure, and returning nothing traceable is unusable in support.
- **A correlation ID** is read from the inbound header or generated, put into
  the context via [`warren/log`](../02-log/spec.md), and echoed in the response
  header.
- **Lifecycle**: `OnStart` binds the listener and returns only once it is
  accepting — so a failed bind is a boot failure, not a silent goroutine death.
  `OnStop` calls `Shutdown` with the configured deadline, then `Close`.
- **The listener binds after every start hook has run**, per
  [06 §5](../06-module-and-bootstrap/spec.md) step 6.
- **Panics** are recovered per request by `app.Recovery`, applied visibly in
  generated code rather than silently by the framework.

## 6. Errors

The status mapping table — this is a public contract, so it belongs in the spec
rather than being discovered in the source:

| `errors.Code` | Status |
|---|---|
| `CodeInvalid` | 400 |
| `CodeUnauthenticated` | 401 |
| `CodePermissionDenied` | 403 |
| `CodeNotFound` | 404 |
| `CodeConflict` | 409 |
| `CodeFailedPrecondition` | 412 |
| `CodeDeadlineExceeded` | 504 |
| `CodeUnavailable` | 503 |
| `CodeUnimplemented` | 501 |
| `CodeInternal` | 500 |

Failure modes of the transport itself:

| Condition | Code | Message |
|---|---|---|
| Port already in use | `CodeUnavailable` | The address, and the `lsof -i :8080` line to find the holder |
| Body is not valid JSON | `CodeInvalid` | The offset and what was expected — never the raw body, which may hold credentials |
| Required path parameter missing from the pattern | `CodeInternal` at boot | The route, the struct field expecting it, and the corrected pattern. A wiring bug, caught at boot |
| Two routes with the same method and pattern | `CodeInvalid` at boot | Both controllers and their files |
| Shutdown deadline exceeded | `CodeDeadlineExceeded` | The count of in-flight requests still open, and how to raise the deadline |

## 7. Configuration

| Option | Default |
|---|---|
| `Port` | 8080 |
| `ReadTimeout` | 15s |
| `WriteTimeout` | 15s |
| `ShutdownTimeout` | 25s (inside lifecycle's 30s total) |
| `WithRouter` | chi |

Defaults are set, not zero: an `http.Server` with no timeouts is the most
common Go production footgun, and shipping it as a default would be inexcusable
in a framework that exists to encode good structure.

## 8. Testing

- **Contract suite for `Router`**, run by both adapters, in the same package.
  AGENT.md: a port change updates the contract suite first, then the drivers.
  Covers method matching, path parameters, wildcards, precedence, groups,
  mounting, and middleware order.
- **Adapter equivalence**: the same controller mounted on chi and on stdlib
  produces byte-identical responses for the whole suite. This is the "swap
  routers with a go.mod line" claim, tested.
- **Status mapping**: every `errors.Code`, table-driven, plus a compile-time
  assertion that the switch is exhaustive.
- **Decode failures**: malformed JSON, wrong types, missing required fields,
  an oversized body.
- **Graceful shutdown**: a request in flight when `Stop` is called completes;
  a new connection is refused. Uses a controllable handler, not `time.Sleep`.
- **Bind failure at boot** produces the §6 message and no partially-started app.
- **`net/http` middleware compatibility**: a third-party-shaped
  `func(http.Handler) http.Handler` works unmodified on both adapters.
- **Integration (`//go:build integration`)**: a real listener, a real client,
  keep-alive reuse, and shutdown under load.
- **Benchmark**: framework overhead per request over a bare `http.HandlerFunc`,
  so the "we compose, we do not rewrite" claim is measurable.

## 9. Invariants touched

- **Invariant 1** — the port module depends on core only; chi lives in the chi
  adapter. The stdlib adapter takes no third-party dependency at all, which is
  what makes "a Warren service with zero HTTP dependencies" true rather than
  marketing.
- **Invariant 2** — no `chi.Mux` in the port, verified by an import test.
- **Invariant 4** — a controller declares routes; a handler never sees one.

## 10. Definition of done

- [ ] Port and both adapters match §4
- [ ] Contract suite written **before** either adapter, and both pass it
- [ ] Adapter equivalence test green
- [ ] Exhaustive status mapping, with a deliberately non-exhaustive switch confirmed to fail the build
- [ ] Unit tests per §8, `-race -shuffle=on`
- [ ] Integration tests behind the build tag
- [ ] Committed overhead benchmark
- [ ] chi audit row confirmed current in `docs/dependencies.md`
- [ ] `make ci` green
- [ ] `docs/` guide: "Choose an HTTP router", including what Fiber costs
- [ ] Runnable example in `examples/http/` — one handler, both routers
- [ ] Changelog fragment

## 11. Open questions

1. **Does `Handle[Req, Res]` decode by reflection or by generated code?** PRD
   §4.1 principle 3 prefers codegen for anything the developer reads, and
   reflection for the container. Decoding is in the request path, so reflection
   costs per request — but a generated decoder per DTO is a lot of generated
   code. **Measure both** against the per-request overhead benchmark before
   choosing.
2. **Where does content negotiation live?** JSON-only at v0.1 is honest and will
   not survive first contact with a user who needs protobuf over HTTP. Design
   `HandleOption` so a codec is pluggable, but ship one codec.
3. **Is 201-on-POST-with-a-body too clever?** It is right most of the time and
   wrong silently. The alternative is explicit status per route, which is more
   typing in generated code. Decide from the dogfooding service.
4. **Should `RequestFrom(ctx)` exist at all?** It is the escape hatch PRD §4.1
   principle 4 demands, and it is also exactly the door through which a use case
   starts importing `net/http` — the thing invariant 4 forbids. Leaning: it
   exists, it is documented as for middleware and controllers only, and
   `warren lint arch` (v0.4) flags its use inside `application/`.
