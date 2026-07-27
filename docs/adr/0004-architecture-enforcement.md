# ADR-0004: Architecture rules are enforced by `warren lint arch`

- **Status:** Accepted
- **Date:** 2026-07-27
- **Relates to:** PRD §3.3, §5.2, §7.4

## Context

PRD §3.3 calls architecture linting "the moat," and PRD §1.1 states the problem
it addresses: "six months in, the domain package imports `gorm`, and nothing
failed the build." Structure that is not enforced decays. Every "clean
architecture in Go" repository demonstrates this — the layering is real on day
one and folklore by month six.

Two questions had to be settled: what analyses the code, and what the rules
apply to.

`fe3dback/go-arch-lint` (526 stars, active) is the closest prior art. It is
standalone and YAML-driven: you describe your components and their allowed
dependencies in a file with no connection to your framework. Reviewed, not
adopted — see below.

## Decision

**`warren lint arch` is a first-class command, built on
`golang.org/x/tools/go/packages`, and it ships in v0.4 — not later.**

PRD §10 marks the v0.4 governance milestone "do not defer them." This ADR makes
that binding: `lint arch` is not allowed to slip past v0.4, because a framework
that promises enforcement and ships it in v0.7 has spent its credibility before
the feature arrives.

**Rules derive from module structure, and are overridable in `warren.yaml`.**
Warren already knows a project's modules and layers, because it generated them.
The default ruleset is therefore implied, not configured:

```yaml
# warren.yaml — defaults; present so they can be relaxed visibly
arch:
  layers:
    domain:         { may_import: [] }
    application:    { may_import: [domain] }
    infrastructure: { may_import: [domain, application] }
    interfaces:     { may_import: [domain, application] }
  modules:
    # a module may not reach into another module's internals
    cross_module: forbidden      # published events and generated clients only
  overrides: []                  # every relaxation is explicit and reviewable
```

Four binding properties:

1. **Zero-config correctness.** A generated project is compliant on day one with
   no `arch:` block written by hand.
2. **Relaxation is visible.** A team that wants a looser layout writes an
   override into `warren.yaml`, where it shows up in code review — rather than
   the rule quietly not existing. PRD §5.2 requires this.
3. **Violations name the fix.** Output is `file:line`, the edge that broke the
   rule, the rule that forbade it, and the two or three ways to fix it
   (introduce a port, move the type, add an override). Consistent with the PRD
   §8 error standard.
4. **Non-zero exit.** It is a CI gate, not a report.

**Warren dogfoods it.** The rules in §5.2 apply to this repository — including
ADR-0001's "only `warren/di` imports `dig`" and ADR-0003's zero-dependency core.
Until `lint arch` exists, those are enforced by the `make lint-deps` script; once
it exists, that script is replaced by the real command. If Warren's own CI
cannot run `warren lint arch` against Warren, the feature is not finished.

## Consequences

### What this buys

- The differentiator is testable. "Does it work?" is answered by a golden-file
  test over fixture projects with known violations, not by a blog post.
- Dogfooding turns the framework's own layering into the feature's test suite.

### What this costs

- `go/packages` type-checks the whole program, which is not instant. The gate
  must stay fast enough to sit in CI on every push; results are cached by
  package hash, and performance is a tracked requirement, not an afterthought.
- False positives are worse than no linter — a team that adds `//nolint` once
  adds it always. Precision is prioritised over recall: a rule that cannot be
  checked reliably is not shipped.

### What we now cannot do

- We cannot enforce anything the type checker cannot see: reflection,
  `plugin`, code generation at build time, or `go:linkname`. Documented as a
  known limit rather than papered over.

## Alternatives considered

**Depend on `fe3dback/go-arch-lint`** — real, working, and would save
substantial effort. Rejected on two grounds. It requires users to hand-write a
component map that duplicates structure Warren already knows, which forfeits
zero-config correctness. And PRD §3.3 stakes the project's central claim on this
capability; delegating it to a 526-star third-party tool makes the moat someone
else's to maintain.

**A custom `go vet` analyser** — would integrate with existing tooling for free.
Rejected because `vet` analysers see one package at a time, and the interesting
rules are about edges between packages and modules. Kept as a possible
*additional* delivery vehicle once the analysis exists.

**Import-restriction via internal packages only** — Go's `internal/` already
enforces some boundaries at compile time, for free. Adopted **in addition**, not
instead: generated layouts use `internal/` wherever it expresses the rule.
Insufficient alone, because it cannot express "domain may not import pgx."

## Revisit when

- Analysis time on a 20-module project exceeds the PRD §8 build budget.
- `go/packages` gains a cheaper whole-program mode.
- False-positive reports arrive from real users — precision problems reopen this.
