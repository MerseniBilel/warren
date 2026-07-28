# Spec: Semantic errors

| | |
|---|---|
| **Module** | `warren/errors` (core — standard library only) |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | Nothing |
| **Blocks** | Everything. Every other package returns these. |
| **PRD** | §4.5, §8, §14.2 |
| **ADRs** | None — this spec introduces no structural decision |
| **Date** | 2026-07-28 |

---

## 1. Problem

A use case must be able to say "this already exists" without knowing that HTTP
calls it 409 and gRPC calls it `AlreadyExists`. Today, every Go team solves this
with either sentinel errors that every transport must know by name, or by
returning HTTP status codes from domain code — which is the layering violation
Warren exists to prevent.

Build this first. Every package in the framework returns these types, and
retrofitting an error model later means changing every signature that touches
one.

## 2. Goals

1. **One error type with a semantic code**, mapped by each transport to its own
   vocabulary (PRD §4.5).
2. **Errors carry a fix.** A message that says what failed without saying how to
   fix it is incomplete (PRD §8).
3. **Full `errors.Is` / `errors.As` / `%w` interoperability.** Warren's errors
   are standard Go errors; nothing is required to special-case them.
4. **A transport that forgets to map a new code fails the build**, not the
   request. This is why `exhaustive` is enabled in
   [`.golangci.yml`](../../../.golangci.yml).
5. Zero allocations on the non-error path, and no stack capture unless asked for.

## 3. Non-goals

- **Not a logging package.** Errors carry structured fields;
  [`warren/log`](../02-log/spec.md) decides what is printed.
- **Not a stack-trace library.** Stack capture is opt-in and off by default.
  `pkg/errors` is banned repo-wide in `.golangci.yml` for the reasons that make
  always-on capture a bad default.
- **No error catalogue or registry.** Codes are a closed enum; application error
  identity is the message and the fields.
- No i18n. Messages are English and are for developers.

## 4. Public API

```go
package errors

// Code is the semantic classification a transport maps to its own vocabulary.
// The set is closed: adding one is a breaking change for every adapter, which
// is exactly the review pressure it should carry.
type Code uint8

const (
    CodeInternal Code = iota // zero value: an unclassified error is a bug, not a 400
    CodeInvalid
    CodeNotFound
    CodeConflict
    CodeUnauthenticated
    CodePermissionDenied
    CodeFailedPrecondition
    CodeUnavailable
    CodeDeadlineExceeded
    CodeUnimplemented
)

func (c Code) String() string

// Error is Warren's error type. Construct through the helpers below.
type Error struct { /* unexported */ }

func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Is(target error) bool   // matches on Code, so errors.Is(err, errors.NotFound(""))
func (e *Error) Code() Code
func (e *Error) Fields() map[string]any // structured detail, for log/transport
func (e *Error) Fix() string            // the copy-pasteable remedy; "" if none

// Constructors. Message arguments are formatted with fmt.Sprintf semantics.
func Internal(format string, args ...any) *Error
func Invalid(field string, err error) *Error      // PRD §14.2 calls it exactly this way
func NotFound(format string, args ...any) *Error
func Conflict(format string, args ...any) *Error
func Unauthenticated(format string, args ...any) *Error
func PermissionDenied(format string, args ...any) *Error
func FailedPrecondition(format string, args ...any) *Error
func Unavailable(format string, args ...any) *Error
func DeadlineExceeded(format string, args ...any) *Error
func Unimplemented(format string, args ...any) *Error

// Builders. Each returns a copy; an *Error is never mutated after construction,
// so a package-level sentinel cannot be corrupted by a caller.
func (e *Error) Wrapping(err error) *Error
func (e *Error) Field(key string, value any) *Error
func (e *Error) Fixed(format string, args ...any) *Error // sets the remedy text
func (e *Error) Op(op string) *Error                     // logical operation, e.g. "user.Register"

// Inspection. Both walk the chain with errors.As.
func CodeOf(err error) Code       // CodeInternal for a nil or unrecognised error
func FieldsOf(err error) map[string]any
```

Nothing here leaves the standard library, so `warren/errors` lives in the core
module and every submodule may depend on it.

**Naming note:** `Wrapping`, `Fixed`, and `Field` are builder *methods*, not
`With*` type names. The `WithTimeout`-style function prefix is fine per
[AGENT.md § Naming](../../../AGENT.md); a type named `ErrorWithFields` would not
be, and none is introduced.

## 5. Behaviour

