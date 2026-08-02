# `github.com/MerseniBilel/warren/persistence/mysql` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2 (decided 2026-08-02)** — see **Why it is deferred** below. Nothing in shipped code depends on it. |
| **Source** | [warren.md §6.2–6.4](../../warren.md) |
| **Module** | own module (`warren/persistence/mysql`) — **not listed in warren.md §1.6** |
| **Mode** | undecided — no mode recorded |
| **Wraps** | undecided — no driver chosen |


## Why it is deferred

**warren.md does not describe this package at all** — it is absent from §1.6's
module table, which lists postgres, mongo and redis. Building it would mean
writing the manifest entry first, and that is an architecture decision to be
agreed rather than an implementation to be started (CLAUDE.md).

## Problem

warren.md says three sentences about MySQL, Mongo, and Redis combined:

> ### 6.2–6.4 `mysql` / `mongo` / `redis`
>
> Same `Repository` and `UnitOfWork` ports. Mongo's UoW uses sessions; Redis
> provides cache + distributed lock rather than repositories.

That is the entire source for this package. Of it, only the first clause applies
to MySQL: **same `Repository` and `UnitOfWork` ports** as declared in
`warren/persistence` §3.3.

Everything else a spec needs — the driver, the mode, the module surface, the
options, the transaction-propagation mechanism, the outbox story, the migration
story, the health check — is not in warren.md. AGENT.md § Before you write code
is explicit: "Do not add a package that `warren.md` does not describe without
agreeing the manifest entry first. The manifest is the plan; a package that is
not in it is either scope creep or a missing decision."

**This is a missing decision, not scope creep** — the §6.2–6.4 heading names
`mysql`, so someone intended it. But the manifest has to be amended before this
package can be specced, let alone built.

MySQL is more thinly recorded than its two neighbours. It appears in **exactly
one place in warren.md: the §6.2–6.4 heading.** It has no `§1.6` module row (the
repository layout lists `persistence/postgres`, `persistence/mongo`, and
`persistence/redis` — not `mysql`), no `§9` dependency-ledger row, and no `§1.7`
dependency-budget line. Postgres has all three.

## Goals

Only one is stated by warren.md, and it is stated at one remove:

- Implement `persistence.Repository` and `persistence.UnitOfWork` (§3.3) against
  MySQL, such that the port's contract suite passes and a domain layer cannot
  tell which driver is underneath.

No other goal can be traced to the source.

## Non-goals

- **Not an ORM.** §3.3's "The deliberate omission is an ORM. No GORM, no ent, no
  sqlc mandate" is a framework-wide position, not a Postgres-only one.
- **Not the ports.** `Repository` and `UnitOfWork` live in `warren/persistence`
  (§3.3, contracts ring, zero implementations). This package implements them.
- **Never imports another adapter** — AGENT.md invariant 4. In particular it does
  not import `warren/persistence/postgres`, and the Postgres spec is not a
  template to copy.
- **Not a Postgres port.** The Postgres surface — `postgres.DB`, `MaxConns`,
  `Migrations(fs, RunOnStart())`, the outbox writer, the advisory-lock leader
  election — is specified for Postgres in §6.1 and **nothing in warren.md extends
  any of it to MySQL**. Deriving a MySQL surface by analogy would be inventing
  public API, which is what this spec exists to avoid.

## Dependency audit

**No driver has been chosen.** warren.md names none: there is no §1.6 module row
and no §9 ledger row for MySQL.

Choosing one is the first decision, and AGENT.md § Adding a dependency governs
it: read the repository and the documentation, check archived status, last ship
date, transitive weight, and licence compatibility; record the findings and the
observation date in this spec; add the row to the §9 ledger; assign a mode and
justify it against the wrap rule. `gh api repos/<owner>/<repo>` and
`gh api repos/<owner>/<repo>/releases/latest` are the commands AGENT.md names.

Until that is done there is nothing to audit here, and no `go.mod`.

## Public API

**None. warren.md states no MySQL surface.**

Writing one here would be invention. What is fixed is only that whatever this
package provides must satisfy §3.3:

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

## Behaviour

Two things are fixed by warren.md and apply to any persistence adapter:

**`UnitOfWork.Do` runs the §3.3 six-step sequence:** begin a transaction and put
it on the context; run `fn`, with repositories picking the transaction up from
the context automatically; drain `PullEvents()` from every aggregate saved in
scope; insert those events into the outbox in the same transaction; commit, so
state and events are atomic; then post-commit, signal the relay to publish.
Postgres (§6.1) is the reference implementation of exactly that. Whether MySQL
can implement each step the same way — and what the outbox table and the relay's
leader election look like without Postgres advisory locks (§5.5) — is not
addressed anywhere in warren.md.

