# Spec — `warren/errors`

**Status:** approved 2026-07-28, implemented
**Roadmap:** v0.1 · item 1 of 11
**Import path:** `github.com/MerseniBilel/warren/errors`
**Depends on:** nothing — this is the first package

---

## 1. Problem

Every other Warren package returns errors from this one, and four transports have
to turn them into their own vocabulary without inspecting strings. Two failures
are already ruled out by [docs/architecture.md §6](../docs/architecture.md):

- **Sentinel errors** (`var ErrNotFound = errors.New(...)`) carry no context.
  The caller learns *what kind* of failure occurred and nothing about which
  resource, which operation, or how to fix it.
- **String matching** in a transport adapter. `strings.Contains(err.Error(),
  "not found")` is how a 404 silently becomes a 500 after someone rewords a
  message.

`errors` is built first because retrofitting an error model rewrites every
signature that touches one.

The second problem is quality. Bad framework errors — especially DI errors — are
the single most common reason people abandon a framework. An error must name
**what failed, who asked for it, and the fix.** That is a structural requirement
on the type, not a style note for whoever writes the message: if the fix is
prose inside a sentence, no test can assert it exists and no renderer can
present it consistently.

## 2. Goals

1. One concrete error type carrying a **semantic code** every transport maps
   exhaustively, checked by the `exhaustive` linter rather than by review.
2. Structured context — operation, key/value fields, a suggested fix — attached
   without string formatting.
3. Full `errors.Is` / `errors.As` / `Unwrap` interoperability, so a Warren error
   is an ordinary Go error to everything that does not know about Warren.
4. A **deterministic** rendered message, so golden tests can assert on the text
   itself. This is what makes the v0.1 exit criterion on the missing-provider
   message testable.
5. Standard library only, permanently.

## 3. Non-goals

- **Stack traces.** They are a debugging aid that costs an allocation and a
  capture on every error in the request path, and the operation chain (§5.4)
  gives the same navigational value at a fraction of the cost. Reconsidered only
  with a concrete report that ops were insufficient.
- **Error groups / multi-errors of our own.** `errors.Join` from the standard
  library is re-exported and is enough.
- **Localisation.** Messages are English, addressed to a developer.
- **HTTP or gRPC status mapping.** That belongs to the transport adapters, which
  import this package; this package imports no transport. Only the *table* of
  which code means what lives in `docs/architecture.md §6`.

## 4. Public API

```go
// Package errors is Warren's error model: one type, one semantic code, and
// enough structure that every transport maps an error without reading its text.
package errors

// Code classifies an error semantically, independently of any transport.
// Its zero value is CodeInternal, so an error that never states a code, and
// any error from outside Warren, is treated as internal.
type Code uint8

const (
	CodeInternal         Code = iota // an invariant broke; the caller cannot fix it
	CodeNotFound                     // the addressed thing does not exist
	CodeConflict                     // the request contradicts current state
	CodeInvalid                      // the request is malformed or fails validation
	CodePermissionDenied             // the caller is known and not allowed
)

// String returns the code's name, for example "NotFound".
func (c Code) String() string

// Error is Warren's error type. The zero value is not usable; construct one
// with NotFound, Conflict, Invalid, PermissionDenied, or Internal.
type Error struct{ /* unexported */ }

// Field is one piece of structured context attached to an Error.
//
// Value precedes Key because a string's length word is not a pointer: trailing
// it takes the struct from 32 scanned bytes to 24, which govet's fieldalignment
// check requires. Always construct a Field with keyed fields, never positionally.
type Field struct {
	Value any
	Key   string
}

// ── Construction ──────────────────────────────────────────────────────────
// Each takes a printf-style message. Do not use %w here — wrap with Wrapping.

func NotFound(format string, args ...any) *Error
func Conflict(format string, args ...any) *Error
func Invalid(format string, args ...any) *Error
func PermissionDenied(format string, args ...any) *Error
func Internal(format string, args ...any) *Error

// ── Builders ──────────────────────────────────────────────────────────────
// Each returns a copy with one addition; the receiver is never modified, so an
// *Error may be shared or stored without a caller being able to mutate it.

// Wrapping records err as the cause. The result unwraps to err, so errors.Is
// and errors.As reach through it. Wrapping(nil) returns the receiver unchanged.
func (e *Error) Wrapping(err error) *Error

// Field attaches structured context. Fields are ordered and are not
// deduplicated: two calls with the same key produce two fields.
func (e *Error) Field(key string, value any) *Error

// Op names the operation that failed, for example "di.Resolve". Ops accumulate
// as an error travels outward and form the chain shown in the message.
func (e *Error) Op(op string) *Error

// Fix records a copy-pasteable remedy, for example
// "add warren.Provide(NewDB) to internal/platform/module.go".
// It is a separate field rather than part of the message so that it can be
// rendered distinctly and asserted on in a test.
func (e *Error) Fix(format string, args ...any) *Error

// ── Reading ───────────────────────────────────────────────────────────────
// The package-level functions accept any error, including one from outside
// Warren, and walk the unwrap chain.

// CodeOf reports the semantic code of err. It returns the code of the
// outermost Error in the chain, and CodeInternal if there is none — including
// when err is nil, which callers must not reach.
func CodeOf(err error) Code

// Ops returns the operation chain, outermost first.
func Ops(err error) []string

// Fields returns every field in the chain, outermost first.
func Fields(err error) []Field

// Fix returns the innermost suggested fix, or "" if none was recorded.
// Innermost wins: the layer closest to the failure knows the real remedy.
func Fix(err error) string

// Error returns the rendered message. See §5.5 for the exact format.
func (e *Error) Error() string

// Unwrap returns the wrapped cause, or nil.
func (e *Error) Unwrap() error

// Detail renders err across multiple lines with its fields and fix, for a
// terminal or a log. Single-line callers use err.Error().
func Detail(err error) string

// ── Standard library re-exports ───────────────────────────────────────────
// So that importing warren/errors never requires also importing errors.

func Is(err, target error) bool
func As(err error, target any) bool
func Unwrap(err error) error
func Join(errs ...error) error
```

