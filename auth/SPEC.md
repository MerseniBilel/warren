# `github.com/MerseniBilel/warren/auth` — SPEC

| | |
|---|---|
| **Status** | **DEFERRED to v0.2 (decided 2026-08-02).** **Amended 2026-08-05:** open questions 1, 2 and 3 are RESOLVED by the v0.1 identity ruling — the identity type is `app.Identity`, the option type is the existing `transport.RouteOption` via `transport.Guard`, and `policy` is the existing `app.AuthorizationPolicy`, of which `app.RequireScope` is the core-resident proof. This module's remaining scope is **verification only**. Open questions 4, 5, 7 and 8 remain, and they are what keeps it deferred. |
| **Source** | [warren.md §7.2](../warren.md) |
| **Module** | own module (`github.com/MerseniBilel/warren/auth`) |
| **Mode** | Wrap (warren.md §9) |
| **Wraps** | `golang-jwt/jwt/v5`, `coreos/go-oidc` |


## Why it is deferred

The authorization half **already ships and is usable**: `app.AuthorizationPolicy`
is a real port, `transport.Guard(policy)` is a real `RouteOption` carried on
every route, and `app.Authorized` composes it. A user writes a twenty-line
policy today and it works over HTTP and consumers alike.

**The identity decision WAS the blocker, and it was taken in v0.1.**
`app.Identity` and its context seam ship in core — warren.md §3.2 and
`app/identity.go`'s doc comments carry the ruling now that `app/SPEC.md` has
retired — so users write policies, handlers and tests today that do not
change when this module lands. That was the right order: the seam is
expressible without a token library, and the verifier is not expressible
without the seam.

What remains is the half that needs the libraries, and it is genuinely not
ready: the audits AGENT.md requires for `golang-jwt/jwt/v5` and
`coreos/go-oidc` have not been run — no observation date, archived check,
release date, licence or transitive footprint is recorded for either — and
four structural questions are open (OQ4 verification configuration, OQ5 what
`go-oidc` is actually for, OQ7 consumer identity propagation, OQ8 who
registers the guard for gRPC and consumers).

Promoting this spec to APPROVED would approve a dependency decision on the
strength of nothing. It stays deferred.

## Problem

Authentication arrives in a transport-shaped envelope — an HTTP `Authorization`
header, a gRPC metadata entry, a message header — and authorization is a
property of the **use case**, not of the protocol that reached it. A framework
that puts both in the same place ends up with authorization that works over HTTP
and silently does nothing for gRPC calls and consumers.

warren.md §7.2 splits them along the two-ring model of §1.4, and that split is
the point of this package.

## Goals

- **Guards run as edge middleware** (§7.2) — the transport-shaped half: pull the
  credential out of whatever envelope it arrived in, validate it.
- **Identity lands on the context** (§7.2) — the seam between the rings. After
  the edge ring runs, who the caller is travels on the `context.Context` that
  every `app.Handler` already receives.
- **`app.Authorized(policy)` runs as core middleware** (§7.2, §3.2) — it wraps
  `app.Handler[Req, Res]`, so *"authorization applies to gRPC and consumers
  too"* (§7.2), written once (§1.4).
- Per-route guards, as §7.2's one code example shows.

## Non-goals

- **Not an identity provider.** Warren validates tokens; it does not issue them,
  store users, or manage sessions. warren.md describes no such surface.
- **Not a policy engine.** §3.2 gives `app.Authorized(policy)` a `policy`
  argument and stops there. No rule language, no OPA, is mentioned anywhere in
  warren.md.
- **Not the owner of transport concerns.** CORS and gRPC interceptors are edge
  middleware owned by the transport adapter (§1.4, §4.1, §4.2). This package
  contributes a guard to that ring; it does not own it.
- **Imports no other adapter** (invariant 4). This module is in the adapters ring
  (§1.1, §1.6) and depends only on the core module's contract packages.

## Dependency audit

**Chosen:** `golang-jwt/jwt/v5` and `coreos/go-oidc`, recorded in §9 as
Auth · Wrap, with an empty note column. §7.2 states the libraries and gives no
comparison, no rejected alternative, and no reason — unlike, say, §5.1's Kafka
table. The Wrap mode is, however, well justified by the wrap rule (AGENT.md
§ Modes): a token library reached from every guarded route would be exactly the
"edits across hundreds of user files" case if it changed.

