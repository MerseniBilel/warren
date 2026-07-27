# Warren Architecture

This describes how Warren itself is built. For how a *service built with*
Warren is laid out, see PRD §5.

The decisions here are recorded in [docs/adr/](adr/); this document is the map,
not the reasoning. Where the two disagree, the ADR wins.

---

## 1. The one idea

Everything below follows from a single commitment:

> **A use case is written once and knows nothing about how it is invoked.**

```go
type Handler[Req any, Res any] interface {
    Handle(ctx context.Context, req Req) (Res, error)
}
```

HTTP, gRPC, CLI, and message consumers are adapters over this type. The adapter
owns its concerns entirely — HTTP owns status codes and content negotiation,
gRPC owns proto marshalling, the consumer owns acks, retries, and DLQ routing.
The handler owns none of them and imports none of them.

If a change would make a handler import a transport package, the change is
wrong. This is the invariant the rest of the architecture exists to protect.

---

## 2. Module graph

Multi-module, with a core that has zero third-party dependencies
([ADR-0003](adr/0003-repo-layout.md)). Dependencies point **inward**; nothing in
the core knows a driver exists.

```
                        ┌──────────────────────────────┐
                        │   core: warren               │
                        │   (standard library only)    │
                        │                              │
                        │   di · lifecycle · log       │
                        │   errors · domain · app      │
                        │   health · config-port       │
                        └──────────────▲───────────────┘
                                       │  implements ports / consumes core
             ┌─────────────────────────┼─────────────────────────┐
             │                         │                         │
    ┌────────┴────────┐       ┌────────┴────────┐       ┌────────┴────────┐
    │  transport/*    │       │   broker/*      │       │  persistence/*  │
    │  http · grpc    │       │  kafka · nats   │       │  postgres · …   │
    │  (chi, echo…)   │       │  memory · …     │       │  (pgx, goose)   │
    └─────────────────┘       └─────────────────┘       └─────────────────┘
             │                         │                         │
             └─────────────────────────┼─────────────────────────┘
                                       │
                     ┌─────────────────┴─────────────────┐
                     │  config · validate · observability │
                     │  auth · resilience · jobs · testing│
                     │  cli · mcp                         │
                     └────────────────────────────────────┘
```

**Ports live one level above drivers.** `warren/broker` defines `Publisher` and
`Subscriber` and depends on nothing. `warren/broker/kafka` implements them. A
user can write and test against the port without pulling any driver — which is
also why the in-memory broker is the default in tests.

### The core's rule

The core module imports nothing outside the standard library, permanently.
`make lint-modules` fails the build otherwise. When a core feature appears to
need a library, the feature is split: the port goes in core, the implementation
goes in a submodule.

This is what makes the claim "a minimal Warren service pulls almost nothing"
demonstrable rather than aspirational.

---

## 3. The dependency rule

Inside a service's bounded context, four layers, and dependencies point one way:

```
   interfaces ─────▶ application ─────▶ domain
        │                  │               ▲
        └──────────────────┴───────────────┘
   infrastructure ─────────────────────────┘   (implements domain ports)

   domain imports NOTHING from the other three.
```

| Layer | Holds | May import |
|---|---|---|
| `domain` | Entities, value objects, domain events, repository **interfaces**, domain services | Nothing outside itself and the shared kernel |
| `application` | Commands, queries, handlers, DTOs, ports | `domain` |
| `infrastructure` | Repository implementations, publishers, external clients | `domain`, `application` |
| `interfaces` | HTTP controllers, gRPC services, consumers | `domain`, `application` |
| `module.go` | Wiring | All four — it is the only file that may |

`warren lint arch` enforces this and exits non-zero on violation
([ADR-0004](adr/0004-architecture-enforcement.md)). Rules are configurable in
`warren.yaml`: a team wanting a looser layout writes an explicit override, which
shows up in code review — rather than the rule quietly not existing.

**Warren dogfoods this.** The rules apply to this repository too, including
"only `warren/di` imports `dig`" ([ADR-0001](adr/0001-dependency-injection.md)).
If Warren's own CI cannot run `warren lint arch` against Warren, the feature is
not finished.

---

## 4. Ports and adapters

Every external system reaches Warren through a port defined in core or in a
port module, and every driver is an adapter behind it.

