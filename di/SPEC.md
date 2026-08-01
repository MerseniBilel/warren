# `github.com/MerseniBilel/warren/di` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — implemented; the three conditions (Open questions 1–3) were settled the same day, the golden diagnostic reproduces byte for byte, and warren.md §2.2 was amended to carry the settled surface |
| **Source** | [warren.md §2.2](../warren.md) |
| **Module** | core |
| **Mode** | Wrap |
| **Wraps** | `go.uber.org/dig` |

## Problem

`dig` and `fx` give you one global type-keyed container: register `*UserRepo`
and every component in the process can inject it — there is no module boundary
(§1.2). And when wiring is wrong, the user reads `missing dependencies for
function ...: missing type: *postgres.Pool`, which names neither who asked for
the type, nor where it was declared, nor what to type to fix it. Warren's stated
DX target is that *a missing provider prints a copy-pasteable fix*, and that is
unreachable while surfacing someone else's diagnostics.

## Goals

- Own the container, scoping, graph validation, and diagnostics (§2.2).
- Give module boundaries a mechanism: **a child container per module**, with
  only exported bindings copied in (§1.2, boot step 2).
- Validate the whole graph before anything is instantiated — every dep
  resolvable, nothing ambiguous, nothing unused (boot step 3). Step 3 is why a
  missing provider is a startup crash with a full resolution chain and not a
  nil-pointer panic in production (§1.3).
- Produce the diagnostic in **Errors** below, verbatim. That block of text is the
  deliverable (AGENT.md invariant 2).
- `Explain` powers `warren explain di` (§2.2, §8).
- Keep the wrap boundary absolute: **users never import `go.uber.org/dig`**.

## Non-goals

- **Not fx.** dig is v1 with strict SemVer and is explicitly designed to power
  application frameworks. Fx would impose *its* lifecycle, and Warren needs
  readiness gating and drain ordering that fx does not model. More decisively:
  "a missing provider prints a copy-pasteable fix" is a stated DX target, and it
  cannot be hit while surfacing someone else's diagnostics (§2.2, §9).
- **Not a compile-time wiring generator.** The scoped-container design assumes
  runtime scoping; `wire`-style codegen would require redesigning the model
  (§1.2).
- **Not consulted at request time.** Reflection runs during boot steps 1–5 only;
  by step 8 the route table holds pre-built closures (§1.4, invariant 7).
- **Not a place where dig leaks.** No `dig.Container`, `dig.Scope`, `dig.In`,
  `dig.Out` or `dig.ProvideOption` in any exported function, method, struct
  field, or error (invariant 2).
- **Not the module declaration.** `warren.NewModule` and its options live in the
  root package (§2.1).

## Public API

`warren.md` §2.2 fixes the following surface. Doc comments are added here; no
signature is changed.

```go
// Container is a dependency-injection scope. The root container holds
// process-wide bindings (config, logger, tracer); each module gets a child
// container via Scope, and a child sees only the bindings its imports export.
type Container interface {
    // Provide registers a constructor in this scope.
    Provide(constructor any, opts ...ProvideOption) error

    // Invoke resolves fn's parameters from this scope and calls it.
    Invoke(fn any) error

    // Scope returns a child container named name. The child is the module
    // boundary: bindings registered in it are private to it unless exported.
    Scope(name string) Container

    // Validate checks the whole graph reachable from this scope: is every
    // dependency resolvable, is anything ambiguous, is anything unused.
    // It is boot step 3 and runs before any singleton is instantiated.
    Validate() error

    // Explain reports how target would be resolved from this scope. It powers
    // `warren explain di`. What Resolution carries is not fixed by warren.md —
    // see Open question 2.
    Explain(target any) Resolution
}

// ProvideOption configures a single Provide call. It is Warren's own type: no
// dig option type may appear in a Warren signature.
type ProvideOption // representation not fixed by warren.md — see Open questions

// Resolution is the result of Explain.
type Resolution // representation not fixed by warren.md — see Open questions

// Resolve resolves T from c.
func Resolve[T any](c Container) (T, error)

// MustResolve resolves T from c and fails if it cannot.
func MustResolve[T any](c Container) T
```

