# `github.com/MerseniBilel/warren/validate` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §2.7](../warren.md) |
| **Module** | core |
| **Mode** | Wrap |
| **Wraps** | `go-playground/validator/v10` |

## Problem

`go-playground/validator/v10` is the library §2.7 and §9 nominate, and its
struct-tag vocabulary is what §2.7's example assumes users already know. (How
widely used it is carries no weight here — AGENT.md § Adding a dependency is
explicit that popularity is not evidence; the written audit is what decides.) Using it directly costs two things
Warren cannot pay:

1. **It leaks `validator.ValidationErrors` into handlers.** warren.md §2.7 names
   this as the reason for the wrap. A handler that inspects
   `validator.ValidationErrors` has taken a dependency on the validation library
   and, worse, has taken responsibility for turning a field failure into a
   response — which is ring 2's job, not the handler's (AGENT.md § The error
   table is load-bearing).
2. **It makes every handler validate its own input.** warren.md §2.7: "Transport
   adapters call it automatically after decode; handlers never invoke it." The
   payoff, stated in warren.md's own comment on the example: **"A bad request
   never reaches `Handle()`."**

The wrap therefore does one job: run the library, and normalise whatever it
returns into the semantic vocabulary of §2.6 — `CodeInvalid`, with per-field
details — so that the failure travels the same path as every other failure and
each adapter's column of the error table handles it unchanged.

## Goals

