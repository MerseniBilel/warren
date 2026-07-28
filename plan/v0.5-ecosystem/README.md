# v0.5 — Ecosystem

> **Goal (PRD §10): Warren is usable by people who are not its author.**

**Specs are not written yet.** They get written when v0.4 ships.

This milestone is mostly breadth: more drivers, more cross-cutting modules, and
the documentation site. The interesting question underneath it is whether the
port interfaces settled in v0.1–v0.3 survive contact with a second and third
driver — that is what a stable port means, and it is what makes community-owned
drivers possible (PRD §12's mitigation for driver maintenance burden).

---

## Features

| # | Feature | Module | Scope |
|---|---|---|---|
| 01 | RabbitMQ driver | `warren/broker/rabbitmq` | `amqp091-go`. Exchanges, routing keys, quorum queues. |
| 02 | NATS driver | `warren/broker/nats` | JetStream. |
| 03 | Mongo driver | `warren/persistence/mongo` | |
| 04 | MySQL driver | `warren/persistence/mysql` | |
| 05 | Redis | `warren/persistence/redis` | Cache and distributed lock. |
| 06 | Auth | `warren/auth` | JWT/OIDC guards, RBAC policy hooks. |
| 07 | Resilience | `warren/resilience` | Circuit breaker, retry, rate limit, bulkhead, timeout. |
| 08 | Jobs | `warren/jobs` | Cron and background workers on the same lifecycle. |
| 09 | `warren extract module` | `warren/cli` | Lift a module into a new repository: in-process subscriptions become broker subscriptions, and a gRPC client is generated for calls that crossed the boundary. |
| 10 | Presets | `warren/cli` | `warren new --preset acme/backend-standard` from a git repository. |
| 11 | `warren dev` | `warren/cli` | Hot reload, watching templates, protos, and migrations. |
| 12 | Documentation site | — | Tooling chosen at v0.4 (docs/README.md). Must support including code from files. |
| 13 | Driver certification checklist | — | What a community driver must pass to be listed: the contract suite, plus the checklist PRD §12 names as the mitigation for driver sprawl. |

## Constraints already known

- **Every new driver runs the existing contract suite unmodified.** If a driver
  needs the suite changed, the port is wrong, and that is a finding about v0.3
  rather than a reason to fork the suite.
- **`extract module` is the most compelling feature in the pitch and the most
  likely to be half-true** (PRD §13.4). Either it works on a real module or it
  comes out of the marketing. v0.3's messaging work produces the evidence.
- **Presets execute code from a git repository.** That is a supply-chain surface
  on a developer's machine, and it needs a threat model before it needs
  features.
- **The docs site must support including code from files** — that requirement
  outranks every other feature, because PRD §8's "every concept has a runnable
  example, 100%" is unenforceable if samples are pasted into markdown
  ([docs/README.md](../../docs/README.md)).

## To settle when this milestone opens

1. **Which drivers does the project maintain, and which are community-owned?**
   PRD §12 flags driver maintenance as a medium risk with a bus factor of one.
   Four brokers and four databases is a lot of surface for one maintainer.
2. **Does `extract module` produce a repository someone would actually keep**, or
   a starting point they immediately rewrite? Prototype on a real module before
   committing to it publicly.
3. **Is `warren dev` worth building** when `air` and `wgo` already exist? A
   worse version of an existing tool is a maintenance liability.
