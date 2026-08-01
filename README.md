# Warren

**A DDD-first application framework and CLI for Go backends.**

> ⚠️ **Pre-release.** Nothing is importable yet. The repository was reset in
> July 2026 and is being rebuilt spec-first: every package gets an approved
> `SPEC.md` before its first line of Go. [warren.md](warren.md) is the design;
> [AGENT.md](AGENT.md) is the rules.

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

- [x] `errors` — the semantic vocabulary; load-bearing for everything
      *(spec approved; implemented, golden-file tested, `make ci` green)*
- [x] `domain` — `Entity`, `Root[K]`, `AggregateRoot`, `Event` *(spec approved;
      implemented, §3.1 example compiles as a test, `make ci` green)*
- [x] `log` — context-carried logger *(spec approved; implemented, Vendor mode
      settled, seeding surface added, `make ci` green)*
- [x] `di` — the container wrap; the golden diagnostic is the product
      *(spec approved; implemented, golden diagnostic byte-for-byte, dig
      v1.19.0 audited, `make ci` green)*
- [x] `lifecycle` — ordered start/stop, readiness gate *(spec approved;
      implemented, `Ready()` handle settled, `make ci` green)*
- [x] `config` (core) — Source-split loading *(implemented: Load, Source,
      env, flags; `Module[T]` lands with the root package)*
- [ ] `config/yaml` — the first file Source *(needs its own spec + YAML
      library audit before the module exists)*
- [ ] `warren` (root) — module system and boot sequence *(spec approved)*
- [ ] `app` — `Handler`/`Middleware`/`Chain` *(approved; built-in middleware
      blocked on the core-port decision)*
- [ ] `broker` (port) — `Message`/`Publisher`/`Subscriber` *(approved; chain
      home open)*

### Phase 2 — transport (blocked until Go 1.27 ships, expected August 2026)

- [ ] Bump toolchain to Go 1.27; verify generic methods compile as designed
- [ ] `transport` (port) — `Registrar` + three concrete generic registrars
- [ ] `transport/http` — chi-backed adapter, the HTTP error column
- [ ] `transport/grpc` — interceptors through the shared chain, the gRPC column
- [ ] Fallback if 1.27 slips: generic free functions (compiles on 1.26; call
      sites change shape)

### Phase 3 — messaging

- [ ] Outbox/inbox ownership decisions (writer split, leader election, module map)
- [ ] `broker/memory` — in-process driver, default in tests
- [ ] `outbox` — writer port + leader-elected relay
- [ ] `inbox` — dedupe store, on by default
- [ ] `broker/kafka` — franz-go driver *(SASL type decision applied)*
- [ ] `broker/rabbitmq`, `broker/nats` — after their manifest entries are written

### Phase 4 — persistence

- [ ] `persistence` (port) — the aggregate-save registration mechanism decided
- [ ] `persistence/postgres` — pool, `UnitOfWork`, outbox table, migrations
- [ ] `persistence/mongo`, `persistence/redis` — after their manifest entries
      are written *(`mysql`: deferred — exists only in a heading)*

### Phase 5 — cross-cutting

- [ ] Core policy ports decided (`Retrying`/`Authorized`/`Traced` mechanism)
- [ ] `observability` — OTel wiring *(spec approved)*
- [ ] `validate` — port in core, implementation in a submodule
- [ ] `health`, `auth`, `resilience`, `jobs`, `testing`, `openapi`

### Phase 6 — the CLI

- [ ] Analyzer (works on any Go project) → `lint arch` → `doctor` → `graph`
- [ ] Generators with golden-file tests, `--dry-run`/`--force`
- [ ] `warren new`, `explain di`, `extract module`

---

## Repository map

| Where | What |
|---|---|
| [warren.md](warren.md) | The package manifest — one entry per package, source of truth |
| [AGENT.md](AGENT.md) | Invariants, conventions, and process — canonical for humans and agents |
| `<package>/SPEC.md` | The package's contract; approved before any code |
| [docs/assets/](docs/assets/) | Usage-flow diagrams for the approved specs |

## Contributing

Read [AGENT.md](AGENT.md) first — the spec-first process, the dependency-audit
rule, and the invariants apply to every change. No feature is implemented
before its spec is approved, and no dependency is adopted without a written
audit.

## License

Apache-2.0 — see [LICENSE](LICENSE).
