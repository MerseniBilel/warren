# `warren/openapi` — SPEC (the document is wrong, and cannot say so)

| | |
|---|---|
| **Status** | **PROPOSED (2026-08-31) — NOT APPROVED.** Three defects found by field test #14, all in one package, all confirmed against the source. Two of them publish a document that misdescribes the service; the third makes the package unreachable by either documented install route. |
| **Source** | Field test #14 (`fieldtest14/REPORT.md`, findings 1, 2, 3, 8, 10), verified against `openapi/schema.go`, `openapi/emit.go`, `cli/internal/scaffold/scaffold.go` |
| **Module** | `github.com/MerseniBilel/warren/openapi` — core + stdlib. No new dependency. |
| **Mode** | Build |
| **Retires** | Deleted when implemented and reviewed. Residue rehomes to `openapi/doc.go` and warren.md §4.3. |

## Why a spec exists for an implemented package

`warren/openapi` shipped on 2026-08-29 and its spec was retired, so by AGENT.md
its contract is its code, its tests, and warren.md §4.3. This spec exists
because the change is not a bug fix inside that contract — **it changes the
emitted document**, which is the package's entire output, and it settles a
naming scheme that every generated client inherits. That is an architecture
decision, not an implementation detail.

**The single most important fact about timing: `warren/openapi` has no
published tag** (finding 3 — `proxy.golang.org/…/openapi/@v/list` returns 404
while all six siblings resolve at v0.2.1). There is no downstream generated
client anywhere. **Every naming decision below is free today and expensive
after the first tag.** That is the argument for taking it now rather than
shipping and amending.

---

## Problem 1 — two types, one schema, no warning

### What happens

`openapi/schema.go:196-207`:

```go
// componentName is the schema key for a named type: the package-qualified name
// with dots removed, so two types of one name in different packages do not
// collide.
func componentName(t reflect.Type) string {
	pkg := t.PkgPath()
	if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
		pkg = pkg[i+1:]      // ← keeps the LAST path element only
	}
	...
	return pkg + "." + t.Name()
}
```

The doc comment states the property the code does not have. The key is the
**last path element** plus the type name, so
`internal/modules/catalog/application.BookView` and
`internal/modules/lending/application.BookView` both key on
`application.BookView`.

`openapi/schema.go:63-73` then makes the collision silent and first-wins:

```go
name := componentName(t)
if _, done := e.components[name]; !done {
	e.components[name] = Schema{Type: "object"}
	e.components[name] = e.inlineStruct(t, route)
}
return Schema{Ref: "#/components/schemas/" + name}
```

The second type is never walked. Both routes `$ref` the first type's schema.

### Why it is certain to happen, not merely possible

`warren g module` names every feature's use-case package `application`.
**Every feature in every Warren service shares one namespace**, by
construction, because the framework's own generator puts them there. Field
test #14 hit it with two features and the most obvious DTO name in the
domain. `GET /books/{id}/loans` published five fields that do not exist and
none of the three that do.

`openapi.Strict()` boots clean and `Refusals()` is empty, because nothing
refuses: `refuse` is called from exactly four sites (`emit.go:45`,
`emit.go:184`, `schema.go:47`, `schema.go:50`) and none of them is here.

### Ruling — shortest unique path suffix, computed per document

The key becomes **the shortest trailing run of import-path segments that makes
the type unique within this document**, plus the type name:

```
one BookView   →  application.BookView
two BookViews  →  catalog.application.BookView
                  lending.application.BookView
```

The comparison is over the (import path, type name) pairs the emitter actually
reached, so it is a total function of the frozen route table. Not a heuristic,
not a hash, and **collision is impossible by construction** rather than
unlikely — which is what was asked for.

Rejected alternatives, with the reason:

- **Full import path as the key.** Collision-proof and stable, but OpenAPI
  component keys must match `^[a-zA-Z0-9._-]+$`, so `/` becomes `.` and every
  generated client's type is named
  `ExampleComLibraryInternalModulesCatalogApplicationBookView`. The document
  would be correct and the clients unusable.
- **Second-to-last segment when the last is `application`/`domain`/
  `infrastructure`.** A heuristic keyed on the CLI's layout convention. It
  breaks for a hand-written project and it is exactly the kind of guess this
  package's own `applyValidate` comment refuses to make.
- **Refuse on collision.** Records the problem without fixing the document.
  A refusal is for something that *cannot* be described; two distinct types
  can both be described.

### The one hazard, stated

