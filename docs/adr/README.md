# Architecture Decision Records

An ADR records a decision that was expensive to make and would be expensive to
re-make. It exists so that six months from now nobody re-opens a settled
question without new information, and so that when new information *does*
arrive, we can see exactly which reasoning it invalidates.

## When to write one

Write an ADR when a decision is **structural** — when reversing it would mean
changing many files, breaking users' import paths, or re-testing a subsystem.

Write one when:

- Adopting, rejecting, or replacing a dependency that appears in a public API
- Changing a layering rule, a port shape, or a module boundary
- Choosing between two designs where the loser was genuinely defensible
- Overturning an earlier ADR

Do **not** write one for reversible choices — a helper's name, a test's
structure, an internal refactor. Those belong in the commit message.

## Status lifecycle

| Status | Meaning |
|---|---|
| `Proposed` | Written, not yet ratified. Implementation may not depend on it. |
| `Accepted` | Ratified. Implementation follows it. Changing it needs a new ADR. |
| `Superseded by ADR-NNNN` | Overturned. Kept for the record; never deleted. |
| `Deprecated` | No longer applies and nothing replaced it. |

An ADR is never edited to change its decision. It is superseded by a new one
that states what changed. The old file stays, because the reasoning that turned
out to be wrong is the most useful thing in the directory.

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-dependency-injection.md) | Dependency injection: wrap `dig` | Accepted |
| [0002](0002-http-router-port.md) | HTTP router port shaped on `net/http`, chi as default | Accepted |
| [0003](0003-repo-layout.md) | Multi-module repository with a zero-dependency core | Accepted |
| [0004](0004-architecture-enforcement.md) | Architecture rules enforced by `warren lint arch` | Accepted |
| [0005](0005-commits-and-changelog.md) | Conventional Commits and changie fragments | Accepted |
| [0006](0006-lint-and-format.md) | golangci-lint v2 as the single quality gate | Accepted |
| [0007](0007-go-version-policy.md) | Track the current Go major release | Accepted |
| [0008](0008-agent-integration.md) | Ship a skill per command and an MCP server | Accepted |

## Template

```markdown
# ADR-NNNN: <short imperative title>

- **Status:** Proposed
- **Date:** YYYY-MM-DD
- **Supersedes:** — | **Superseded by:** —

## Context
What forces are at play? What did we observe? Cite evidence — versions,
dates, measurements, links. State the constraint that makes this hard.

## Decision
What we are doing, stated so a reader can act on it without reading further.

## Consequences
### What this buys
### What this costs
### What we now cannot do

## Alternatives considered
For each: what it was, and the specific reason it lost. A rejected option
with no stated reason will be re-proposed within the year.

## Revisit when
The concrete signal that should reopen this. "Never" is a valid answer;
"when we have time" is not.
```