- Normalise validation failures into `*Error` with `CodeInvalid` and per-field
  details (warren.md §2.7, and the §9 ledger note "errors normalised to
  `warren.Error`").
- Keep `validator.ValidationErrors` out of handlers entirely.
- Preserve the `validate:` struct-tag vocabulary users already know — warren.md
  shows `validate:"required,email"`, `validate:"required,min=2,max=64"`,
  `validate:"oneof=..."`, `validate:"required,min=1"` across §2.4 and §2.7.

## Non-goals

- **Not a validation DSL.** Warren does not invent its own constraint language;
  the tags are the library's.
- **Not called by handlers.** warren.md §2.7 is explicit: "handlers never invoke
  it." Anything that makes handler-side validation the natural path is wrong.
- **Not a transport concern.** Which adapter calls it, and when, is the
  adapter's business; this package does not know a request exists.
- **Not the thing that decides the HTTP status.** It produces `CodeInvalid`; the
  `INVALID` row of the §2.6 table (400 / `InvalidArgument` / → DLQ) is owned by
  each adapter.

## Public API

**warren.md fixes no Go surface for this package.** §2.7 is four sentences and
one struct-tag example; it names no exported function, type, or option. The only
Go it shows is a *user's* DTO, reproduced here because the tag vocabulary is the
part of the contract warren.md does fix:

```go
// User-side shape. The tags are go-playground/validator/v10's vocabulary,
// unchanged.
type RegisterUser struct {
	Email string `json:"email" validate:"required,email"`
	Name  string `json:"name"  validate:"required,min=2,max=64"`
}
// A bad request never reaches Handle().
```

[AGENT.md § Spec-driven development](../AGENT.md) calls the public API section
"the contract under review" and says `warren.md` already fixes the public
surface for most packages. For this package it does not, and the surface is not
mine to invent — a validate function's signature, whether it is an interface or
a free function, and whether adapters receive it by injection or by import are
all structural calls, which AGENT.md § When you are unsure reserves for the
human. They are recorded as Open questions 3 and 4, and this section is filled
in once they are answered.

What *is* fixed, and what any proposed surface must satisfy:

- It produces `*Error` (per §2.6) carrying `CodeInvalid`.
- It attaches **per-field** details — plural, one per failing field, which
  points at repeated `WithDetail(k, v)` or an equivalent.
- It is callable by a transport adapter, which lives in a **separate Go module**
  (warren.md §1.6). So the entry point must be exported.

## Behaviour

### Where it runs — the edge ring, after decode

warren.md §1.4, the transport spine:

```
HTTP request ─┐
gRPC call ────┼─▶ edge middleware ─▶ decode ─▶ validate ─┐
Kafka msg ────┘   (transport-specific)                   │
                                                         ▼
                                              core middleware chain
```

Validation sits **before** the core middleware chain and after decode, on the
transport-specific side of the diagram — so it is in the **edge ring** of the
two-ring model (§1.4, AGENT.md § Two middleware rings). AGENT.md requires the
ring to be named in the spec; this is it. The consequence: the adapter owns the
call, and a handler that is invoked directly in a test (warren.md §7.5's
`warrentest.Invoke`) does not go through it.

warren.md §10's end-to-end trace confirms the position:

```
chi → edge middleware (CORS, auth, correlation ID)
    → decode JSON → validate → app.Handler
```

### What it produces

- A failure is a `*Error` with `CodeInvalid` and per-field details (§2.7).
- That error then travels the §2.6 `INVALID` row, which each adapter owns:
  **400** on HTTP, **`InvalidArgument`** on gRPC, **→ DLQ, never retry** for a
  consumer. The consumer column is why normalising matters — a validation
  failure on a message must not be retried, and only the code tells the broker
  middleware that.
- A success returns nothing and the request proceeds into the core middleware
  chain.

### Who else uses the tag vocabulary

- **`warren/config` (§2.4)** annotates config structs with the same `validate:`
  tags and states "Validation runs at boot. A missing `WARREN_POSTGRES_DSN` is a
  startup failure with the field path named." This matches boot step 0, "load
  config ... validated" (§1.3). Whether config's boot validation runs through
  *this* package is not stated — see Open questions 5.
- **`warren/openapi` (§4.3)** "reads route registrations plus DTO struct tags
  (including `validate:` constraints) and emits OpenAPI 3.1." It reads the tags;
  it does not call this package.

## Errors

**Every failure this package produces is a `*Error` with `CodeInvalid`.** That
is the whole of what warren.md fixes.

| Aspect | Status |
|---|---|
| Code | `CodeInvalid` — fixed by warren.md §2.7. |
| Details | Per-field — fixed by warren.md §2.7. The key format (field name? JSON name? dotted path?) and the value shape are **open**. |
| Message text | **Open.** warren.md fixes no string for a validation failure. |
| Multiple failing fields | **Open** whether one `*Error` carries all of them or one error is returned per field. "per-field details" reads as one error with many details, but warren.md does not say it. |

AGENT.md § Errors requires error messages to tell the user how to fix the
problem. A validation message therefore has to name the field and the constraint
it failed — `"email must be a valid email address"` is a fix; `"validation
failed"` is a bug in the error message. §2.4 sets the same bar for config: "a
startup failure with the field path named." The exact wording is the human's
call and then becomes golden-file-tested contract.

## Testing

- **Golden-file test for every error message** (AGENT.md § Testing). Once the
  text is agreed: one golden file per constraint kind exercised
  (`required`, `email`, `min`, `max`, `oneof`), plus a multi-field failure, plus
  a nested-struct failure — nesting matters because §2.4's config structs are
  nested and the message must carry the field path.
- **Allocation benchmark on the request path.** This runs on **every** request,
  not only failing ones, which makes it the hottest thing in these four
  packages. Benchmark the passing path on a two-field DTO and the failing path
  with one failing field. See Open questions 2 — this benchmark is also the
  evidence for or against the reflection question.
- **`validator.ValidationErrors` never escapes.** A test that asserts the
  returned error unwraps to nothing from `go-playground/validator/v10` is the
  test of the wrap boundary itself, and is the analogue of the dig-leak rule in
  AGENT.md invariant 2.
- **No Docker, no network, no sleeps** (AGENT.md § Testing). None needed.
- `t.Parallel()`, table-driven, subtests named for behaviour.
- **The dependency audit** required by AGENT.md § Adding a dependency —
  archived? last release? transitive tree? licence? — is **not yet written**.
  It must be recorded in this spec, with the observation date, before
  `go-playground/validator/v10` enters any `go.mod`. AGENT.md: "A package with
  no written audit does not go into a `go.mod`."

## Definition of done

- [ ] Open questions 1 and 2 are resolved by the human — they are architecture
      questions, not implementation details, and both block writing any code.
- [ ] The public surface is agreed, written into the Public API section above,
      and warren.md §2.7 amended to carry it (AGENT.md: a spec that contradicts
      warren.md needs warren.md amended in the same change; a spec that *adds*
      public surface warren.md never stated is the same situation).
- [ ] The dependency audit for `go-playground/validator/v10` is written into
      this spec with its observation date and added to the warren.md §9 ledger
      row.
- [ ] Failures are `*Error` with `CodeInvalid` and one detail per failing field.
- [ ] A test asserts no `go-playground/validator/v10` type is reachable from any
      returned error.
- [ ] Golden files exist for every message text.
- [ ] Allocation benchmarks exist for the passing and failing paths.
- [ ] `make ci` passes (once the Makefile exists).

## Open questions

1. **This package breaks invariant 1 as written, and warren.md contradicts
   itself about it.** AGENT.md invariant 1: "The root module imports the
   standard library and `go.uber.org/dig`. That is the entire list,
   permanently — not 'for now,' not 'except this one.'" warren.md §1.1 and
   §1.6 both place `validate` in the **core** module, and §2.7 says it wraps
   `go-playground/validator/v10`. Those cannot both hold. Three data points make
   it a real inconsistency rather than a reading error:
   - §1.1's kernel box is labelled "stdlib + dig only" and lists `validate`
     inside it.
   - §1.6 puts `validate/` under "MODULE: core (stdlib + dig)".
   - §1.7's dependency budget lists `validator` as a **direct dep in the user's
     `go.mod`** for the HTTP-only profile — which is what you would expect if
     validation lived in a submodule, not in core.

   AGENT.md states the resolution rule: "If a core feature seems to need a
   library: **define the port in core, implement it in a submodule.** That is
   the move, every time." Applying it: does `warren/validate` become a port in
   core with the validator-backed implementation in its own module, or does
   invariant 1 gain an exception? **This is the human's call and it blocks
   everything else in this spec.**

2. **Reflection on the request path.** AGENT.md invariant 7 and warren.md §1.4:
   "Reflection runs during steps 1–5 only. ... Per-request cost is a map lookup
   and direct calls." `go-playground/validator/v10` is reflection-driven and
   §1.4's own diagram places `validate` on the per-request path, after decode.
   These conflict. Options: accept reflection here as a stated exception;
   pre-compile per-type validators at boot (steps 1–5) and call a closure per
   request, which is exactly the shape §1.4 describes for routes; or something
   else. This decides whether the wrap is thin or substantial.

3. **What is the exported surface?** A free function taking a value? An
   interface providing it to adapters through DI? Both? Adapters are separate
   modules and must reach it somehow. Not guessing — see Public API.

4. **Is there any configuration surface** — custom constraint registration, a
   custom tag name, custom messages, translations? Users will want at least
   custom constraints, and warren.md shows nothing.

5. **Does `warren/config` validate through this package?** §2.4 uses the same
   `validate:` tags and says "Validation runs at boot", but never names this
   package. If it does, config depends on validate and inherits question 1's
   answer; if it does not, there are two validators in the kernel with one tag
   vocabulary.

6. **Naming: `*warren.Error` or `*errors.Error`?** §2.7 says failures surface as
   `*warren.Error`, and the §9 ledger says "errors normalised to
   `warren.Error`". But §2.6 defines `*Error` in package `warren/errors`, whose
   qualified name is `errors.Error`. §4.2 also says `warren.Error`. The same
   question is raised in [`errors/SPEC.md`](../errors/SPEC.md) Open questions 3;
   it needs one answer, applied in both places, with warren.md corrected.

7. **What is a detail key?** The Go field name, the `json:` name, or a dotted
   path for nested structs? §2.4's config structs are nested two deep, and §2.4
   promises "the field path named", which argues for a dotted path. Not fixed by
   warren.md.

8. **Where does validation sit for a consumer?** §1.4 puts `Kafka msg` through
   the same decode-then-validate edge, and the `INVALID` row sends it straight
   to the DLQ with no retry. Confirm that a message failing validation is
   dead-lettered rather than nacked — the table says so, but it is worth
   agreeing explicitly, since it means a schema change can dead-letter a whole
   topic.
