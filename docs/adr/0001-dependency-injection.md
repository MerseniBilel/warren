# ADR-0001: Dependency injection — wrap `uber-go/dig`

- **Status:** Accepted
- **Date:** 2026-07-27
- **Relates to:** PRD §13.1 (open question), PRD §6.6

## Context

PRD §13.1 lists this as the structural decision to settle first, and proposes
prototyping three approaches in week 1: a `dig` wrapper, generics-based explicit
registration, and `wire` codegen.

**`google/wire` is archived.** Verified 2026-07-27: the repository is read-only,
last pushed 2025-08-22, with 108 issues left open. The compile-time codegen
option is not available, so week 1 is a two-way comparison.

The remaining forces:

- PRD §4.1 principle 2 requires the DI graph to be validated at **boot**, not at
  first request. A missing provider must kill the process on startup.
- PRD §7.4 requires `warren graph di` and `warren explain di UserRepo`. Both
  need introspection into the resolved graph, not just the ability to resolve it.
- PRD §8 makes DI error message quality a headline feature and names it "the
  single most common reason DI frameworks are abandoned."
- PRD §4.1 principle 1 says Go developers reject frameworks that hide control
  flow. Whatever we choose must not leak reflection magic into user code.
- Module imports/exports (PRD §14.1) need scoping — a module's private
  providers must not be visible to sibling modules.

`dig` was audited (see [dependencies.md §3.2](../dependencies.md)). It is
feature-complete for all of the above — `Scope` for module boundaries, value
groups for collecting controllers and consumers, `Decorate` for overrides,
`DryRun(true)` for boot validation without construction, and `Visualize()` for
DOT output. It has 2,040 importers and is the engine under `uber-go/fx`.

Its last commit is 2025-05-13 — fourteen months before this audit.

## Decision

**Use `go.uber.org/dig`, wrapped behind `warren/di`, and never expose it.**

Three binding rules:

1. **`warren/di` is the only package in the repository permitted to import
   `dig`.** Enforced by `warren lint arch` and by CI.
2. **No `dig` type appears in any Warren public signature** — not in a
   parameter, a return, a struct field, or an error type. Users of Warren must
   be able to read the entire public API without knowing `dig` exists.
3. **Warren owns error reporting.** `dig`'s errors are caught at the `warren/di`
   boundary and re-rendered as Warren errors carrying the resolution chain, the
   requesting file, and a copy-pasteable fix, per PRD §8.

`uber-go/fx` is not used. Warren owns lifecycle.

## Consequences

### What this buys

- `warren graph di` and `warren explain di` become cheap — `Visualize()` and the
  callback hooks already produce the data.
- Boot-time validation is `DryRun(true)` plus an `Invoke` of every root. A
  missing provider fails the process before the listener binds.
- Module scoping maps onto `dig.Scope` directly, so imports/exports are a real
  visibility boundary rather than a naming convention.

### What this costs

- Reflection at startup. Acceptable: it runs once, and PRD §8 budgets 50 ms of
  framework startup overhead — measured against that, not assumed.
- A dependency that is currently dormant. Registered as a risk, mitigated by
  rules 1–3 above.
- The wrapper is real work. Passing `dig` errors through unmodified would be
  cheaper and would forfeit the §8 differentiator, so it is not an option.

### What we now cannot do

- We cannot offer compile-time-verified injection. Graph errors are boot errors,
  not build errors. This is a real gap versus what `wire` offered, and it is
  closed as far as it can be by failing loudly at boot.

## Alternatives considered

**`google/wire`** — archived 2025-08-22. Would have given compile-time safety
and zero runtime reflection. Not a live option.

**`uber-go/fx`** — rejected for the reason PRD §6.6 states: it owns application
lifecycle, and Warren's lifecycle (ordered start/stop, readiness gating,
graceful consumer drain) is a product feature. Adopting `fx` would mean either
fighting it or exposing it, and its error-message quality is a known complaint
in exactly the area PRD §8 stakes a claim.

**`samber/do`** — actively maintained (v2.1.0, 2026-07-20), generics-based, and
genuinely attractive. Rejected for now because it lacks `dig`'s graph
introspection and scope model, both of which map directly onto shipped features.
**It is the designated fallback** if rule 1 ever needs to be executed — the
wrapper exists to make that swap cheap.

**Hand-written generics-based registration, no dependency** — the other half of
PRD §13.1's week-1 prototype. Still worth building as a spike, because it would
remove the dormancy risk entirely. Rejected as the default because value groups,
optional dependencies, and decorators are substantial to reimplement well, and
that effort competes directly with the three differentiators in PRD §3.3.

## Revisit when

- `dig` fails to build on a current Go release and no fix lands within 60 days.
- A CVE is filed against `dig` and goes unpatched.
- The week-1 generics spike demonstrates the full feature set in under ~800
  lines with better error messages. If so, remove the dependency and supersede
  this ADR.
- Quarterly, as part of the standing dependency re-audit.
