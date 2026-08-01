# `github.com/MerseniBilel/warren/errors` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — table, constructors, `Error`'s surface, `Unwrap`, rendering, and the `Is` collision all settled (Open questions 1–7 and 9 resolved below); the one remaining open question (8, the conformance suite's home) blocks nothing in this package |
| **Source** | [warren.md §2.6](../warren.md) |
| **Module** | core |
| **Mode** | Build |
| **Wraps** | — |

## Problem

Warren's first claim is transport-agnostic use cases: one
`app.Handler[Req, Res]`, with HTTP, gRPC, and message consumers as thin adapters
over it (AGENT.md § What Warren is). That claim dies the moment a handler has to
decide that "this failure is a 409". A handler that maps a code to a status has
broken ring 2 (AGENT.md § The error table is load-bearing).

So the failure has to travel as *meaning*, not as a status. Domain code returns
`errors.Conflict(...)`; each adapter translates that meaning into its own
protocol's vocabulary — 409, `AlreadyExists`, or an ack. warren.md §1.4 states
it directly: **"The error table (§2.6) is the load-bearing piece."** warren.md
§2.6 calls this package **"load-bearing for the entire transport story."**

This is the package the three-protocol story rests on. If the vocabulary is
wrong, or if an adapter is allowed to invent its own, "one handler serves three
protocols" stops being true.

## Goals

- Own **the semantic error vocabulary** — a closed set of codes that mean
  something to a domain expert and nothing to a protocol (warren.md §2.6).
- Give domain and application code constructors that read like the failure they
  describe: `errors.Conflict("user already exists")`, `errors.NotFound("user",
  id)`, `errors.Invalid("email", err)` (warren.md §3.1, §6.1, §10).
- Let a caller ask about meaning without type assertions: `Is(err, CodeConflict)`.
- Carry per-error detail — `WithDetail(k, v)` — so an adapter has something to
  put in a response body beyond the code.
- Depend on the standard library only. This is kernel, and the kernel has no
  knowledge that HTTP, SQL, or Kafka exist (warren.md §1.1).

## Non-goals

- **This package does not translate.** It owns the vocabulary; **each adapter
  owns its column** of the table. No `HTTPStatus()` method, no `GRPCCode()`
  method, no mapping table exported from here. AGENT.md: "Each adapter owns its
  column." The moment a status code is reachable from this package, the kernel
  knows HTTP exists and invariant 1 is gone.
- **Not a replacement for `errors` in the standard library.** Nothing here
  replaces `fmt.Errorf("...: %w", err)`; AGENT.md § Errors requires `%w`
  wrapping with added context throughout the codebase.
- **Not an open code set.** The seven codes in §2.6 are the vocabulary. Adding
  one changes every adapter's column and is an architecture change (AGENT.md
  § Modes).
- **Not a retry policy.** `CodeUnavailable` is annotated "retryable" in §2.6 and
  `app.Retrying(policy)` retries on it (warren.md §3.2), but the policy lives in
  `app`, not here.

## Public API

Taken from warren.md §2.6, with doc comments added. The four constructors
warren.md originally elided behind `// ...` were agreed on 2026-08-01 and
warren.md §2.6 now lists all seven (Open questions 2, resolved).

