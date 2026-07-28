# v0.2 — DDD core

> **Goal (PRD §10): DDD primitives are real types, not a folder convention.**

**Specs are not written yet, deliberately.** They get written when v0.1 ships
and the dogfooding service has told us which of the guesses below were wrong.
PRD §10 makes that dogfooding an exit criterion of v0.1 for exactly this reason:
domain primitives designed against an imagined service will be wrong in ways
only usage reveals.

What follows is the feature list and the constraints already known.

---

## Features

| # | Feature | Module | Scope |
|---|---|---|---|
| 01 | Domain primitives | `warren/domain` | `Entity`, `ValueObject`, `AggregateRoot`, `Event`, `Specification[T]` as embeddable bases. Core module, standard library only. |
| 02 | In-process event bus | `warren/domain` | Publish domain events raised by aggregates; the in-process default that a broker later replaces without the handler changing. |
| 03 | Repository and UnitOfWork ports | `warren/persistence` | `Repository[T, ID]`, `UnitOfWork`, transaction context. Ports only — no driver. |
| 04 | Postgres driver | `warren/persistence/postgres` | `pgx` v5. Repository implementation, transaction propagation through context, the outbox table (the relay itself is v0.3). |
| 05 | Migrations | `warren/persistence/postgres` | `pressly/goose` as a library, so `warren` runs migrations in-process. `warren g migration`. |
| 06 | Validation | `warren/validate` | Struct-tag validation over `go-playground/validator`, called by transport adapters before the handler. |
| 07 | Command and query buses | `warren/app` | Deferred from v0.1 §3. Decorators for logging, transaction, retry, metrics. |
| 08 | `warren g entity` | `warren/cli` | `--fields "email:Email,name:string"`. Entity, its test, and the repository interface. |
| 09 | `warren g value-object` | `warren/cli` | `--validate`. |
| 10 | `warren g event` | `warren/cli` | `--publish`. |
| 11 | `warren g command` / `g query` | `warren/cli` | `--transport http`. Handler, DTO, controller route, and the AST wiring. |
| 12 | `warren g repository` | `warren/cli` | `--driver postgres`. Interface in `domain/`, implementation in `infrastructure/`. |
| 13 | Echo and Gin adapters | `warren/transport/http/{echo,gin}` | Deferred from v0.1. Both run the v0.1 contract suite unchanged — which is the test of whether the port is genuinely router-agnostic. |
| 14 | `--layout simple` | `warren/cli` | A module with no domain layer. PRD §12's mitigation for "DDD is a hard sell to teams that want CRUD" — let them grow into it. |

## Constraints already known

- **`warren/domain` is core, so standard library only.** Any primitive that
  seems to need a library is split: port in core, implementation in a submodule.
- **`UnitOfWork` commits state and the outbox atomically** (PRD §6.4). That is
  the single hardest requirement in this milestone and it constrains the
  transaction-propagation design — which means it is decided first, not last.
- **The contract suite comes before the driver.** AGENT.md: a port change
  updates the contract suite first, then the drivers. `Repository` and
  `UnitOfWork` get suites that Postgres, and later MySQL and Mongo, all run.
- **Every generator needs a golden-file test and a skill.** Seven new commands
  here; the CLI foundation
  ([v0.1 #09](../v0.1-skeleton/09-cli-foundation/spec.md)) exists so that is
  cheap rather than a discipline nobody keeps.
- **`assert.EventPublished[UserRegistered](t, aggregate)`** (PRD §9) shapes the
  event API. Designing events without writing that assertion first produces an
  API that cannot support it.
- **pgx, goose, and validator all have audit rows already**
  ([dependencies.md §3.6, §3.8](../../docs/dependencies.md)). Re-verify at
  adoption; the audit is dated 2026-07-27.

## To settle when this milestone opens

1. **Does `AggregateRoot` use embedding or an interface with a helper?**
   Embedding is idiomatic and puts framework fields in the user's domain type,
   which is the coupling this milestone exists to avoid.
2. **How does a transaction travel?** Through `context.Context` — convenient, and
   invisible in signatures — or explicitly in the repository call. The first is
   what everyone does; the second is what "explicit over magic" (PRD §4.1) asks
   for.
3. **Does `Specification[T]` compile to SQL, or filter in memory?** Compiling is
   the useful version and welds the port to a query language.
4. **What does `warren g entity --fields` do with a type it does not know?**
   `email:Email` implies a value object that may not exist yet.
