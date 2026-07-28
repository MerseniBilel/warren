# v1.0 — Stability

> **Goal (PRD §10): the API is frozen and the project is safe to depend on.**

**Specs are not written yet.** Most of this milestone is not features.

v1.0 is a promise, not a release: after it, a breaking change costs a major
version, and every user's upgrade is a project of its own. The work is
therefore mostly proving that the API deserves that promise.

---

## What v1.0 requires

| # | Item | Definition of done |
|---|---|---|
| 01 | API freeze | Every exported identifier reviewed for whether it should be exported at all. The cheapest breaking change is the one made before v1.0. |
| 02 | Semantic-versioning commitment | Written policy: what breaks a major, what a minor may add, and the deprecation window. |
| 03 | Migration tooling | `warren upgrade` migrates templates and config across framework versions — migrating, not overwriting (PRD §12). |
| 04 | Three or more production adopters | Not counting the author's. PRD §11 sets this. |
| 05 | Benchmark suite | Committed and tracked over time: startup overhead, per-request transport overhead, DI resolution. The PRD §8 targets become regressions rather than memories. |
| 06 | Complete API reference | pkg.go.dev, with every exported identifier documented and every concept carrying a runnable example. |
| 07 | Governance | PRD §12 names bus factor 1 as a high risk. A single-maintainer v1.0 is a promise one person cannot keep. |

## Constraints already known

- **`godox` is re-enabled at v1.0.** [`.golangci.yml`](../../.golangci.yml)
  disables it pre-1.0 with the note "those are a working tool, not debt to gate
  on." At v1.0 a `TODO` in shipped code becomes a defect.
- **ADR-0007 (Go version policy) is flagged as the ADR most likely to be
  superseded at v1.0.** "Track the current Go major, no compatibility path
  backwards" is defensible for a pre-1.0 framework and hostile for a v1.0
  dependency that enterprises must adopt on their own schedule.
- **Pre-1.0 breaking changes bump the minor version** under Go's semantic import
  versioning for `v0.x` ([CONTRIBUTING.md](../../CONTRIBUTING.md)). That
  freedom ends here, so use it beforehand.

## To settle before this milestone opens

1. **Does ADR-0007 survive?** See above. Deciding it at v1.0 under adopter
   pressure is deciding it badly.
2. **What is the deprecation window** — one minor, two, or a time period? It has
   to be stated before the first deprecation, not after.
3. **Which packages are `internal/`?** Anything exported at v1.0 is exported for
   years. This review is the single highest-value pre-1.0 task and the easiest
   to skip.
4. **Does event sourcing stay out of scope?** PRD §13.7 asks whether it belongs
   at all, or is a v2 module that risks defining the project as niche. A v1.0
   API freeze that later has to accommodate an `EventStore` is worth thinking
   about now.
