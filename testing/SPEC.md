# `github.com/MerseniBilel/warren/testing` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §7.5](../warren.md) |
| **Module** | own module (`github.com/MerseniBilel/warren/testing`) |
| **Mode** | Vendor |
| **Wraps** | — vendored: `testcontainers-go`, `stretchr/testify` |

## Problem

Testing a module built on Warren means booting part of the DI graph: a handler is
resolved from a scoped container, its dependencies come from module imports, and
what it does is partly *publishing an event* rather than returning a value. Doing
that by hand in every test means re-implementing module wiring per test file, and
substituting a dependency means either a mocking framework — which AGENT.md
§ Testing forbids — or bespoke wiring.

warren.md §7.5 makes it four calls: build the module under test with
substitutions, invoke a handler, assert on what was published, close.

## Goals

- Boot one module for a test, with dependencies replaced: `NewModuleTest(t,
  module, Replace[T](fake), WithMemoryBroker())` (§7.5).
- Invoke a handler by its request/response types: `Invoke[Req, Res](app, ctx,
  req)` (§7.5).
- Assert an event was published: `AssertPublished[Event](t, app)` (§7.5).
- Tear down deterministically: `app.Close()` (§7.5).
- Provide integration helpers that spin real Postgres and Kafka **behind a build
  tag** (§7.5).
- Be the home for hand-written fakes (AGENT.md § Testing).

## Non-goals

- **No mocking framework.** AGENT.md § Testing: "Do not add a mocking framework.
  Hand-written fakes live in `warren/testing`." `Replace[T](fake)` substitutes a
  value you wrote; it does not generate one.
- **Not a core dependency.** CLAUDE.md forbids adding an assertion library to the
  **core** module. This is not core: §1.6 gives `warren/testing` its own module,
  so `testify` and `testcontainers-go` live here and never reach a service's
  runtime `go.mod`. Nothing in this module may be imported by the core module or
  by any adapter's non-test code.
- **Not a replacement for a module's own tests.** This is the harness; the
  assertions about behaviour belong to the package under test.
- **Not the owner of generator testing.** §7.5's closing sentence — "Every CLI
  generator has golden-file tests — templates break silently otherwise" — is an
  obligation on `warren/cli` (§8, generator rules; AGENT.md § Testing), stated in
  §7.5 because that is where testing is discussed. It is listed under Open
  questions as a placement issue, not adopted as a goal here.

## Dependency audit

**Chosen:** `testcontainers-go` (real Postgres/Kafka for integration tests) and
`stretchr/testify` (assertions). §9 records both as **Vendor**, with an empty
note. §7.5 gives no comparison and no rejected alternative.

Vendor is the right mode by the wrap rule (AGENT.md § Modes): the rule is about
libraries whose replacement would force edits across hundreds of *user* files,
and a test helper is imported deliberately, per test file, by a user who chose
it. §7.5's own example calls `require.NoError(t, err)` directly — testify is used
in the open, not hidden. Swapping either is a breaking change Warren accepts.

**Outstanding.** warren.md records **no observation date, no archived check, no
last-release date, no licence check, and no transitive footprint** for either
library. AGENT.md § Adding a dependency requires that written audit before either
enters a `go.mod`. `testcontainers-go` deserves particular attention: it pulls
the Docker client and a large transitive tree, which is tolerable only because
this module never enters a service's runtime dependency set. Both audits go here
and into §9 before implementation.

## Public API

warren.md §7.5 gives one worked example and no signatures. Note the import alias:

```go
func TestRegisterUser(t *testing.T) {
    app := warrentest.NewModuleTest(t, user.Module(),
        warrentest.Replace[domain.UserRepository](fakes.NewUserRepo()),
        warrentest.WithMemoryBroker(),
    )
    defer app.Close()

    res, err := warrentest.Invoke[RegisterUser, UserDTO](app, ctx,
        RegisterUser{Email: "a@b.com", Name: "Ada"})

    require.NoError(t, err)
    warrentest.AssertPublished[domain.UserRegistered](t, app)
}
```

Provisional, and **not fixed by warren.md** — the returned type, the option
type, and the shape of `Replace` are all unstated:

```go
// Package testing boots a Warren module for a test with its dependencies
// substituted, invokes handlers by request and response type, and asserts on
// the events a handler published.
package testing

// NewModuleTest boots module for the duration of the test, applying opts.
func NewModuleTest(t *testing.T, module warren.Module, opts ...Option) /* type not fixed */

// Replace substitutes the binding for T with the given value.
func Replace[T any](v T) Option

// WithMemoryBroker binds the in-process broker instead of a real one.
func WithMemoryBroker() Option

// Invoke resolves the handler for Req and Res and calls it with req.
func Invoke[Req, Res any](app /* type not fixed */, ctx context.Context, req Req) (Res, error)

// AssertPublished fails the test unless an event of type E was published.
func AssertPublished[E any](t *testing.T, app /* type not fixed */)
```

Two conventions collide with §7.5's example, and both need the human:

- **`Invoke[Req, Res](app, ctx, req)` puts the context second.** AGENT.md
  § General: "`context.Context` is the first parameter, always." Either the
  convention has a stated exception for test helpers, or §7.5's example needs
  amending to `Invoke[Req, Res](ctx, app, req)`. Do not silently pick one.
- **`Replace[T](fake)` names a type parameter and a value.** `Replace` is a
  function, so AGENT.md's ban on `With` in *type* names is not at issue, and
  `WithMemoryBroker()` is the standard Go options idiom that AGENT.md explicitly
  permits — it is warren.md's own spelling and stays.

## Behaviour

- **A test boots a real graph.** §2.1 exposes `Start(ctx)`/`Stop(ctx)` "for
  tests", and §1.3's boot sequence — flatten, scope, validate, instantiate,
  register — is what makes `Invoke` able to resolve a handler by type. A module
  test therefore exercises graph validation (§1.3 step 3) as a side effect: a
  module with a missing provider fails the test at construction, not at
  invocation.
- **Substitution and module encapsulation.** §2.1: anything not in `Exports` is
  private to the module. `Replace[domain.UserRepository](...)` substitutes a
  binding, but whether it may reach an unexported binding, or one inside an
  imported module, is unresolved — see Open question 5.
- **`WithMemoryBroker()` and the memory driver.** §5.4 makes `broker/memory` the
  "default in tests and in modular monoliths" — in-process pub/sub behind the
  same `Publisher`/`Subscriber` ports (§3.4). Because it is in-process, a module
  test needs no Docker and no network.
- **`AssertPublished` observes the publication path.** §3.3's `UnitOfWork.Do`
  drains `PullEvents()` from every aggregate saved in the transaction and writes
  the events to the outbox in the same commit; §5.5's relay later drains the
  outbox to the broker. Which of those two points `AssertPublished` inspects
  changes what the assertion means, and warren.md does not say — see Open
  questions.
- **Teardown.** `app.Close()` is called via `defer` in §7.5. The framework's own
  shutdown API is `Stop(ctx) error` (§2.1) and its ordering is §2.3's, so
  `Close()` is a different, context-free spelling — see Open questions.
- **Fakes live here** (AGENT.md § Testing). §7.5's example resolves its fake from
  a `fakes` package, which reads as the *application's* own; both can be true —
  Warren ships fakes for its ports, users write fakes for theirs.

## Testing

This package is a test harness and is itself tested:

- **No Docker, no network, no sleeps in unit tests** (AGENT.md § Testing). The
  module-test path — `NewModuleTest`, `Replace`, `Invoke`, `AssertPublished`,
  `Close` — must run entirely in process on the memory broker, or the harness has
  made every test that uses it an integration test.
- **`testcontainers-go` is only ever reached from files behind
  `//go:build integration`**, matching §7.5's "behind a build tag". This is worth
  an explicit check: an accidental import from a non-tagged file makes Docker a
  requirement for the whole suite. A build of the package without the tag must
  not pull the Docker client.
- **Golden-file tests for error text** (AGENT.md § Testing). The failure messages
  are this package's product surface — an `AssertPublished` that failed must name
  the event type expected and list what was actually published, per AGENT.md
  § Errors ("error messages tell the user how to fix it"). None of that text is
  fixed by warren.md, so all of it is new and all of it gets golden files.
- Harness behaviour under test: a replaced binding is the one resolved; a module
  with an unresolvable dependency fails at `NewModuleTest`; `Invoke` on a
  request type no handler serves fails with a message naming the type;
  `AssertPublished` fails when nothing was published and passes when it was;
  `Close` is safe to call once (its return value, if any, is Open question 3).