| Port | Defined in | Adapters |
|---|---|---|
| `Handler[Req, Res]` | `app` | HTTP, gRPC, CLI, consumers |
| `Repository[T, ID]` | `persistence` | postgres, mysql, mongo |
| `UnitOfWork` | `persistence` | per driver |
| `Publisher` / `Subscriber` | `broker` | kafka, rabbitmq, nats, memory |
| `Router` | `transport/http` | chi, stdlib, echo, gin |
| `Config` | `config` | koanf-backed loader |

### The rule that keeps drivers swappable

**No driver type may appear in a Warren public signature.** Not `*dig.Container`,
not `*chi.Mux`, not `*pgx.Conn`, not `*kgo.Client`. This is checked in review
and, where a linter can see it, by `depguard`.

The inverse rule matters just as much: **the raw client is always reachable**
through an explicit escape hatch (PRD §4.1 principle 4). Abstraction that cannot
be escaped is a prison, and users will vendor around it.

### HTTP specifically

The HTTP port is shaped on `net/http`, because that is the contract the
ecosystem actually shares ([ADR-0002](adr/0002-http-router-port.md)). chi is the
default; stdlib `ServeMux` is a zero-dependency option; Echo and Gin are
supported. Fiber is fasthttp-based and therefore cannot satisfy an
`http.Handler` port — its adapter is separate and community-owned.

Middleware, route groups, and mounting are **Warren's**, not the router's,
because they must behave identically across HTTP, gRPC, and consumers.

---

## 5. Lifecycle

Warren owns application lifecycle rather than delegating it — which is the main
reason `fx` was rejected ([ADR-0001](adr/0001-dependency-injection.md)).

```
build container  →  validate graph  →  OnStart (ordered)  →  serving
                                                                │
                     drain ◀── stop accepting ◀── signal ───────┘
                       │
                       └─▶ OnStop (reverse order)  →  exit
```

**Graph validation happens before anything starts.** A missing provider kills
the process at boot, not on the first request (PRD §4.1 principle 2). This is
`dig`'s dry-run mode plus an invoke of every root.

**Shutdown drains before it stops.** Consumers finish in-flight messages, the
HTTP server stops accepting and completes open requests, and `OnStop` hooks run
in reverse start order. Readiness flips to failing at the start of drain, so a
load balancer stops routing before the listener closes.

---

## 6. Errors

One error type with a semantic code (PRD §4.5). Each transport maps it into its
own vocabulary:

| Semantic code | HTTP | gRPC | Consumer |
|---|---|---|---|
| `NotFound` | 404 | `NOT_FOUND` | ack, log |
| `Conflict` | 409 | `ALREADY_EXISTS` | ack, log |
| `Invalid` | 400 | `INVALID_ARGUMENT` | DLQ |
| `PermissionDenied` | 403 | `PERMISSION_DENIED` | DLQ |
| `Internal` | 500 | `INTERNAL` | nack, retry |

Domain code returns semantic errors and knows nothing about 404. The
`exhaustive` linter is enabled on these switches, so a new code that an adapter
forgets to map fails the build rather than falling through to a 500.

---

## 7. What this architecture forbids

Stated explicitly, because the useful part of a design is what it rules out:

- A handler that imports `net/http`, `grpc`, or a broker client.
- A domain package that imports a driver, an ORM, or a transport.
- A driver type in a Warren public signature.
- A third-party import in the core module.
- Cross-module reach into another module's internals — modules communicate by
  published events and generated clients only. This is what makes
  `warren extract module` possible at all.
- A committed `replace` directive.
- Reflection in the request path. Reflection belongs to container construction
  at boot; the hot path is generated or explicit.

---

## 8. Reading order

| To understand | Read |
|---|---|
| Why these dependencies | [dependencies.md](dependencies.md) |
| Why `dig`, and the swap plan | [ADR-0001](adr/0001-dependency-injection.md) |
| Why `net/http`, and why not Fiber | [ADR-0002](adr/0002-http-router-port.md) |
| Why multi-module, and its costs | [ADR-0003](adr/0003-repo-layout.md) |
| How the arch rule is enforced | [ADR-0004](adr/0004-architecture-enforcement.md) |
| How to test each layer | [testing.md](testing.md) |
| How agents drive the CLI | [agent-integration.md](agent-integration.md) |
