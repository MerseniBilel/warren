# ADR-0007: Track the current Go major release

- **Status:** Accepted
- **Date:** 2026-07-27

## Context

Go's own support policy maintains the two most recent major releases. As of
2026-07-27 that is Go 1.25 (2025-08-12) and Go 1.26 (2026-02-10, current patch
1.26.5 released 2026-07-07).

A library's `go` directive is a hard floor on its consumers: declaring `go 1.26`
means a team on Go 1.25 cannot use Warren at all. Frameworks usually lag by one
release to avoid excluding users.

The counter-pressure is that Warren is a framework built on reflection, code
generation, and static analysis — the areas where recent Go releases deliver the
most. Go 1.26 alone brings the matured Green Tea garbage collector, `new` taking
an expression, and self-referential generic type parameters, the last of which
directly affects how `Repository[T, ID]` and `Handler[Req, Res]` can be
expressed.

## Decision

**Warren requires the current Go major release. `go.mod` declares `go 1.26`.**

- The `go` directive is raised within one minor release of a new Go major.
  Raising it is a `feat!:` commit and a minor version bump of Warren while
  pre-1.0.
- **No `toolchain` directive** in any module. Pinning a toolchain in a library
  forces a download on consumers; the floor is the `go` directive's job.
- CI tests the **current major and the next release candidate**. Testing against
  the RC is what makes "raise within one minor" achievable rather than
  aspirational.
- Warren does not use `GOEXPERIMENT` features in shipped code. As of Go 1.26
  that excludes `simd/archsimd` and `runtime/secret`.

This is deliberately more aggressive than the usual library posture, and the
cost is stated below rather than hidden.

## Consequences

### What this buys

- Generics can be used as they are now, not as they were two releases ago.
  Self-referential type parameters matter for the DDD primitives in PRD §4.4.
- One Go version to support means one set of build tags, one CI axis, and no
  compatibility shims accumulating in the core.
- Static-analysis tooling (`golang.org/x/tools`) tracks current Go closely;
  staying current keeps `warren lint arch` on supported ground.

### What this costs

- **Teams on the previous Go major cannot adopt Warren.** This is a real
  adoption cost against the PRD §3.4 target of teams of 3–30, some of whom
  upgrade slowly. It is accepted knowingly, and it is the first thing to
  reconsider if adoption stalls for this reason.
- A six-month upgrade treadmill for users, forever.

### What we now cannot do

- We cannot backport fixes to a Warren release built against an older Go.
  Support is: current Warren minor, current Go major.

## Alternatives considered

**Support the two most recent Go majors, matching Go's own policy** — the
conventional and more welcoming choice. Rejected for v0.x specifically: during
pre-1.0 the cost of compatibility shims is paid in the core's design, at exactly
the moment the core is being designed. **This is the most likely ADR here to be
superseded at v1.0**, when API stability becomes worth more than language
currency.

**Pin a `toolchain` directive** — would guarantee everyone builds with the same
compiler. Rejected: it triggers toolchain downloads for consumers and is
considered poor practice for libraries.

## Revisit when

- **At v1.0.** Stability commitments likely outweigh language currency then;
  expect this to become "two most recent majors."
- Adoption feedback shows the Go floor is what is blocking real users.
- A Go release lands with nothing Warren needs — evidence the aggressive policy
  is not buying anything.
