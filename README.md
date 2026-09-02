# Warren

**A DDD-first application framework and CLI for Go backends.**

Write a use case once. Serve it over HTTP and over a message queue without
touching it — from one route table that `transport/grpc` reads too, when it
lands. Let CI fail the build when someone imports the database from the domain
layer. Warren is for Go teams who want a layered application and are tired of
the layering being a naming convention.

```
go install github.com/MerseniBilel/warren/cli/cmd/warren@latest
warren new myapp --module github.com/you/myapp
cd myapp && go mod tidy && go run ./cmd/myapp
```

That serves `POST /users`, `/healthz` and `/readyz` on `:8080`, with a
correlation ID on every log record. An evaluator who had never seen the project
went from `go install` to a real `201` in **under four minutes**, and ran
`warren lint arch` clean on the first try.

Here is the use case that route calls. Read what is **absent**:

```go
type WriteNote struct {
	ID   string `json:"id"   validate:"required"`
	Text string `json:"text" validate:"required"`
}

type NoteView struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func (h *writeNote) Handle(ctx context.Context, cmd WriteNote) (NoteView, error) {
	if _, err := h.notes.Find(ctx, cmd.ID); err == nil {
		return NoteView{}, errors.Conflict("note %s already exists", cmd.ID)
	}
	n := domain.Note{ID: cmd.ID, Text: cmd.Text}
	if err := h.notes.Save(ctx, n); err != nil {
		return NoteView{}, err
	}
	return NoteView{ID: n.ID, Text: n.Text}, nil
}
```

No `http.ResponseWriter`. No status code, no JSON encoding, no router, no
driver. `errors.Conflict` becomes **409** over HTTP, `AlreadyExists` over gRPC
and an ack on a queue, because one table owns that mapping and your handler is
not in it. Routing it is one line in a controller —
`r.Post("/notes", c.write)` — and `validate:` runs before `Handle` is ever
called.

Then the part that is hard to get from a router:

```
$ warren lint arch
No violations in 13 packages.

  Checked: the layer rule, the handler/transport rule and the handler/driver
  rule — each directly and through a helper package.
```

Same command, same binary, runs over Warren's own repository in Warren's own
CI. And **most wiring errors surface at boot rather than on request 1** — a
missing provider, an unexported dependency, a duplicate route, a path wildcard
no field binds, a middleware order that would corrupt a transaction — each one
refuses to start and names the file, the line and the fix. Two measured
exceptions are named below rather than denied. That diagnostic quality is the
deliverable the rest of this repository exists to protect.