**Outstanding.** warren.md records **no observation date, no archived check, no
last-release date, no licence check, and no transitive footprint** for either
library. AGENT.md § Adding a dependency requires all of it in writing before the
dependency enters a `go.mod` — "a package with no written audit does not go into
a go.mod", and star counts are not evidence. Both audits must be run, recorded
here, and folded into §9 before implementation starts.

## Public API

**warren.md gives exactly one line of surface for this package** (§7.2):

```go
r.HTTP().Get("/users/{id}", c.get, auth.RequireScope("users:read"))
```

That is a per-route option, in the same position as the broker options of §5.1
(`r.Events().On(topic, handler, broker.WithRetry(...), ...)`). Note that §3.5's
own `Registrar` example registers the same route without the third argument, so
the option parameter is variadic.

**Nothing else is specified.** The signature of `RequireScope`, the type of the
option it returns, and which of `HTTPRegistrar`/`GRPCRegistrar`/`EventRegistrar`
accept it are all absent. Provisionally, and pending Open question 1:

```go
// Package auth validates caller credentials at the transport edge, places the
// resulting identity on the context, and supplies the policy checks that
// app.Authorized runs as core middleware.
package auth

// RequireScope returns a route option that rejects a call whose identity does
// not carry the named scope.
func RequireScope(scope string) /* option type not fixed by warren.md */
```

The following are named by warren.md but given **no** Go at all, and must be
agreed before this spec is approved:

| Named in warren.md | What is missing |
|---|---|
| "identity lands on the context" (§7.2) | the identity type, and the accessor that reads it back |
| `app.Authorized(policy)` (§3.2, §7.2) | the type of `policy`, and how a policy is written |
| token validation | issuer, audience, key source, clock skew, algorithms — no configuration surface exists |
| OIDC | discovery URL, JWKS refresh — `coreos/go-oidc` is named in §7.2 and §9 and never used in an example |
| module wiring | every other adapter has a `Module`/`Server`/`Broker` constructor (§4.1, §5.1, §6.1, §7.1). §7.2 shows none |

Per AGENT.md § Spec-driven development, the public API section is the contract
under review; this one is deliberately incomplete rather than invented, and the
gaps are Open questions.

## Behaviour

The one behaviour warren.md does fix is the ring split, and it is the whole
design:

```
credential in transport envelope
      │
      ▼  EDGE ring  (transport-shaped, adapter-owned — §1.4)
   guard: extract + validate token
      │
      ▼  identity on context.Context
      │
      ▼  CORE ring  (wraps app.Handler[Req,Res] — §1.4, §3.2)
   app.Authorized(policy)
      │
      ▼
   Handle(ctx, req)
```

- **Why the split.** Edge middleware "cannot be shared" (§1.4) because an HTTP
  header, a gRPC interceptor, and a consumer's ack semantics have nothing in
  common. Core middleware wraps the handler itself, so one authorization check
  covers all three transports — §7.2's stated payoff: *"so authorization applies
  to gRPC and consumers too."*
- **The context is the seam.** Identity is put on `context.Context`, which is
  already the first parameter of `Handler.Handle` (§3.2) and is never stored in a
  struct (AGENT.md § General). This is the same carrying pattern as §2.5's
  logger.
- **The handler stays clean.** A use case reading identity from the context
  imports no transport package (invariant 6) and does not know whether the call
  arrived over HTTP, gRPC, or Kafka.
- **`app.Authorized` has the same unresolved mechanism as `app.Traced`.** `app`
  is a core contract package: stdlib-and-dig only (invariant 1), zero
  implementations (invariant 5). A policy check backed by JWT claims is an
  implementation, and `policy`'s type would have to be declared in core for
  `app.Authorized(policy)` to compile there. warren.md never states how the
  contract package and this adapter meet. See Open questions.

## Testing

- **No Docker, no network, no sleeps in unit tests** (AGENT.md § Testing). OIDC
  discovery and JWKS fetching are network I/O: every test that performs them goes
  behind `//go:build integration`, along with any real identity-provider
  container.
- Unit tests sign tokens with a locally generated key and assert on the guard's
  decision. Token expiry is tested by injecting the time, never by sleeping.
- **The ring split is the property to test, not just the parser.** The same
  handler, guarded by the same policy, must be rejected identically when invoked
  over HTTP, over gRPC, and from a consumer — that is §7.2's claim and it needs a
  test per transport.
- **Golden-file tests for every error message** (AGENT.md § Testing). warren.md
  fixes no error text for this package, so every message written during
  implementation is new and needs a golden file — including whatever a rejected
  call reports.
- `t.Parallel()`, table-driven subtests named for behaviour.
- Negative cases are the interesting ones: absent credential, malformed token,
  wrong issuer, wrong audience, expired, unknown signing key, valid token with
  the wrong scope.

