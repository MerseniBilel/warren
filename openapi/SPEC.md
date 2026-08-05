# `warren/openapi` — SPEC

| | |
|---|---|
| **Status** | **APPROVED 2026-08-05 · scheduled v0.2, rank 1.** The architecture is ruled (see **Why it is not in v0.1**); no code yet, and the module must not be created until there is. Nothing in shipped code depends on it, and nothing in core waits on it. |
| **Source** | [warren.md §4.3](../warren.md) |
| **Module** | own module (`warren/openapi`) |
| **Mode** | Build |
| **Wraps** | — |

> **This spec was thin on purpose** while §4.3 fixed no Go surface. The
> architect ruling of 2026-08-05 settles the architecture; warren.md §4.3 now
> carries it, and the questions it answered are marked RESOLVED below.


## Why it is not in v0.1

**Not because the architecture is undecided — it is ruled (warren.md §4.3).
Because a v0.1 tag would cost users nothing to skip and cost Warren a frozen
output format.**

openapi is a pure downstream consumer of `*transport.Table`, which IS frozen
in v0.1. A user on `warren v0.1.0` runs `go get warren/openapi@v0.2.0` and it
works against controllers they already wrote, with zero lines of migration.

**The `app.Identity` contrast is the whole argument, and it is worth stating
so nobody re-derives it.** `Identity` had to ship in v0.1 because
`transport.Guard(p app.AuthorizationPolicy)` is a CORE signature: deferring it
would have forced a `RouteOption` change after the tag, and rewritten every
call site. Here the migration cost is `go get`. That asymmetry — core surface
versus downstream add-on — is the test, and openapi fails it in the direction
that means waiting is free.

What a v0.1 tag WOULD freeze is `Emit`'s signature, the option set, the
component-naming scheme, and most expensively **the exact bytes of the emitted
document**, because users diff that output in CI. A golden that changes in
v0.2 breaks every pipeline that pinned it. This repository's own method is to
field-test with an engineer told to try to break it, and rounds 3, 4 and 5
found seven, four and ten defects in code that had already been reviewed. An
emitter's failure mode is publishing a WRONG document, and clients generated
from a wrong document fail in someone else's repository. Freezing an output
format before one real API has been through it is the thing that method
forbids.

Two independent field tests named this — with `auth` — as the biggest gap for
shipping a real service. That is evidence for **rank 1 in v0.2**, which it
has, not for landing inside the tag: the gap is real for the user shipping a
SERVICE, and because the module is additively installable that is not the same
date as Warren shipping a TAG. The acute pain those testers hit — a misspelled
field silently accepted — already has a v0.1 answer in `transport.StrictJSON()`
and `http.Codec(...)`, which named openapi as the durable fix.

## Scope

**In scope for the first release** — the pure `Table → OpenAPI 3.1` emitter:
`Emit(*transport.Table, ...Option) (Document, error)`, `Module(...)` serving
`/openapi.json` through a `transport.Raw` route, hand-rolled 3.1 document
types, zero third-party, JSON output only, and the `Refusal` machinery with
`Strict()`.

**Out of scope for the first release** — the Scalar/Swagger bundle (`/docs`
lands once the embed-versus-CDN audit is run, so the bundle does not gate the
useful half), `security`/`securitySchemes`, prose, AsyncAPI, gRPC, and any CLI
command.

**Additive core changes that ship in the emitter's own PR, not before the
tag** — `func (t *Table) Validator() validate.Validator`, an
`app.ScopeDescriber` implemented by the unexported `scopePolicy`, promoting
`transport/http`'s error envelope to `warren/errors`, and `transport.Doc(...)`
if it earns its place. Each is additive under Go's compatibility rules: new
method, new interface on an unexported type, new fields on a struct only
`Builder.Fill` constructs.

## Problem

Hand-written OpenAPI drifts. Annotation comments drift more quietly, because the
compiler never reads them. Warren already holds both halves of the answer at
boot: the route registrations built at step 5 (§1.3), and the DTO struct tags —
`json:` and `validate:` — that the transport adapters already use to decode and
validate (§2.7). The document should be derived from those, not maintained
alongside them.

**The claim, in §4.3's words: "No annotations, no separate spec file to drift."**
That is the whole product of this package. A design that requires either has
failed.

## Goals

Everything §4.3 states, and nothing more:

- Read the frozen **`*transport.Table`** plus **DTO struct tags** — `json:`,
  `param:`, `query:`, `validate:` — and emit **OpenAPI 3.1**.
- Serve it at **`GET /openapi.json`**, built once at boot. CI boots the binary
  and fetches it; there is no export command.
- Serve **Scalar UI at `/docs`** once the bundle audit is run — after the
  emitter, so the audit does not gate the useful half.

## Non-goals

- **No annotations.** No magic comments, no `// @Summary`, no doc-comment DSL.
- **No checked-in source spec.** The Go is the source; an `openapi.yaml` is an
  *output*, and publish-only: Warren commits none of its own, and whether a
  user diffs one in CI is the user's choice.
