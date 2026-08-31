# `warren/transport` — SPEC (a path wildcard nothing binds)

| | |
|---|---|
| **Status** | **PROPOSED (2026-08-31) — NOT APPROVED.** One check, in a function that already exists, closing the reverse of a refusal the package already makes. It is a **boot-behaviour change**: applications that boot today will stop booting. |
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

**It is not a legitimate minority, because it publishes an invalid document.**
OpenAPI 3.1 requires every template expression in a path to have a
corresponding path parameter, and `openapi`'s `parametersOf`
(`openapi/emit.go:207`) derives parameters from `param:` tags alone. A
wildcard no field binds is therefore a second defect wearing a disguise: the
service serves a path segment its own published contract does not mention.

The remedy is cheap and the diagnostic should say it plainly: **declare the
field even if the handler ignores it.** One struct field with a `param:` tag
makes the binding explicit, makes the document correct, and costs nothing at
request time — `bindParams` walks a precomputed index either way.

This satisfies AGENT.md's requirement that a refusal have no correct reading,
and it needs no opt-out for the same reason `checkWildcards`' forward
direction has none.

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
  In-repo: to be enumerated by running the check against
  `transport/`, `transport/http/`, `openapi/`, `testing/`, the CLI's golden
  projects and `fieldtest14/library` before implementation. The field test's
  own repro is a deliberate one.
- The remedy for every one of them is a one-line struct field, and the
  diagnostic quotes it.

## Definition of done

- [ ] A wildcard no field binds fails registration, with the golden diagnostic.
- [ ] Two unbound wildcards on one route produce two errors in one boot
      failure, alongside any other registration error.
- [ ] `{rest...}` binds the name `rest` (the existing `TrimSuffix`), so a
      multi-segment wildcard is checked identically. Test both spellings.
- [ ] A `query:` setter does NOT satisfy a path wildcard.
- [ ] `Raw`, `OnEvent` and `ProtocolGRPC` routes are unaffected. Test each.
- [ ] warren.md §4.2 records both directions of the check, not one.