> **Pre-release: v0.1 in progress.** The kernel, the ports, the CLI,
> `transport/http`, `persistence/postgres`, `broker/kafka`, `observability` and
> `validate/playground` are implemented and tagged; `openapi` is implemented
> but not yet tagged, so it resolves as a pseudo-version. `auth`'s verifier,
> `transport/grpc`, `broker/rabbitmq`, `broker/nats` and the Mongo, Redis and
> MySQL drivers are **deferred to v0.2**, each with its reason recorded in
> [warren.md](warren.md) rather than left as an open question. See
> [Status & roadmap](#status--roadmap) for what that costs you today.

**New here? Start with [GETTING_STARTED.md](GETTING_STARTED.md)** — a complete
service, from nothing to a running HTTP API, in one page. [warren.md](warren.md)
is the design; [AGENT.md](AGENT.md) is the rules.

---

## What Warren is

Four claims define it — everything in this repository exists to protect one of
them:

1. **Transport-agnostic use cases.** One `app.Handler[Req, Res]`; HTTP, gRPC,
   and message consumers are thin adapters over it. A handler imports no
   transport and no driver — *no `net/http`, no `pgx`, no `kgo`. That is the
   entire point.* Two rules with two different remedies — move the routing to
   the controller; declare the port in the domain — and `warren lint arch`
   checks both, directly and through a helper package.
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

One handler is written once and serves HTTP today, with gRPC and message
consumers reading the same route table — `transport/grpc` is the deferred half,
so "three protocols" is the design, not yet the present tense.

**Wiring errors surface at boot, not on request 1.** A missing provider, an
unexported dependency, a duplicate route, a path wildcard no field binds, a
field on a bodyless route that nothing can populate, a corrupting middleware
order — all of them refuse to start, with a diagnostic naming the file, the
line, and the fix. That is the claim, and it is deliberately narrower than the
one this line used to make. Two known gaps remain, both measured by a field
test and both scheduled rather than denied: a handler that injects a repository
but no unit of work is caught on request 1 (a 500, with the cause in the log),
and `warrentest.Invoke` boots the graph without the transport edge, so
`validate:` tags do not run under it. "Every error, never on request 1" was an
absolute with live exceptions, which is worse than a smaller true claim.

Swapping a driver is **one line of `platform`**, not of `main.go`, and how
many other lines depends on the driver's shape: a driver whose ports
`platform` can re-export (the in-process broker) is invisible to every
feature module, while one that must be its own module (`platform.Broker()`
for Kafka, `platform.Postgres()`) is imported by each feature that consumes
it — because a module may export only what its own providers return. This
line used to claim `main.go` and one edit; a field test diffed two scaffolds
and found N+1 files, none of them `main.go`.

---

## Install

The three commands at the top of this page are the whole setup. The generated
`go.mod` requires the published framework — core plus `transport/http`, and
`persistence/postgres` or `broker/kafka` when you ask for them — so
`go mod tidy` resolves it from the module proxy and there is no `replace`
anywhere.

To use the framework without the CLI, `go get github.com/MerseniBilel/warren`
and its adapters directly; [GETTING_STARTED.md](GETTING_STARTED.md) writes a
service by hand that way, one file at a time.

**Working on Warren itself** is the one case that needs more. Build the CLI
from your checkout and scaffold against it, so a change to the framework is
exercised by a real service before it is tagged:

```
cd cli && go build -o ~/.local/bin/warren ./cmd/warren
warren new myapp --module github.com/you/myapp --framework /path/to/warren
```

`--framework` writes `replace` directives into **the new project's** `go.mod`,
pinning it to your filesystem. That is why it is not the default, and it is
not a committed replace in this repository either (invariant 8).

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
- [x] Remaining decisions folded into their specs and re-approved *(every
      spec decided 2026-08-02: 12 approved and implemented, 10 deferred to
      v0.2, zero drafts left)*
- [x] Specs retired and the deferrals' reasons rehomed *(2026-09-02: the ten
      spec-only directories are gone and their rulings live in
      [warren.md](warren.md) — §4.2 for gRPC, §5.2/§5.3 for the brokers,
      §6.2–6.4 and §9 for the stores, §7.2 for auth. A directory holding a
      spec and no Go was seven half-built things to a visitor)*
- [x] Tooling rebuilt: Makefile, CI workflow, `golangci` config, module-rules
      check (`scripts/invariants.sh`)
- [x] Dependency audits run (`dig` first) — no library enters a `go.mod`
      without one *(dated audits in the [warren.md](warren.md) §9 ledger: dig
      v1.19.0, cobra v1.10.2, franz-go v1.21.5, playground v10.30.3, and the
      rejections — `dave/dst`, `robfig/cron`, `x/tools` — with their reasons)*

### Phase 1 — kernel (in dependency order)

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

- [x] `transport` (port) — sealed `Registrar`, generic methods, route
      table of pre-built closures
- [x] Bump toolchain to Go 1.27; generic methods compile as designed, and
      inference works — `r.Post("/users", c.register)` needs no type
      arguments, including for a concrete handler struct *(2026-08-29)*
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
      first priority. Zero third-party dependencies; 13 allocations per
      request, asserted by a test)*
- [ ] `transport/grpc` — **deferred to v0.2**, and the reasons are decided
      rather than open: a handler's `Req` must stay a plain Go struct or the
      HTTP adapter mis-encodes the same handler, so the wire needs generated
      proto messages and a *generated* shim between them — which needs
      `warren g proto`, the harder of the two artifacts. A proto codec over
      plain structs was prototyped, measured (faster than JSON) and rejected:
      no reflection descriptor, and field numbers in Go struct tags. The round
      found **zero required changes to core `transport`**. Every remaining
      design question is ruled in [warren.md](warren.md) §4.2