```go
// Package errors defines Warren's semantic error vocabulary: a closed set of
// codes that describe what went wrong in terms a domain expert would use, with
// no reference to any transport.
//
// Domain and application code returns these errors. Each transport adapter
// owns the translation from a Code into its own protocol — HTTP status, gRPC
// code, or consumer ack semantics. Nothing in this package knows those
// mappings exist.
package errors

// Code is the semantic classification of a failure. The set is closed: these
// seven codes are the whole vocabulary, and every adapter maps every one of
// them.
type Code string

const (
	// CodeInvalid means the request was malformed or violated a constraint.
	// The caller must change the request before retrying.
	CodeInvalid Code = "INVALID"

	// CodeNotFound means the addressed resource does not exist.
	CodeNotFound Code = "NOT_FOUND"

	// CodeConflict means the request collided with the current state — a
	// duplicate, or a transition the aggregate does not allow from here.
	CodeConflict Code = "CONFLICT"

	// CodeUnauthenticated means the caller's identity was absent or could not
	// be established.
	//
	// It describes the caller's identity, not yours. A service that fails to
	// authenticate to something downstream — Postgres, S3, another API —
	// returns CodeUnavailable, never CodeUnauthenticated.
	CodeUnauthenticated Code = "UNAUTHENTICATED"

	// CodePermissionDenied means the caller is known but is not allowed to
	// perform this operation.
	CodePermissionDenied Code = "PERMISSION_DENIED"

	// CodeUnavailable means a dependency was temporarily unreachable: the same
	// request may succeed later unchanged. It is the only code warren.md
	// annotates retryable, and the one app.Retrying(policy) retries (§3.2).
	// The §2.6 table also retries CodeInternal in its consumer column.
	//
	// This is the code for failing to authenticate to a downstream dependency
	// — Postgres, S3, another API. That failure is about your service's
	// credentials, not the caller's identity, so it is CodeUnavailable and it
	// retries; it is never CodeUnauthenticated.
	CodeUnavailable Code = "UNAVAILABLE" // retryable

	// CodeInternal means the failure was not anticipated. It carries no promise
	// that retrying helps.
	CodeInternal Code = "INTERNAL"
)

// Error is Warren's semantic error. It carries a Code, a message, an optional
// wrapped cause, and any details attached with WithDetail. All fields are
// unexported; adapters read them through the accessor methods below.
type Error struct {
	code    Code
	msg     string
	cause   error
	details map[string]any
}

// Invalid reports that field failed validation or conversion, wrapping err as
// the reason. The resulting Error carries CodeInvalid and the message
// "field <field> is invalid".
func Invalid(field string, err error) *Error

// NotFound reports that no resource of the named kind exists with this id. The
// resulting Error carries CodeNotFound and the message
// "<resource> <id> not found"; id is rendered with %v.
func NotFound(resource string, id any) *Error

// Conflict reports that the request collided with current state. The resulting
// Error carries CodeConflict. args are fmt operands for msg, printf-style;
// when args is empty, msg is used verbatim (no % expansion).
func Conflict(msg string, args ...any) *Error

// Unauthenticated reports that the caller's identity was absent or could not
// be established; reason is the message verbatim. It describes the CALLER's
// identity — a downstream auth failure is Unavailable, never this.
func Unauthenticated(reason string) *Error

// PermissionDenied reports that the known caller may not perform action. The
// message is "not allowed to <action>".
func PermissionDenied(action string) *Error

// Unavailable reports that a dependency was temporarily unreachable, wrapping
// err as the cause. The message is "<dependency> is unavailable". This is the
// retryable code — and the right one when your own service fails to
// authenticate downstream.
func Unavailable(dependency string, err error) *Error

// Internal reports an unanticipated failure, wrapping err as the cause. The
// message is "unexpected failure".
func Internal(err error) *Error

// Code returns the semantic classification.
func (e *Error) Code() Code

// Message returns the human-readable message, without the code prefix and
// without the cause.
func (e *Error) Message() string

// Details returns a copy of the details attached with WithDetail; mutating the
// returned map does not touch e. The copy is shallow — the map is copied, the
// values are shared. It returns nil when no detail was attached.
func (e *Error) Details() map[string]any

// Error renders "CODE: message", with ": cause" appended when a cause is
// wrapped. This is the string that lands in logs.
func (e *Error) Error() string

// Unwrap returns the wrapped cause, or nil — so the standard library's
// errors.Is and errors.As see through a Warren error.
func (e *Error) Unwrap() error

// WithDetail attaches the key/value pair to e and returns e, so adapters have
// structured context to put in a response body. It mutates e — errors are
// constructed, decorated, and returned on one failure path, never shared. It
// is a method, not a type name, and so is permitted by AGENT.md § Naming.
func (e *Error) WithDetail(k string, v any) *Error

// Is reports whether err, or any error it wraps, carries code — the whole
// chain is searched, joined errors included, so a Warren error wrapped inside
// another Warren error is still found. It never panics, a typed-nil *Error
// included.
func Is(err error, code Code) bool
```

Two behaviours hardened by the 2026-08-01 review:

- **`Is` searches the whole chain.** `Is(Internal(NotFound(...)), CodeNotFound)`
  is true — "or any error it wraps" means what it says. The asymmetry with
  adapters is deliberate: an adapter's status mapping translates the
  *outermost* code (wrapping is recategorization); `Is` answers whether the
  meaning appears anywhere.