- **No import of a transport adapter.** Adapters never import each other
  (invariant 4, §1.6), and §1.6 gives this package its own module with no
  third-party dependency. Registrations reach it through the contracts ring:
  it injects the `*transport.Table` already bound in the root scope, and
  imports `net/http` (stdlib) only for the handler it hands to
  `transport.Raw` as `any`.
- **Not a validator or a request router.** `warren/validate` owns constraint
  enforcement (§2.7); this package only *describes* the constraints it finds.

## Public API

The landing zone ruled on 2026-08-05. It follows every sibling adapter — a
`warren.Module` constructor plus functional options — and adds one pure
function, which is what makes golden-file testing possible.

```go
func Module(opts ...Option) warren.Module
func Emit(t *transport.Table, opts ...Option) (Document, error)

func Title(string) Option
func Version(string) Option                  // the API's version, not Warren's
func Description(string) Option
func Server(url, description string) Option  // repeatable; never derived
func SpecPath(string) Option                 // "/openapi.json"
func DocsPath(string) Option                 // "/docs"; "" disables the UI
func Guard(app.AuthorizationPolicy) Option   // an internal API is not public
func Strict() Option                         // every refusal becomes a boot failure

type Document struct{ /* hand-rolled OpenAPI 3.1 */ }
func (d Document) JSON() ([]byte, error)     // also valid YAML 1.2
func (d Document) Refusals() []Refusal

type Refusal struct {
    Route  string // "POST /users"
    Type   string // the reflect.Type that could not be described
    Reason string
}
```

`servers` is never derived: the Table knows no port, scheme or base path, and
`http://localhost:8080` in every generated client is worse than absence.

## Behaviour

**Generation runs at boot, once, never per request.** The inputs exist after
step 5 of §1.3, when controllers have registered and `Builder.Fill` has frozen
the table; reflection over struct tags is a boot-time activity (invariant 7).
The document is built in an `OnStart` hook at step 6, and `/openapi.json`
serves precomputed bytes — so the allocation cost per request is a small fixed
number independent of how many routes exist, and that is asserted by a test.

**What it reads**:

- Route registrations — method, path, and the `Req`/`Res` types behind each
  `r.HTTP()` registration (§3.5).
- DTO struct tags — `json:` for names and shapes, `validate:` for constraints.
  §2.7's example is the whole input:

  ```go
  type RegisterUser struct {
      Email string `json:"email" validate:"required,email"`
      Name  string `json:"name"  validate:"required,min=2,max=64"`
  }
  ```

  `required` is a required property; `email` is a format; `min`/`max` are length
  constraints. The full tag-to-schema mapping is Open question 6.

**What it emits**: OpenAPI 3.1.

**Must never**: import `warren/transport/http` or any other adapter
(invariant 4); require an annotation; require a hand-maintained spec file; run
generation on the request path.

## Error mapping

**None.** This package owns no transport column of §2.6. `warren/transport/http`
owns HTTP, `warren/transport/grpc` owns gRPC, the broker adapters own the
consumer column.

Which responses it DOCUMENTS is settled, and only from data that exists: 400
on any route with a body or a validated field, **401 and 403 on any route
carrying a Guard**, and 500 always. A 404 is NOT emitted, because whether a
handler can return `NotFound` is not visible in its type — guessing it would
document a response the route may never produce.

## Escape hatch

**None, and none is needed.** Mode is Build: there is no wrapped library to
escape to. A user who wants a document this package will not produce edits the
emitted JSON downstream, which costs nothing at runtime because the document
is an artefact rather than a contract the server enforces.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing).
  Generation is a pure function from registrations and types to a document.
- **Golden-file tests are the primary test form here.** The emitted document for
  a fixed set of routes and DTOs is compared byte-for-byte against a committed
  golden file — this is the same rule that covers generators and error text, and
  a spec generator breaks silently without it. Every `validate:` constraint that
  the tag mapping supports gets a row.
- **Integration tests behind `//go:build integration`** — booting a real app
  with `http.Server` plus `openapi.Module`, fetching `/openapi.json`, and
  running the structural checks on the parsed result.
- **Structural invariants instead of a meta-schema dependency.** Every `$ref`
  resolves to a declared component; every component key is a legal name; two
  generic instantiations sanitising to one name is a BOOT FAILURE, not a
  silent overwrite. A JSON Schema validator would be a test-only require, and
  a test-only require still lands in `go.mod` — which the zero-dependency rule
  forbids. External meta-schema validation, if wanted, runs in CI as a TOOL.
- **Determinism, run 100 times.** `paths` and `components.schemas` come from
  maps and Go randomises range order; without this the goldens flap in CI.
- **A differential test against `encoding/json`** — marshal a populated
  fixture DTO and assert every JSON key appears in the emitted schema and vice
  versa. That is what makes "the document matches what the server actually
  sends" a fact rather than a claim.
