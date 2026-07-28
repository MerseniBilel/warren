# Spec: <Feature name>

<!--
Copy to plan/<milestone>/<nn>-<feature>/spec.md and fill in every section.

A section with nothing to say is written as "None." — not deleted. An empty
"Errors" section is a claim that nothing can fail, and it should be visible
that the claim was made.

Delete this comment.
-->

| | |
|---|---|
| **Module** | `warren/<module>` |
| **Milestone** | v0.x |
| **Status** | Draft |
| **Depends on** | <specs that must ship first, or "Nothing"> |
| **Blocks** | <specs waiting on this> |
| **PRD** | §x.y |
| **ADRs** | <ADR-000n, or "None — this spec introduces no structural decision"> |
| **Date** | YYYY-MM-DD |

---

## 1. Problem

What a developer cannot do today, and what it costs them. Two paragraphs at
most. If this section needs more, the feature is too large — split it.

## 2. Goals

Numbered, testable statements. "Fast" is not a goal; "resolves a 200-provider
graph in under 20 ms" is.

## 3. Non-goals

What this feature will not do, and where that lives instead. This section is
what stops scope creep during implementation, so it is not optional.

## 4. Public API

The exported surface, as Go. This is the contract under review — reviewing
prose and then discovering the signatures at merge time is how a spec becomes
theatre.

```go
// ...
```

State explicitly which types cross a module boundary, and confirm that no
driver type appears (AGENT.md invariant 2).

## 5. Behaviour

Semantics, ordering guarantees, concurrency, and edge cases. Answer at least:

- What happens on the zero value?
- What is safe for concurrent use, and what is not?
- What happens on `context` cancellation?
- What is the behaviour on repeat invocation?

## 6. Errors

Every failure mode, its semantic code, and **what the message says**. PRD §8
makes error quality a feature: a message must name what failed, who asked for
it, and a copy-pasteable fix.

| Condition | Code | Message |
|---|---|---|
| | | |

## 7. Configuration

Any `warren.yaml` keys, environment variables, or functional options this
introduces, with defaults. "None." if it introduces none.

## 8. Testing

What proves this works, mapped to the tiers in `docs/testing.md`.

- **Unit** — no Docker, no network, no sleeps.
- **Golden file** — mandatory for anything that generates code.
- **Contract suite** — mandatory for a port; drivers run the same suite.
- **Integration** — behind `//go:build integration`.
- **Benchmark** — where the spec states a performance goal.

Name the specific cases that would fail if the feature regressed. "Unit tests
for the package" is not a test plan.

## 9. Invariants touched

Which of `AGENT.md`'s six invariants this feature interacts with, and how it
stays inside them. Most features touch at least invariant 1.

## 10. Definition of done

- [ ] Public API matches §4, or this spec has been updated to match the code
- [ ] Unit tests per §8, passing under `-race -shuffle=on`
- [ ] `make ci` green
- [ ] Doc comments on every exported identifier
- [ ] Documentation page written in the same pull request
- [ ] Skill added or updated (CLI commands only — ADR-0008)
- [ ] Changelog fragment (`make changelog`)
- [ ] Runnable example in `examples/`, compiled by CI (PRD §8: 100%)

## 11. Open questions

Numbered, each with who or what resolves it. A question left here at approval
time is fine; a question discovered during implementation gets added here and
answered in the same pull request.