- **Every method is nil-receiver safe.** The classic slip
  `var e *errors.Error; ...; return e` produces a non-nil `error` interface
  holding a nil pointer; `Error()` renders `<nil>`, `Code()` returns the zero
  `Code` (which adapters map to `INTERNAL`), and `Is` neither panics nor
  matches. AGENT.md's no-panic rule holds on this path too.

`*Error` satisfies the `error` interface — warren.md §10 returns
`errors.Invalid("email", err)` directly as a function's `error` result.

**The type's qualified name is `errors.Error`** (Open questions 3, resolved):
warren.md's stray references to `warren.Error` in §1.4, §2.7, §4.2, and the §9
ledger were corrected on 2026-08-01. Code that also touches driver sentinels
imports this package under the alias `werrors`, keeping the bare `errors`
identifier for the standard library (Open questions 7, resolved; warren.md
§6.1 amended).

## Behaviour

### The table — one vocabulary, three columns

Reproduced verbatim from warren.md §2.6 (and restated identically in AGENT.md
§ The error table is load-bearing):

| Code | HTTP | gRPC | Consumer |
|---|---|---|---|
| `INVALID` | 400 | `InvalidArgument` | → DLQ (never retry) |
| `NOT_FOUND` | 404 | `NotFound` | ack + log |
| `CONFLICT` | 409 | `AlreadyExists` | ack (idempotent replay) |
| `UNAUTHENTICATED` | 401 | `Unauthenticated` | → DLQ (never retry) |
| `PERMISSION_DENIED` | 403 | `PermissionDenied` | → DLQ (never retry) |
| `UNAVAILABLE` | 503 | `Unavailable` | nack + backoff retry |
| `INTERNAL` | 500 | `Internal` | nack + retry, then DLQ |

Two normative paragraphs sit under the table in both source documents. They are
contract text for the adapters that implement the columns, so they are
reproduced here in warren.md's wording:

**Why the two auth codes dead-letter rather than retry or ack.** The message
won't get a better token by waiting — retrying an expired credential just burns
the backoff budget and delays the inevitable. And acking means "handled, delete
it" — but an auth failure on a queue is a bug in your own system (a service
published without proper identity, or with someone else's), and acking destroys
the evidence. The DLQ stops the message, keeps it for inspection, and fires the
DLQ alert. Which is correct, because this should wake someone up.

**`UNAUTHENTICATED` describes the caller's identity, not yours.** If your
service failed to authenticate to something downstream — Postgres, S3, another
API — that is not `UNAUTHENTICATED`; it is `UNAVAILABLE`, and it retries.
Adapter authors get this wrong constantly, which is why the rule sits next to
the table — and in the doc comments on `CodeUnauthenticated` and
`CodeUnavailable` above, so it is visible at the point of use.

**Read the ownership carefully. This package owns the `Code` column and
nothing else.**

- The **HTTP** column is implemented by `warren/transport/http` (warren.md §4.1).
- The **gRPC** column is implemented by `warren/transport/grpc` — §4.2 says it
  is wrapped "specifically so ... `warren.Error` maps to `codes.Code` without
  handler involvement."
- The **Consumer** column is implemented by the broker middleware chain —
  `Retry(backoff)` and `DeadLetter` in warren.md §3.4.

The table is reproduced here because it is the contract those three adapters
implement, not because this package executes it. Each adapter's own SPEC.md
restates its column and tests it; this document is where the rows are agreed.

### Where the errors come from and where they go

warren.md §1.4, the transport spine:

```
Handler[Req, Res].Handle
         │
         ├──▶ encode success
         └──▶ errors.Error
                  NotFound → 404 / NotFound / ack
                  Conflict → 409 / AlreadyExists / ack
                  Internal → 500 / Internal / nack
```

- **Domain code** returns them: `errors.Conflict("user already active")`
  (warren.md §3.1). §10 notes the domain package's entire import list is
  `warren/domain, warren/errors`.
- **Application code** returns them: `errors.Invalid("email", err)` and
  `errors.Conflict("user already exists")` (warren.md §10).
- **Repository adapters** translate driver errors into them:
  `errors.NotFound("user", id)` on `pgx.ErrNoRows` (warren.md §6.1). This is the
  boundary where a driver error stops being a driver error.