There is deliberately **no `New` and no `Errorf`.** Every error made through
this package carries a semantic code, and the zero-code case is exactly the
"someone will map this to a 500 by accident" failure the package exists to
prevent. Code that genuinely wants an uncoded sentinel imports the standard
library's `errors` directly and states so.

## 5. Behaviour

### 5.1 Immutability

Builders return a shallow copy with the one addition. `Wrapping`, `Field`, `Op`,
and `Fix` never modify their receiver. This makes a package-level
`var errClosed = errors.Conflict("...")` safe to `Wrapping` per call site
without callers corrupting each other — and it is why the type has no exported
fields.

### 5.2 Code resolution

`CodeOf` returns the code of the outermost `*Error`. An outer layer that
deliberately reclassifies — a repository turning a driver's row-not-found into
`CodeNotFound` — wins over what it wrapped, which is the intended direction:
the outer layer has more context about what the caller asked for.

A non-Warren error, and `nil`, resolve to `CodeInternal`. That means an
unmapped error becomes a 500 rather than a misleading 404.

`CodeOf` walks the chain explicitly rather than through `As`, because passing a
target to `As` forces it onto the heap and every transport calls `CodeOf` on
every error it maps. That is what buys the zero-allocation line in §5.6, and it
sets the traversal rule in §7.

### 5.3 Fields

Ordered slice, never a map. Map iteration order is randomised, and a golden test
on a message containing randomly ordered fields fails intermittently — which is
worse than having no golden test, because it teaches everyone to re-run CI.

### 5.4 Operation chain

`Op` accumulates outward. A `di` failure surfacing through boot reads:

```
warren.Run: di.Build: no provider for *sql.DB
```

This is the navigational value a stack trace would give, at one string per
layer, and it names the *logical* operation rather than the Go function that
happened to be on the stack.

### 5.5 Message format

`Error()` is `ops joined by ": "`, then the message, then the wrapped cause:

```
<op>: <op>: <message>: <cause>
```

Ops are omitted when absent, and the cause when there is nothing wrapped.
Fields and the fix are **not** in `Error()` — they belong to `Detail` and to
structured logging, so that a single-line log entry stays one line.

`Detail` renders:

```
warren.Run: di.Build: no provider for *sql.DB

  requested by  internal/modules/orders/module.go:14
  chain         *orders.Handler → *orders.Repository → *sql.DB

  fix: add di.Provide[*sql.DB](c, NewDB) to internal/modules/orders/module.go
```

Exact spacing is fixed by a golden file, because this is the text the v0.1 exit
criterion tests.

### 5.6 Allocation

An error is constructed on the request path, so the budget is stated up front
and enforced by a benchmark:

| Operation | Allocations |
|---|---|
| `NotFound("...")` with no args | ≤ 1 |
| `.Op(...)` / `.Fix(...)` | ≤ 2 (copy + the addition) |
| `.Field(k, v)` | ≤ 2, amortised |
| `CodeOf` on a five-deep chain | 0 |

## 6. Every error message this package emits

The package raises no errors of its own — it *is* the error package, and it
panics nowhere (AGENT.md forbids `panic` in library code). Two edge cases
instead resolve silently and predictably:

| Situation | Behaviour |
|---|---|
| `Wrapping(nil)` | returns the receiver unchanged; no sentinel "wrapped nil" error |
| `CodeOf(nil)` | returns `CodeInternal`; callers should not pass nil, and this makes it harmless |
| `Code` value outside the constants | `String()` returns `Code(7)`, so a bad value is visible rather than blank |

