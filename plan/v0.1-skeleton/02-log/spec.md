# Spec: Logging

| | |
|---|---|
| **Module** | `warren/log` (core — standard library only) |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [01-errors](../01-errors/spec.md) |
| **Blocks** | [04-lifecycle](../04-lifecycle/spec.md), [08-transport-http](../08-transport-http/spec.md) |
| **PRD** | §6.1, §6.6 |
| **ADRs** | None |
| **Date** | 2026-07-28 |

---

## 1. Problem

A framework needs to log — startup, shutdown, request completion, consumer
errors — without imposing a logging library on the user. PRD §6.6 settles the
library question with one word: `log/slog`, *"stdlib; no opinion imposed on
users."*

What remains is the part `slog` does not solve: getting a request-scoped logger
from an HTTP middleware down to a use case that must not import `net/http`, and
having a correlation ID travel with it.

## 2. Goals

1. **Carry a `*slog.Logger` through `context.Context`**, with a correlation ID,
   so a handler logs with request context without knowing what a request is.
2. **Never require the user to adopt a Warren logger type.** They configure
   `slog` and hand it over.
3. **Attach Warren error fields automatically**, so
   [`errors.Field`](../01-errors/spec.md) data reaches the log without every
   call site restating it.
4. A sensible default when nobody configures anything, because a framework that
   is silent on boot looks broken.

## 3. Non-goals

- **No `log.Logger` wrapper type.** Wrapping `*slog.Logger` in a Warren type
  would be exactly the imposed opinion PRD §6.6 rejects, and it would break
  every `slog` handler in the ecosystem. The API traffics in `*slog.Logger`.
- No log shipping, sampling, or rotation. That is the deployment's job.
- No trace correlation yet — OTel is v0.4
  ([v0.4 plan](../../v0.4-governance/README.md)). The correlation-ID field is
  designed so the OTel trace ID can populate it without an API change.

## 4. Public API

```go
package log

// FromContext returns the logger stored in ctx, or slog.Default() if none is.
// It never returns nil: a logging call must not be the thing that panics.
func FromContext(ctx context.Context) *slog.Logger

// NewContext returns a copy of ctx carrying l.
func NewContext(ctx context.Context, l *slog.Logger) context.Context

// Attrs returns a copy of ctx whose logger carries the given attributes, so a
// transport adapter can add request-scoped fields once.
func Attrs(ctx context.Context, args ...any) context.Context

// CorrelationID returns the correlation ID carried by ctx, or "" if none is.
func CorrelationID(ctx context.Context) string

// NewCorrelationContext returns a copy of ctx carrying id, and a logger that
// includes it as an attribute.
func NewCorrelationContext(ctx context.Context, id string) context.Context

// ErrorAttrs renders a Warren error as slog attributes: its code, its fields,
// and its operation. Non-Warren errors yield a single "error" attribute.
func ErrorAttrs(err error) []slog.Attr
```

That is the whole surface. Everything else is `slog`.

## 5. Behaviour

- **`FromContext` never returns nil.** A missing logger yields `slog.Default()`.
  Nil-checking a logger at every call site is how logging becomes the thing that
  crashes a service.
- **The context key is unexported**, so nothing outside the package can collide
  with or forge it. `revive`'s `context-keys-type` rule is enabled for this.
- **`Attrs` is copy-on-write.** The parent context's logger is unchanged, so a
  per-request field cannot leak into a sibling request.
- **Correlation IDs are not generated here.** The HTTP adapter reads an inbound
  header or generates one; this package only carries it. Generation is a
  transport policy — it needs to know which header the caller uses.
- **The framework's own logs use one namespace**: every framework attribute is
  prefixed `warren.`, so a user can filter framework noise out of their own logs
  with one predicate.
- **Default level is `Info`.** Startup, shutdown, and each bound listener log at
  `Info`; per-request logging is the adapter's decision and defaults to off,
  because a framework that logs every request by default is a framework that
  doubles someone's log bill without asking.

## 6. Errors

| Condition | Code | Message |
|---|---|---|
| `NewContext` given a nil logger | — | Documented as a no-op returning ctx unchanged. It does not panic; a logging call is never the failure. |

The package returns no errors. `ErrorAttrs(nil)` returns nil.

## 7. Configuration

Configured through `slog` itself, by the user, before handing the logger to
`warren.New`. Warren adds no keys of its own at v0.1. The default when nothing
is supplied is a `slog.TextHandler` on stderr at `Info` — text rather than JSON,
because the default case is a developer watching a terminal.

## 8. Testing

Unit only.

- **`FromContext` on a background context** returns `slog.Default()`, not nil.
- **Isolation**: `Attrs` on a child context does not affect the parent's output,
  asserted by capturing both through a recording handler.
- **Concurrency**: derive 100 contexts from one parent in parallel under `-race`
  and assert no attribute crosses between them.
- **`ErrorAttrs`** for a Warren error with fields, a wrapped chain, and a plain
  `errors.New` — all three shapes.
- **Correlation ID survives** a chain of `Attrs` calls.
- **Golden test on the framework's boot output**, because "what a service prints
  when it starts" is the first thing every user sees, and it should not change
  by accident.

## 9. Invariants touched

- **Invariant 1** — `log/slog` is standard library; core placement is correct.
- **Invariant 4** — this package is how a handler gets a request-scoped logger
  without importing `net/http`.

## 10. Definition of done

- [ ] Public API matches §4
- [ ] Unit tests per §8, `-race -shuffle=on`
- [ ] `make ci` green
- [ ] Doc comment on every exported identifier
- [ ] `docs/` concept page: logging and correlation
- [ ] Runnable example in `examples/logging/`
- [ ] Changelog fragment

## 11. Open questions

1. **Does the correlation ID belong in `warren/log` or in a
   `warren/context` package?** Request ID, tenant, and user identity are the same
   shape of problem and will all want to travel the same way. Deciding this at
   v0.1 with one field risks a package split at v0.3; deciding it now with three
   speculative fields risks designing for needs nobody has confirmed. Revisit
   when auth (v0.5) or OTel (v0.4) adds the second field.
2. **Should the default handler be JSON when stderr is not a TTY?** Convenient,
   and it is behaviour that changes based on something invisible. Leaning no —
   an explicit flag in `warren new`'s generated `main.go` instead, where the user
   can see it.