- **No allocation benchmark on a request path**, because there is no request
  path: generation runs at boot, and `/docs` serves a prebuilt document.

## Open questions

**Seven of nine are RESOLVED by the architect ruling of 2026-08-05 and are
recorded in warren.md §4.3. Do not re-litigate them.**

1. **RESOLVED — public API.** A `warren.Module` in `main.go`, matching every
   sibling, plus the pure `Emit(*transport.Table, ...Option) (Document,
   error)` that makes golden-file testing possible. Not a CLI library.
2. **RESOLVED — how it obtains registrations.** At runtime, through the
   contracts ring, and core grows nothing: `*transport.Table` is already bound
   in the ROOT scope at boot step 2 and is already injectable from any module
   scope (`testing/warrentest.go` does exactly this today). The static path is
   closed, not merely disfavoured — `Request`/`Response` reach `HTTPRoute` as
   type arguments inferred at the call site, so recovering them needs full
   type checking via the `x/tools/go/packages` §9 dropped on 2026-08-02 so
   that `lint arch` works on a project that does not compile.
3. **RESOLVED — which ring.** An adapter, by §1.1's own test: separate module,
   leaf, imports contracts only, appears in a service's `go.mod`. Tooling
   never does that.
4. **RESOLVED — who mounts it.** Whichever adapter claims `ProtocolHTTP`. This
   package registers `transport.Raw(r, ProtocolHTTP, "GET /openapi.json", h)`
   and passes an `http.Handler` as `any`; it imports `net/http` (stdlib) and
   no adapter, so invariant 4 holds, and no `net/http` type appears in a
   public signature, so invariant 3 holds. `Table.Unserved()` already fails
   the boot if this module is added with no HTTP server.
5. **NARROWED — Scalar: embed or CDN?** Still the human's, still a dependency
   decision under AGENT.md § Adding a dependency, and deliberately NOT gating
   the useful half: `/openapi.json` ships first and `/docs` follows the audit.
6. **RESOLVED — the `validate:` mapping, including the trap nobody had
   spotted.** `required`, `format`, `minLength`/`maxLength`,
   `minimum`/`maximum`, and `enum` from `oneof`; an unmappable constraint is a
   Refusal, not a guess. The trap: under `validate.None()` every tag is
   accepted and NOTHING is enforced, so a tag-reading emitter would publish
   `required` for an API that accepts `{}` — a lie the framework generated.
   `*transport.Table` cannot currently tell, so the emitter's own PR adds the
   additive `func (t *Table) Validator() validate.Validator` and emits no
   tag-derived constraint under `None()`, with one Refusal for the document.
   **This also closes `validate/playground/SPEC.md` open question 2.**
7. **RESOLVED — gRPC and events are out of scope.** OpenAPI describes HTTP.
   AsyncAPI is a separate package and a separate decision, not a stretch goal.
8. **RESOLVED — `warren openapi export` is STRUCK**, from warren.md §4.3 and
   from `cli/SPEC.md`. CI boots the binary and fetches `/openapi.json`. One
   mechanism instead of two.
9. **RESOLVED — publish-only.** Warren commits no `openapi.yaml` of its own;
   whether a user diffs one in CI is the user's choice, not a thing this
   package requires.

**Still open, both for the human, both post-approval:** the Scalar bundle
audit (5 above), and whether `transport.Doc(summary, description)` earns a
core `RouteOption` — without it the document has no prose, because
"RegisterUser" is not "Registers a user" and inventing prose from a Go name is
the kind of guess this package must not make.
## Definition of done

1. OpenAPI 3.1 emitted from the frozen `*transport.Table` plus `json:`,
   `param:`, `query:` and `validate:` tags — zero annotations, no source spec
   file anywhere in the repository.
2. `openapi.Module(...)` serves `/openapi.json` through a `transport.Raw`
   route, with the document built ONCE in an `OnStart` hook after boot step 5.
   `/docs` waits on the bundle audit and does not gate this.
3. `Emit(*transport.Table, ...Option) (Document, error)` is a pure function —
   which is what makes the golden-file tests possible.
4. Every refusal reaches all three of `Document.Refusals()`, a boot WARN, and
   an `x-warren-undescribed` extension on the operation; `openapi.Strict()`
   turns the set into a boot failure.
5. `transport.Raw` routes are EMITTED, with a Refusal — never omitted. A
   document smaller than the API is the one error a generated client acts on.
6. `openapi/go.mod` requires the core module and nothing else, and the module
   is added to the Makefile's `MODULES` list so `scripts/invariants.sh` sees
   it.
7. The tests in **Testing** pass, including determinism over 100 runs and the
   differential check against `encoding/json`.
8. warren.md §4.3 already carries the architecture; it is corrected in the
   same pull request if the implementation diverges, and this spec is retired
   on completion.