- **The zero `Code` is `CodeInternal`.** An error that nobody classified is a
  500, never a 400. Defaulting an unclassified failure to a client error is how
  frameworks hide their own bugs.
- **`*Error` is immutable after construction.** Every builder returns a copy, so
  `var ErrNoSuchUser = errors.NotFound("user not found")` at package level is
  safe to return from many goroutines.
- **`Error()` renders** `op: message` when an `Op` is set, then the wrapped
  chain via `: `. Fields and the fix are **not** in `Error()` — they are
  retrieved structurally. Stuffing everything into the string is what makes
  errors unparseable and log lines unreadable.
- **`Is` matches on `Code`**, so `errors.Is(err, errors.NotFound(""))` works.
  `As` retrieves the concrete `*Error` for the code, fields, and fix.
- **Wrapping a non-Warren error preserves it.** `CodeOf` walks the chain and
  returns the first Warren code it finds; `context.Canceled` and
  `context.DeadlineExceeded` map to `CodeDeadlineExceeded` without needing to be
  wrapped, because every transport otherwise duplicates that check.
- **Stack capture is off.** If a stack proves necessary it is added behind an
  explicit `Stack()` builder, decided by evidence from the dogfooding service.

## 6. Errors

The package's own failure modes are limited; the table below is instead the
contract for **what every Warren error message must contain**, since that is the
feature PRD §8 stakes a claim on:

| Condition | Code | Message |
|---|---|---|
| Value fails a domain rule | `CodeInvalid` | Names the field and the rule: `email: must contain @` |
| Aggregate not found | `CodeNotFound` | Names the type and the identifier: `user 7f3a not found` |
| Uniqueness violated | `CodeConflict` | Names what collided: `user with email a@b.c already exists` |
| Dependency unreachable | `CodeUnavailable` | Names the dependency and the fix: `postgres unreachable at localhost:5432 — is the container running?` |
| Programmer error | `CodeInternal` | Names the invariant broken and the file |

A message that would be equally true of ten different failures is a bug in the
message. Review checks this (CONTRIBUTING § What review looks for, item 4).

## 7. Configuration

None. No global state, no `init()`, no configurable renderer.

## 8. Testing

Unit only — this package touches nothing external.

- **Code mapping is exhaustive.** A table test enumerating every `Code`,
  compiled against a switch with no `default`, so adding a code without updating
  the test fails the build rather than silently defaulting.
- **`Is` / `As` / `%w` interop**: wrapping a Warren error in a stdlib error and
  back, three levels deep, and asserting `CodeOf` still finds it.
- **Immutability**: `Field` on a shared sentinel from two goroutines under
  `-race`, asserting the original is unchanged.
- **`context.Canceled` and `context.DeadlineExceeded`** map without wrapping.
- **Golden files for message rendering**, so a message regression shows up as a
  diff a human reads. The messages are the feature; they get the same treatment
  as generated code.
- **Benchmark**: `Error()` rendering and `CodeOf` on a five-deep chain, to hold
  the zero-allocation claim in §2.

## 9. Invariants touched

- **Invariant 1** — core module, standard library only. Nothing here needs
  otherwise.
- **Invariant 3** — this package is what lets domain code return semantic
  failures without importing a transport, which is what makes the dependency
  rule survivable in practice.

## 10. Definition of done

- [ ] Public API matches §4
- [ ] Unit tests per §8, `-race -shuffle=on`
- [ ] Golden files for every message form
- [ ] `make ci` green
- [ ] Doc comment on every exported identifier, starting with its name
- [ ] `docs/` concept page: the error model and the transport mapping table
- [ ] Runnable example in `examples/errors/`
- [ ] Changelog fragment
- [ ] `exhaustive` settings in `.golangci.yml` confirmed to catch a non-exhaustive switch over `Code` — verified by deliberately writing one and watching it fail

## 11. Open questions

1. **Is `CodeFailedPrecondition` earning its place?** gRPC distinguishes it from
   `CodeInvalid`; HTTP mostly does not (both are 4xx). Ten codes that map cleanly
   beat twelve that adapters guess at. Resolve from the dogfooding service.
2. **`Op` as a string, or a typed operation?** A string is simple and matches
   Go's `*os.PathError` idiom. Typed would let `warren graph` correlate errors to
   handlers — but that is a v0.4 concern and this is a v0.1 decision.
3. **Should `Fix()` be structured** (a command plus prose) rather than free text?
   The CLI could then offer to run it. Deferred until `warren doctor` (v0.4)
   shows what shape it wants.
