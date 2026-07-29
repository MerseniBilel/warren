# Warren Roadmap

Six milestones. Week numbers assume one maintainer — treat them as ordering,
not as dates.

**Nothing here is built yet.** Each item becomes a spec before it becomes code;
see [AGENT.md](../AGENT.md) § *Spec-driven development*.

Two rules shape the order, and both are easier to state now than to defend
later:

- **v0.1 is dogfooded on a real service before v0.2 starts.** Domain primitives
  designed against an imagined service are wrong in ways only usage reveals.
- **v0.4 does not slip.** Everything before it is table stakes that Kratos and
  go-zero already ship. The reason to choose Warren arrives in v0.4, so moving
  v0.1 scope into v0.2 is not a neutral trade.

---

## v0.1 — Skeleton · weeks 1–6

> **Goal: the author can build a real service with it.**

Build order. Each item depends only on what precedes it; `errors` is first
because every other package returns its types and retrofitting an error model is
a rewrite of every signature that touches one.

- [x] **`errors`** — one type, semantic codes, `Wrapping`/`Field`/`Op`/`Fix`
      builders · [spec](../errors/SPEC.md)
- [x] **`log`** — context plumbing over `log/slog`, no wrapper type ·
      [spec](../log/SPEC.md)
- [x] **`di`** — `Provide[T]` · `Supply[T]` · `Contribute[T]` · `Resolve[T]` ·
      `Group[T]` · `Validate` · `Build` · `Graph` · [spec](../di/SPEC.md).
      `Scope` moved to v0.2 — see [di/SPEC.md §11.2](../di/SPEC.md): its only real
      consumer is transaction propagation, and designing it before that is decided
      is designing against a guess
- [ ] **`lifecycle`** — ordered start, reverse stop, drain, grace period
- [ ] **`config`** — loader port in core, koanf implementation in a submodule
- [ ] **`app`** — `Handler[Req, Res]`, `Middleware`, `Chain`, logging/recovery/timeout
- [ ] **`warren`** — `New`, `Module`, `Run`; the boot sequence in one place
- [ ] **`transport/http`** — port, contract suite, then the chi and stdlib adapters
- [ ] **`cli` foundation** — generator engine, golden-file harness, skill generation
- [ ] **`warren new`** — 0 → running service in under 2 minutes
- [ ] **`warren g module`** — generate a module and wire it into `main.go` by AST edit

**Exit criteria — v0.2 does not start until all are true:**

- [ ] A real service runs on it in production
- [ ] `warren new` → running service in **under 2 minutes**, measured on a cold machine
- [ ] `warren new` → first endpoint with a passing test in **under 10 minutes**
- [ ] Framework startup overhead **under 50 ms**, with a committed benchmark
- [ ] A missing provider prints the resolution chain, the requesting file, and a copy-pasteable fix — verified by a golden test on the message itself
- [ ] Both generators have golden-file tests and skills
- [ ] `make ci` green on Linux, macOS, and Windows
- [ ] The core module still has zero third-party dependencies
- [ ] Every documented concept has a runnable, CI-compiled example

---

## v0.2 — DDD core · weeks 7–12

> **Goal: DDD primitives are real types, not a folder convention.**

- [ ] Domain primitives — `Entity`, `ValueObject`, `AggregateRoot`, `Event`, `Specification[T]`
- [ ] In-process event bus — the default a broker later replaces without the handler changing
- [ ] `Repository[T, ID]` and `UnitOfWork` ports, with their contract suites
- [ ] `di.Scope` — request-scoped resolution, decided **with** transaction
      propagation rather than before it (deferred from v0.1; `di/SPEC.md §11.2`)
- [ ] Postgres driver — `pgx`, transaction propagation, the outbox table
- [ ] Migrations — `goose` as a library, `warren g migration`
- [ ] Validation, called by transport adapters before the handler
- [ ] Command and query buses with decorators
- [ ] Generators: `g entity` · `g value-object` · `g event` · `g command`/`g query` · `g repository`
- [ ] Echo and Gin adapters — the test of whether the HTTP port is genuinely router-agnostic
- [ ] `--layout simple` — a module with no domain layer, for teams that want CRUD first

