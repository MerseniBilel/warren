# `warren/transport` — SPEC (a path wildcard nothing binds)

| | |
|---|---|
| **Status** | **APPROVED 2026-09-02 → IMPLEMENTED, PENDING REVIEW; retires on merge.** Shipped in `281365c` BEFORE approval and with none of the tests below — which is how it reached `main` red. Approved as it stands, with the "no correct reading" overclaim corrected (below) and the definition of done completed. It is a **boot-behaviour change**: applications that boot today stop booting. |
| **Source** | Field test #14 (`fieldtest14/REPORT.md`, finding 7), verified against `transport/params.go:176-238` and `transport/transport.go:830-840` |
| **Module** | core (`transport/`) — stdlib + dig |
| **Mode** | Build |
| **Retires** | Deleted when implemented and reviewed. Residue rehomes to `transport/params.go`'s doc comments and warren.md §4.2. |

## Problem

`r.Get("/books/{id}", h)` where the request type has **no** `param:"id"` field
boots green and fails on every request, in one of two ways:

```
# with validate:"required" on the field → 400, for ever
{"error":{"code":"INVALID","message":"field id is invalid","details":{"id":"is required"}}}

# without it → the handler receives an empty identifier
GET /books/<a real id>  →  404 {"error":{"code":"NOT_FOUND","message":"book  not found"}}
```

and the published OpenAPI document for that route becomes structurally
invalid — a templated path with no matching parameter:

```
/books/{id} = {"get":{"parameters":null}}
```

The forward direction is already refused. `checkWildcards`
(`transport/params.go:176`) fails registration for a `param:"id"` field whose
pattern declares no `{id}`, with a diagnostic that names the field, the
pattern, the consequence and the fix. The reverse — a `{id}` no field binds —
is unchecked.

This is the class of mistake warren.md §1.3's headline rule exists to catch:
*"every error the framework can detect surfaces at boot, never on request 1"*.
All the information is present at boot step 5, in the function that already
holds it.

## Ruling — refuse, in `checkWildcards`, with no escape hatch

`wildcards(pattern)` already returns the declared names
(`transport/params.go:223`). `setters` already carries the bound `param:`
names. The check is the existing loop, run the other way:

```
for each w in wildcards(pattern):
    if no non-query setter has s.name == w:  fail
```

### Why no escape hatch, when there appears to be a legitimate minority

The apparent minority is a route like `/tenants/{tenant}/books/{id}` whose
handler binds only `{id}` while a core middleware reads the tenant out of
`transport.ParamsFromContext`. That reading is available and would work.

**Corrected 2026-09-02. This section previously claimed the refusal "satisfies
AGENT.md's requirement that a refusal have no correct reading". That was an
overclaim, and an overclaim in a spec is the confident lie rule 4 exists to
prevent — so here is the honest version.**

**There IS a correct reading. It costs one struct field, and `Raw` covers
anyone who will not write it.** That is the argument for refusing, not the
absence of a minority.

The minority is real: `transport.ParamsFromContext` is exported, documented
and taught (`GETTING_STARTED.md`), and a guard reading `{tenant}` through it
on a route whose request type binds only `{id}` is a program that works today.

Three reasons the refusal still stands:

1. **The document is wrong either way.** OpenAPI 3.1 requires every template
   expression in a path to have a corresponding path parameter, and
   `openapi`'s `parametersOf` (`openapi/emit.go:207`) derives parameters from
   `param:` tags alone. A wildcard no field binds serves a path segment its
   own published contract does not mention.
2. **The remedy is one field, and it is what the framework's own suite
   already writes.** `transport/http/serve_test.go`'s `tenantReq` declares
   ``Tenant string `param:"tenant"` `` on a handler that never reads it. The
   supposed aggrieved minority is already complying, voluntarily, in Warren's
   own canonical multi-tenant test. It costs nothing at request time —
   `bindParams` walks a precomputed index either way.
3. **The opt-out already exists and is already exercised.** `Registrar.Raw`
   calls `r.record` directly and never enters `register`, so a raw route's
   wildcards are neither checked nor published;
   `serve_test.go` registers `GET /raw/t/{tenant}/doc` with a
   params-reading guard and no request struct at all, and it is green. A
   second, narrower opt-out (`transport.IgnoreWildcard`) was proposed and
   **rejected on 2026-09-02**: one exported symbol per objection is how a
   public API stops being a design.

AGENT.md's "no correct reading" test governs boot PANICS. This is a returned
registration error joined with every other one, so that test does not bind it
— which is just as well, because it would not pass it.

### It cannot false-positive on `Raw`

