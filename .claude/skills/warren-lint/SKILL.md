---
name: warren-lint
description: Use when checking a Warren project's architecture, when `warren lint arch` reports a violation, or when deciding how to fix a layer, cross-module, transport or driver import breach.
---

# Checking the architecture

```
warren lint arch [dir]
```

It reads the import graph and reports every package that breaks one of four
rules. It reads imports **syntactically**, so it works on a project that does
not compile — which is when it matters most, because the fix for a layer
violation usually breaks the build first.

**Exit codes: `0` clean · `1` violations found · `2` could not run.** The
three are distinct on purpose: a CI that cannot tell "could not analyse" from
"clean" has quietly stopped enforcing anything. Never collapse them, and
never `|| true` this command.

## The four rules

Each of the four is checked **directly and through a helper package** — an
in-module package that belongs to no layer and no feature. The direct form of
every rule reads one file, which makes it satisfiable by accident: move the
import into a helper and the check goes quiet while the dependency is exactly
as real. `go list -deps` still shows it, and so does this tool.

**The layer rule.** `domain` imports nothing from `application`,
`infrastructure`, or `interfaces`. The dependency arrow points inward, always.
This is what lets a domain be tested without a database, versioned on its own,
and extracted later.

**The cross-module rule.** A feature module does not reach into another
feature's internals. What crosses a module boundary is what the module
exports — a port — never a concrete type from its `infrastructure`, and never
its `domain` types pulled in sideways. **This rule finds feature modules by a
`modules` path segment**: `internal/modules/<feature>/…`, the tree `warren
new` generates. A project laid out otherwise is told so — see the disclosure
below.

**The handler/transport rule.** Nothing in `domain/` or `application/` imports
a transport package: `net/http`, `warren/transport`, chi, gin, echo, gorilla,
fiber, grpc, websocket.

**The handler/driver rule.** Nothing in `domain/` or `application/` imports a
driver: `database/sql` (including `database/sql/driver`), pgx, lib/pq,
go-sql-driver/mysql, mongo-driver, go-redis, franz-go, kafka-go, sarama,
amqp091-go, nats.go, and Warren's own adapter modules —
`warren/persistence/{postgres,mysql,mongo,redis}`,
`warren/broker/{kafka,rabbitmq,nats,memory}`, `warren/observability`.

`warren/persistence` and `warren/broker` themselves are **contract** packages
and are legal in every layer: a domain naming `persistence.UnitOfWork` is the
pattern, not the violation.

The driver list is closed and deliberately excludes cloud and SaaS SDKs — S3,
Stripe, Twilio. Those are the layer rule's business: an outbound client
belongs in `infrastructure/` behind a port, and the moment the port is
declared the layer rule catches the shortcut. An unbounded list maintained by
whoever last hit a false negative is a heuristic in a different hat.

`module.go` is exempt from all four: it is the composition root of its feature
and is *supposed* to see everything, including the driver it hands to a
repository. Nothing else in the feature is. The **controller** is unlayered
and therefore exempt too — it is precisely where a use case meets a protocol.
`infrastructure/` is exempt from the transport and driver rules by
construction: an adapter holding a driver, or calling a third-party API over
`net/http`, is the reason it exists.

## The "NOT checked" disclosure — **this is not a failure**

Every report ends with one paragraph about the cross-module rule. When the
project has no `modules` path segment, it reads:

```
  NOT checked: the cross-module rule. It compares feature modules, which it
  finds by a `modules` path segment — internal/modules/<feature>/… , the tree
  `warren new` generates — and this project has none, so nothing was compared
  across features.
```

**It does not change the exit code.** A clean project with this paragraph
exits `0`; it is a disclosure, not a finding. Do not "fix" it, do not treat a
build as red because of it, and do not report it as a violation.

What it means is narrower and more useful: half the rule set found nothing
because it had nothing to compare. Before this paragraph existed, a real
cross-feature import in an `internal/notes/{domain,application}` tree printed
"No violations" and exited 0 — a check that did not run reading as a check
that passed. If you see it and the project *does* have feature modules, they
are not where the linter looks: move them under `internal/modules/<feature>/`,
which is what `warren new`, `warren g module` and the DI diagnostics all
assume.

When features are found, the same slot reads
`Checked 3 feature modules for cross-module imports.` instead.

The tool does not infer feature roots and will not be made to: the only
plausible heuristic ("a directory whose children include `domain/` and
`application/`") fires on the project root of a single-feature app and would
report cross-module violations for imports that cross nothing. A linter that
invents the boundary it polices is worse than one that admits it found none.

## Fixing a violation, in order of preference

**domain → infrastructure.** The domain is reaching for a driver. Declare a
**port** in the domain — an interface named for what the domain needs, not for
what implements it — and put the implementation in `infrastructure`. The
generated repositories show the shape.

**domain → application.** Usually a type that is in the wrong layer. A view or
a DTO belongs in `application`; a value object or an invariant belongs in
`domain`. Move the type, do not add the import.

**handler imports a transport package.** Move the routing to the feature's
`controller.go`, which is unlayered and is exactly where a use case meets a
protocol. If the handler *calls* an external service, declare a port in the
domain and put the HTTP client in `infrastructure`.

**handler imports a driver.** Different mistake, different fix — "move the
routing to the controller" is nonsense advice for a `pgx` import. Declare a
port in the domain and put the driver code in `infrastructure`;
`warren g repository` writes both halves and the `module.go` wiring.

**the domain imports a driver.** The strong form, and the one people argue
with: it includes implementing `driver.Valuer` or `sql.Scanner` on a domain
type so it can be stored. That reads as a small convenience and it is the
whole coupling — the type now names its storage technology. The mapping
belongs in `infrastructure`, in the repository that translates between the
domain type and its columns. The domain type stays plain.

**reaches a transport package / a driver, through a helper.** The report names
the shortest chain and the package the import actually lives in. Split that
package: the part the handler needs — a tenant, an identity, a decision, a
plain value — is almost always free of the offending import, and only the edge
middleware or the store code needs it. Two packages, and the handler imports
the half without it.

**feature A → feature B's internals.** Two ways out, and they are not
equivalent:

- If A genuinely needs a *capability* B owns: B exports a port with
  `warren.Exports[...]()`, and A depends on the port — **and the port's
  interface must live in a self-contained package outside
  `internal/modules/`** (`internal/contracts/<owner>/`, declaring the
  interface and its own types and importing no feature). `Exports` alone is
  not enough: to *name* the type A has to import the package declaring it,
  and this rule refuses A importing B's packages. A port left inside B is a
  port no other feature can legally reach.
- If A is reacting to something that *happened* in B: A should consume B's
  **event**, not call into it. `warren g consumer` writes that. The event is
  a wire contract, so B stays extractable into its own service — which a
  direct Go import would destroy.

## What not to do

Do not silence it. There is no ignore file and no `//nolint` for these rules
by design: they are the invariants the framework rests on, and a project that
suppresses them has the directory layout of a Warren application and none of
the properties.

Do not move the offending file into `module.go`'s package to use the
exemption. That makes the composition root do the feature's work and hides
the coupling rather than removing it.

Do not move an import into an unlayered helper to quiet a finding. Every rule
is checked through helpers as well, so it will be reported again — with the
chain — and the dependency was never removed in the first place.