- **`warren/validate`** produces them with `CodeInvalid` and per-field details
  (warren.md §2.7).
- **`app.Retrying(policy)`** consumes them: it retries on `CodeUnavailable`
  (warren.md §3.2), which is what the "retryable" annotation on the constant is
  for.
- **`app.Metered()`** consumes them: "error counter by code" (warren.md §3.2).

### Consequences of the ring rules

- **No reflection on the request path** (AGENT.md invariant 7). Errors are
  constructed per request, on the failure path. Construction is a struct
  allocation and string formatting — nothing more.
- **No driver type in a public signature** (invariant 3). No `codes.Code`, no
  `http.Response`, nothing from any driver appears here.
- **No `panic`, no `init()`, no package-level mutable state** (AGENT.md
  § General).

## Errors

This package *is* the error vocabulary; the question here is what text the
constructed errors carry.

The message texts were agreed on 2026-08-01. Every row is contract, covered by
a golden file:

| Constructor | Message text |
|---|---|
| `Invalid(field, err)` | `field <field> is invalid`; `err` becomes the wrapped cause. |
| `NotFound(resource, id)` | `<resource> <id> not found`; `id` rendered with `%v`. |
| `Conflict(msg, args...)` | `fmt.Sprintf(msg, args...)`; `msg` verbatim when `args` is empty. |
| `Unauthenticated(reason)` | `<reason>`, verbatim. |
| `PermissionDenied(action)` | `not allowed to <action>`. |
| `Unavailable(dependency, err)` | `<dependency> is unavailable`; `err` becomes the wrapped cause. |
| `Internal(err)` | `unexpected failure`; `err` becomes the wrapped cause. |
| `(*Error).Error() string` | `CODE: message`, with `: cause` appended when a cause is wrapped. Details are **not** rendered — they are structured payload for adapters, not log text. |

These meet the AGENT.md § Errors bar — `NotFound` names the resource kind and
the id it looked for; `Invalid` names the field and carries the violated
constraint in its cause; `Unavailable` names *what* was unreachable, which is
the fact an operator needs.

## Testing

