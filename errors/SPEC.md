# `github.com/MerseniBilel/warren/errors` — SPEC

| | |
|---|---|
| **Status** | Draft — **table complete (2026-08-01)**, not yet approved: constructor signatures, `Error`'s fields, `Unwrap`, and the `Is` collision (Open questions 2–7) must be decided before this package can be implemented. |
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

Taken from warren.md §2.6, with doc comments added. **The `// ...` in warren.md
elides four constructors** — see Open questions 2.

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

// Error is Warren's semantic error. It carries a Code, a message, and any
// details attached with WithDetail.
//
// The field layout is not fixed by warren.md — see Open questions 4.
type Error struct{ /* ... */ }

// Invalid reports that field failed validation or conversion, wrapping err as
// the reason. The resulting Error carries CodeInvalid.
func Invalid(field string, err error) *Error

// NotFound reports that no resource of the named kind exists with this id. The
// resulting Error carries CodeNotFound.
func NotFound(resource string, id any) *Error

// Conflict reports that the request collided with current state. The resulting
// Error carries CodeConflict. Whether args are printf operands or slog-style
// key/value pairs is not stated by warren.md — both §2.6 call sites pass none.
// See Open question 2.
func Conflict(msg string, args ...any) *Error

// ... Unauthenticated, PermissionDenied, Unavailable, and Internal complete the
// set (AGENT.md § Errors names all seven). warren.md elides their signatures
// behind "// ..." — see Open questions 2.

// WithDetail returns e with the key/value pair attached, so adapters can put
// structured context in a response body. It is a method, not a type name, and
// so is permitted by AGENT.md § Naming.
func (e *Error) WithDetail(k string, v any) *Error

// Is reports whether err, or any error it wraps, carries code. It is how
// callers ask about meaning without a type assertion.
func Is(err error, code Code) bool
```

`*Error` must satisfy the `error` interface — warren.md §10 returns
`errors.Invalid("email", err)` directly as a function's `error` result — so
`func (e *Error) Error() string` exists as a consequence. Its text is not fixed
by warren.md; see Errors and Open questions 5.

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
         └──▶ warren.Error
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

**warren.md fixes no message text for any constructor.** It fixes the *call
sites* — `Conflict("user already active")`, `Conflict("user already exists")` —
but those strings are supplied by the caller, not by this package. For the two
constructors that compose a message from parts, the text is open:

| Constructor | Message text |
|---|---|
| `Invalid(field, err)` | **Open.** warren.md fixes neither the format nor whether `err`'s text is included. |
| `NotFound(resource, id)` | **Open.** warren.md fixes neither the format nor how `id any` is rendered. |
| `Conflict(msg, args...)` | Caller-supplied format string; this package adds nothing that warren.md states. |
| `Unauthenticated`, `PermissionDenied`, `Unavailable`, `Internal` | **Open** — signatures themselves are elided (Open questions 2). |
| `(*Error).Error() string` | **Open.** warren.md does not state whether the rendered text includes the code, the details, or the wrapped error. |

AGENT.md § Errors requires that **error messages tell the user how to fix the
problem**: "provider not found" is called out there as a bug in the error
message. Whatever text is agreed must meet that bar — for `NotFound`, naming
the resource kind and the id it looked for; for `Invalid`, naming the field and
the constraint it failed. The exact wording needs the human's decision before
implementation, because every one of these strings then gets a golden file and
becomes part of the contract.

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

- [ ] `Code`, the seven constants, `Error`, `Is`, `WithDetail`, and all seven
      constructors exist, each with a doc comment starting with the
      identifier's name.
- [ ] `*Error` satisfies `error` (fixed by §10's use of it as a return value).
- [ ] Unwrap behaviour is decided in `warren.md` and then implemented — see
      Open question 6; warren.md does not currently state it.
- [ ] Package compiles under the core module importing the standard library
      only — no `net/http`, no `google.golang.org/grpc`, no driver of any kind.
- [ ] Every message text agreed by the human is recorded in the Errors section
      above and covered by a golden file.
- [ ] Allocation benchmarks exist for the constructors and `WithDetail`.
- [ ] `Is` tests cover direct, `%w`-wrapped, and non-Warren errors.
- [x] The two missing table rows (Open questions 1) are resolved and warren.md
      §2.6 is amended in the same change — done 2026-08-01: both documents now
      carry the seven-row table and the two normative paragraphs, and §1.4's
      diagram was corrected to `Conflict → 409 / AlreadyExists / ack`.
- [ ] `make ci` passes (once the Makefile exists).

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
2. **Four constructors are elided behind `// ...`.** warren.md gives signatures
   for `Invalid`, `NotFound`, and `Conflict` only. AGENT.md § Errors names all
   seven semantic errors, so `Unauthenticated`, `PermissionDenied`,
   `Unavailable`, and `Internal` exist — but with what signatures? `Internal`
   plausibly wraps a cause (`Internal(err error)`), `Unavailable` plausibly
   names the dependency, and `PermissionDenied` plausibly names the action. Not
   guessing.
3. **What is the name of this type — `errors.Error` or `warren.Error`?**
   warren.md §2.6 defines `*Error` in package `warren/errors`, so the qualified
   name is `errors.Error`. But §2.7, §4.2, and the §9 ledger all say
   `warren.Error`, which would put the type in the root package. Three
   references to one name and one to the other. Which package owns the type? If
   it is `warren/errors`, the other three references need correcting.
4. **What are `Error`'s fields, and are any exported?** warren.md never shows
   the struct body. An adapter needs to read the code and the details to build a
   response, so *something* must be reachable — accessor methods, exported
   fields, or a single `Detail()` map. Not inventing this.
5. **What does `(*Error).Error()` render?** Message only, or `CODE: message`, or
   message plus details? This is the string that lands in logs, so it is a
   product decision.
6. **Does `*Error` implement `Unwrap() error`?** `Invalid(field, err)` takes a
   cause, and AGENT.md mandates `%w` wrapping, so unwrapping is strongly
   implied — but warren.md does not state it, and it decides whether
   `stdlib errors.Is`/`errors.As` see through a Warren error.
7. **`Is` collides with `errors.Is` from the standard library.** warren.md §6.1
   shows one generated function calling both `errors.Is(err, pgx.ErrNoRows)` and
   `errors.NotFound("user", id)` under the same `errors` identifier, which
   cannot compile: this package's `Is(err error, code Code) bool` does not
   accept a sentinel error. Either the generated repository imports the two
   packages under distinct names, or `Is` is renamed, or it takes `any` and
   dispatches. warren.md §6.1 needs correcting whichever way this goes.
8. **Where does the cross-adapter conformance test live?** "Every adapter maps
   every code" is the property that keeps the table honest, but the core module
   cannot import the adapters and the adapters never import each other
   (invariant 4). `warren/testing` is the natural home — is it?
9. **Is `Code` closed by construction?** `type Code string` lets a user write
   `Code("WHATEVER")`, which no adapter maps. Is that acceptable, or should the
   adapters treat an unknown code as `INTERNAL`? warren.md is silent, and it
   changes what every adapter's default branch does.