The settled additions (2026-08-01, warren.md §2.2 amended in the same change):
`New() Container` constructs the root; `ProvideOption` is a functional option
with two constructors — `Exported()` (marks the binding visible to importing
modules; read by the bootstrapper's copy-in and by the diagnostics) and
`DeclaredAt(file, line)` (the module declaration site, captured by the root
package at `NewModule` time — the "declared in module.go:14" line);
`Resolution` is a self-rendering tree — `Target`, `Found`, `Provider`,
`Scope`, `Site`, `Inputs []Resolution` — whose `String()` is the output of
`warren explain di`.

**The wrap boundary.** `go.uber.org/dig` is imported by this package and by
nothing else in the repository (invariant 2). Users never import dig.

## Dependency audit

**`go.uber.org/dig`, observed 2026-08-01** via `gh api repos/uber-go/dig`:
not archived, MIT licence, 4,486 stars, 33 open issues, last push 2025-05-13;
latest release v1.19.0 (2025-05-13). Matches the §9 ledger row (v1, strict
SemVer, built to power frameworks). Transitive footprint: zero runtime
dependencies — dig's `go.mod` requires only test libraries (`stretchr/testify`
and friends), which land in `go.sum` but not in the build. Mode: Wrap.

## Behaviour

### Boot steps 1–4

| Step | This package's part |
|---|---|
| 1 flatten module graph | Resolve imports, detect cycles → fail. The line between the root package's graph walk and this package is not fixed by `warren.md` — see Open questions. |
| 2 build scopes | One child `Container` per module via `Scope(name)`; copy in the exported bindings of each imported module and nothing else. |
| 3 VALIDATE GRAPH | `Validate()` — every dep resolvable? ambiguous? unused? → fail. |
| 4 instantiate | Singletons, topological order. |

Step 3 runs to completion **before** step 4: no singleton is constructed until
the graph is known good. This ordering may not be rearranged (AGENT.md § Two
orderings you may not rearrange).

### Scoping is the module boundary (§1.2)

```
                    root container
                (config, logger, tracer)
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
   platform scope    user scope        billing scope
   ├ *pgxpool.Pool   ├ *UserService    ├ *InvoiceService
   ├ broker.Conn     ├ UserRepository  ├ InvoiceRepo
   └ exports: both   └ exports:        └ imports: platform, user
                       UserRepository        ↳ sees UserRepository
                                             ↳ CANNOT see *UserService
```

A binding registered in a scope is private to that scope unless the module
exported it. Resolution from a scope searches the scope itself and the exported
surfaces of its imports — not the whole process.

### Not on the request path

The container is not consulted at request time (§1.4, invariant 7). Everything
this package does happens during boot steps 1–4.

## Errors

**The diagnostics are the product.** Raw dig produces:

```
missing dependencies for function ...: missing type: *postgres.Pool
```

Warren must produce exactly this instead — this block is the golden-file target:

```
✗ cannot resolve dependency

    domain.UserRepository
      └─ required by *user.RegisterUserHandler
           └─ required by *user.UserController
                └─ declared in internal/modules/user/module.go:14

  No provider found in scope "user" or its imports.

  Did you mean:
    • postgres.NewUserRepository is registered in scope "billing" but not exported.
      Add to billing's module: warren.Exports[domain.UserRepository]()
    • Or provide it locally:  warren.Providers(postgres.NewUserRepository)
```

Anything that leaks dig's error text through to a user is a bug, not a shortcut
(AGENT.md invariant 2).

The constituent parts, each of which the implementation must be able to produce:

| Part | Content |
|---|---|
| Header | `✗ cannot resolve dependency` |
| Chain | The unresolved type, then each requester in turn, indented, ending at the declaration site as `file:line` |
| Verdict | `No provider found in scope "<scope>" or its imports.` |
| Suggestion — unexported | Names the constructor, the scope it is registered in, and the exact `warren.Exports[T]()` line to add, naming the module to add it to |
| Suggestion — local | The exact `warren.Providers(...)` line to add |

Other errors this package produces, whose text `warren.md` does not fix —
agreed 2026-08-01, all rendered in `di/diagnostic.go` and covered by tests:

| Condition | Text |
|---|---|
| Ambiguous binding (Provide time for same-scope duplicates, step 3 across scopes) | `✗ ambiguous binding` — names the type, the count, the asking scope, and every provider with its scope and position; ends with "Keep one, or move the extra binding into the module that needs it." |
| Unused provider (step 3) | **Deferred with Open question 4** — `Validate` does not yet check unused. |
| Constructor returned an error (step 4) | `✗ constructor failed` — presents the constructor's own error verbatim and wraps it (`Unwrap`), so `errors.Is`/`As` reach the cause. The cause is recovered via `dig.RootCause`; dig's framing is discarded. |
| Provider cycle (Provide time, pre-empting dig; re-checked at step 3) | `✗ dependency cycle` — the loop of types joined with `→`, then "Break the loop: one of these constructors must stop depending on the other." |
| `Provide`/`Invoke` on a non-function or a func with no outputs | `✗ not a constructor` — says what a constructor is and what was got. |
| Variadic constructor | `✗ variadic constructor` — names it and says each parameter must be a single concrete requirement. |
| Unanticipated dig failure | `✗ container failure` — sanitized, names the constructor, asks for a bug report. Deliberately carries no third-party wording. |
| `MustResolve` failure | Panics with the resolution diagnostic — the kernel's one sanctioned panic; see Open question 1. |

## Testing

- **Golden-file test for the block above.** The full diagnostic, byte for byte,
  produced from a fixture graph shaped like §1.2's (`user` requires
  `domain.UserRepository`; `postgres.NewUserRepository` is registered in
  `billing` and not exported). This is the single most important test in the
  package.
- **Golden-file test for every other error message** once its text is agreed.
- **Leak test.** An assertion that no dig error text ever reaches a caller:
  drive every failure path and assert the message matches Warren's format and
  contains no dig phrasing (`missing dependencies for function`).
- **Boundary test.** A static check that `go.uber.org/dig` is imported by this
  package only, and that no exported identifier here mentions a dig type
  (invariant 2). This is a candidate for `warren lint arch`.
- **Encapsulation contract suite** on §1.2's graph: from `billing`,
  `Resolve[UserRepository]` succeeds and `Resolve[*UserService]` fails.
- **Ordering test.** `Validate()` fails before any constructor runs — assert no
  singleton was constructed on the failure path.
- **Allocation benchmark.** No request path exists in this package; the number
  that must be defended is that the container is not consulted at request time
  (invariant 7), benchmarked where the route table is invoked.
- Unit tests: no Docker, no network, no sleeps.

## Definition of done

- [x] Spec approved.
- [x] `ProvideOption` and `Resolution` shapes agreed — 2026-08-01, recorded
      in Public API above and in warren.md §2.2.
- [x] `go.uber.org/dig` audited and recorded per AGENT.md § Adding a
      dependency — see Dependency audit above; §9 ledger row updated with the
      observation date.
- [x] Public API implemented exactly as in Public API above, with doc
      comments — `di/di.go`, `di/container.go`, `di/diagnostic.go`.