## Definition of done

- [ ] Dependency audits for `golang-jwt/jwt/v5` and `coreos/go-oidc` run,
      recorded above with their observation date, and added to warren.md §9.
- [ ] Open questions 1–6 answered by the human, and this spec's Public API
      section completed and re-approved **before** any code — the current section
      is not a contract that can be implemented against.
- [ ] `auth.RequireScope("users:read")` works as a per-route option exactly as
      §7.2 writes it.
- [ ] Identity is placed on the context by the edge guard and readable by any
      code holding the context; nothing stores it in a struct.
- [ ] `app.Authorized(policy)` rejects identically over HTTP, gRPC, and a
      consumer, with a test per transport.
- [ ] No `jwt` or `oidc` type appears in a Warren exported signature (invariant
      3); raw access, if any, is a named escape hatch.
- [ ] Golden files exist for every error message this package emits.
- [ ] `make ci` passes (once the Makefile exists — AGENT.md § Repository state).

## Open questions

warren.md gives this package one paragraph and one line of code. Most of it is
undetermined, and none of it should be guessed.

1. **What is the option type returned by `RequireScope`?** §7.2 passes it to
   `r.HTTP().Get`, but §3.5's `Registrar` shows no options parameter at all, and
   §5.1's consumer options are `broker.*`. Is there one shared route-option type
   in `transport`, one per registrar, or does each package define its own?
2. **What is the identity type on the context, and how is it read back?**
   "Identity lands on the context" is the whole seam between the two rings and
   warren.md names neither a type nor an accessor. Is it a struct, an interface,
   or a claims map? Which package declares it — `auth` is an adapter, so a
   handler reading identity from it would import an adapter.
3. **What is `policy` in `app.Authorized(policy)`?** Its type must live in the
   core module for §3.2 to compile there, but a JWT-scope policy is an
   implementation, and core is stdlib-and-dig only (invariant 1) with zero
   implementations in contract packages (invariant 5). Same mechanism as
   `app.Traced()`, which is now answered and not by this spec: the value RIDES
   THE CONTEXT, so the core middleware takes no argument and the adapter seeds
   it (warren.md §3.2; `observability/SPEC.md` asked it and has since retired).
   `app.AuthorizationPolicy` is the core-side answer here, and
   `app.RequireScope` is the core-resident proof that a policy needs no token
   library.
4. **How is token validation configured?** No issuer, audience, key source,
   allowed algorithms, clock skew, or JWKS refresh interval appears anywhere in
   warren.md. Is there an `auth.Module(...)` like every other adapter has, and
   what are its options?
5. **What is `coreos/go-oidc` for, concretely?** It is named in §7.2 and §9 and
   never appears in an example. Discovery and JWKS for JWT validation, or full
   OIDC login/callback flows? The second is a much larger surface and would need
   its own manifest entry.
6. **RESOLVED (2026-08-01):** warren.md §2.6 gained the rows — `UNAUTHENTICATED`
   → 401 / `Unauthenticated` / → DLQ (never retry) and `PERMISSION_DENIED` →
   403 / `PermissionDenied` / → DLQ (never retry) — plus the rule that
   `UNAUTHENTICATED` describes the caller's identity, not yours: downstream auth
   failure is `UNAVAILABLE`. What identity a *consumed message* carries remains
   open as question 7. **Which errors does a rejected call return?** §2.6 defines
   `CodeUnauthenticated` and `CodePermissionDenied`, and §2.6's table maps codes
   to HTTP/gRPC/consumer outcomes — but that table has rows for `INVALID`,
   `NOT_FOUND`, `CONFLICT`, `UNAVAILABLE`, and `INTERNAL` only. The two auth
   codes exist in the `Code` list and have **no row in the translation table**,
   so what an unauthenticated Kafka message does — ack, nack, DLQ — is undefined.
   This needs answering in §2.6, not here.
7. **What happens to consumers?** A message from a broker has no bearer token.
   If `app.Authorized(policy)` applies to consumers (§7.2 says it does), what
   identity is on the context there — a service identity, the identity of the
   original caller propagated in `Message.Headers` (§3.4), or none?
8. **Does authentication have an edge presence in every transport adapter?** §10
   shows "auth" in the HTTP edge chain. gRPC's edge is `grpc.Interceptors(...)`
   (§4.2), which lists `Recovery()` and `Tracing()` and no auth interceptor. Who
   registers the guard for gRPC and for consumers?
