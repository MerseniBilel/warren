# Warren

**A DDD-first application framework and CLI for Go backends.**

> ⚠️ **Pre-release, v0.1 in progress.** Most of the framework is importable
> and works today — use cases, errors, domain, config, DI, lifecycle, the
> module system, boot step 5, the consumer chain, the transactional outbox,
> the persistence and transport ports, health, validation, the CLI, and
> **`transport/http`**, which serves a real HTTP service over
> `net/http.ServeMux` and adds nothing to your `go.mod` but itself, and
> **`persistence/postgres`**, whose unit of work commits aggregate state and
> the outbox rows for that aggregate's events in one transaction. What is
> **not** in v0.1: `openapi`, `auth` (the JWT/OIDC **verifier** — the identity
> type and the policies ship in `app`), `transport/grpc`,
> `broker/rabbitmq`, `broker/nats`, and the Mongo/Redis/MySQL drivers — each
> deferred to v0.2 **with the reason recorded in its own spec**, not left as
> an open question. The short version: `openapi`'s architecture is now ruled
> and its spec approved — it is a pure add-on over a route table frozen in
> v0.1, so `go get` gets it in v0.2 with no migration —
> `auth` needs two dependency audits that have not been run, and a third
> broker driver answers nothing that `broker/memory` plus the shared contract
> suite does not.
> Two planned modules are not on that list at all any more. `resilience` was
> **dropped** — retry and timeout are core-ring and ship, while a breaker
> guards an outbound call Warren does not make. `jobs` was **dropped** too: a
> scheduler is an ordinary `lifecycle.Hook`, which starts after its
> dependencies and is joined before them by construction, and `outbox.Elector`
> already gives leader-only — **but note that one elector is one advisory
> lock.** A field test wired a scheduler and the outbox relay to the same one;
> whichever goroutine woke first took it and the other silently did nothing
> for the life of the process. That is refused with a diagnostic now, and
> leader-only work that must run alongside the relay belongs inside the
> function the relay leads with, until Warren exports a second elector.
> The repository is being rebuilt spec-first: every
> package gets an approved `SPEC.md` before its first line of Go, retired once
> the package is implemented and reviewed. [warren.md](warren.md) is the
> design; [AGENT.md](AGENT.md) is the rules.
>
> **New here? Start with [GETTING_STARTED.md](GETTING_STARTED.md)** — a
> complete service, from nothing to a running HTTP API, in one page.

---

## What Warren is

Four claims define it — everything in this repository exists to protect one of
them:

1. **Transport-agnostic use cases.** One `app.Handler[Req, Res]`; HTTP, gRPC,
   and message consumers are thin adapters over it. A handler imports no
   transport package — *no `net/http`, no `pgx`, no `kgo`. That is the entire
   point.*
2. **Real module encapsulation.** A provider is private to its module unless
   exported, and imports are explicit — not one global container where
   everything sees everything.
3. **DDD as real types**, not folder naming conventions — aggregates, events,
   and the transactional outbox as compiler-checked constructs.
4. **Architecture enforced in CI.** `warren lint arch` fails the build when
   `domain/` imports `infrastructure/` — for your project and for Warren's own
   repository, same command.

Warren is **not** a web framework, an ORM, or a deployment platform. It
composes existing routers and drivers behind stable ports, and its dependency
budget is defensible: the kernel is standard library + `dig`, permanently.

```
TOOLING     warren/cli — templates · AST editor · analyzer     build-time only
ADAPTERS    transport/http · transport/grpc · broker/kafka     separate modules,
            persistence/postgres · observability · …           never import each other
CONTRACTS   app.Handler · broker.Publisher · Registrar · …     ports & shared types
KERNEL      warren · di · lifecycle · config · log · errors    stdlib + dig only
```

One line of `main.go` swaps Kafka for RabbitMQ. One handler serves three
protocols. Every error the framework can detect surfaces at boot — never on
request 1.

---

## Status & roadmap

Progress is spec-first: ☑ means *done and verified*, not *started*.

### Phase 0 — foundation

- [x] Package manifest written ([warren.md](warren.md)) and repository reset to it
- [x] Rules rewritten ([AGENT.md](AGENT.md), [CLAUDE.md](CLAUDE.md))
- [x] All 32 packages scaffolded with a `SPEC.md` each
- [x] Every spec audited against the manifest — no invented API survived
- [x] Design contradictions found and catalogued (25 specs blocked on them)
- [x] Core decisions taken: config `Source` split, auth-code DLQ rows,
      `Root[K]` constraint, concrete registrars on Go 1.27