**The hard requirement:** `UnitOfWork` commits state and the outbox atomically.
That constrains the transaction-propagation design, so it is decided first, not
last.

---

## v0.3 — gRPC and messaging · weeks 13–20

> **Goal: the transport-agnostic claim is proved, not asserted.**

Until a second and third transport exist, "write a use case once, expose it
three ways" is an intention. If `app.Handler` has to change to accommodate gRPC
or a consumer, **that is the finding** — and far better learned here than at
v1.0.

- [ ] gRPC transport — handler-first, optional proto generation
- [ ] One `Middleware` value applying across HTTP, gRPC, and consumers
- [ ] Broker ports — `Publisher`, `Subscriber`, `Message` envelope
- [ ] In-memory broker **first** — it defines the contract suite Kafka must pass
- [ ] Kafka driver — `franz-go`, consumer groups, drain on shutdown
- [ ] Broker middleware — retry, DLQ, idempotency, tracing, concurrency limits
- [ ] Transactional outbox and inbox
- [ ] `warren g consumer`, `warren g proto`, `cmd/worker` entrypoint

---

## v0.4 — Governance · weeks 21–26

> **The differentiators land here. Do not defer them.**

Nothing else in the Go ecosystem enforces architecture in CI. Everything before
this milestone is table stakes; the risk to manage is not technical, it is that
v0.1–v0.3 overrun and Warren ends up a slightly different Kratos.

- [ ] **`warren lint arch`** — the moat. Correct with zero configuration, or it gets turned off in week two
- [ ] `warren doctor` — convention drift, missing wiring, dead providers
- [ ] `warren graph modules` · `graph di` · `graph events`
- [ ] `warren explain di` — trace how one dependency resolves
- [ ] OpenAPI 3.1 generation from handler and DTO metadata
- [ ] OpenTelemetry wired across every transport and broker **by default**
- [ ] Testing harness — boot a module in isolation, `assert.EventPublished[T]`
- [ ] MCP server — read-mostly resources over the project's structure

---

## v0.5 — Ecosystem · weeks 27–36

> **Goal: Warren is usable by people who are not its author.**

The question underneath this milestone is whether the ports settled in v0.1–v0.3
survive contact with a second and third driver. Every new driver runs the
existing contract suite **unmodified** — if a driver needs the suite changed,
the port is wrong.

- [ ] Drivers: RabbitMQ, NATS, Mongo, MySQL, Redis
- [ ] Auth — JWT/OIDC guards, RBAC policy hooks
- [ ] Resilience — circuit breaker, retry, rate limit, bulkhead, timeout
- [ ] Jobs — cron and background workers on the same lifecycle
- [ ] `warren extract module` — lift a module into its own repository
- [ ] Presets and `warren dev`
- [ ] Documentation site — must support including code from files
- [ ] Driver certification checklist for community-owned drivers

---

## v1.0 — Stability

> **Goal: the API is frozen and the project is safe to depend on.**

v1.0 is a promise, not a release: after it, a breaking change costs a major
version and every user's upgrade becomes a project of its own.

- [ ] API freeze — every exported identifier reviewed for whether it should be exported at all
- [ ] Semantic-versioning commitment and a stated deprecation window
- [ ] `warren upgrade` — migrates templates and config, rather than overwriting
- [ ] Benchmark suite, so the performance targets become regressions rather than memories
- [ ] Three or more production adopters, not counting the author's
- [ ] Complete API reference, every concept carrying a runnable example
- [ ] Governance — a single-maintainer v1.0 is a promise one person cannot keep

---

## Not on the roadmap

Recorded so the decision is visible rather than forgotten:

- **Event sourcing.** Post-1.0 at the earliest, and only with an adopter asking
  for it. It risks defining the project as niche.
- **Annotation-based route codegen.** The thing most likely to make a Go
  developer close the tab. Explicit registration ships first and stays the
  primary path regardless.
- **A Fiber adapter.** `fasthttp` cannot implement an `http.Handler`-shaped
  port. Community-owned if it happens at all.
- **A deployment story.** That is Encore's game.
