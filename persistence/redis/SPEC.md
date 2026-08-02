# `github.com/MerseniBilel/warren/persistence/redis` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2, and ENTANGLED (decided 2026-08-02)** — see **Why it is deferred** below. Nothing in shipped code depends on it. |
| **Source** | [warren.md §6.2–6.4](../../warren.md) |
| **Module** | own module (`warren/persistence/redis`) |
| **Mode** | Wrap (warren.md §9) |
| **Wraps** | `redis/go-redis/v9` (warren.md §9) — no audit |


## Why it is deferred

warren.md describes this in one clause. More usefully: it is entangled with
`jobs`, whose open question 4 asks what backs `LeaderOnly()` when there is no
Postgres and points here for the answer.

**Decide `jobs` and `redis` together, or the wrong lock gets built.** The
advisory-lock elector already exists in `persistence/postgres`; where a shared
one lives, given invariant 4 forbids adapters importing each other, is the
question that unblocks both.

## Problem

warren.md says three sentences about MySQL, Mongo, and Redis combined:

> ### 6.2–6.4 `mysql` / `mongo` / `redis`
>
> Same `Repository` and `UnitOfWork` ports. Mongo's UoW uses sessions; Redis
> provides cache + distributed lock rather than repositories.

For Redis the operative clause is the last one, and it **contradicts the first
sentence of the same paragraph**: the paragraph opens by saying all three drivers
implement the same `Repository` and `UnitOfWork` ports, then says Redis provides
cache and a distributed lock "rather than repositories". Both cannot be true. The
more specific clause is presumably the intended one — Redis is not an aggregate
store — but the manifest needs correcting rather than interpreting.

The rest of what warren.md records about Redis is three more mentions:

- **§1.6** — `warren/persistence/redis` is its own module, driver `redis/go-redis`.
- **§9** — ledger row: `redis/go-redis/v9`, Mode **Wrap**, note "cache + lock".
- **§5.6** — the inbox's "dedupe store (Postgres or Redis) keyed on `Message.ID`
  with TTL", enabled by default.

That is everything. There is no surface, no options, no `Cache` interface, no
`Lock` interface, no health check, and no statement of what "cache" means as an
API. Two concepts are named — cache, distributed lock — and neither is defined.

AGENT.md § Before you write code: "Do not add a package that `warren.md` does not
describe without agreeing the manifest entry first." The entry exists but is one
clause long, and it is not enough to build from.

**One structural question comes before any API question:** if Redis provides
neither repositories nor a `UnitOfWork`, it is not a persistence adapter in the
sense §3.3 means, and `warren/persistence/redis` is the wrong path for it. See
Open questions.

## Goals

Only what warren.md states, which is two nouns:

- **Cache** — a caching facility over Redis.
- **Distributed lock** — a lock usable across processes, which §5.5's
  leader-elected outbox relay and §7.4's `jobs.LeaderOnly()` both plausibly want,
  though warren.md never connects them to this package.

Both are named and neither is specified.

## Non-goals

- **Not a repository store.** "Redis provides cache + distributed lock **rather
  than repositories**." Aggregates are not persisted here, and `Repository` from
  §3.3 is not implemented — notwithstanding the opening sentence of §6.2–6.4,
  which says the opposite and is flagged as a contradiction above.
- **Not a `UnitOfWork`.** Same clause, same reasoning. Redis has no place in the
  §3.3 six-step commit sequence, because that sequence's guarantee is that
  aggregate state and outbox rows share one transaction — and Redis holds neither.
- **Not a cache abstraction over other backends.** warren.md defines no `Cache`
  port in the contracts ring (§3 lists `domain`, `app`, `persistence`, `broker`,
  `transport`, and nothing else), so there is nothing here for a memcached
  adapter to implement.
- **Never imports another adapter** — AGENT.md invariant 4. In particular, if the
  inbox dedupe store (§5.6) can be either Postgres or Redis, that choice cannot be
  made by one adapter importing the other.