- [ ] Remaining decisions folded into their specs and re-approved
- [x] Tooling rebuilt: Makefile, CI workflow, `golangci` config, module-rules
      check (`scripts/invariants.sh`)
- [ ] Dependency audits run (`dig` first) — no library enters a `go.mod` without one

### Phase 1 — kernel (buildable on Go 1.26, in dependency order)

All seven implemented packages were adversarially reviewed on 2026-08-01
(31 reproduced findings across two review rounds, all fixed with regression
tests) and their specs retired — the code, tests, golden files, and warren.md entries are the
contract now.

- [x] `errors` — the semantic vocabulary; load-bearing for everything
      *(implemented; spec retired)*
- [x] `domain` — `Entity`, `Root[K]`, `AggregateRoot`, `Event`; the §3.1
      example compiles as a test *(implemented; spec retired)*
- [x] `log` — context-carried logger, Vendor mode, exported seeding surface
      *(implemented; spec retired)*
- [x] `di` — the container wrap; the golden diagnostic reproduces byte for
      byte; dig v1.19.0 audited *(implemented; spec retired)*
- [x] `lifecycle` — ordered start/stop, `Ready()` readiness gate
      *(implemented; spec retired)*
- [x] `config` (core) — Source-split loading: Load, Source, env, flags
      *(implemented; spec retired — `Module[T]` lands with the root package)*
- [ ] `config/yaml` — the first file Source *(v0.2; needs its own spec + YAML
      library audit before the module exists)*
- [x] `validate/playground` — the full tag vocabulary (`email`, `min`, `oneof`,
      …) as its own module, with every tag checked AT BOOT so a typo is a
      diagnostic rather than a production panic *(implemented. Core refuses
      those tags by design and its diagnostic told users to install this —
      a promise in shipped runtime output that CI was asserting on)*
- [x] `warren` (root) — module system, boot sequence, run loop
      *(implemented with `config.Module[T]`; adversarially reviewed — 8
      findings fixed — spec retired)*
- [x] `app` core — `Handler`/`HandlerFunc`/`Middleware`/`Chain` *(implemented;
      a five-middleware chain adds 0 allocs; §10 handler compiles verbatim)*
- [x] `app` built-in middleware — `Retrying`/`Traced`/`Metered`/`Authorized`
      *(implemented over the app-owned ports: RetryPolicy,
      AuthorizationPolicy, context-carried Telemetry)*
- [x] `app.Transactional` — over the one-method `app.UnitOfWork` port
      *(implemented; the app spec is retired)*
- [x] `broker` port + consumer chain — envelope, Pipeline (Recover/Drain/
      TraceExtract/Deduplicate/DeadLetter/Retry/ConcurrencyLimit), options
      *(implemented; §2.6 disposition table one test per code)*
- [x] `inbox` — dedupe-store port + stdlib memory store *(implemented)*

### Phase 2 — transport

- [x] `transport` (port) — sealed `Registrar`, generic free functions, route
      table of pre-built closures *(implemented on Go 1.26 — the "Fix A"
      shape; the 1.27 method form is a mechanical call-site rewrite)*
- [ ] Bump toolchain to Go 1.27; verify generic methods compile as designed
      *(and that inference works — explicit type arguments are needed today)*
- [x] `warren g repository --driver postgres` — plain SQL over `postgres.DB`
      carrying the three rules no compiler enforces, plus the table's
      migration and a `cmd/migrate` binary *(CI compiles the generated
      repository; the migrate path was run against a real Postgres)*
