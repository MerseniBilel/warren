# Spec: DI approach spike

| | |
|---|---|
| **Module** | None — throwaway code, deleted when the decision is recorded |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | Nothing |
| **Blocks** | [03-di](../03-di/spec.md), and therefore the whole critical path |
| **PRD** | §13.1, §4.1, §7.4, §8 |
| **ADRs** | [ADR-0001](../../../docs/adr/0001-dependency-injection.md) — this spike is its stated revisit trigger |
| **Date** | 2026-07-28 |

---

## 1. Problem

PRD §13.1: *"DI: `dig` wrapper vs. generics-based explicit registration vs.
`wire` codegen? Leaning `dig` + generic sugar. Prototype all three in week 1 —
this decision is structural and hard to reverse."*

It is hard to reverse because every module definition, every generated
`module.go`, and the shape of `warren.New(...)` sit on top of it. Changing it
after v0.2 means rewriting every generated project in the wild.

`google/wire` is archived (verified 2026-07-27, last push 2025-08-22, 108 open
issues — [dependencies.md §3.2](../../../docs/dependencies.md)). So this is a
two-way comparison, not three. ADR-0001 already chose the `dig` wrapper and
named its own revisit criterion: *"the week-1 generics spike demonstrates the
full feature set in under ~800 lines with better error messages."* This spike
tests that criterion rather than assuming it fails.

## 2. Goals

1. Produce enough of each approach to compare them on **evidence**, not
   preference.
2. Settle it in **five working days**. See §5 — the timebox is the design.
3. Leave a written decision: ADR-0001 confirmed, or superseded by ADR-0009.

## 3. Non-goals

- Shipping either prototype. Both are deleted; [03-di](../03-di/spec.md) is
  written afresh against the chosen approach.
- Comparing on runtime resolution speed. Both resolve at boot, once, inside a
  50 ms budget (PRD §8). Speed is not the discriminator and measuring it first
  would be measuring the easy thing.
- Re-evaluating `fx` or `samber/do`. ADR-0001 disposed of both with reasons; if
  the spike changes those reasons it says so explicitly.

## 4. Public API

None — the spike is throwaway. Both prototypes implement the **same fixture**,
so they are compared on the same problem:

```go
// The fixture both prototypes must satisfy. Chosen because each line
// corresponds to a feature v0.1 or v0.4 actually needs.
//
//  1. Constructor providers  func(deps...) (T, error)          — PRD §4.2
//  2. Interface binding      NewPostgresUserRepo -> UserRepo    — PRD §14.1
//  3. Module scoping         module-private providers invisible
//                            to siblings unless exported        — PRD §14.1
//  4. Value groups           collect all Controllers, all
//                            Consumers                          — PRD §14.1
//  5. Boot validation        missing provider kills the process
//                            before the listener binds           — PRD §4.1
//  6. Graph introspection    nodes and edges as plain data, for
//                            `warren graph di` / `explain di`    — PRD §7.4
//  7. Decoration/override    replace a provider in tests         — PRD §9
//  8. Cycle detection        named, with the full cycle printed
```

## 5. Behaviour

**Timebox: five working days, both prototypes built by the same person against
the same fixture.** If the generics prototype has not met the ADR-0001 bar by
day five, ADR-0001 stands and the spike ends. This is deliberate: an open-ended
comparison of a working option against a hypothetical one always favours the
hypothetical, because the hypothetical has no bugs yet.

**Prototype A — `dig` wrapper.** Wrap `go.uber.org/dig` per ADR-0001's three
binding rules. The work is almost entirely in error translation: catching
`dig`'s errors at the `warren/di` boundary and re-rendering them per PRD §8.

**Prototype B — hand-written generics container.** No third-party dependency.
`Provide[T]` / `Resolve[T]` over a type-keyed registry, topological sort for
ordering, explicit cycle detection.

Both must handle the fixture's item 5 identically: **a missing provider fails
the process at boot**, never at first request (PRD §4.1 principle 2).

## 6. Errors

The error messages are the primary output of this spike, not a side effect. For
each of these cases, both prototypes produce a message, and the messages are
compared side by side in the ADR:

| Condition | What the message must contain |
|---|---|
| Missing provider | The missing type, the full resolution chain that requested it, the file and line of the requesting provider, and a copy-pasteable `warren.Provide(...)` line |
| Ambiguous provider | Every candidate with its source location, and how to disambiguate |
| Cycle | The complete cycle as a chain, not "cycle detected" |
| Constructor returned an error | The constructor's name and file, with the underlying error wrapped via `%w` |
| Wrong arity or non-function provided | What was passed, what was expected, and the file that passed it |

PRD §8: *"a missing provider prints the resolution chain, the file that
requested it, and a copy-pasteable fix. This is a first-class feature, not
polish — it is the single most common reason DI frameworks are abandoned."*

## 7. Configuration

None.

## 8. Testing

The fixture in §4 is the test suite, written **once** and run against both
prototypes through a shared interface that exists only in the spike. Both
prototypes pass the same tests or the comparison is not a comparison.

Error messages are captured as **golden files** so the ADR quotes real output
rather than a paraphrase of what the output would be.

## 9. Invariants touched

- **Invariant 1 (core is stdlib-only).** Prototype B would satisfy it trivially;
  prototype A is why `warren/di` may need to be a separate module. See the open
  question in [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md) §11
  — the spike must record which prototype makes that question disappear.
- **Invariant 2 (no driver type in a public signature).** Prototype A must
  demonstrate this holds through the wrapper, including in error types.

## 10. Definition of done

- [ ] Both prototypes pass the §4 fixture
- [ ] Golden files capture every §6 message from both
- [ ] Line counts recorded for each, measured on comparable feature coverage
- [ ] A decision written: ADR-0001 confirmed with a note, or ADR-0009 supersedes it
- [ ] [dependencies.md §5](../../../docs/dependencies.md) open item "PRD §13.1 week-1 prototype" closed
- [ ] Both prototypes deleted from the repository
- [ ] [03-di](../03-di/spec.md) updated to match the decision, and moved to `Approved`

No changelog fragment: nothing user-visible ships (ADR-0005).

## 11. Open questions

1. **Does prototype B's error output actually beat prototype A's?** This is the
   whole question. `dig`'s messages are poor, but prototype A *re-renders* them
   — so the comparison is between two things Warren wrote, and B's advantage may
   be smaller than it looks from the outside.
2. **Can prototype B do value groups and decoration well?** ADR-0001 flags these
   as "substantial to reimplement well." If B needs 800 lines for items 1–3 and
   another 600 for items 4 and 7, that answers the question by itself.
3. **How much does the dormancy risk actually weigh?** `dig`'s last commit is
   2025-05-13. It is feature-complete and has 2,040 importers, so dormant is not
   the same as abandoned — but it is the one argument for B that does not depend
   on B being good.
