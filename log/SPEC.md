# Spec — `warren/log`

**Status:** approved 2026-07-29, implemented
**Roadmap:** v0.1 · item 2 of 11
**Import path:** `github.com/MerseniBilel/warren/log`
**Depends on:** `log/slog`, `context` · and `warren/errors`, for `Err`

---

## 1. Problem

A handler is transport-agnostic, so it cannot be handed a logger by the HTTP
layer without importing it. It has exactly one thing that crosses every layer:
`context.Context`. So the logger travels there.

Two failures this package exists to prevent:

- **A logger passed as a constructor argument to everything.** It reaches every
  struct in the codebase, and then a request-scoped field like `request_id`
  either does not exist or is threaded by hand through fifteen call sites.
- **`slog.Default()` at the call site.** It works, and it silently loses every
  request-scoped attribute, because the default logger knows nothing about the
  request in flight. The bug does not show up until someone tries to trace a
  failure in production.

`docs/architecture.md §3` states the constraint on the solution:

> `log/slog` is the standard. Our layer is context plumbing and nothing else —
> no logging framework, ever.

So this package writes no `Logger` type, no `Handler`, no levels, no formatting,
and no output. `log/slog` already has all of those and is in the standard
library. What it does not have is a convention for carrying a logger on a
context, and a way to turn a `warren/errors` error into structured attributes.
That is the whole scope.

## 2. Goals

1. A logger reaches any layer through `context.Context` alone.
2. Request-scoped attributes are attached once, at the edge, and appear on every
   record logged downstream.
3. A `warren/errors` error logs as structured data — its code, operation chain,
   fields, and fix — not as a flattened string a human has to re-parse.
4. A test can capture output without touching global state.
5. Standard library only, and **no type of our own in the public signature** —
   callers hold a `*slog.Logger` and use its methods directly.

## 3. Non-goals

- **A `Logger` type, interface, or wrapper.** The moment one exists, it grows
  methods, and the "thin layer" is a logging framework. `From` returns
  `*slog.Logger` and the caller uses `slog`'s own API.
- **Convenience level functions** — `log.Info(ctx, …)`, `log.Error(ctx, …)`.
  They read well and they are a second way to do what `From(ctx).InfoContext`
  already does, which makes call sites inconsistent across a codebase. See §11.1.
- **Choosing a handler, a format, or a destination.** JSON to stderr is a
  decision for `warren new`'s generated `main.go`, where a user can read it and
  change it. A framework that picks the log format is a framework people fight.
- **Sampling, rate limiting, or redaction.** Handler concerns. A user who wants
  them wraps their own `slog.Handler`, which is the extension point `slog`
  already provides.
- **Calling `slog.SetDefault`.** Warren does not mutate a process-wide global on
  a user's behalf.

## 4. Public API

```go
// Package log carries a *slog.Logger on a context, and turns a warren/errors
// error into structured attributes. It defines no logger type of its own.
package log

// Into returns a copy of ctx carrying logger.
//
// The transport adapter calls this once per request, after attaching the
// attributes that identify the request. Passing a nil logger returns ctx
// unchanged, so a caller need not branch.
func Into(ctx context.Context, logger *slog.Logger) context.Context

// From returns the logger carried by ctx.
//
// It returns slog.Default() when ctx carries none, and when ctx is nil, so a
// call site never has to check and never panics. That fallback is the standard
// library's global, not one of Warren's: this package holds no state.
func From(ctx context.Context) *slog.Logger

// With returns a copy of ctx carrying From(ctx) with args attached, in
// slog's alternating key/value form.
//
//	ctx = log.With(ctx, "request_id", id, "tenant", tenant)
//
// Every record logged through From(ctx) downstream carries them.
func With(ctx context.Context, args ...any) context.Context

// WithAttrs is With for callers that already hold typed attributes, and avoids
// the alternating-argument form's runtime cost and its failure mode.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context

// Err returns a grouped attribute describing err: its message, semantic code,
// operation chain, fields, and suggested fix.
//
//	log.From(ctx).ErrorContext(ctx, "cannot create order", log.Err(err))
//
// Err(nil) returns the empty slog.Attr, which slog discards, so a caller need
// not branch on whether it has an error.
func Err(err error) slog.Attr
```

