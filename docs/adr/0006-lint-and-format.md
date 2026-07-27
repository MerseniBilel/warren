# ADR-0006: golangci-lint v2 is the single quality gate

- **Status:** Accepted
- **Date:** 2026-07-27

## Context

Warren's pitch is that structure which is not enforced decays (PRD §1.1, §3.3).
Applying that to ourselves: a style guide nobody can run is a document nobody
follows. Quality rules must be executable, and there must be exactly one command
that runs all of them, or contributors will run a different subset than CI does
and every PR will surface a surprise.

golangci-lint v2.12.2 (2026-05-06) uses config schema `version: "2"`, which
separates **formatters** from **linters** as distinct top-level sections. That
matters here: it means one tool can own both formatting and analysis, so there
is no second tool to keep in sync.

## Decision

**`.golangci.yml` on schema version 2 is the single source of truth, and
`make lint` is the single command. CI runs exactly that command.**

### Formatting is not a matter of opinion

Formatters are configured, run with `--fix`, and never discussed in review:

| Formatter | Role |
|---|---|
| `gofmt` | The baseline. Non-negotiable. |
| `gofumpt` | Stricter superset. Removes the remaining stylistic choices. |
| `gci` | Deterministic import grouping: stdlib, third-party, then Warren. |
| `golines` | Wraps at 120 columns so diffs stay readable side by side. |

Import grouping is not cosmetic here. Warren's layering rules are about who
imports what, and a consistently grouped import block makes a violation visible
to a human reader before the linter runs.

### Linter selection principle

**Enable by default; disable with a stated reason.** The inverse — enumerating
an allowlist — means new linters never get adopted, and the ones that catch real
bugs are exactly the ones nobody remembers to add.

Every disabled linter carries a comment saying why. A disable without a reason
is a bug in the config.

### Severity tiers

1. **Correctness** — `errcheck`, `govet`, `staticcheck`, `bodyclose`,
   `rowserrcheck`, `nilerr`, `contextcheck`. Never disabled, never `//nolint`
   without a reviewed justification.
2. **Safety** — `gosec`, `noctx`, `sqlclosecheck`. A framework's bugs become
   every user's bugs.
3. **Maintainability** — `revive`, `gocritic`, `cyclop`, `dupl`. Tunable.
4. **Style** — owned by formatters, not argued in review.

### Rules specific to Warren

- **`depguard` enforces ADR-0003 and ADR-0001** — the zero-dependency core, and
  "only `warren/di` imports `dig`." These are architecture rules that happen to
  be checkable by a linter today, so they run today rather than waiting for
  `warren lint arch`.
- **`exhaustive` is on** for the semantic error codes in PRD §4.5. A transport
  adapter that forgets to map a new error code must fail the build, not fall
  through to a 500.
- **Test files relax `dupl`, `funlen`, `gosec`, and `errcheck`.** Table tests
  repeat themselves by nature, and enforcing DRY in tests makes them harder to
  read, which is the opposite of the point.
- **Generated files are excluded**, matched by the standard generated-code
  header rather than by path, so hand-written files in a `gen/` directory are
  still linted.

### `//nolint` policy

Every `//nolint` must name the linter and give a reason:

```go
//nolint:gosec // G404: this is a jittered backoff, not a security context.
```

Bare `//nolint` is itself a lint failure (`nolintlint`).

## Consequences

### What this buys

- One command locally and in CI. No drift.
- Formatting arguments end.
- Two architecture invariants are enforced from day one rather than from v0.4.

### What this costs

- A strict default set is noisy on a new codebase. Accepted — it is far cheaper
  now, with no code, than after 20,000 lines.
- `golines` at 120 columns occasionally reflows a line in a way a human would
  not have chosen. Cheaper than the alternative discussion.

## Alternatives considered

**A hand-picked minimal linter set** — less noise, faster. Rejected: it
optimises for the contributor's comfort over the user's correctness, and for a
framework those are not equally weighted.

**`gofmt` only, no `gofumpt`** — the stdlib-purist position. Rejected because
`gofumpt` closes the remaining stylistic gaps, and every gap left open becomes a
review comment eventually.

**Separate tools for format and lint** (e.g. `treefmt` + golangci-lint) —
rejected on the "one command" principle; v2's `formatters` section makes it
unnecessary.

## Revisit when

- golangci-lint ships a schema version 3.
- A linter in tier 1 produces false positives that force repeated `//nolint`.
- Lint wall-clock exceeds the CI budget on the full module matrix.