Verified: `checkWildcards` is called from exactly one place —
`transport.go:836`, inside `register[Req, Res]`, already gated on
`p == ProtocolHTTP`. `Registrar.Raw` (`transport.go:696-723`) calls `r.record`
directly and never reaches it. `OnEvent` never calls it either, and the gRPC
exemption at `transport.go:832-834` is preserved verbatim.

A `Raw` route with a wildcard remains undescribed-but-declared in the
document; that is recorded as out of scope in `openapi/SPEC.md`.

### It is a registration failure, not a panic

`r.fail(err)` appends to `Builder.errs` and surfaces with every other
registration problem from `Table()`, so one boot names all of them. Against
AGENT.md's admission test this fails criterion 3 — `reg.fail` is three lines
away — so a panic would be wrong here, exactly as it was for the nil handler
on 2026-08-09.

## Diagnostic

One per unbound wildcard, in the shape the forward check already uses:

```
✗ path wildcard nothing binds

    pattern GET /books/{id} declares {id}, and no field of BookRequest
    carries `param:"id"`

    Every request binds "" for it: with validate:"required" the route 400s
    for ever, without it the handler receives an empty identifier and
    reports NOT_FOUND. The published OpenAPI path is invalid either way —
    a templated segment with no parameter.

  Add the field, even if the handler ignores it:

      ID string `param:"id"`

  or drop {id} from the pattern.
```

When the request type has a `param:` field for a *different* name the message
should say so — the near-miss (`param:"bookID"` against `{id}`) is the likely
cause and the forward check already prints that class of hint.

## What goes red

**ESCALATE — this stops applications booting that boot today.** Specifically:

- Any route whose pattern declares a wildcard the request type does not bind.
- The remedy for every one of them is a one-line struct field, and the
  diagnostic quotes it.

**ENUMERATED 2026-09-02, which is the step that was skipped before
implementation.** `make ci` across all eight modules: the only failures were
three test fixtures in `transport/transport_test.go`
(`TestNilHandlerJoinsOtherRegistrationFailures`,
`TestEveryJoinedFailureLeadsWithItsOwnHeadline`,
`TestRegistrationNeedsNoTypeArguments`), all registering `/users/{id}` against
a request type with no `param:` tag. Nothing in `transport/http/`, `openapi/`,
`testing/`, `cli` or any adapter trips it: `warren g` writes a `param:` field
per wildcard by construction (`cli/internal/generate/generate.go`, *"Every
wildcard becomes a `param:` field"*), and the CLI's compile tests build and
test generated trees against this checkout. `GETTING_STARTED.md`'s only
wildcard route already declared its field.

The one documentation defect the enumeration DID find: `GETTING_STARTED.md`'s
tenant-guard section taught `p.Path("tenant")` without ever showing the route
or the field it requires, so a reader following that page hit this boot
failure from the page that taught them. Fixed in the same change.

## Definition of done

- [x] A wildcard no field binds fails registration, with the golden diagnostic.
      `TestPathWildcardNothingBindsIsRefused`, golden
      `transport/testdata/path_wildcard_nothing_binds.golden`.
- [x] Two unbound wildcards on one route produce two errors in one boot
      failure, alongside any other registration error.
      `TestTwoUnboundWildcardsAreTwoSiblingFailures`, and
      `TestNilHandlerJoinsOtherRegistrationFailures` for the alongside half.
- [x] `{rest...}` binds the name `rest` (the existing `TrimSuffix`), so a
      multi-segment wildcard is checked identically. Test both spellings.
      `TestBothSpellingsOfAWildcardAreChecked`, three subtests.
- [x] A `query:` setter does NOT satisfy a path wildcard.
      `TestAQueryTagDoesNotSatisfyAPathWildcard`.
- [x] `Raw`, `OnEvent` and `ProtocolGRPC` routes are unaffected. Test each.
      `TestOnlyHTTPRoutesAreWildcardChecked`, three subtests.
- [x] warren.md §4.2 records both directions of the check, not one.

Added while completing the above, and not in the original list:

- [x] The near-miss hint names the field the user must write, for THIS
      wildcard. The hint hardcoded `ID` whatever the wildcard was called, so
      `{rest...}` was answered with ``ID string `param:"rest"` `` — wrong code in
      a diagnostic whose whole value is that it can be pasted.
      `TestTheHintNamesTheFieldTheUserMustWrite`.
- [x] The near-miss diagnostic names the tagged field beside the unbound
      wildcard. `TestTheNearMissIsNamed`.
- [x] `GETTING_STARTED.md` shows the route and the declared-but-ignored field,
      and names `Raw` as the alternative.