## 7. Interoperability

- `errors.Is(err, sql.ErrNoRows)` reaches through any number of `Wrapping`
  layers, because `*Error` implements `Unwrap`.
- `errors.As(err, &pgErr)` likewise.
- **`*Error` deliberately does not implement `Is(target error) bool`.** Matching
  two errors because they share a code is a footgun: it makes every `NotFound`
  equal to every other, and it is not what a reader expects `errors.Is` to mean.
  Compare codes with `CodeOf(err) == CodeNotFound`.
- `fmt.Errorf("...: %w", warrenErr)` produces a standard wrapped error that
  `CodeOf` still reads through, so a caller that forgets `Wrapping` is degraded,
  not broken.
- **`CodeOf`, `Ops`, `Fields`, and `Fix` follow single-error unwrapping only.**
  An error built by `Join` holds several unrelated errors, so it has no single
  semantic code, operation chain, or fix — it reports `CodeInternal` and
  contributes nothing to the other three. `Is` and `As` still traverse it,
  because those are the standard library's and joining is exactly what they are
  for. A caller wanting the code of one branch inspects that branch.

## 8. Enforcement

Two linter settings ship with this package, in `.golangci.yml`:

- `exhaustive` configured over `errors.Code`, so a transport adapter that adds
  no case for a new code fails the build. This is the mechanism
  `docs/architecture.md §8` names.
- `govet`'s printf analyser told about the five constructors and `Fix`, so
  `NotFound("...: %w", err)` is a build failure rather than a message with a
  stray `%!w`.

## 9. Testing

Unit tests only — no Docker, no network, no sleeps.

- Table-driven, `t.Parallel()`, subtests named for behaviour.
- **Golden file on `Detail` output**, including the multi-line DI example, since
  that string is a v0.1 exit criterion.
- Immutability: build an error, derive three variants, assert the original is
  unchanged in every field.
- Interop: `Is` and `As` reach through `Wrapping`, through `fmt.Errorf("%w")`,
  and through `Join`.
- `CodeOf` on: nil, a stdlib error, a wrapped Warren error, a Warren error
  wrapped in a stdlib error, and a five-deep mixed chain.
- Ordering: `Ops` and `Fields` are outermost-first and stable across 1000 runs.
- Benchmarks covering construction, each builder, `CodeOf`, `Error`, and
  `Detail`, with `-benchmem`.
- A test asserting the §5.6 budget with `testing.AllocsPerRun`. It is built
  under `//go:build !race`, because the race detector adds allocations of its
  own and makes an exact count meaningless — so it runs under `make test-short`
  and not under `make test`.
- `Example` functions for each constructor and for `Detail`, compiled by CI —
  the exit criterion requires every documented concept to carry a runnable
  example.

## 10. Definition of done

- [x] `errors/` implements §4 exactly, standard library only
- [x] Every exported identifier has a doc comment starting with its name
- [x] Golden test on `Detail`, committed alongside its `testdata/` — five cases,
      the missing-provider one byte-identical to §5.5
- [x] Benchmarks committed, §5.6 budget met
- [x] `govet` printf settings added to `.golangci.yml`. `exhaustive` needed no
      change: it already applies to every enum type, so `Code` is covered by the
      existing block
- [x] `Example` functions for every constructor, `Detail`, `CodeOf`, `Ops`,
      `Fields`, `Wrapping`, and `Fix`
- [x] `docs/roadmap.md` v0.1 item 1 ticked
- [x] This spec corrected wherever the code diverged — §5.2, §7, and §9
- [x] `make ci` green on macOS (exit 0, golangci-lint v2.12.2, 0 issues), and
      on ubuntu-latest, windows-latest and macos-latest in CI

## 11. Decisions taken

Agreed 2026-07-28. Recorded rather than settled quietly, because they shape
every package that follows.

1. **`Fix` as a first-class field.** It makes the exit criterion assertable and
   lets the CLI render remedies consistently, at the cost of one more field on
   a hot-path type. The alternative is prose inside the message.
2. **No `New` / `Errorf`.** Forces a semantic code on every Warren error. The
   cost is that a package wanting a plain sentinel imports stdlib `errors` too.
3. **`Detail` lives here, not in `cli`.** Every package then renders errors
   identically, and `di` does not grow its own formatter. The cost is that a
   presentation concern sits in a core package.
4. **Five codes, no more.** `Unavailable` and `DeadlineExceeded` are the two
   most likely additions, and both matter for the broker work in v0.3. Adding a
   code later is a breaking change for every `exhaustive` switch — so the
   question is whether to pay for them now or accept that cost then.
5. **Zero value of `Code` is `CodeInternal`.** The alternative is a `CodeUnset`
   that every transport must then handle in its `exhaustive` switch.
