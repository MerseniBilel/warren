# `warren/cli/internal/arch` — honest reporting and the driver rule — SPEC

| | |
|---|---|
| **Status** | **APPROVED — code landed 2026-08-09; NOT yet retirable.** Written 2026-08-09 by architect ruling on field test #13, findings F3 and F4. Scoped to those two; the rest of `lint arch` shipped 2026-08-02 and is described by warren.md §8. Definition of done 1-5 are implemented in `arch.go`, `arch_test.go`, `testdata/*.golden` and `.claude/skills/warren-lint/SKILL.md`. **Items 6-9 — `GETTING_STARTED.md`, `README.md:51-54`, the warren.md §8 amendments, and `make ci` across all seven modules — are outstanding, and item 10 (deleting this file) must wait for them.** |
| **Source** | [warren.md §8](../../../warren.md) (line 2539), [§2.1](../../../warren.md) (line 369-380, the cross-module ruling of 2026-08-08), [README.md:51-54](../../../README.md) |
| **Module** | `warren/cli` — build-time only, never in a service's `go.mod` |
| **Mode** | Build |
| **Wraps** | — |

> **This spec amends `warren.md` §8 and `README.md`.** §8's entire enumeration
> of what `lint arch` checks is one comment line (2572: "layer and cross-module
> violations, non-zero exit"), and it has never mentioned the handler/transport
> rule that has shipped since day one. README.md:51-54 makes a claim the tool
> does not currently enforce. Both are listed at the end and land in the same
> change.

---

## Problem

Two findings, one theme: the linter is silently narrower than the documents
that sell it.

**F3 — the cross-module rule is inert in the layout `GETTING_STARTED` teaches,
and the tool does not say so.** `arch.go:71-80`, `featureOf`, finds a feature
module by a literal `modules` path segment. `GETTING_STARTED.md` prescribes
`internal/notes/{domain,application,infrastructure}` (lines 43-48, 59, 86, 110,
178, 202). In that tree `featureOf` returns `""` for every package, the
cross-module rule and its through-a-helper variant are both skipped, and a
byte-identical cross-feature violation reports:

```
No violations in 7 packages.
```

with exit 0. Move the same tree under `internal/modules/` and the same
violation gets a three-paragraph remedy. Nothing in the output says that half
the rule set did not run.

The layer rules are unaffected — `layerOf` (`arch.go:60-69`) takes the *last*
recognised segment and works in any tree — which is what makes the failure so
quiet: the tool visibly works.

Worse than the tester reported: **`GETTING_STARTED.md` contradicts itself.**
It teaches layout A (`internal/notes/…`) in §1-§5 and layout B
(`internal/modules/notes/…` plus `internal/platform/`) from §8 onward
(lines 442, 449, 457, 465, 685-686). `warren new` generates layout B; the DI
missing-provider diagnostic recommends `internal/modules/` by name
(`di/diagnostic.go:101`); warren.md §2.1's 2026-08-08 architect ruling is
written entirely in terms of `internal/modules/` and
`internal/contracts/<owner>/`. Layout A exists in exactly one document, in its
first five sections.

**F4 — `lint arch` does not enforce the README's headline claim.**
README.md:51-54: "A handler imports no transport package — *no `net/http`, no
`pgx`, no `kgo`. That is the entire point.*" `net/http` is caught. `pgx`,
`database/sql`, `warren/persistence/postgres` and `kgo` in `application/` **or
`domain/`** are all clean: `arch.go:429-440` holds only `transportPackages`,
and the handler check at `:185` gates on `isTransportPackage` alone.

The `domain` remedy makes it sharper still — `arch.go:528-530` prints "The
domain layer is the one part of the application that depends on nothing" for a
`net/http` import, while a `pgx` import in the same file says nothing at all.

---

## Goals

1. **The report says what it did not check.** Zero violations must mean "these
   rules found nothing", never "some rules did not run".
2. **A driver in `domain/` or `application/` is a violation**, by a named
   prefix list, with its own diagnostic and its own remedy.
3. **`GETTING_STARTED` teaches one layout**, and it is the one `warren new`
   generates and the one every other tool already assumes.
4. **No heuristic is added.** See the ruling below.

## Non-goals

1. **Inferring feature roots.** Ruled out; the argument is below.
2. **A configuration file or a `--feature-root` flag.** Escalated, not decided
   here.
3. **Anything requiring more than `go/parser` in `ImportsOnly` mode.** That
   choice is recorded at warren.md:2551 and 2635 and is what lets the tool run
   on a project that does not compile. It is not reopened.
4. **Cloud and SaaS SDKs in the driver list.** See "The list is closed, and
   why", below.

---

## Ruling — F3: docs plus disclosure, not inference

**In scope: the documentation, and the tool's *reporting*.**
**Out of jurisdiction: inferring feature roots.**

The standing ruling is "a linter that guesses is one people switch off", and
three linter proposals have already been declined on it. It applies here in
full, and it decides the shape.

Inferring a feature root means picking a heuristic — "a directory whose
children include `domain/` and `application/` is a feature" is the only
plausible one — and that heuristic fires on `internal/notes/` **and on the
project root of a single-feature application whose layers sit directly under
`internal/`**. In that project every package becomes a member of one inferred
feature, or of several, depending on where the walker started; a correct layout
would then produce cross-module findings for imports that are not
cross-anything. A linter that invents the boundary it then polices is worse
than one that admits it found no boundary.

So the tool does not guess. It says what it did.

The documentation is where the actual defect is. `warren new` generates
`internal/modules/`; `di`'s missing-provider diagnostic instructs users in
terms of `internal/modules/`; warren.md §2.1's architect ruling of 2026-08-08
is written in terms of `internal/modules/` and `internal/contracts/<owner>/`;
and `GETTING_STARTED` itself switches to `internal/modules/` at §8. One
document's first five sections teach a second, undocumented convention, and
that convention is the one in which the linter goes quiet. **Fix the
document.**

**Escalated to the product owner, as their call and not mine:** whether
`warren lint arch` should later gain a way to name the feature root explicitly
— a `--feature-root` flag or a `warren.yaml` key. It is a genuine question:
`internal/modules/` is a convention Warren invented, and a team that adopts
Warren into an existing tree may not be able to move to it. But it is a
decision about **how opinionated the CLI is**, not about correctness, and it is
against the grain of `arch.go:63-64`'s stated design ("configuration would be a
barrier to a linter's first run"). Nothing in this round depends on the answer.

## Ruling — F4: build the check, and narrow the README's wording

**In scope: a driver rule, by named prefix, direct and through a helper.**
**Also in scope: one wording correction to README.md.**

This is not the guessing case. `transportPackages` (`arch.go:429-440`) is
already an explicit, closed, named list with a comment saying why it is not a
heuristic. A `driverPackages` list is the same construct pointed at the other
half of the sentence the README actually makes. Declining it would leave the
README claiming an enforcement the tool does not perform, and AGENT.md's
governing constraint — "Every boundary below is enforced by the same
`warren lint arch` that ships to users" — is the reason the claim was made in
the first place.

**But the README's wording is a category error and must be fixed with it.** It
calls `pgx` and `kgo` "transport packages". They are drivers. The linter now
has two rules with two different remedies — "move the routing to the
controller" is nonsense advice for a `pgx` import — and the README should name
both:

> A handler imports no transport and no driver — no `net/http`, no `pgx`, no
> `kgo`. That is the entire point.

That is a narrowing of the sentence, not a retraction. The claim survives
because the check lands.

## Ruling — the list is closed, and why

The v0.1 `driverPackages` list is SQL and NoSQL drivers, message-broker
clients, and Warren's own adapter modules. **Cloud and SaaS SDKs are
deliberately excluded** — `aws-sdk-go-v2`, `cloud.google.com/go`, Stripe,
Twilio and the rest. A domain package importing the S3 SDK is the same
violation in principle, and the list of SaaS clients is unbounded. An unbounded
list maintained by whoever last hit a false negative *is* the heuristic, wearing
a different hat: it would be permanently incomplete, and its incompleteness
would be indistinguishable from approval.

The rule that catches those is the layer rule — an outbound client belongs in
`infrastructure/` behind a port, which warren.md:2394-2397 and 2500-2502
already assert is "where `warren lint arch` already sends it" — and it catches
them the moment the port is declared in the domain and implemented in
infrastructure. **Escalated to the product owner:** whether a later round adds
an opt-in `--strict-outbound` mode that treats *any* non-stdlib, non-project
import in `domain/` as a finding. That is a policy question about how much a
first run may say, and it is theirs.

`database/sql/driver` **is** in scope, by prefix under `database/sql`, and this
is deliberate. The legitimate-looking minority — a domain value object
implementing `driver.Valuer` so it can be stored — is exactly the coupling
Warren argues against: the mapping belongs in `infrastructure/`. The remedy
text names that case explicitly rather than leaving the reader to guess why
their `Money` type is being reported.

---

## Public API as Go

```go
// Report is the result of a check.
type Report struct {
	Violations []Violation
	Packages   int

	// Features is the number of distinct feature modules found. Zero means
	// the cross-module rule compared nothing, which the report says out
	// loud: a check that did not run must never read as a check that
	// passed.
	Features int
}
```

`Violation` is unchanged. `Options`, `RuleSet`, and `Check` are unchanged.

Two unexported additions, mirroring the transport rule exactly:

```go
// driverPackages are the import prefixes that make a package a driver. The
// list is explicit rather than heuristic, for the same reason
// transportPackages is: a linter that guesses is one people switch off.
var driverPackages = []string{ /* the closed list below */ }

func isDriverPackage(path string) bool
```

New `Violation.Rule` values: `"driver"` and `"driver-chain"`.

---

## Behaviour

### B1 — the driver rule

Gated on `handlerLayers` (`arch.go:424-427`: `domain`, `application`) exactly
as the transport rule is. `infrastructure/` is exempt by construction — an
adapter holding a driver is the reason it exists — and the controller is
unlayered and therefore exempt.

The v0.1 list:

```
database/sql
github.com/jackc/pgx
github.com/lib/pq
github.com/go-sql-driver/mysql
go.mongodb.org/mongo-driver
github.com/redis/go-redis
github.com/twmb/franz-go
github.com/segmentio/kafka-go
github.com/IBM/sarama
github.com/rabbitmq/amqp091-go
github.com/nats-io/nats.go
github.com/MerseniBilel/warren/persistence/postgres
github.com/MerseniBilel/warren/persistence/mysql
github.com/MerseniBilel/warren/persistence/mongo
github.com/MerseniBilel/warren/persistence/redis
github.com/MerseniBilel/warren/broker/kafka
github.com/MerseniBilel/warren/broker/rabbitmq
github.com/MerseniBilel/warren/broker/nats
github.com/MerseniBilel/warren/broker/memory
github.com/MerseniBilel/warren/observability
```

`github.com/MerseniBilel/warren/persistence` and
`github.com/MerseniBilel/warren/broker` are **not** on it and must not be: they
are contract packages, and a domain naming `persistence.UnitOfWork` is the
pattern, not the violation. The prefix matcher must therefore match on
`path == p || strings.HasPrefix(path, p+"/")` — the same function
`isTransportPackage` uses — so that listing `…/persistence/postgres` never
catches `…/persistence`.

### B2 — the driver rule through a helper

The direct rule reads one file, which makes it satisfiable by accident: move
the import into an unlayered helper and the check goes quiet while the
dependency is exactly as real. That is not hypothetical — it is the shape
`findTransportChains` (`arch.go:349-…`) was written for, after a field test's
application layer reached `net/http` across 19 packages and lint said nothing.
The driver rule gets the same treatment, through the same helper-only
traversal, reporting the same shortest chain.

The implementation must **generalise** `reachesTransport` to take a predicate
rather than gain a near-identical twin. Two breadth-first searches that differ
by one function call is how the two rules drift apart.

### B3 — the disclosure

`Report.Features` counts distinct non-empty `featureOf` results across the
walk.

`Report.String()` with **no violations**:

```
No violations in 7 packages.

  Checked: the layer rule, the handler/transport rule and the handler/driver
  rule — each directly and through a helper package.

  NOT checked: the cross-module rule. It compares feature modules, which it
  finds by a `modules` path segment — internal/modules/<feature>/… , the tree
  `warren new` generates — and this project has none, so nothing was compared
  across features.
```

With **violations**, the same paragraph follows the count:

```
3 violation(s) in 7 packages.

  NOT checked: the cross-module rule. …
```

When `Features > 0` the "NOT checked" paragraph is replaced by one line:

```
  Checked 3 feature modules for cross-module imports.
```

**Exit code is unchanged.** Zero violations exits 0 whether or not the
cross-module rule ran. This is a disclosure, not a failure: a tool that starts
failing builds because of how a directory is named would be switched off, which
is the outcome every ruling in this file is written to avoid.

### B4 — the diagnostics

`✗ handler imports a driver` — `Layer == "application"`:

```
✗ handler imports a driver

    internal/modules/orders/application/place.go:9
      package example.com/svc/internal/modules/orders/application   (layer: application)
        imports github.com/jackc/pgx/v5

  A use case says WHAT must happen, never HOW it is stored or delivered. The
  moment it names pgx it can only run where a Postgres pool can be built: not
  in a unit test, not behind a second driver, not in another service.

  That rule is what makes app.Handler[Req, Res] the same type on HTTP, gRPC
  and a consumer, and what lets warren/persistence/postgres be swapped for
  another driver in one line of main.go.

  Fix:
    • Declare a PORT in the domain — an interface in the domain's own words —
      and put the pgx code in infrastructure, where a driver is allowed. Wire
      the two in module.go, the one file permitted to see all four layers.
      `warren g repository` writes both halves.
```

`✗ the domain imports a driver` — `Layer == "domain"`. Reuses the sharper
domain wording that `arch.go:526-539` already prints for the transport rule:

```
✗ the domain imports a driver

    internal/modules/orders/domain/money.go:6
      package example.com/svc/internal/modules/orders/domain   (layer: domain)
        imports database/sql/driver

  The domain layer is the one part of the application that depends on
  nothing — not the other layers, not a protocol, and not a database. A
  domain type that knows how it is stored cannot be tested without the
  store, versioned apart from it, or moved into another service.

  This includes implementing driver.Valuer or sql.Scanner on a domain type.
  It reads as a small convenience and it is the whole coupling: the type now
  names its storage technology.

  Fix:
    • Declare a PORT in the domain and put the mapping in infrastructure,
      where database/sql is allowed. A repository that translates between the
      domain type and its columns is the pattern; the domain type stays plain.
```

`driver-chain` reuses the `transport-chain` structure verbatim
(`arch.go:540-554`), substituting "driver" for "transport package" and naming
the offending hop and the split remedy.

---

## Testing

**Every one of these must fail before the fix and pass after.**

**F3 — disclosure**

1. `TestZeroFeaturesIsDisclosed` — a fixture tree under
   `internal/notes/{domain,application,infrastructure}` with a real
   cross-feature import; assert the report contains "NOT checked: the
   cross-module rule" and `Features == 0`.
2. `TestFeatureCountIsReportedWhenPresent` — the same tree under
   `internal/modules/`; assert `Features == 2` and the one-line "Checked 2
   feature modules" form.
3. `TestDisclosureAppearsWithViolationsToo`.
4. `TestDisclosureDoesNotChangeExitCode` — zero violations still exits 0.
5. Golden files for all three report shapes.

**F4 — the driver rule**

6. `TestPgxInApplicationIsAViolation` — **the F4 regression test.**
7. `TestPgxInDomainGetsTheDomainRemedy`.
8. `TestDatabaseSQLDriverInDomainIsAViolation` — the `driver.Valuer` case,
   with the remedy naming it.
9. `TestWarrenPersistencePostgresInApplicationIsAViolation`.
10. `TestWarrenPersistenceContractPackageIsNotAViolation` — the prefix-matching
    guard: `…/persistence` and `…/broker` must stay clean in every layer.
11. `TestDriverInInfrastructureIsClean`.
12. `TestDriverInControllerIsClean` — unlayered.
13. `TestDriverThroughAHelperIsAViolation` — application → `internal/store` →
    pgx; assert the reported line is the handler's own import of the helper
    and the chain is the shortest one.
14. `TestDriverChainReportsShortestPath`.
15. Golden files for each diagnostic.

**Regression**

16. The existing `lint arch` golden files pass unchanged except where B3 adds
    the new paragraph — and every such change is reviewed as a deliberate
    diff, not regenerated in bulk.

No Docker, no network. Fixture trees under `testdata/`, which `arch.go:120-124`
already skips when walking.

---

## Definition of done

1. `Report.Features` exists and is populated.
2. B3's three report shapes ship with golden files.
3. `driverPackages`, `isDriverPackage`, the `driver` and `driver-chain` rules,
   and their two remedies ship with golden files.
4. `reachesTransport` is generalised, not duplicated.
5. `.claude/skills/warren-lint/SKILL.md` is updated: it is a CLI command and
   AGENT.md § What a skill is makes the skill a hard deliverable. It must
   describe the driver rule, both remedies, and — most importantly — the
   "NOT checked" disclosure, because an agent that sees it must know it is not
   a failure.
6. `GETTING_STARTED.md` teaches one layout. See below.
7. `README.md:51-54` is narrowed as ruled.
8. `warren.md` §8 carries the amendments below.
9. `make ci` green across all seven modules, quoted.
10. This spec is **deleted** in the same change.

---

## What warren.md must say

1. **§8, line 2572** — replace the one-line comment with the actual rule list:
   the layer rule, the cross-module rule, the handler/transport rule and the
   handler/driver rule, each direct and through a helper; and a sentence that
   the cross-module rule requires a `modules` path segment and says so when it
   finds none.
2. **§8** — one line that `lint arch` exits non-zero only on a violation, and
   that a disclosure is not a violation.
3. **§8** — a line stating that `internal/modules/<feature>/` is the layout
   `warren new` generates and the layout the cross-module rule recognises,
   with a pointer to §2.1's 2026-08-08 ruling. Note the tension with line 38
   ("that is `warren new`'s concern, documented separately"): the manifest need
   not own the app layout, but the linter's own requirement is the linter's
   business and belongs in §8.
4. **§1.1 / line 40** — "Every boundary below is enforced by the same
   `warren lint arch` that ships to users" is the claim F4 falsified. It
   becomes true for drivers when this lands. It remains **false for the ring
   rules** (`RuleSet.Rings`) applied to Warren's own tree in any complete
   sense — that is a separate audit and should not be quietly assumed by this
   change.

## What README.md must say

- **Lines 51-54** — as ruled: "A handler imports no transport and no driver —
  no `net/http`, no `pgx`, no `kgo`. That is the entire point." The current
  wording calls drivers transport packages, and the two rules have two
  different remedies.

## What GETTING_STARTED.md must say

- **One layout, everywhere.** Convert §1-§5 to `internal/modules/notes/…`:
  lines 43-48 (the tree), 59 (`mkdir -p`), 86, 110, 178, 202 (the four
  headings), 120 (the import path), 770 (the boot diagnostic's quoted path).
  §8 already uses it (442, 449, 457, 465, 685-686).
- **Line 202's file name** — layout B puts the module value in `module.go`
  (line 457) and layout A puts it in `controller.go` (line 202, `var Module` at
  232). Pick one. `warren new` and `warren g module` generate `module.go`.
- **`cmd/notes/main.go` (48, 59, 262) vs `cmd/app/main.go` (465)** — pick one.
  `warren new` generates `cmd/<app>/main.go`.
- **Line 445-446 vs 450/460/466** — the prose says `warren g module` "expects
  `platform.Module()` to exist"; the example declares `var Postgres` and calls
  `platform.Postgres()`. One of the two is wrong; check the generator and fix
  the other.
- **Lines 51-52 and 681** — the two `warren lint arch` mentions. Add the
  cross-module rule and the driver rule to what the page says the tool checks,
  and add one sentence that a project not under `internal/modules/` is told so
  in the output.

---

## Open questions

1. **`--feature-root` / `warren.yaml`** — escalated above; the product owner's
   call, not this spec's. **Rehome to:** `cli/SPEC.md`, which already carries
   the CLI's open questions.
2. **`--strict-outbound`** — escalated above. **Rehome to:** `cli/SPEC.md`.
3. **The `Rings` rule set applied to Warren's own repository** has never been
   audited against warren.md §1.1, and `scripts/invariants.sh` covers only what
   grep can see. Whether `lint arch --rings` actually enforces the four rings
   on this tree is unknown and is not answered here. **Rehome to:**
   `cli/SPEC.md`.