That is the entire surface: four functions for the context, one for errors.

## 5. Behaviour

### 5.1 The context key

An unexported zero-size struct type, unexported value. Nothing else can collide
with it and nothing outside the package can read the logger out except through
`From`.

### 5.2 `With` derives, it does not accumulate separately

`With(ctx, args…)` is defined as `Into(ctx, From(ctx).With(args…))`. There is no
second store of attributes and no custom `slog.Handler`: `slog.Logger.With`
already does exactly this, and reusing it means Warren's attributes and the
user's behave identically because they *are* the same mechanism.

The consequence, stated because it is the design's one real limitation: a call
to `slog.InfoContext(ctx, …)` on the **default** logger does not pick up
context attributes, because it never consults the context's logger. §11.2 is
the decision to accept that for v0.1.

### 5.3 `Err`'s shape

`Err` groups under the key `error`:

| Attribute | Source | Present when |
|---|---|---|
| `message` | `err.Error()` | always |
| `code` | `errors.CodeOf(err).String()` | always |
| `ops` | `errors.Ops(err)` | the chain is non-empty |
| `fix` | `errors.Fix(err)` | one was recorded |
| *one per field* | `errors.Fields(err)` | any exist |

Fields are emitted as siblings inside the group, under their own keys, so
`error.order_id` is queryable rather than buried in a rendered string. A field
whose key collides with `message`, `code`, `ops`, or `fix` is emitted anyway:
`slog` permits duplicate keys, and silently dropping a caller's context is
worse than a duplicate.

A non-Warren error yields `message` and `code=Internal`, which is what
`errors.CodeOf` reports for it. `Err` never inspects an error's text.

### 5.4 Cost

`From` and `Into` are the hot path — a request that logs nothing still pays for
them if the adapter attaches a logger.

The budget is measured against `slog.DiscardHandler`, whose `WithAttrs` returns
itself, so what is asserted is **this package's own cost**:

| Operation | Allocations |
|---|---|
| `From(ctx)`, with a logger or without | 0 |
| `Into(ctx, logger)` | 1 — `context.WithValue`'s node |
| `With` / `WithAttrs` | 3 — the node, `slog`'s clone, its attr slice |
| `Err(err)` on an error with no fields | 5 |
| `Err(err)` with fields | 6 |

A real handler adds its own cost on top, and that is the handler's trade rather
than this package's: `slog.JSONHandler` preformats attributes, which takes
`With` from 3 allocations to 8 and makes every subsequent record cheaper. It is
paid once per request, at the edge. Benchmarks use a `JSONHandler` and report
that realistic figure; the budget test isolates ours.

`Err` builds `[]slog.Attr` and closes it with `slog.GroupValue` rather than
calling `slog.Group`, whose variadic `...any` boxes every attribute
individually — that is the difference between 5 allocations and 11.

## 6. Every error message this package emits

None. `log` returns no errors: a logging call that fails is `slog`'s problem,
and this package has no failure of its own to report. Four edge cases resolve
silently and predictably instead:

| Situation | Behaviour |
|---|---|
| `From(ctx)` with no logger | `slog.Default()` |
| `From(nil)` | `slog.Default()`, no panic |
| `Into(ctx, nil)` | returns ctx unchanged |
| `Err(nil)` | the empty `slog.Attr`, which `slog` discards |

## 7. Interoperability

- The value in the context is a `*slog.Logger`, so anything that accepts one
  works: `slog.NewJSONHandler`, `slog.NewTextHandler`, `otelslog`, a user's own
  handler.
- `From(ctx).Handler()` reaches the underlying handler, which is the escape
  hatch for code that needs it.
- A test injects `slog.New(slog.NewJSONHandler(&buf, nil))` with `Into` and
  reads `buf`. No global is touched, so tests stay parallel-safe — which is why
  `slog.SetDefault` is a non-goal rather than an oversight.
- `slog.DiscardHandler` (Go 1.24+) covers "log nothing in this test", so this
  package ships no discard helper of its own.