**Shutdown position (§2.3 step 5, and AGENT.md § Shutdown):** "DB pools, broker
connections close" — **last**, after readiness has gone to 503, after servers
drained, after consumers stopped fetching, and after the outbox relay flushed.
The relay's final flush reads the outbox table, so this pool must still be open
when it runs. On the way up (§1.3 step 6) the pool starts **first**: "pool →
repos → consumers → servers".

Everything else — connection pooling options, transaction isolation, how
`pgx.ErrNoRows`'s MySQL equivalent maps to `errors.NotFound`, whether migrations
run at boot, whether a ping health check self-registers per §2.8 — is
undetermined.

## Testing

The rules bind even though the surface does not exist yet (AGENT.md § Testing):

- **The `warren/persistence` contract suite must pass.** That suite is the
  definition of a correct driver, and it is owned by the port package: "Every
  port change updates the contract suite first, then the drivers."
- **Unit tests: no Docker, no network, no sleeps.** A real MySQL goes behind
  `//go:build integration`.
- **Golden-file tests for any generated repository**, if a
  `warren g repository --driver mysql` is ever specced — AGENT.md: templates
  break silently otherwise.
- **Golden-file tests for error text**, for every message this spec ends up
  stating. There are currently none to test.

## Definition of done

This package is not ready to be built. Done, for now, means the decisions exist:

- [ ] **warren.md amended** so `mysql` appears in the §1.6 repository layout with
      a driver, in the §9 dependency ledger with a mode, and — if it changes the
      user's dependency budget — in §1.7. AGENT.md: the manifest is agreed first.
- [ ] Driver chosen and audited per AGENT.md § Adding a dependency, with an
      observation date recorded here.
- [ ] Mode assigned (Build / Wrap / Vendor) and justified against the wrap rule.
- [ ] Public API written as Go in this spec and approved — AGENT.md: "The spec's
      public API section is the contract under review."
- [ ] Open questions below answered.
- [ ] Only then: implementation, contract suite green, invariants 3 and 4 checked.

**Do not create the `go.mod` before that.** AGENT.md: "Do not create a new module
unless its first real code lands in the same change. An empty module is a release
obligation with no user."

## Open questions

Everything below is undetermined because warren.md does not address it.

1. **Is MySQL in scope at all?** It is named in the §6.2–6.4 heading and nowhere
   else — not in the §1.6 module list, not in the §9 ledger, not in §1.7. Mongo
   and Redis are in §1.6; MySQL is not. Is this an intended package with a missing
   manifest entry, or a heading that outran the plan?
2. **Which driver?** `go-sql-driver/mysql` over `database/sql` is the obvious
   candidate, but warren.md records no choice and AGENT.md forbids adopting one
   without a written audit.
3. **What mode?** Postgres is Wrap. Wrap implies a port in front and a named
   escape hatch for the raw handle; Vendor implies direct use.
4. **What is the module surface?** Postgres has `Module(DSN, MaxConns,
   Migrations(fs, RunOnStart()))`. Nothing says MySQL mirrors it, and mirroring it
   without agreement would be inventing API.
5. **Is there a MySQL equivalent of `postgres.DB`** — the callable that returns
   the ambient transaction if one exists, else the pool? §6.1 calls it "the only
   piece of framework magic"; the concept is Postgres-specific in the text.
6. **Outbox on MySQL.** §5.5 gives the outbox two modes, polling (portable) and
   CDC via Postgres logical replication. Does MySQL get polling only, binlog CDC,
   or nothing? And what replaces `outbox.PostgresAdvisoryLock` for leader
   election?
7. **Migrations.** goose supports MySQL, but §9 records goose only against
   Postgres, and §1.7's budget line is Postgres-shaped.
8. **Does `warren new --db mysql` exist?** §8's scaffold command shows
   `--db postgres`. The set of accepted values is not stated.
9. **Does a MySQL repository generator exist?** §8 shows
   `warren g repository user/User --driver postgres`. Whether `--driver mysql` is
   a supported value, and what its golden file contains, is unspecified.
10. **`Repository[T domain.Root[ID], ID domain.ID]` (§3.3) constrains `T` to
    `domain.Root`, but §3.1 declares `AggregateRoot[T ID]` and no `Root`.** The
    port package owns the fix; noted here because this driver has to satisfy
    whichever name survives.
