# `github.com/MerseniBilel/warren/persistence/mongo` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2 — as a DESIGN ROUND, not a build (decided 2026-08-02)** — see **Why it is deferred** below. Nothing in shipped code depends on it. |
| **Source** | [warren.md §6.2–6.4](../../warren.md) |
| **Module** | own module (`warren/persistence/mongo`) |
| **Mode** | undecided — **no §9 ledger row** |
| **Wraps** | `mongo-driver` (warren.md §1.6) — no version, no audit |


## Why it is deferred

warren.md describes this in one clause, so there is nothing to implement
against. But it is the most interesting of the deferrals and should not be
treated as a routine one.

**Mongo is the only remaining candidate that would genuinely stress the
`Repository`/`UnitOfWork` port**: no SQL, transactions that need a replica set,
and no advisory lock for the outbox elector. If the port is wrong, that is
where it shows — and it is worth knowing before three more adapters depend on
it. So v0.2 opens this as a design conversation about whether the port is
right, which is exactly what CLAUDE.md says to bring to the human rather than
discover by writing code.

## Problem

warren.md says three sentences about MySQL, Mongo, and Redis combined:

> ### 6.2–6.4 `mysql` / `mongo` / `redis`
>
> Same `Repository` and `UnitOfWork` ports. Mongo's UoW uses sessions; Redis
> provides cache + distributed lock rather than repositories.

Of that, two clauses apply to Mongo:

1. **Same `Repository` and `UnitOfWork` ports** — the ones declared in
   `warren/persistence` §3.3.
2. **Mongo's UoW uses sessions** — i.e. the transaction the `UnitOfWork` puts on
   the context is a Mongo session, not a SQL transaction.

Plus one line elsewhere: §1.6 lists `warren/persistence/mongo` as its own module
with `mongo-driver` as the driver.

That is everything. The module surface, the options, how a repository picks the
session up from the context, the outbox story, the health check, and the mode are
all absent. AGENT.md § Before you write code: "Do not add a package that
`warren.md` does not describe without agreeing the manifest entry first."
The entry exists here but is one line long, and it is not enough to build from.

**Mongo also has no row in the §9 dependency ledger.** Postgres, Kafka,
RabbitMQ, NATS, and Redis all do. AGENT.md § Modes says the ledger is where a
mode is recorded, so Mongo currently has no recorded mode.

## Goals

Two, both traceable to the source:

- Implement `persistence.Repository` and `persistence.UnitOfWork` (§3.3) against
  MongoDB, such that the port's contract suite passes and a domain layer cannot
  tell which driver is underneath.
- **The `UnitOfWork` is session-based.** Sessions are the mechanism warren.md
  names, so a Mongo `Do` opens a session, carries it on the context, and commits
  it — see Behaviour.

## Non-goals

- **Not an ODM/ORM.** §3.3's "The deliberate omission is an ORM. No GORM, no ent,
  no sqlc mandate" is a framework-wide position, and there is nothing
  Postgres-specific about it. Repositories are the user's to edit.
- **Not the ports.** `Repository` and `UnitOfWork` live in `warren/persistence`
  (§3.3, contracts ring, zero implementations). This package implements them.
- **Never imports another adapter** — AGENT.md invariant 4.
- **Not a Postgres port.** `postgres.DB`, `MaxConns`, `Migrations(fs,
  RunOnStart())`, the outbox writer, and `outbox.PostgresAdvisoryLock` are
  specified for Postgres in §6.1 and §5.5, and **nothing in warren.md extends any
  of them to Mongo**. Deriving a Mongo surface by analogy would be inventing
  public API.

## Dependency audit

**Outstanding, and partly undecided.**

warren.md §1.6 names `mongo-driver` as the module's dependency. It does not say
which — `go.mongodb.org/mongo-driver` v1 and the v2 module are different import
paths — and Mongo has **no §9 ledger row at all**, so no mode is recorded either.

None of what AGENT.md § Adding a dependency step 1 requires has been done: no
archived check, no last-ship date, no transitive-weight check, no licence check,
no observation date. AGENT.md: "**A package with no written audit does not go
into a `go.mod`.** Star counts are not evidence."

One licence point is worth flagging before the audit runs rather than after:
AGENT.md requires a licence "Apache-2.0/MIT/BSD/ISC compatible", and the MongoDB
*server* is SSPL. The Go driver's own licence needs reading rather than assuming
from the server's, in either direction.

- [ ] Choose the exact module path and major version.
- [ ] Audit it — `gh api repos/mongodb/mongo-go-driver`,
      `gh api repos/mongodb/mongo-go-driver/releases/latest` — record findings and
      the observation date here.
- [ ] Add the §9 ledger row with a mode, and justify the mode against the wrap
      rule.

## Public API

**None. warren.md states no Mongo surface.** Writing one here would be
invention.

What is fixed is only that whatever this package provides must satisfy §3.3:

```go
type Repository[T domain.Root[ID], ID domain.ID] interface {
	FindByID(context.Context, ID) (T, error)
	Save(context.Context, T) error
	Delete(context.Context, ID) error
}

type UnitOfWork interface {
	Do(ctx context.Context, fn func(context.Context) error) error
}
```

Those declarations belong to `warren/persistence` and are quoted, not redefined.
Note that `Do`'s signature is driver-neutral: the session never appears in it, so
whatever carries the session must ride on the `context.Context`.

## Behaviour