## 8. Enforcement

- `depguard` already bans `sirupsen/logrus` repository-wide, with the reason
  pointing here. `zap`, `zerolog`, and `log` (the old standard-library logger)
  are added to the same block by this change, so "no logging library" is a build
  failure rather than a convention.
- `sloglint` is already enabled by `default: all`. It is configured here to
  require the context-aware call form — `InfoContext` over `Info` — since a
  record logged without the context loses the very attributes this package
  exists to carry.

## 9. Testing

Unit tests only — no Docker, no network, no sleeps.

- Round trip: `From(Into(ctx, l)) == l`.
- Fallback: `From(context.Background())` and `From(nil)` both return
  `slog.Default()`, and neither panics.
- `Into(ctx, nil)` returns the same context.
- `With` accumulates across two calls, and does not mutate the parent context —
  a logger read from the parent afterwards still lacks the child's attributes.
- **Golden test on JSON output**, with `ReplaceAttr` dropping `time` so the
  record is deterministic. Cases: a plain message; a message with context
  attributes; `Err` on a full Warren error; `Err` on a foreign error; `Err(nil)`
  emitting nothing.
- `Err` field-to-attribute mapping, including the duplicate-key case.
- Benchmarks with `-benchmem` for `From`, `Into`, `With`, and `Err`, and an
  allocation test against §5.4 under `//go:build !race`, for the reason given in
  `errors/SPEC.md` §9.
- `Example` functions for `Into`, `From`, `With`, and `Err`.

## 10. Definition of done

- [x] `log/` implements §4 exactly, standard library plus `warren/errors`
- [x] Every exported identifier has a doc comment starting with its name
- [x] Golden test on JSON output, committed with its `testdata/` — five cases,
      including a foreign error and a colliding field key
- [x] Benchmarks committed, §5.4 budget met
- [x] `depguard` (zap, zerolog, the old `log`) and `sloglint` (`context: all`)
      settings added to `.golangci.yml`
- [x] `Example` functions for all five exported functions, plus `Err(nil)`
- [x] `docs/roadmap.md` v0.1 item 2 ticked
- [x] This spec corrected wherever the code diverged — §5.4 rewritten against
      measured figures
- [x] `make ci` green on macOS (exit 0, golangci-lint v2.12.2, 0 issues).
      Linux and Windows are CI's to confirm

## 11. Decisions taken

Agreed 2026-07-29.

1. **No `log.Info(ctx, …)` convenience functions.** `From(ctx).InfoContext(ctx,
   …)` is more to type and names the context twice. The argument for omitting
   them is that two ways to log means a codebase uses both, and the second one
   cannot express `WarnContext` with a group without growing more API. The
   argument against is purely ergonomic, and it is a real cost paid at every
   call site.

2. **`With` derives a logger rather than storing attributes for a custom
   handler.** The alternative — Warren ships a `slog.Handler` decorator that
   reads attributes from the context — makes plain `slog.InfoContext(ctx, …)`
   pick up request attributes too, and is what OpenTelemetry integration in v0.4
   will likely want for trace and span IDs. It costs a handler we own and a
   second mechanism. My recommendation is to defer it to v0.4 and revisit it
   there with the OTel work, rather than build it now against a guess.

3. **`Err` lives in `log`, not as `LogValue` on `*errors.Error`.** Implementing
   `slog.LogValuer` in `errors` would make `slog.Any("error", err)` render
   structurally with no import of this package at all — which is genuinely
   nicer at the call site. It also makes `errors` carry a presentation concern
   and a dependency on `log/slog`. Both are standard library, so no invariant
   forbids it; this is a taste and coupling judgement, and it is reversible in
   either direction later.

4. **The `error` group key is fixed, not configurable.** A configurable key is
   one setting nobody changes and every log query has to account for.

5. **`WithAttrs` alongside `With`.** Two entry points, but the alternating
   `...any` form is `slog`'s documented footgun — an odd argument count degrades
   at runtime rather than failing to compile. Warren's own code would use
   `WithAttrs` throughout.