Adding a colliding type in a second feature **renames the first feature's
schema key**, and any client generated from the old document changes with it.
This is unavoidable under any shortest-unique scheme. It is acceptable because
(a) the rename is visible in the document diff, where today's wrongness is
not, and (b) it is a rename, not a silent lie about field names.

### Definition of done

- [ ] Two features with same-named request DTOs emit two schemas; each route
      `$ref`s its own. Golden test.
- [ ] Three-way collision across three path depths resolves to the shortest
      suffix that separates all three. Golden test.
- [ ] A type used by two routes in the SAME package still emits once — the
      dedup the current code buys is preserved.
- [ ] The `componentName` doc comment states what the code does.

---

## Problem 2 — `time.Time` is `{"type":"object"}`, and `json.Marshaler` is unhandled

### What happens

`time.Time` is `reflect.Struct`, so `schemaFor` sends it to `structSchema`,
which registers `time.Time` in components and walks its fields. All three
(`wall`, `ext`, `loc`) are unexported, so the walk yields an empty object:

```json
"time.Time": { "type": "object" }
```

with no refusal. Verified against go1.27.0: `encoding/json` marshals
`time.Time` as `"1970-01-01T00:00:00Z"` — a string.

warren.md §4.3 lists *"the shape of any `json.Marshaler` outside `time.Time`"*
among the things the emitter refuses, which reads as a promise that `time.Time`
is handled. **Neither half is implemented.** There is no `json.Marshaler`
check anywhere in the package; it refuses neither `time.Time` nor any other
Marshaler, and reflects through both.

The same defect reaches Go 1.27's new stdlib `uuid`: `uuid.UUID` is
`[16]byte` with a `MarshalText`, so `encoding/json` emits a string and the
emitter would publish `{"type":"array","items":{"type":"integer"}}`.

### Ruling — a three-rule ladder, checked in order, no guessing

Measured on go1.27.0 (`scratchpad/tm`), a struct of one field per case
marshals as:

```
{"t":"1970-01-01T00:00:00Z","u":"00000000-…-000000000000","k":"0","m":{"raw":1},"z":1000000000}
```

so each rule below is a statement about `encoding/json`'s specified behaviour,
not an inference:

1. **A type in the known table** gets its known schema. The table is closed and
   starts with exactly one row:

   | type | schema |
   |---|---|
   | `time.Time` | `{"type":"string","format":"date-time"}` |

   Adding a row is an amendment to this table, in a diff, with the marshalled
   output quoted.

2. **Otherwise, a type implementing `encoding.TextMarshaler`** →
   `{"type":"string"}`. This is not a guess: `encoding/json` is specified to
   quote a `TextMarshaler`'s output, so the wire form is *always* a JSON
   string. A `format` is added only from the table above
   (`uuid.UUID` → `format: "uuid"` is the second row this earns).

3. **Otherwise, a type implementing `json.Marshaler`** → emit `{}` and
   **refuse**, naming the type. Its wire form is arbitrary bytes chosen by a
   method reflection cannot read. This is the case warren.md §4.3 already
   promises and the package does not implement.

Checked before the `reflect.Kind` switch, and on the pointer type as well as
the value type, because `json.Marshal` checks both.

`time.Duration` stays `{"type":"integer"}` — it has no marshaller and is
nanoseconds on the wire, which `integer` describes correctly.

### Definition of done

- [ ] `time.Time` field emits `{"type":"string","format":"date-time"}` inline,
      with no `time.Time` component entry. Golden test.
- [ ] A `TextMarshaler` type emits `{"type":"string"}`, no refusal.
- [ ] A `json.Marshaler`-only type emits `{}`, records a Refusal naming the
      type, appears in `x-warren-undescribed`, and fails `openapi.Strict()`.
- [ ] warren.md §4.3's refusal list is corrected to describe the ladder.

---

## Problem 3 — the refusal mechanism covers two cases out of the set it promises

`openapi/doc.go:16-24` is unambiguous:

> WHAT IT CANNOT DESCRIBE, IT REFUSES OUT LOUD. … Every refusal reaches
> `Document.Refusals()`, a boot WARN, and an `x-warren-undescribed` extension
> on the operation itself; `openapi.Strict()` turns the set into a boot
> failure.

`refuse` has four call sites: the validator being `validate.None()`, a `Raw`
route, an interface-typed field, and an unmapped `reflect.Kind`. Everything
else the emitter declines to describe is dropped in silence.

The field test found the second instance (report finding 8):
`validate:"required,isbn13"` is enforced by the service — a bad ISBN is a 400
naming the field — and published as a bare `{"type":"string"}`.
`openapi/schema.go:110-116` says why, and says it correctly:

> A tag this does not know is IGNORED rather than guessed at — a wrong
> constraint in a published document is worse than a missing one, because a
> client generator enforces it.

The reasoning is right and the conclusion is half-done: ignoring is correct,
**silently** ignoring is what the package's own contract forbids.

### Ruling

`applyValidate` records a Refusal for every token it does not map, naming the
route, the field and the token, with the reason *"the service enforces this
and no JSON Schema keyword expresses it, so the published schema is less
strict than the server"*.

**Do not extend the mapping table to `isbn13`.** go-playground's `isbn13`
accepts hyphens and spaces, so any `pattern` this emitter invented would be a
constraint a generated client enforces and the server does not — the exact
failure `applyValidate`'s comment exists to prevent.

### Is this one bug or three?

The coordinator asked. **Two, and problem 1 is not one of them.**

- Problem 1 is a **naming** bug. Both types are fully describable; the emitter
  gave them one key. No refusal could have caught it, because nothing was
  refused — a wrong answer was published as a confident one. This is the
  serious one precisely because the refusal machinery is irrelevant to it.
- Problems 2 and 3 are **one** bug with two symptoms: `refuse` is wired to two
  structural cases when the stated contract is "everywhere the emitter
  declines to describe something". `time.Time` and `isbn13` are the same
  omission at two layers.

### Noise, and why it is the right noise

An application using twenty unmapped `validate:` tokens will record twenty
refusals, and `openapi.Strict()` will refuse to boot. That is correct and it
is what `Strict()` is documented to be — *"the gate a team turns on once its
document is complete"*. The refusals are the list of things the document does
not say. Suppressing them to keep `Strict()` comfortable would be choosing a
quiet gate over a true one.

### Definition of done

- [ ] An unmapped `validate:` token records a Refusal and reaches
      `x-warren-undescribed`; a mapped one does not.
- [ ] `openapi.Strict()` fails to boot on a route carrying `isbn13`. Golden
      diagnostic.
- [ ] Every `refuse` reason has a golden test (AGENT.md § Testing).

---

## Problem 4 — the package is unreachable by both documented install routes

Two halves, and only the second is mine to fix.

**The tag is the human's.** `go get github.com/MerseniBilel/warren/openapi@latest`
404s while all six siblings resolve at v0.2.1. README lists the package as
shipped. Nothing in this spec changes that; it needs a release.

**The `--framework` half is one line, and it is a repeat of a defect its own
comment documents.** `cli/internal/scaffold/scaffold.go:356-363`:

```go
var frameworkModules = []string{
	"", "/transport/http", "/persistence/postgres",
	"/observability", "/broker/kafka", "/validate/playground",
}
```

The comment above it reads:

> EVERY one, not only those a given scaffold requires today: a replace for an
> unrequired module is inert, but the moment a user runs `go get` for one that
> is missing it resolves from GITHUB instead of the local checkout, silently,
> and they are running two versions of the framework at once. A field test hit
> exactly that with validate/playground.

`openapi` is the module the list now misses, and field test #14 hit exactly
that, again. **Confirmed: adding `"/openapi"` to this slice is the whole fix.**
`Replaces` iterates the slice and needs no change.

### Definition of done

- [ ] `"/openapi"` in `frameworkModules`.
- [ ] A test asserts the slice covers every `go.mod` in the repository, so the
      next module cannot be forgotten. This is the second omission; a list
      maintained by memory has now failed twice.

---

## Deliberately out of scope

- **`/healthz` and `/readyz` are absent from the document** (report finding
  10). They are adapter routes, not module routes, and the emitter reads
  `transport.Table`. Emitting them means the health adapter registering into
  the table, which is a transport decision, not an openapi one. Recorded here
  so the next reader does not re-derive it; not fixed here.
- **A `Raw` route whose pattern carries a wildcard** emits a templated path
  with no `parameters` — structurally invalid OpenAPI, the same shape as
  `transport/SPEC.md`'s problem. `addRaw` (`emit.go:177-200`) already refuses
  the route, so the document admits it is undescribed. Fixing it means
  deriving parameters from the pattern alone, which is expressible
  (`transport.wildcards`) but is a second change to the same file; it belongs
  in whichever of the two lands second.

## Testing

Every item above gets a golden test — the document IS the product, and
`openapi/emit_test.go` already carries the pattern. The collision test needs a
fixture with two same-named types in two packages; `openapi/` has no such
fixture today and one must be added under `openapi/internal/`.