- [x] Fallback if 1.27 slips: generic free functions (compiles on 1.26; call
      sites change shape) *(the contingency was taken, then retired. v0.1–v0.2
      shipped `transport.Get(r, pattern, h)` as a free function on Go 1.26;
      1.27 landed on schedule and the call sites were rewritten to
      `r.Get(pattern, h)` on 2026-08-29. As predicted, it was a refactor of
      call sites rather than of the design — `Register`'s body changed, the
      route table did not)*

### Phase 3 — messaging

- [x] Outbox/inbox ownership decisions (writer split, leader election, module
      map) *(`Store.Append` is the writer and runs in the caller's
      transaction; leadership is `outbox.Elector` plus `outbox.Electors` for
      named ones; outbox and inbox are OPTIONS on the persistence module
      rather than siblings, because they need its pool)*
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
- [ ] `broker/rabbitmq`, `broker/nats` — deferred to v0.2. Both need a
      dependency audit that has not been run, and RabbitMQ needs one
      port-semantics contradiction resolved first (§3.4 calls `Message.Key`
      the routing key; §5.2 says the topic becomes it). Reasons in
      [warren.md](warren.md) §5.2 and §5.3

### Phase 4 — persistence

- [x] `persistence` (port) — `Repository`, `UnitOfWork`, the Track/Collect
      enlistment seam, in-process driver + contract suite *(implemented)*
- [x] `persistence/postgres` — the `UnitOfWork`, `postgres.DB`, the outbox
      store with `LISTEN`/`NOTIFY`, an advisory-lock elector, a durable inbox,
      and plain-SQL migrations *(implemented; passes `persistence.RunContract`
      unmodified against a real Postgres. **Never migrates at boot** — that
      races every replica of a rolling deploy. One third-party dependency:
      `pgx`; goose rejected)*
- [ ] `persistence/mongo`, `persistence/redis` — deferred to v0.2; the Mongo
      design round is closed and found the port needs no change, while Redis
      needs a structural decision first, because a cache and a lock are not
      §3.3 persistence and no `Cache`/`Lock` port exists. *(`mysql` is not
      deferred but undecided — it has a heading and a ledger row and nothing
      else.)* Reasons in [warren.md](warren.md) §6.2–6.4

### Phase 5 — cross-cutting

- [x] Core policy ports decided: `RetryPolicy`/`AuthorizationPolicy`/
      `Telemetry` live in `app`, telemetry rides the context
- [x] `observability` — OTel wiring: handlers, HTTP and broker propagation
      instrumented by one import, composed at BOOT so the request path decides
      nothing *(implemented; DB spans need one explicit `postgres.Configure`
      line. 24 third-party modules, confined here by an invariant — a service
      that does not import it pays nothing)*
- [x] `validate` — port in core, implementation in a submodule *(the port is
      `validate/validate.go`; `validate/playground` implements it, and core
      refuses the tags it cannot check rather than ignoring them)*
- [x] `health` — check registry, liveness/readiness verdicts, root-scope
      binding *(implemented; the routes land with the transport adapters)*
- [x] `warren/testing` (`warrentest`) — boot a module with fakes, Invoke by
      type, AssertPublished, Golden *(implemented; stdlib + core only)*
- [x] `app.Identity` — the identity seam, policies and the 401/403 split (v0.1)
- [x] `app.Timeout` + the resilience ruling: module DROPPED, not deferred (v0.1)
- [ ] `openapi` — OpenAPI 3.1 from the frozen route table plus DTO tags. No
      annotations, no IDL, no checked-in spec file. Raw routes are EMITTED with
      an `x-warren-undescribed` rather than omitted, and constraints are
      published only when the application's validator actually enforces them
      *(implemented 2026-08-29; zero third-party dependencies. The three
      defects that unticked this on 2026-08-31 are **all fixed and verified by
      a stranger** on 2026-09-02: same-named DTOs from two features are
      disambiguated by the shortest unique path suffix, `time.Time` emits
      `{"type":"string","format":"date-time"}`, and
      `go get github.com/MerseniBilel/warren/openapi` resolves. It stays
      unticked for ONE remaining reason — the module has no released tag, so
      it resolves only as a pseudo-version.)*