- [x] The golden diagnostic reproduces byte for byte —
      `di/testdata/missing_provider.golden`, produced from the §1.2 fixture
      graph (`di/internal/fixture/{domain,user,postgres}`, whose package
      names are load-bearing for the diagnostic's text).
- [x] No dig type in any exported signature, field or error; the leak test
      drives every failure path and asserts no dig phrasing survives.
- [x] `Validate()` runs to completion before step 4 instantiates anything —
      it runs entirely off this package's own provider records and never
      calls dig; proven by test (a constructor with a side effect stays
      unrun on the failure path).
- [x] Encapsulation contract suite passes on §1.2's graph: a sibling's
      private binding is invisible, an exported one still needs the
      bootstrapper's copy-in (simulated by a forwarding provider), and the
      diagnostics tell the two cases apart.
- [x] `Explain` output is sufficient to implement `warren explain di`: the
      resolution tree carries provider, scope, and site per step and renders
      itself.
- [x] `warren.md` amended in the same change: §2.2 gained `New`, the two
      `ProvideOption` constructors, the `Resolution` shape, the `MustResolve`
      panic note, and the Validate/Scope semantics; AGENT.md § General now
      names the `MustResolve` exception; the §9 DI ledger row carries the
      audit date.

## Open questions

1. **RESOLVED (2026-08-01) — `MustResolve` panics, as the one named
   exception.** The `Must` prefix is Go's documented panic convention
   (`regexp.MustCompile`, `template.Must`), the call runs at boot, and the
   panic value is the full resolution diagnostic. AGENT.md § General was
   amended to name the exception and to forbid any further `Must*` without
   amending that line. The signature stays as §2.2 fixed it.
2. **RESOLVED (2026-08-01) — `Resolution` is a self-rendering tree.**
   `Target`, `Found`, `Provider`, `Scope`, `Site`, and `Inputs []Resolution`;
   `String()` renders the indented tree `warren explain di` prints. Recorded
   in warren.md §2.2.
3. **RESOLVED (2026-08-01) — `ProvideOption` has two constructors, both
   needed by the golden diagnostic.** `Exported()` marks a binding visible to
   importing modules — the bootstrapper's copy-in reads it, and the
   diagnostic needs it to tell "registered but not exported" apart from
   "exported but not imported". `DeclaredAt(file, line)` carries the module
   declaration site. Named bindings and groups are deliberately absent until
   something needs them.
4. **Is "unused" a boot failure or a warning?** Step 3 lists "unused?" under
   "→ fail", but `warren doctor` separately reports "dead providers" (§8).
   **Deferred:** "unused" is not decidable inside `di` alone — a terminal
   provider (a server) is consumed by nobody and used by everybody, which
   only the root package's entry-point model can express. Decide when the
   root `warren` package is specified; until then `Validate` checks
   resolvable and ambiguous, and warren.md §2.2 says so.
5. **RESOLVED (2026-08-01) — both, layered.** The constructor's own
   `file:line` is recovered from its function pointer at `Provide` time
   (`runtime.FuncForPC`); the module declaration site is passed in by the
   root package via `DeclaredAt`, captured at `NewModule` time. The
   diagnostic prefers the declaration site and falls back to the constructor
   position.
6. **RESOLVED (2026-08-01) — the validator indexes every provider in every
   scope,** and the candidate search is by exact type. Every `Provide` call
   records constructor name, position, inputs, outputs, exportedness, and
   scope in this package's own records; the "Did you mean" search walks the
   whole tree for providers of the missing type that are not visible from
   the failing scope.
7. **RESOLVED (2026-08-01) — `Explain` works before `Validate` and on an
   unresolvable target.** Both run off the same provider records; an
   unresolvable target renders as "no provider visible from this scope"
   with `Found` false.
8. **RESOLVED (2026-08-01) — split by graph.** The root package owns boot
   step 1: module-graph flattening and module-import cycles, before any
   scope exists. This package owns the *provider* graph, including provider
   cycles — detected at `Provide` time (pre-empting dig, whose cycle message
   must not leak) and again at `Validate`. Each side's cycle message belongs
   to the side that detects it.
9. **RESOLVED (2026-08-01) — `Scope` creates on first use and looks up after.**
   A repeat call with the same name returns the same child; proven by test.
   Recorded in warren.md §2.2.