- `t.Parallel()`, table-driven subtests named for behaviour.

## Definition of done

- [ ] Dependency audits for `testcontainers-go` and `stretchr/testify` run,
      recorded above with their observation date, and added to warren.md §9.
- [ ] The package/alias question (Open question 1) and the context-position
      question (Open question 2) answered, and warren.md §7.5 amended if the
      example changes.
- [ ] The §7.5 example compiles and passes, exactly as written or as amended.
- [ ] The whole module-test path runs with no Docker, no network, and no sleep.
- [ ] Every `testcontainers-go` reference is behind `//go:build integration`,
      with a check that the untagged build does not pull it.
- [ ] Hand-written fakes for Warren's own ports live here; no mocking framework
      is added (AGENT.md § Testing).
- [ ] This module is imported by no runtime code in core or in any adapter.
- [ ] Golden files for every failure message the harness emits.
- [ ] `make ci` passes (once the Makefile exists — AGENT.md § Repository state).

## Open questions

1. **What is this package called in Go?** Its path is `warren/testing` (§1.6) but
   every call in §7.5 is qualified `warrentest.`. If the package clause says
   `package testing`, every test file that imports it must alias one of the two
   `testing` packages, and the harness's own signatures take `*testing.T` from
   the standard library — workable but hostile. Options: name the package
   `warrentest` at the path `warren/testing`; move the path to
   `warren/warrentest`; or keep `package testing` and document the alias as
   mandatory. This is a naming decision for the human, and §1.6 or §7.5 gets
   amended either way.
2. **Should `Invoke` take the context first?** §7.5 writes `Invoke[Req, Res](app,
   ctx, req)`; AGENT.md § General says context is always first. One of the two
   documents is wrong.
3. **What type does `NewModuleTest` return?** §7.5 calls `app.Close()` on it,
   passes it to `Invoke` and to `AssertPublished`, and it is clearly not
   `*warren.App`, whose shutdown is `Stop(ctx) error` (§2.1). Is it a distinct
   test-application type, and does it register `t.Cleanup` so the `defer
   app.Close()` is belt-and-braces?
4. **What does `AssertPublished` actually observe?** Events raised on an
   aggregate but not yet committed (§3.1 `PullEvents`), outbox rows written
   inside the transaction (§3.3 step 4), or messages that reached the broker
   (§5.5's relay)? With `WithMemoryBroker()` and no Postgres, is there an outbox
   at all in a module test — and does the relay run?
5. **Does `Replace` bypass module encapsulation?** §2.1 makes non-exported
   providers private to their module. Can a test replace a binding that the
   module does not export, and can it replace one inside an *imported* module?
6. **Where does `broker/memory` live?** §5.4 describes it, but §1.6's module list
   omits it while listing kafka, rabbitmq, and nats. If it is inside the core
   module it is an implementation next to the `broker` contract package
   (invariant 5); if it is its own module, `WithMemoryBroker()` makes
   `warren/testing` depend on it — and whether that crosses invariant 4's
   "adapters never import each other" depends on which ring `warren/testing` is
   in. §1.1's ADAPTERS ring does not list it.
7. **What is the integration surface?** §7.5 says "integration helpers spin real
   Postgres/Kafka behind a build tag" and names not one function. What are they
   called, do they reuse containers across a package, and how does a test get the
   DSN or broker address into the module under test?
8. **Is the CLI golden-file obligation this package's?** §7.5 states "every CLI
   generator has golden-file tests", but generators are §8's subject and the rule
   is already in AGENT.md § Testing. Does `warren/testing` ship the golden-file
   comparison helper the CLI uses, or does §8 own its own? A CLI helper here
   would put a test-only module in the tooling ring's dependency set.

Carried forward from the retired kernel specs (2026-08-01):

5. **Where does the cross-adapter conformance test live?** "Every adapter
   maps every code of §2.6" is the property that keeps the error table
   honest, but the core module cannot import the adapters and adapters never
   import each other (invariant 4). This package is the natural home — decide
   before the first transport adapter is built. *(was errors' open question
   8)* The same decision owns the home of the exported aggregate-semantics
   and lifecycle contract suites, which are implemented as internal tests in
   `domain` and `lifecycle` until a reusable package exists.