- [ ] `auth` (verifier) — deferred to v0.2. The design is settled (`app.Identity`
      and the policies shipped in v0.1); what is missing is the two dependency
      audits for `golang-jwt/jwt/v5` and `coreos/go-oidc`, which have not been
      run. Reasons in [warren.md](warren.md) §7.2

### Phase 6 — the CLI *(the discovery engine: scaffolding real apps is how
### weaknesses get found)*

- [x] `warren new` — a scaffold that compiles and tests against today's
      framework, with the CI gate that builds it (the anti-rot mechanism)
- [x] `warren version`; core **and all six submodules** tagged v0.2.0, so a
      scaffold's go.mod resolves without a `replace`. v0.1.0 tagged core alone
      — a scaffold also requires `transport/http`, so it never resolved, and
      `--framework <path>` was the only working route
- [x] `warren g module|entity|command|repository|consumer` — golden-file
      tested, idempotent, stdlib AST editing (no `dst`: it has published no
      releases and sat untouched through Go 1.19–1.27), and everything the
      five write compiles, vets and passes its own tests in a real project
      on every CI run
- [x] `warren lint arch` — four rules read from the import graph: layer,
      cross-module, handler/transport and handler/driver, each checked
      directly **and through a helper package**; works on a project that does
      not compile; runs in Warren's own CI over Warren, same binary. The
      report discloses which rules did not run — a project outside
      `internal/modules/` is told the cross-module rule compared nothing,
      rather than passing silently *(`--rules=rings` next)*
- [ ] v0.2+: `doctor`, `graph`, `explain di`, `templates eject`
- [ ] v0.3+: `extract module`, `add <adapter>`, `migrate layout`

### Two planned modules that were dropped, not deferred

Neither will be built, and the reasons are structural rather than schedule.
Full versions in [warren.md](warren.md) §7.3 and §7.4.

- **`resilience` — dropped 2026-08-05.** Retry and timeout are core-ring and
  already ship as `app.Retrying`, `app.Timeout` and
  `broker.ExponentialBackoff`. A circuit breaker and a rate limiter guard an
  **outbound** call, and Warren ships no outbound client — so they belong in
  your `infrastructure/` adapter, where two lines of `sony/gobreaker` do the
  job better than a wrapper would. `scripts/invariants.sh` refuses the
  dependency so the module cannot be re-derived by accident.
- **`jobs` — dropped 2026-08-05.** A scheduler is an ordinary
  `lifecycle.Hook`: it starts after its dependencies and is joined before them
  by construction. Leader-only work is `outbox.Electors`, and the important
  word is **by name** — one `Elector` is one advisory lock, so a field test
  that wired a scheduler and the outbox relay to the same one starved whichever
  lost the race, silently, for the life of the process. A scheduler mints its
  own: `el, err := electors.Elector("ticket/sla-sweeper")`. Different names
  lead at the same time, and the relay's own name is reserved, so asking for it
  fails the boot rather than competing for it.

---

## Repository map

| Where | What |
|---|---|
| [warren.md](warren.md) | The package manifest — one entry per package, source of truth |
| [AGENT.md](AGENT.md) | Invariants, conventions, and process — canonical for humans and agents |
| [GETTING_STARTED.md](GETTING_STARTED.md) | A complete service, from nothing to a running HTTP API, in one page |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to open a change here — the checks, the commit shape, what a review looks for |
| `<package>/SPEC.md` | A change still under review. It is **deleted** when its package is implemented and reviewed, its residue rehomed to `warren.md`, doc comments and tests first. There are no spec-only directories |

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md), then [AGENT.md](AGENT.md) — the
invariants and the dependency-audit rule apply to every change. A **new
package, a public API change, or a change that stops working code from
working** needs an approved spec first; a bug fix, a test, a doc change or a
new adapter over an existing port goes straight to a pull request. No
dependency is adopted without a written audit.

## License

Apache-2.0 — see [LICENSE](LICENSE).
