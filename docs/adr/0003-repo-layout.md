# ADR-0003: Multi-module repository with a zero-dependency core

- **Status:** Accepted
- **Date:** 2026-07-27
- **Relates to:** PRD §6, §6.6, §4.1 principle 5

## Context

PRD §6.6 selects a multi-module layout, citing OpenTelemetry-Go as precedent,
and PRD §4.1 principle 5 requires that "a minimal HTTP service pulls almost
nothing." PRD §12 names scope explosion as a high-severity risk — the inventory
in §6 describes roughly twenty-five packages across five concerns.

A single-module repository would be far easier to develop in: one `go.mod`, one
`go test ./...`, no version skew between packages, no release choreography. Its
cost is that every user of Warren inherits every dependency Warren has ever
taken. A service that only serves HTTP would carry the Kafka client, the
Postgres driver, and the OpenTelemetry SDK in its dependency graph.

For a framework whose pitch includes "your `go.mod` stays honest," that cost is
disqualifying.

## Decision

**Multi-module, with a core that has zero third-party dependencies — permanently
and enforced.**

```
warren/                                  module: .../warren          [stdlib only]
├── di/  lifecycle/  log/  errors/       core packages, no external imports
├── domain/  app/  health/
│
├── config/                              module: .../warren/config
├── validate/                            module: .../warren/validate
├── transport/
│   ├── http/                            module: .../warren/transport/http
│   ├── http/stdlib/                     module: ...             [stdlib only]
│   ├── http/echo/  http/gin/            module: ... (one each)
│   └── grpc/                            module: .../warren/transport/grpc
├── broker/                              module: .../warren/broker        [ports]
│   ├── memory/                          module: ...             [stdlib only]
│   └── kafka/  rabbitmq/  nats/         module: ... (one each)
├── persistence/                         module: .../warren/persistence   [ports]
│   └── postgres/  mysql/  mongo/redis/  module: ... (one each)
├── observability/  auth/  resilience/   module: ... (one each)
├── jobs/  testing/  outbox/  inbox/
├── cli/                                 module: .../warren/cli
└── mcp/                                 module: .../warren/mcp
```

Three binding rules:

1. **The core module imports nothing outside the standard library.** Not for
   convenience, not temporarily. CI fails on a non-stdlib import in the core's
   `go.mod`. If a core feature appears to need a library, the feature is split:
   the port goes in core, the implementation goes in a submodule.
2. **Ports live one level above drivers.** `warren/broker` defines `Publisher`
   and `Subscriber` and depends on nothing; `warren/broker/kafka` implements
   them. A user can write against the port without pulling any driver.
3. **A driver module depends only on its own driver.** `broker/kafka` may import
   a Kafka client. It may not import chi, pgx, or another broker.

Development across modules uses **`go.work`**, which is git-ignored. `replace`
directives are never committed — they break `go get` for users, silently.

Submodules are created **when their first real code lands**, not up front. An
empty module is a release obligation with no user.

## Consequences

### What this buys

- A minimal Warren HTTP service on the stdlib adapter has a dependency graph of
  Warren core and nothing else. That is a demonstrable claim, and it is the one
  most likely to survive contact with a sceptical Go reviewer.
- Drivers version independently. A Kafka driver fix ships without a core
  release, and a core release does not force every driver to retag.
- Community-owned drivers become tractable (PRD §12's mitigation for driver
  maintenance burden), because a driver is a module with a stable port, not a
  patch to the monorepo core.

### What this costs

- Real tooling overhead. `go test ./...` does not cross module boundaries;
  scripts must iterate modules. The Makefile and CI matrix in this repo exist
  largely to absorb that cost.
- Release choreography. A core change consumed by drivers means: tag core, bump
  drivers, tag drivers. Automated in the release workflow, but genuinely more
  complex than one tag.
- Cross-module refactors are multi-step: you cannot rename a core symbol and fix
  all callers in one atomic commit, because callers depend on a *published*
  version. `go.work` makes this bearable locally; CI catches skew.

### What we now cannot do

- We cannot make a breaking core change and fix drivers in the same commit. Core
  API changes must be additive within a major version, or staged over two
  releases. This is a discipline, and it is the main ongoing cost of this ADR.

## Alternatives considered

**Single module** — dramatically simpler to develop and release. Rejected
because it forfeits PRD §4.1 principle 5 entirely, and with it a central pitch.
A single-module Warren would have a `go.mod` no more honest than Kratos's.

**Two modules — core plus everything-else** — a middle ground with much less
choreography. Rejected because "everything else" is where all the heavy
dependencies are, so users would still pull Kafka to use Postgres. It gets the
costs of splitting without the benefit.

**Separate repositories per driver** — maximum isolation and the strongest
community-ownership story. Rejected as premature: cross-repo CI, coordinated
releases, and contribution friction are large fixed costs to pay before there
are any external drivers. A module can be promoted to its own repository later
without changing its import path if the path is chosen well; that option is
deliberately preserved.

## Revisit when

- The release choreography becomes the dominant maintenance cost.
- An external team wants to own a driver and the monorepo is what blocks them.
- The module count exceeds ~30 and the CI matrix stops being tractable.
