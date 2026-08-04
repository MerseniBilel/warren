# `github.com/MerseniBilel/warren/app` — identity seam — SPEC

| | |
|---|---|
| **Status** | **Approved 2026-08-05** — architect ruling, v0.1 / Go 1.26 shape. Scoped to the identity seam ONLY; the rest of `app` shipped 2026-08-01 and is described by warren.md §3.2. |
| **Source** | [warren.md §3.2](../warren.md), [§7.2](../warren.md), [auth/SPEC.md](../auth/SPEC.md) |
| **Module** | core (`github.com/MerseniBilel/warren`) — stdlib only |
| **Mode** | Build |

## Problem

`app.AuthorizationPolicy` shipped on 2026-08-01. `transport.Guard(policy)` is a
`RouteOption` carried on every route and evaluated **before** decode.
`app.Authorized` composes the policy into the core middleware chain.

And **there is no identity type anywhere in core** — `grep` returns zero hits.
So:

- The only thing a policy can meaningfully read off its `ctx` is the caller,
  and that has no declared type. A port whose sole input is undeclared is an
  incomplete port.
- **Nothing in the repository can produce `CodeUnauthenticated` or
  `CodePermissionDenied`.** Two rows of warren.md §2.6 — the load-bearing
  error table — are untested by construction. `transport/http/render.go` maps
  them to 401 and 403 for values no shipped code creates.
- Every service invents its own context key, so nothing composes: a policy
  written for one service cannot be read by another, and neither can be read
  by `warren/auth` when it lands.

Two independent field tests named this as the gap that stops them shipping.

## Decision

Ship the **seam** in v0.1 and keep the **verifier** in v0.2.

The type is not optional — every service that ships on v0.1 will define one.
The choice is between one shape Warren owns and can evolve additively, and N
shapes users own, each of which must be rewritten when `warren/auth` arrives.
`auth/SPEC.md` itself says core gets more expensive to change after v0.1 tags;
that is an argument for landing the type now, not for landing nothing.

Kratos is the control group: no core identity type, and the observable result
is `claims.(jwt.MapClaims)["authorityId"].(string)` in application code.

## Public API

All in package `app`, file `app/identity.go`. No new package, no new module,
no dependency — one new stdlib import (`log/slog`).

```go
type Identity struct {
    Subject string         // the principal
    Issuer  string         // who vouched for it, or ""
    Scopes  []string       // the OAuth2 "scope" claim, already split
    Claims  map[string]any // everything else. May be nil. Read-only once seeded.
}

func (id Identity) HasScope(scope string) bool
func (id Identity) LogValue() slog.Value   // redacts Claims

func WithIdentity(ctx context.Context, id Identity) context.Context
func IdentityFromContext(ctx context.Context) (Identity, bool)

func RequireAuthenticated() AuthorizationPolicy
func RequireScope(scopes ...string) AuthorizationPolicy

func Claim[T any](id Identity, name string) (T, bool)
```

### Why a struct, not an interface

Identity is **data**; `Telemetry`, `RetryPolicy` and `AuthorizationPolicy` are
**behaviour**. `app.RequestInfo` is the shipped precedent for data. Beyond
that:

- An interface reintroduces the typed-nil trap `WithTelemetry` already defends
  against with a `reflect` probe — a non-nil interface holding a nil pointer
  would read as identity **present**.
- A test writes `app.Identity{Subject: "u-1"}`. An interface would put a
  hand-written fake in every user's test package, forever, to say that.
- Measured: the shape does not change the allocation count. Struct, pointer,
  wrapper and bare string all cost 2 on the context path. There is no
  performance argument either way.

### Why not `Identity[T]`

Each instantiation gets its own context key, so a guard seeding `Identity[A]`
and a handler reading `Identity[B]` see **no identity at all** — absence
looking like presence, invisible at boot. `AuthorizationPolicy.Authorize(ctx)
error` is non-generic and already shipped, so a generic identity could only be
read by type-switching at request time (invariant 7). `Claims` plus the
generic free function `Claim[T]` serves app-defined claims instead.

### Why these four fields

`Subject` and `Issuer` are the only claims every credential format agrees on.
`Scopes` is the one claim OAuth2 and OIDC spell identically, and it is what
lets `RequireScope` live in core without core parsing a token. `Claims` is the
pressure valve that makes additive evolution possible.

Deliberately absent: `Audience`, `ExpiresAt`, `NotBefore`, `Roles`, `Tenant`.
Expiry and audience are the **verifier's** business — an identity on the
context is by definition already verified, and a field for expiry invites
handlers to re-check it badly. They live in `Claims` until v0.2 proves one
deserves promotion, and promotion is additive.

## Behaviour

**Absence must never look like presence.** Three mechanisms, stacked:

1. **The accessor returns an ok-bool**, so
   `app.IdentityFromContext(ctx).Subject` **does not compile**. That is
   `go build`, not a convention a reviewer has to catch. This is a deliberate
   asymmetry with `log.CorrelationID(ctx) string`: an absent correlation ID is
   cosmetic, an absent identity is a security decision.
   `transport.Params.Path` already returns an ok-bool for the same reason.