- [x] `warren new` scaffolds a service that **serves**: a controller
      registering `POST /users`, `whttp.Server` wired in `main.go`, health
      probes, and `log.Handler` installed so every record carries the
      correlation ID *(the scaffold's own compile test builds and runs it)*
- [x] `transport/http` — the HTTP error column, health probes, the edge ring,
      drain-before-stop *(implemented on **`net/http.ServeMux`**, not chi:
      the sealed `Registrar` already discards everything a router is bought
      for, and chi measured worst of five candidates on this project's own
      first priority. Zero third-party dependencies; 17 allocations per
      request, asserted by a test)*
- [ ] `transport/grpc` — **deferred to v0.2**, and the reasons are decided
      rather than open: a handler's `Req` must stay a plain Go struct or the
      HTTP adapter mis-encodes the same handler, so the wire needs generated
      proto messages and a *generated* shim between them — which needs
      `warren g proto`, the harder of the two artifacts. A proto codec over
      plain structs was prototyped, measured (faster than JSON) and rejected:
      no reflection descriptor, and field numbers in Go struct tags. The round
      found **zero required changes to core `transport`**
- [ ] Fallback if 1.27 slips: generic free functions (compiles on 1.26; call
      sites change shape)

### Phase 3 — messaging

- [ ] Outbox/inbox ownership decisions (writer split, leader election, module map)
- [x] `broker/memory` — in-process driver, default in tests *(implemented;
      passes the exported `broker/brokertest` contract suite)*
- [x] `outbox` — writer port, relay, elector, memory store *(implemented;
      the SQL store and advisory-lock elector land with postgres)*
- [x] `inbox` — dedupe store, on by default *(port + memory store shipped
      with the broker chain)*
- [x] `broker/kafka` — franz-go driver: one client, one group, in-process
      fan-out, mark-the-prefix offsets, and publish errors carrying the code
      the outbox relay switches on *(implemented; **at-least-once plus inbox
      dedupe, not exactly-once** — §5.1 claimed otherwise and §5.5 was right.
      4 third-party modules, the smallest adapter footprint after
      `transport/http`'s zero)*
- [ ] `broker/rabbitmq`, `broker/nats` — after their manifest entries are written

### Phase 4 — persistence

- [x] `persistence` (port) — `Repository`, `UnitOfWork`, the Track/Collect
      enlistment seam, in-process driver + contract suite *(implemented)*
- [x] `persistence/postgres` — the `UnitOfWork`, `postgres.DB`, the outbox
      store with `LISTEN`/`NOTIFY`, an advisory-lock elector, a durable inbox,
      and plain-SQL migrations *(implemented; passes `persistence.RunContract`
      unmodified against a real Postgres. **Never migrates at boot** — that
      races every replica of a rolling deploy. One third-party dependency:
      `pgx`; goose rejected)*
- [ ] `persistence/mongo`, `persistence/redis` — after their manifest entries
      are written *(`mysql`: deferred — exists only in a heading)*

### Phase 5 — cross-cutting

- [x] Core policy ports decided: `RetryPolicy`/`AuthorizationPolicy`/
      `Telemetry` live in `app`, telemetry rides the context
- [x] `observability` — OTel wiring: handlers, HTTP and broker propagation
      instrumented by one import, composed at BOOT so the request path decides
      nothing *(implemented; DB spans need one explicit `postgres.Configure`
      line. 24 third-party modules, confined here by an invariant — a service
      that does not import it pays nothing)*
- [ ] `validate` — port in core, implementation in a submodule
- [x] `health` — check registry, liveness/readiness verdicts, root-scope
      binding *(implemented; the routes land with the transport adapters)*
- [x] `warren/testing` (`warrentest`) — boot a module with fakes, Invoke by
      type, AssertPublished, Golden *(implemented; stdlib + core only)*
- [x] `app.Identity` — the identity seam, policies and the 401/403 split (v0.1)
- [x] `app.Timeout` + the resilience ruling: module DROPPED, not deferred (v0.1)
- [ ] `auth` (verifier), `openapi`

### Phase 6 — the CLI *(the discovery engine: scaffolding real apps is how
### weaknesses get found)*

- [x] `warren new` — a scaffold that compiles and tests against today's
      framework, with the CI gate that builds it (the anti-rot mechanism)
- [x] `warren version`; core tagged v0.1.0 so a scaffold's go.mod resolves
- [x] `warren g module|entity|command|repository|consumer` — golden-file
      tested, idempotent, stdlib AST editing (no `dst`: it has published no
      releases and sat untouched through Go 1.19–1.27), and everything the
      five write compiles, vets and passes its own tests in a real project
      on every CI run
- [x] `warren lint arch` — the layer rule and the cross-module rule, read
      from the import graph; works on a project that does not compile; runs
      in Warren's own CI over Warren, same binary *(`--rules=rings` next)*
- [ ] v0.2+: `doctor`, `graph`, `explain di`, `templates eject`
- [ ] v0.3+: `extract module`, `add <adapter>`, `migrate layout`

---

## Repository map

| Where | What |
|---|---|
| [warren.md](warren.md) | The package manifest — one entry per package, source of truth |
| [AGENT.md](AGENT.md) | Invariants, conventions, and process — canonical for humans and agents |
| `<package>/SPEC.md` | The contract of a package **not yet implemented**; approved before any code, retired once the package ships |
| [docs/assets/](docs/assets/) | Usage-flow diagrams for the approved specs |

## Contributing

Read [AGENT.md](AGENT.md) first — the spec-first process, the dependency-audit
rule, and the invariants apply to every change. No feature is implemented
before its spec is approved, and no dependency is adopted without a written
audit.

## License

Apache-2.0 — see [LICENSE](LICENSE).