**`UnitOfWork.Do` runs the §3.3 six-step sequence, with a session in place of a
SQL transaction:** begin — here, start a session and its transaction — and put it
on the context; run `fn`, with repositories picking the session up from the
context automatically; drain `PullEvents()` from every aggregate saved in scope;
insert those events into the outbox **in the same transaction**; commit, so state
and events are atomic; then post-commit, signal the relay to publish. Postgres
(§6.1) is the reference implementation of exactly this sequence, and warren.md's
only stated deviation for Mongo is the session.

Two consequences follow from the source but are not resolved by it. Mongo
transactions require a replica set or sharded cluster, so step 5's atomicity
guarantee is conditional on deployment topology in a way Postgres's is not; and
the "outbox table" of step 4 is a collection here, with no schema stated
anywhere. Both are open questions, not assumptions to build on.

**Shutdown position (§2.3 step 5, and AGENT.md § Shutdown):** "DB pools, broker
connections close" — **last**, after readiness has gone to 503, after servers
drained, after consumers stopped fetching, and after the outbox relay flushed.
The relay's final flush reads the outbox, so the client must still be connected
when it runs. On the way up (§1.3 step 6) the pool starts **first**: "pool →
repos → consumers → servers".

Everything else — client options, read/write concern, how a driver-specific
"no documents" error maps to `errors.NotFound` the way §6.1 maps `pgx.ErrNoRows`,
whether a ping health check self-registers per §2.8, whether index or schema
setup runs at boot — is undetermined.

## Testing

The rules bind even though the surface does not exist yet (AGENT.md § Testing):

- **The `warren/persistence` contract suite must pass.** That suite is the
  definition of a correct driver, and it is owned by the port package: "Every
  port change updates the contract suite first, then the drivers."
- **Unit tests: no Docker, no network, no sleeps.** A real MongoDB — which for
  session transactions means a replica set — goes behind `//go:build integration`.
- **Golden-file tests for any generated repository**, if a
  `warren g repository --driver mongo` is ever specced.
- **Golden-file tests for error text**, for every message this spec ends up
  stating. There are currently none to test.
- The atomicity test is the load-bearing one, as for Postgres: commit an
  aggregate and assert the outbox document exists; fail after `Save` and assert
  neither exists.

## Definition of done

This package is not ready to be built. Done, for now, means the decisions exist:

- [ ] **warren.md amended** with a §9 ledger row for Mongo carrying an exact
      library and a mode, and §6.2–6.4 expanded into a real manifest entry —
      owns / wraps / surface / usage, the shape §"How to Read This Document"
      requires of every package.
- [ ] Driver audited per AGENT.md § Adding a dependency, observation date recorded
      here.
- [ ] Public API written as Go in this spec and approved — AGENT.md: "The spec's
      public API section is the contract under review."
- [ ] Open questions below answered.
- [ ] Only then: implementation, contract suite green, invariants 3 and 4 checked.

**Do not create the `go.mod` before that.** AGENT.md: "Do not create a new module
unless its first real code lands in the same change."

## Open questions

Everything below is undetermined because warren.md does not address it.

1. **Which `mongo-driver`, and what mode?** §1.6 gives a bare name, §9 has no row
   at all. Wrap (port in front, named escape hatch for the raw client) or Vendor
   (used directly)? Postgres is Wrap; nothing says Mongo follows.
2. **What is the module surface?** Postgres has `Module(DSN, MaxConns,
   Migrations(fs, RunOnStart()))`. `mongo.Module(...)` with what options — URI,
   database name, pool size, read/write concern? None are stated.
3. **How does a repository pick the session up from the context?** Postgres has
   `postgres.DB`, a callable returning the ambient transaction or the pool, which
   §6.1 calls "the only piece of framework magic". Is there a `mongo.DB`
   equivalent, and does it return a session, a database handle, or a collection?
4. **Session transactions need a replica set or sharded cluster.** A standalone
   `mongod` cannot start one, so `UnitOfWork.Do` cannot be atomic there. Does
   Warren require a replica set, fail at boot when it finds a standalone (which
   the §1.3 rule — "every error the framework can detect surfaces at boot" —
   argues for), or degrade to non-transactional writes?
5. **What is the outbox collection, and who creates it?** §6.1 has Postgres
   provide "outbox table + writer"; §5.5 puts the writer in `warren/outbox`.
   Neither says anything about a Mongo equivalent.
6. **How is the relay leader-elected against Mongo?** §5.5 names
   `outbox.PostgresAdvisoryLock`, which is Postgres-only. Is there a Mongo
   mechanism, does the relay require Postgres regardless of the primary store, or
   is the outbox Postgres-only in practice?
7. **CDC mode.** §5.5's low-latency outbox mode is "Postgres logical
   replication". Mongo change streams are the obvious analogue, but warren.md
   never mentions them.
8. **What replaces the `pgx.ErrNoRows` → `errors.NotFound("user", id)` mapping?**
   §6.1 fixes that one line for Postgres; the Mongo equivalent is unstated.
9. **Does a ping health check self-register?** §2.8 says adapters self-register
   and names only postgres and kafka.
10. **Does `warren new --db mongo` exist, and is there a
    `warren g repository --driver mongo`?** §8 shows `--db postgres` and
    `--driver postgres`; the accepted value sets are not stated.
11. **`Repository[T domain.Root[ID], ID domain.ID]` (§3.3) constrains `T` to
    `domain.Root`, but §3.1 declares `AggregateRoot[T ID]` and no `Root`.** The
    port package owns the fix; noted here because this driver has to satisfy
    whichever name survives.