2. **`WithIdentity` refuses an `Identity` whose `Subject` is empty** and
   returns the context unchanged — the same refusal `WithTelemetry` makes for
   a nil Telemetry. A guard bug that produces an empty subject makes the
   request read as unauthenticated, which is fail-closed.
3. **`(Identity{}, false)` is the only absent form.** No nil, no typed-nil, no
   sentinel.

**`RequireScope` never merges the two codes.** No identity is
`CodeUnauthenticated` (401); an identity lacking the scope is
`CodePermissionDenied` (403). They answer different questions.

**`LogValue` redacts `Claims`.** Without it, one
`log.FromContext(ctx).InfoContext(ctx, "handled", "identity", id)` dumps a
claims map — emails, phone numbers, sometimes a nested token — into every log
line. `slog.LogValuer` is the stdlib's redaction seam.

**`Claims` is read-only once seeded**, and documented as such. It is carried
by reference, not copied: copying per request would cost an allocation
proportional to the token, and the map is the verifier's output.

## Performance

Measured, go1.26.3/darwin-arm64. The budget in
`transport/http/alloc_internal_test.go` is **18** and the current cost is
**17** — one allocation of headroom, which is less than this seam costs.

| Path | Allocations |
|---|---|
| Unguarded route (the committed budget test) | **17**, unchanged |
| `IdentityFromContext`, absent | **0** |
| `IdentityFromContext`, present | **0** |
| `WithIdentity` | **2** (112 B) |

The 2 are charged to the **application's** edge middleware and only on
requests that carry a credential. **The framework installs no identity
middleware**, which is what protects the budget — with one allocation of
headroom, a default identity step would blow the committed ceiling on the
first request.

A benchmark for this must use **runtime-valued** strings: with compile-time
constants the box becomes a static symbol and measures a fake 1 allocation.

## Testing

1. Absence on a bare context, and on a five-deep context carrying a logger, a
   correlation ID, params and a stamp — the shape of a real request.
2. Empty subject is absence: `WithIdentity` returns the context **unchanged**
   (`got == ctx`) and reads back `(Identity{}, false)`.
3. Round trip: every field equal; `Claims` is the same map; `HasScope` true,
   false, and false on a nil slice.
4. The committed allocation numbers, with runtime-valued strings.
5. `transport/http`'s `TestAllocations` still prints 17 — the regression test
   for "the framework installs no identity middleware".
6. The two codes are never merged: bare context → `CodeUnauthenticated`;
   identity holding `["b"]` asked for `"a"` → `CodePermissionDenied`;
   `["a","b"]` → nil.
7. Golden files for both policies' messages.
8. **Transport parity**, which §7.2 claims and nothing could test before: the
   same handler behind the same policy is 401/403 with the standard envelope
   over HTTP, and dead-lettered **without retry** through `broker/memory`.
9. `Guard` denies before decode end to end: a syntactically broken body on a
   guarded route with no identity is **401, not 400**. `transport.Guard`'s doc
   comment claims this today with no policy able to prove it.
10. `LogValue` never leaks: `slog.Any("identity", id)` with a claims map
    containing secrets renders subject, issuer and a scope **count**, and the
    record contains neither value. Golden file.
11. Race: two concurrent requests with different identities through the same
    pre-built route closure observe only their own.
12. `Claim[T]` returns `(zero, false)` for a missing key, a wrong type, and a
    nil map. Never panics.

## What this does NOT ship

JWT signature verification, issuer/audience/algorithm/clock-skew config, OIDC
discovery, JWKS fetch and rotation, the `golang-jwt`/`go-oidc` wrapping and
their outstanding audits, per-transport guard registration for gRPC and
consumers, and the identity-over-the-broker header convention. All v0.2, all
in `warren/auth`, which stays DEFERRED.

Adopting `warren/auth` in v0.2 changes no policy, no handler and no test
written against v0.1 — that is the point of shipping the seam first.

**Event routes carry no identity in v0.1.** There is no header convention and
no propagating decorator (`auth/SPEC.md` OQ7), so `app.Authorized` composed
into a consumer chain denies every message and dead-letters it — §2.6 sends
`UNAUTHENTICATED` to the DLQ without retry. That is fail-closed and correct,
and it is a 3 a.m. incident if it is not written down. It is documented on
`Authorized` and in §7.2.

## Open questions

None. This spec exists to close `auth/SPEC.md` open questions 1, 2 and 3;
4, 5, 7 and 8 remain there and are what keeps that module deferred.

## Definition of done

- [ ] `app/identity.go` with the surface above, no new dependency.
- [ ] All twelve tests, including the golden files and the parity test.
- [ ] `transport/http` budget unmoved at 17/18.
- [ ] warren.md §3.2, §7.2, §1.6, §2.6 and §9 amended.
- [ ] `auth/SPEC.md` amended: OQ1–3 resolved, "Why it is deferred" rewritten.
- [ ] README and GETTING_STARTED updated.
- [ ] `make ci` green; this spec retired into warren.md §3.2.
