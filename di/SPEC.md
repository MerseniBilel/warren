# `github.com/MerseniBilel/warren/di` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — conditions: `Resolution` and `ProvideOption` shapes plus the `MustResolve`/no-panic conflict (Open questions 1–3) settled before those parts are implemented; the golden diagnostic is binding as written |
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

**The wrap boundary.** `go.uber.org/dig` is imported by this package and by
nothing else in the repository (invariant 2). Users never import dig.

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

Other errors this package produces, whose text `warren.md` does **not** fix:

| Condition | Text |
|---|---|
| Ambiguous binding (step 3) | **Open.** Must name the type, every scope providing it, and how to disambiguate. |
| Unused provider (step 3) | **Open.** Must name the constructor and its declaring module. Whether this fails boot or warns is an open question below. |
| Constructor returned an error (step 4) | **Open.** Must name the constructor and its module, and wrap the cause with `%w`. |
| `Provide` on a non-constructor | **Open.** |
| `MustResolve` failure | **Open** — and see Open questions: `warren.md` gives no error return, AGENT.md forbids panic in library code. |

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

- [ ] Spec approved.
- [ ] `ProvideOption` and `Resolution` shapes agreed (Open questions).
- [ ] `go.uber.org/dig` audited and recorded per AGENT.md § Adding a dependency,
      with the observation date, and confirmed against the §9 ledger row
      (`uber-go/dig` · Wrap · v1, strict SemVer, built to power frameworks).
- [ ] Public API implemented exactly as in Public API above, with doc comments.
- [ ] The golden diagnostic reproduces byte for byte.
- [ ] No dig type in any exported signature, field or error.
- [ ] `Validate()` runs to completion before step 4 instantiates anything.
- [ ] Encapsulation contract suite passes on §1.2's graph.
- [ ] `Explain` output is sufficient to implement `warren explain di`.
- [ ] `warren.md` amended in the same change if any signature diverged.

## Open questions

1. **`MustResolve` has no error return.** §2.2 gives
   `MustResolve[T any](c Container) T`, which implies a panic, but AGENT.md
   § General says "no `panic` in library code". Which gives way — is
   `MustResolve` an accepted exception, or should the signature change?
2. **What is `Resolution`?** §2.2 names it as `Explain`'s return type and fixes
   nothing about it. `warren explain di UserRepository` (§8) is its only stated
   consumer. What does it carry, and does it render itself?
3. **What is `ProvideOption`?** No option constructor is listed anywhere in
   `warren.md`. Is there a first one (named bindings? groups?), or does the
   parameter exist purely for future use?
4. **Is "unused" a boot failure or a warning?** Step 3 lists "unused?" alongside
   "resolvable?" and "ambiguous?" under "→ fail", but an unused provider is not
   obviously fatal, and `warren doctor` separately reports "dead providers" (§8).
5. **How is the declaration site (`internal/modules/user/module.go:14`) obtained
   at runtime?** The golden diagnostic requires file and line for a module
   declaration. Is this captured at `NewModule` time, recovered from the
   constructor's function pointer, or supplied by the CLI analyzer?
6. **How are "Did you mean" candidates found?** The suggestion requires knowing
   that `postgres.NewUserRepository` exists in scope `billing` and is not
   exported. Does the validator index every provider in every scope for this, and
   is the search by exact type only?
7. **Does `Explain` work before `Validate`, and on an unresolvable target?** The
   diagnostic and `Explain` appear to need the same index; §2.2 does not say
   whether one is built on the other.
8. **Who owns boot step 1?** §1.2 says "the bootstrapper walks the whole graph
   first, then materialises containers", which puts flattening and cycle
   detection in the root package; §2.2 gives this package "the container,
   scoping, graph validation, diagnostics". Does `di` receive an already-resolved
   graph, or own step 1 itself? The cycle-detection error message belongs to
   whichever side owns it.
9. **Does `Scope` create or look up?** `Scope(name string) Container` is used at
   boot step 2 to create one child per module; whether a repeat call with the
   same name returns the same container is unstated.