- **Golden-file test for every error message** (AGENT.md § Testing: "Every error
  message in a spec gets a golden-file test"). Once the text above is agreed,
  each constructor's rendered output — and `(*Error).Error()` with and without
  details and a wrapped cause — gets a golden file. The diagnostics are the
  product; untested error text rots immediately.
- **Allocation benchmark on the request path.** The failure path is a request
  path: a 4xx-heavy endpoint constructs one of these per request. Benchmark
  allocations for `NotFound`, `Invalid`, `Conflict`, and for `WithDetail`
  chained twice.
- **`Is` is exercised through wrapping.** `Is(fmt.Errorf("...: %w", NotFound(...)),
  CodeNotFound)` must report true, since AGENT.md § Errors mandates `%w`
  wrapping everywhere and the adapters see the wrapped error, not the original.
- **Table-driven, `t.Parallel()`, named for behaviour** (AGENT.md § Testing).
- **No Docker, no network, no sleeps.** Nothing here needs any of them.
- **Code coverage of the table is an adapter concern.** Each adapter's suite
  asserts its own column; this package's suite asserts that all seven codes
  exist and are distinct. A cross-package test that every adapter maps every
  code is worth having but cannot live in the core module — see Open questions 7.
- **The two auth rows are tested exactly like the other five.** Each adapter's
  suite asserts `UNAUTHENTICATED → 401 / Unauthenticated / DLQ` and
  `PERMISSION_DENIED → 403 / PermissionDenied / DLQ` alongside its other
  mappings. The consumer suite in particular must assert both codes dead-letter
  **without a retry attempt** — the never-retry semantics are the contract, per
  the normative paragraphs under the table.

## Definition of done

- [x] `Code`, the seven constants, `Error`, `Is`, `WithDetail`, and all seven
      constructors exist, each with a doc comment starting with the
      identifier's name — `errors/errors.go`, 2026-08-01.
- [x] `*Error` satisfies `error` (fixed by §10's use of it as a return value).
- [x] Unwrap behaviour decided in `warren.md` §2.6 and implemented — `Unwrap()`
      returns the wrapped cause (Open question 6, resolved).
- [x] Package compiles under the core module importing the standard library
      only — no `net/http`, no `google.golang.org/grpc`, no driver of any kind;
      enforced by `scripts/invariants.sh` in `make ci`.
- [x] Every message text agreed by the human is recorded in the Errors section
      above and covered by a golden file — `errors/testdata/*.golden`.
- [x] Allocation benchmarks exist for the constructors and `WithDetail`.
- [x] `Is` tests cover direct, `%w`-wrapped, and non-Warren errors.
- [x] The two missing table rows (Open questions 1) are resolved and warren.md
      §2.6 is amended in the same change — done 2026-08-01: both documents now
      carry the seven-row table and the two normative paragraphs, and §1.4's
      diagram was corrected to `Conflict → 409 / AlreadyExists / ack`.
- [x] `make ci` passes — fmt, vet, golangci-lint, invariants, `go test -race`,
      all green on 2026-08-01.

## Open questions

1. **RESOLVED (2026-08-01) — the table now has seven rows.** warren.md §2.6 and
   AGENT.md § The error table is load-bearing were both amended:
   `UNAUTHENTICATED → 401 / Unauthenticated / → DLQ (never retry)` and
   `PERMISSION_DENIED → 403 / PermissionDenied / → DLQ (never retry)`. Both
   auth codes dead-letter because retrying will not mint a better token, and
   acking destroys the evidence of a producer publishing with the wrong
   identity — the DLQ preserves the message and fires the alert. The
   caller-identity rule was added alongside: `UNAUTHENTICATED` describes the
   caller's identity, not yours; a service failing to authenticate downstream
   returns `UNAVAILABLE`. §1.4's diagram was corrected to `Conflict → 409 /
   AlreadyExists / ack` in the same change, removing its contradiction with
   §2.6. The full table and both normative paragraphs are reproduced in
   Behaviour above.
2. **RESOLVED (2026-08-01) — all seven constructors have signatures.**
   `Unauthenticated(reason string)`, `PermissionDenied(action string)`,
   `Unavailable(dependency string, err error)` — it names *what* was
   unreachable — and `Internal(err error)`. warren.md §2.6 was amended to list
   all seven; the full surface is in Public API above.
3. **RESOLVED (2026-08-01) — the type is `errors.Error`.** warren.md §2.6
   defines it in `warren/errors`, and that won; the stray `warren.Error`
   references in §1.4, §2.7, §4.2, and the §9 ledger were corrected.
4. **RESOLVED (2026-08-01) — fields unexported, read through accessors.**
   `code Code`, `msg string`, `cause error`, `details map[string]any`, reached
   via `Code()`, `Message()`, `Details()` (which returns a copy), and
   `Unwrap()`. Exported fields would let an adapter mutate a shared error;
   accessors keep the type's invariants where they belong.
5. **RESOLVED (2026-08-01) — `Error()` renders `CODE: message`,** appending
   `: cause` when one is wrapped. The code in front means a bare log line still
   says what kind of failure it was; details stay out of the text because they
   are structured payload for response bodies, not prose.
6. **RESOLVED (2026-08-01) — yes, `Unwrap() error` returns the cause.**
   Standard-library `errors.Is`/`errors.As` see through a Warren error, which
   the `%w` mandate in AGENT.md § Errors already assumed.
7. **RESOLVED (2026-08-01) — import alias, no rename.** Generated code that
   touches driver sentinels imports this package as
   `werrors "github.com/MerseniBilel/warren/errors"`, keeping the bare
   `errors` identifier for the standard library. `Is(err, code)` keeps its
   name and signature; warren.md §6.1 was amended to use `werrors.NotFound`.
8. **Where does the cross-adapter conformance test live?** "Every adapter maps
   every code" is the property that keeps the table honest, but the core module
   cannot import the adapters and the adapters never import each other
   (invariant 4). `warren/testing` is the natural home — is it? **Deferred:**
   decide in `testing/SPEC.md` before the first transport adapter is built;
   nothing in this package blocks on it.
9. **RESOLVED (2026-08-01) — `Code` stays an open string type; adapters
   default unknown codes to `INTERNAL`.** Closing the set by construction
   would cost an opaque type for no safety an adapter can rely on anyway (a
   corrupted wire value is still possible). Instead the rule is on the reading
   side, stated under the §2.6 table: a code the table does not list maps to
   500 / `Internal` / nack + retry, then DLQ — the safe default for the
   unknown. Every adapter's suite must assert its default branch does this.