- **No `*redis.Client` in an exported signature** — AGENT.md invariant 3. Mode
  **Wrap** (§9) means users must not import go-redis directly; the raw client is
  reachable through a named escape hatch only, as `http.Raw` is for chi.

## Dependency audit

**Outstanding.**

warren.md records the decision — `redis/go-redis/v9`, Mode **Wrap**, §9 — but
none of what AGENT.md § Adding a dependency step 1 requires: no archived check, no
last-ship date, no transitive-weight check, no licence check, no observation date.

AGENT.md: "**A package with no written audit does not go into a `go.mod`.** Star
counts are not evidence. 'It is popular' is not evidence." The initial audit
caught `google/wire` and `git-chglog` archived with no README saying so — a
widely-used package is exactly the case where the check gets skipped.

- [ ] Audit `redis/go-redis/v9` — `gh api repos/redis/go-redis`,
      `gh api repos/redis/go-redis/releases/latest` — record findings and the
      observation date here, and add the date to the §9 ledger row.
- [ ] Confirm the transitive set is small. §1.7's dependency budget has no Redis
      line, so what adding Redis costs a user's `go.mod` is currently unstated.

## Public API

**None. warren.md states no Redis surface.** Writing one here would be
invention — and the risk is higher for this package than for the other drivers,
because "cache" and "distributed lock" are familiar enough shapes that plausible
Go writes itself. Two named concepts are not an API.

For the avoidance of doubt about what is *not* here: this package does **not**
declare `Repository` or `UnitOfWork`. Those are `warren/persistence` §3.3 types,
and §6.2–6.4 says Redis provides cache and lock rather than repositories.

## Behaviour

Only two behavioural facts trace to warren.md.

**Shutdown position (§2.3 step 5, and AGENT.md § Shutdown):** "DB pools, broker
connections close" — **last**, after readiness has gone to 503 (1), after servers
stopped accepting and drained (2), after consumers stopped fetching and in-flight
messages acked (3), and after the outbox relay flushed (4). If the Redis
connection backs the inbox dedupe store, step 3's in-flight messages need it
open, which is precisely why it closes at 5 and not earlier. On the way up (§1.3
step 6) it starts **first**: "pool → repos → consumers → servers".

**Inbox dedupe (§5.6):** the dedupe store may be Redis, keyed on `Message.ID`
with a TTL, and it is enabled by default because "at-least-once delivery means
duplicates are certain, not hypothetical". Whether that store is implemented in
this package or in `warren/inbox` is not stated — and invariant 4 constrains the
answer, since `warren/inbox` cannot import an adapter if it is itself one.

Everything else — cache semantics, key namespacing, serialisation, TTL defaults,
lock acquisition and renewal, what happens when a lock's holder dies, whether a
ping health check self-registers per §2.8 — is undetermined.

## Testing

The rules bind even though the surface does not exist yet (AGENT.md § Testing):

- **The `warren/persistence` contract suite** is the gate for a driver that
  implements the §3.3 ports. If Redis implements neither, the suite does not apply
  and this package needs its own contract tests for whatever cache and lock ports
  are eventually agreed. Which of the two is true is the first open question.
- **Unit tests: no Docker, no network, no sleeps.** That is a sharp constraint for
  a distributed lock, whose interesting behaviour is expiry — and AGENT.md forbids
  sleeps outright, so lock tests need an injectable clock rather than a
  `time.Sleep`. A real Redis goes behind `//go:build integration`.
- **Golden-file tests for error text**, for every message this spec ends up
  stating. There are currently none to test.
- **Lock correctness will be the load-bearing test**, as atomicity is for
  Postgres — but only once the lock semantics are agreed (Open question 4).
  warren.md says "distributed lock" and nothing about expiry, renewal, or what
  happens when a holder dies, so the specific guarantees to test are undecided.

## Definition of done

This package is not ready to be built. Done, for now, means the decisions exist:

