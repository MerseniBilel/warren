# `warren/validate/playground` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved. **Blocked on its dependency audit**, which is the reason this file exists. |
| **Source** | [warren.md §2.7a](../../warren.md) |
| **Module** | own module (`warren/validate/playground`) |
| **Mode** | Wrap |
| **Wraps** | `go-playground/validator/v10` |

## Why this package exists, and why it is not in core

`warren/validate` in core holds the `Validator` port and one stdlib
implementation, `Required()`, which enforces `required` and **refuses every
other token at boot**. That is deliberate — invariant 1 makes the core module
stdlib plus `dig`, permanently, so a tag library cannot live there — and it
has a real cost, stated plainly:

> A DTO carrying `validate:"required,email"` — the example `warren.md` itself
> used until 2026-08-02 — **cannot boot** without `validate.None()`, which
> turns validation off for the entire application.

This package is what removes that cost. It implements `Validator` over
`go-playground/validator/v10`, so the full tag vocabulary works and no
`validator.ValidationErrors` value ever leaves the module.

## What it owns

Everything core deliberately does not: the tag vocabulary (`email`, `min`,
`max`, `oneof`, `dive`, …), custom constraints, tag renaming, and
translations. Compiled once per type at boot — `Plan` is a boot-step-5 call —
so the request path is a fixed walk, per invariant 7.

```go
func New(opts ...Option) validate.Validator
```

Installed with `transport.WithValidator(playground.New())`.

## Contract with core

- Errors are normalised into `errors.Invalid` with per-field details **before
  leaving this module**. The detail key rule is core's and does not change
  here: the **JSON wire name, dotted for nesting, embedded structs flattened**
  — `address.postcode`, never `Base.email`.
- The detail *value* shape is shared with core and must be settled before an
  HTTP adapter depends on it. Core's `"is required"` does not generalise to
  `min`/`oneof`, and once a 400 body is public it cannot change.
- `dive` is the token that makes element validation work. Core refuses a
  slice or map of tagged elements at boot for exactly this reason; this
  package is what lifts that refusal.

## Blocking: the dependency audit

**No dependency is adopted until someone has read its repository and
documentation and written down what they found** (AGENT.md § Adding a
dependency). `go-playground/validator/v10` has never been audited. Until it
is, this module is not created — AGENT.md also forbids empty modules ahead of
the code that fills them.

Required, recorded here with the observation date and reconciled with the
[warren.md §9](../../warren.md) ledger row:

- [ ] `gh api repos/go-playground/validator` — archived?, `pushed_at`, stars,
      open issue count.
- [ ] `gh api repos/go-playground/validator/releases/latest` — the **real**
      latest-release date, not the README's claim. The initial audit found two
      widely-recommended packages archived and neither README said so.
- [ ] Licence, and the full transitive set a service inherits.
- [ ] Whether its reflection is per-call or cached per type — this package
      promises boot-time compilation, and a library that re-walks tags per
      request cannot deliver it.

## Definition of done

- [ ] Audit above complete, recorded, and the §9 row updated.
- [ ] Spec approved.
- [ ] `New()` returns a `validate.Validator`; no `validator` type appears in
      any exported signature (invariant 3's spirit: no driver type escapes).
- [ ] Golden-file test per diagnostic.
- [ ] A cross-package test proving this and core agree on what `required`
      accepts, and on the detail-key rule — two enforcers, one tag
      vocabulary, and nothing currently checks they agree.
- [ ] Benchmark with allocation counts on the compiled rule, and a
      `testing.AllocsPerRun` assertion of an exact count (invariant 7).
- [ ] `warren.md` §2.7a amended in the same change if anything diverges.

## Open questions

1. **The detail-value shape.** Core emits the constant `"is required"`. That
   cannot generalise: `min=2` needs the constraint and probably the rejected
   value, and an i18n key is a different thing again. Decide before
   `transport/http` fixes a 400 body, because after that it is a breaking
   change to every client.
2. **Does `openapi` (§4.3) read `validate:` tags?** If it does, it will
   advertise constraints the *installed* validator may not enforce — core
   enforces only `required`. Either openapi learns which validator is
   installed, or it documents that it describes intent rather than
   enforcement. Nobody owns this today.
