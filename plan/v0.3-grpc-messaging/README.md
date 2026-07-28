# v0.3 — gRPC and messaging

> **Goal (PRD §10): the transport-agnostic claim is proved, not asserted.**

**Specs are not written yet.** They get written when v0.2 ships.

Until a second and third transport exist, "write a use case once, expose it
three ways" is a design intention. This milestone is where it becomes a fact or
turns out to be false — and if `app.Handler` needs to change to accommodate
gRPC or a consumer, **that is the finding**, and it is far better learned here
than at v1.0.

---

## Features

| # | Feature | Module | Scope |
|---|---|---|---|
| 01 | gRPC transport | `warren/transport/grpc` | Server and client, interceptor chain, reflection, health service. Handler-first with optional proto generation (PRD §13.2). |
| 02 | Unified middleware | `warren/app` | One `Middleware` value applying across HTTP, gRPC, and consumers. See the open question below. |
| 03 | Broker ports | `warren/broker` | `Publisher`, `Subscriber`, `Message` envelope (CloudEvents-compatible), middleware. No driver. |
| 04 | In-memory broker | `warren/broker/memory` | The default in tests and in a modular monolith. Built first: it is the reference the contract suite is written against. |
| 05 | Kafka driver | `warren/broker/kafka` | `twmb/franz-go`. Consumer groups, partition-aware handling, drain on shutdown. |
| 06 | Broker middleware | `warren/broker` | Retry with backoff, DLQ routing, idempotency, tracing propagation, panic recovery, concurrency limits, graceful drain — applying to every driver (PRD §6.3). |
| 07 | Transactional outbox | `warren/outbox` | Poller and CDC modes. Commits with the aggregate in one transaction (v0.2's `UnitOfWork`). |
| 08 | Inbox / dedupe | `warren/inbox` | Idempotent consumption. |
| 09 | `warren g consumer` | `warren/cli` | `--event billing.customer.created --broker kafka`. |
| 10 | `warren g proto` | `warren/cli` | `--service UserService`. |
| 11 | `cmd/worker` entrypoint | `warren/cli` | The second entrypoint in PRD §5.1, and the reason `g module` reserved `--entrypoint`. |

## Constraints already known

- **The in-memory broker is written first.** It defines the contract suite that
  Kafka then has to pass; writing Kafka first produces a port shaped like Kafka.
- **franz-go was chosen on maintenance evidence** — 6 open issues against
  `segmentio/kafka-go`'s 264 ([dependencies.md §3.7](../../docs/dependencies.md))
  — and because the outbox needs transactions and exactly-once semantics, which
  it supports and kafka-go does not fully.
- **`kgo.Client` must not appear in any public signature** (AGENT.md invariant
  2). The escape hatch is importing the driver module directly.
- **Graceful drain is a lifecycle stop hook** and depends on v0.1's reverse
  ordering guarantee: the consumer drains before the database pool closes, or
  in-flight messages fail at commit.
- **Handler-first for gRPC** (PRD §13.2). Proto-first teams are already served by
  Kratos; competing there is competing on someone else's strength.

## To settle when this milestone opens

1. **Can `Middleware[Req, Res]` genuinely be shared across transports?**
   [v0.1 #07 §11.1](../v0.1-skeleton/07-app-handler/spec.md) flags this as the
   most consequential open question in the handler design, and this is the
   milestone that answers it. If the answer is no, PRD §3.3's differentiator
   needs restating honestly.
2. **Does a consumer's `Req` come from the message body alone**, or does the
   handler need envelope metadata? Needing metadata means either leaking the
   envelope into the handler or a second interface — and the second option costs
   the "one use case, three exposures" claim.
3. **Outbox relay: poller or CDC by default?** Polling is portable and adds
   latency. CDC is Postgres-specific and operationally heavier.
4. **Is `extract module` (v0.5) still credible** given how modules actually turn
   out to communicate here? PRD §13.4 asks whether it is realistic or a demo —
   this milestone produces the evidence, and cutting it from the marketing is a
   legitimate outcome.