- [ ] **The §6.2–6.4 contradiction resolved** in warren.md — Redis either
      implements the §3.3 ports or it does not.
- [ ] **warren.md amended** with a real manifest entry for Redis: owns / wraps /
      surface / usage, the shape §"How to Read This Document" requires of every
      package, plus a §1.7 dependency-budget line.
- [ ] **The path question settled** — whether a cache-and-lock package belongs
      under `persistence/` at all (Open question 1).
- [ ] `redis/go-redis/v9` audited per AGENT.md § Adding a dependency, observation
      date recorded here and in the §9 ledger.
- [ ] Public API written as Go in this spec and approved — AGENT.md: "The spec's
      public API section is the contract under review."
- [ ] Open questions below answered.
- [ ] Only then: implementation, invariants 3 and 4 checked — no `*redis.Client`
      in an exported signature, no adapter-to-adapter import.

**Do not create the `go.mod` before that.** AGENT.md: "Do not create a new module
unless its first real code lands in the same change. An empty module is a release
obligation with no user."

## Open questions

Everything below is undetermined because warren.md does not address it.

1. **§6.2–6.4 contradicts itself: does Redis implement `Repository` and
   `UnitOfWork` or not?** The paragraph's first sentence says all three drivers
   share the ports; its last clause says Redis provides cache and lock "rather
   than repositories". The manifest needs to say one thing.
2. **Should this live under `persistence/` at all?** A cache and a distributed
   lock are not persistence in the §3.3 sense. §1.1's adapters ring lists
   `persistence/postgres` and `observability` side by side, so a top-level
   `warren/cache` or `warren/redis` is available as a shape. The path is a
   structural decision, which AGENT.md reserves for the human.
3. **What is the cache API?** A typed `Get`/`Set`/`Delete` with TTL? A
   read-through helper? Core middleware in the §3.2 sense, wrapping
   `app.Handler[Req, Res]`? Nothing is stated, and §3.2's built-in middleware
   table lists no caching entry.
4. **What is the distributed-lock API, and who uses it?** §5.5's relay is
   "leader-elected" but names only `outbox.PostgresAdvisoryLock`; §7.4 has
   `jobs.LeaderOnly()` with no mechanism named. Is a Redis lock one of the
   pluggable leader-election backends, and if so, how does that dependency cross
   the adapter boundary invariant 4 draws?
5. **Is there a `Cache` or `Lock` port in the contracts ring?** §3 defines
   `domain`, `app`, `persistence`, `broker`, and `transport` only. Mode **Wrap**
   (§9) means "port interface in front" (AGENT.md § Modes) — so a port is implied
   and does not exist. Where does it go?
6. **Who implements the §5.6 inbox dedupe store?** It is "Postgres or Redis",
   enabled by default. If `warren/inbox` holds the port, this package holds one
   implementation — but §1.6 lists no module for `warren/inbox` at all, and
   invariant 4 forbids one adapter importing another.
7. **What are the serialisation and key-namespacing rules?** A shared Redis across
   services needs a prefix; caching a typed value needs an encoding. Neither is
   mentioned.
8. **Does a ping health check self-register?** §2.8 says adapters self-register
   and names only postgres and kafka.
9. **What is the module surface?** `redis.Module(...)` with what options — URL,
   pool size, TLS, cluster vs sentinel vs single node? None are stated, and
   cluster mode in particular changes what a correct distributed lock looks like.
10. **What is the named escape hatch for the raw client?** Mode Wrap plus
    invariant 3 requires one — `http.Raw(func(mux *chi.Mux){...})` is the pattern
    — and warren.md names none for Redis.
11. **§2.3's usage example wires Redis by hand.** It shows a user-written
    `NewRedisClient` in a `warren.NewModule("cache", ...)` with `OnStart`/`OnStop`
    hooks, i.e. a service using Redis without this package existing. Is that
    illustrative only, or does it indicate the intended level of support?
