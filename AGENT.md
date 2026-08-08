# AGENT.md — Instructions for AI agents working in this repository

This is the canonical instruction file for any coding agent. Tool-specific
files (`CLAUDE.md`, `.cursorrules`, `.github/copilot-instructions.md`) point
here rather than restating it — two copies of the same rules drift within a
month, and then neither is trusted.

**Read this and [warren.md](warren.md) before your first edit.** `warren.md` is
the package manifest: one entry per package, what it owns, what it wraps, what
it exposes. It is the source of truth for the architecture. This file is the
source of truth for the rules.

---

## Repository state

The repository was reset in July 2026. Everything except the licence and
`warren.md` was deleted, because the previous implementation had drifted from
the design.

What exists right now:

```
warren.md         the design — package manifest, architecture, dependency ledger
AGENT.md          this file
CLAUDE.md         Claude Code's pointer to this file
README.md         the public front door and the roadmap checkboxes
LICENSE           Apache-2.0
NOTICE
<pkg>/SPEC.md     one spec per UNIMPLEMENTED package; specs retire on completion
go.mod            the core module — stdlib only until warren/di lands
Makefile          fmt · vet · lint · invariants · test, iterated per module
.github/          CI: the same targets, plus golangci-lint v2
.golangci.yml     linter config
scripts/invariants.sh   the greppable invariants (1, 2, 8, naming), run in CI
errors/ domain/ log/ di/ lifecycle/ config/ warren (root)
                  implemented kernel packages — specs retired; code, golden
                  tests, and warren.md are the contract
docs/assets/      one usage diagram per approved spec, .puml + .png
```

The tooling was rebuilt on 2026-08-01: `make ci` is the gate, and it runs fmt,
vet, golangci-lint, the invariants script, and `go test -race` across every
module in the Makefile's `MODULES` list. Do not claim a check passed without
running it. The invariants grep cannot see (driver types in public signatures,
ring imports) still bind you in review either way.

Module path: `github.com/MerseniBilel/warren`. Go 1.27 on its release
(invariant 9); toolchain 1.26.x until then.

---

## What Warren is

A DDD-first application framework and CLI for Go backends. Four claims define
it, and every rule below protects one of them:

1. **Transport-agnostic use cases.** One `app.Handler[Req, Res]`; HTTP, gRPC,
   and message consumers are thin adapters over it.
2. **Real module encapsulation.** A provider is private to its module unless
   exported, and imports are explicit — not one global type-keyed container
   where everything sees everything.
3. **DDD as real types**, not folder naming conventions.
4. **Architecture enforced in CI** — `warren lint arch` fails the build when
   `domain/` imports `infrastructure/`.

Warren is **not** a web framework, an ORM, or a deployment platform. It composes
existing routers and drivers behind stable ports.

**The governing constraint: Warren obeys its own dependency rule.** If the
kernel imported `net/http`, the architecture-linting pitch would be a lie. Every
boundary below is enforced by the same `warren lint arch` that ships to users.

---

## The four rings

```
TOOLING     warren/cli                                build-time only
            templates · AST editor · analyzer         never in a service go.mod
─────────────────────────────────────────────────────────────────────────
ADAPTERS    transport/http   transport/grpc           separate go modules
            broker/kafka     broker/memory            never import each other
            persistence/postgres   observability
─────────────────────────────────────────────────────────────────────────
CONTRACTS   app.Handler   broker.Publisher/Subscriber ports & shared types
            persistence.Repository/UnitOfWork         implementation-free
            transport.Registrar   domain.*            (one exception, below)
─────────────────────────────────────────────────────────────────────────
KERNEL      warren · di · lifecycle · config          stdlib + dig only
            log · errors · validate · health
```

**Dependencies point downward only.**

- **Kernel** has no knowledge that HTTP, SQL, or Kafka exist.
- **Contracts** are pure interfaces, so an adapter and a user's domain package
  both depend on `broker.Publisher` without ever meeting.
- **Adapters** are leaves. `broker/kafka` and `persistence/postgres` are
  mutually invisible — which is what makes them independently versionable.
- **Tooling** is a one-way street: the CLI imports the runtime to analyse it;
  the runtime never imports the CLI.

**The one carve-out: the root `warren` package is the composition root, and it
may import the contracts ring.** Boot step 5 hands every controller a
`transport.Registrar` and freezes the result into a `*transport.Table`, so the
bootstrapper has to know that `transport.Controller` exists — there is nowhere
else that knowledge could live, because only the bootstrapper holds the module
scopes the controllers were built in. **No other kernel package may**, and
`scripts/invariants.sh` enforces the second half: a `di`, `lifecycle`, `config`,
`log`, `errors`, `validate`, or `health` that knows what a route is has
collapsed the ring. The carve-out is exactly one package wide, and adding to it
is an architecture decision, not an implementation detail. (Added 2026-08-02
with boot step 5; the reasoning is recorded in `transport/http/SPEC.md` ⚠1.)

Inside a user's application the rule is the familiar one, and `warren lint arch`
is the thing that enforces it:

```
interfaces ──▶ application ──▶ domain ◀── infrastructure
domain imports NOTHING from the other three.
```

Only `module.go` may see all four layers.

---

## Non-negotiable invariants

Breaking any of these fails CI. They are not style preferences.

### 1. The core module depends on the standard library and `dig`. Nothing else.

The root module (`github.com/MerseniBilel/warren`) imports the standard library
and `go.uber.org/dig`. That is the entire list, permanently — not "for now,"
not "except this one."

If a core feature seems to need a library: **define the port in core, implement
it in a submodule.** That is the move, every time.

### 2. `go.uber.org/dig` is imported by `warren/di` and by nothing else.

`di` is a **Wrap** (see Modes below): dig is the engine, and the wrap boundary
is absolute. No dig type — `dig.Container`, `dig.Scope`, `dig.In`, `dig.Out`,
`dig.ProvideOption` — appears in any Warren exported function, method, struct
field, or error. **Users never import dig.**

The reason this is worth the discipline: *"a missing provider prints a
copy-pasteable fix"* is a stated product target, and it is unreachable while
surfacing someone else's diagnostics. Compare — raw dig:

```
missing dependencies for function ...: missing type: *postgres.Pool
```

Warren:

```
✗ cannot resolve dependency

    domain.UserRepository
      └─ required by *user.RegisterUserHandler
           └─ required by *user.UserController
                └─ declared in internal/modules/user/module.go:14

  No provider found in scope "user" or its imports.

  Did you mean:
    • postgres.NewUserRepository is registered in scope "billing" but not exported.
      Add to billing's module: warren.Exports[domain.UserRepository]()
    • Or provide it locally:  warren.Providers(postgres.NewUserRepository)
```

That second block is the deliverable. Anything that leaks dig's error text
through is a bug, not a shortcut.

### 3. No driver type in a public signature

`*chi.Mux`, `*pgxpool.Pool`, `*kgo.Client`, `*grpc.Server`, `amqp.Delivery` —
none may appear in any Warren exported signature. This is what makes every
driver swappable, and it is why "swap Kafka for RabbitMQ" is one line in
`main.go`.

Raw handles are reachable through **named escape hatches only** —
`http.Raw(func(mux *chi.Mux){...})`, injecting `*kgo.Client` deliberately. An
escape hatch is an explicit opt-out, never the default path.

### 4. Adapters never import each other

`broker/kafka` and `persistence/postgres` are mutually invisible. Every adapter
depends only on the core module's contract packages.

### 5. Contract packages contain zero implementations

`domain`, `app`, `persistence`, `broker`, `transport` are interfaces, types, and
pure functions. A concrete driver in a contract package collapses the ring.

**The one deliberate exception:** the three protocol registrars in
`warren/transport` (§3.5) are concrete structs with generic methods. Go 1.27
permits type parameters on methods of concrete types and forbids them on
interface methods permanently, so the registrar API is only expressible this
way. They hold no driver type — they erase handlers into route closures. No
other concrete type enters the contracts ring without amending this invariant.

### 6. Handlers import no transport

If a change makes a use case import `net/http`, `grpc`, or a broker client, the
change is wrong. This is the framework's whole point.

### 7. No reflective *dispatch*, and no container lookup, on the request path

**Amended 2026-08-02.** This invariant used to read "no reflection on the
request path", and that was already false in shipped, approved code:
`transport/params.go`'s `bindParams` calls `reflect.ValueOf(req).Elem()` and
`FieldByIndex` per request, `validate`'s compiled `Rule` does the same, and
`encoding/json` is reflection top to bottom. A rule the code breaks is worse
than no rule — the first serious reviewer disproves it in ten minutes, and
everything else on this list loses credit with it.

What is true, and what is actually worth defending:

**Every decision is made at boot.** Which fields carry parameters, which
setters convert them, which rules run, which codec encodes, which
middleware wraps which handler, which closure serves a route — all resolved
during boot steps 1–5 and frozen. By the time the app serves, the route
table holds pre-built closures with middleware already composed:

```go
type route struct {
    invoke func(ctx context.Context, raw []byte) ([]byte, error)
}
```

**What remains at request time is a fixed walk over precomputed indices.**
`FieldByIndex` over an `[]int` computed at boot is a pointer offset, not a
name lookup; there is no `reflect.Type` search, no tag re-parse, no method
resolution by string, and no type switch over user types.

**The DI container is never consulted at request time.** Go teams will
assume otherwise, so this gets stated, tested, and benchmarked.

**And the cost is a number, not an adjective.** Every request path carries a
`testing.AllocsPerRun` test asserting an *exact* allocation count, so a
regression fails CI rather than drifting in a benchmark nobody reads.

### 8. No committed `replace` directive

It breaks `go get` for users, silently. Use a git-ignored `go.work` for
cross-module development.

### 9. Go 1.27, from the day it ships

Warren tracks the current Go major release. Go 1.27 (expected August 2026)
delivers the generic methods that §3.5's registrars require, and Warren adopts
it on release. Until it ships the installed toolchain is 1.26.x and **nothing
may depend on a 1.27 feature** — which is why the transport layer waits while
the kernel is built. No `toolchain` directive in any module, and no
compatibility path to older releases.

---

## Modes: how a dependency decision is classified

Every third-party decision in `warren.md` carries one of three modes. Use the
same vocabulary in specs and in review.

| Mode | Meaning |
|---|---|
| **Build** | Warren owns it outright. No third-party equivalent is acceptable. |
| **Wrap** | Good library, but users must not import it directly. Port interface in front, raw handle available as an escape hatch. |
| **Vendor** | Imported and used directly. Swapping it would be a breaking change we accept. |

**The wrap rule:** *if changing a library would force edits across hundreds of
user files, it must be behind a port.* That is the whole test. Apply it before
arguing about anything else.

The current ledger lives in [warren.md §9](warren.md). Deviating from a
recorded mode is an architecture change, not an implementation detail — bring it
to the human.

---

## Two orderings you may not rearrange

These are load-bearing. They are the reason several packages are Build rather
than borrowed, and changing either one silently is how the framework's promises
break.

### Boot — every error the framework can detect surfaces at boot, never on request 1

```
 0  load config          layered: defaults → file → env → flags, validated
 1  flatten module graph resolve imports, detect cycles → fail
 2  build scopes         one child container per module, copy exported bindings
 3  VALIDATE GRAPH       every dep resolvable? ambiguous? → fail
 4  instantiate          singletons, topological order
 5  register             controllers + consumers build route tables in memory
 6  OnStart              dependency order: pool → repos → consumers → servers
 7  readiness opens      health endpoint flips green
 8  serve
```

Step 3 is why a missing provider is a startup crash with a full resolution
chain, not a nil-pointer panic in production.

`warren.NewModule` returns an **inert value**. Nothing registers on
construction. The bootstrapper walks the entire graph before materialising
containers — that ordering is what makes cycle detection and encapsulation
*checkable* rather than emergent. A module declaration that registers a side
effect breaks steps 1–3.

### Shutdown — readiness closes first

```
SIGTERM
  1. readiness probe → 503        ← load balancer drains BEFORE anything stops
  2. HTTP/gRPC servers stop accepting, in-flight requests finish
  3. consumers stop fetching, in-flight messages ack
  4. outbox relay flushes
  5. DB pools, broker connections close
  6. force-exit deadline (default 30s)
```

Closing readiness *before* stopping servers is the ordering most hand-rolled Go
services get backwards, and it is why rolling deploys drop requests. It is also
why `lifecycle` is Build and not `fx` — fx does not model readiness gating or
drain ordering.

---

## The error table is load-bearing

`warren/errors` is not a convenience package. It is the reason domain code can
return `errors.Conflict(...)` and never import `net/http`. One table, three
transports:

| Code | HTTP | gRPC | Consumer |
|---|---|---|---|
| `INVALID` | 400 | `InvalidArgument` | → DLQ (never retry) |
| `NOT_FOUND` | 404 | `NotFound` | ack + log |
| `CONFLICT` | 409 | `AlreadyExists` | ack (idempotent replay) |
| `UNAUTHENTICATED` | 401 | `Unauthenticated` | → DLQ (never retry) |
| `PERMISSION_DENIED` | 403 | `PermissionDenied` | → DLQ (never retry) |
| `UNAVAILABLE` | 503 | `Unavailable` | nack + backoff retry |
| `INTERNAL` | 500 | `Internal` | nack + retry, then DLQ |

Each adapter owns its column. A handler that maps a code to a status itself has
broken ring 2.

**`UNAUTHENTICATED` describes the caller's identity, not yours.** A service
that fails to authenticate to something downstream — Postgres, S3, another
API — returns `UNAVAILABLE` (retryable), never `UNAUTHENTICATED`. The two auth
codes dead-letter on a queue because retrying will not mint a better token and
acking destroys the evidence of a producer publishing with the wrong identity.

---

## Two middleware rings, deliberately

- **Edge** middleware is transport-shaped — CORS, gRPC interceptors, consumer
  ack semantics — and **cannot be shared**. It belongs to the adapter.
- **Core** middleware wraps `app.Handler[Req, Res]` itself, so a transaction
  decorator, retry policy, or authorization check is written once and applies
  to HTTP, gRPC, and consumers identically.

Conflating the two is the mistake that makes "transport-agnostic" frameworks
leak. When adding middleware, name which ring it is in, in the spec.

---

## Spec-driven development — write the spec first

**No feature is implemented before its spec exists and is approved.** This is a
hard process rule, not a suggestion, and it applies to you exactly as it applies
to a human contributor.

1. **Write the spec** in `SPEC.md` **inside the package directory it
   describes** — `errors/SPEC.md`, `di/SPEC.md`, `transport/http/SPEC.md`. It
   states: the problem, goals, non-goals, the **public API as Go**, behaviour,
   every error message, testing, and a definition of done.

   The spec lives with its code, not in a central `specs/` tree, for one
   reason: a change to the code and the correction to its spec then appear in
   the same directory and the same diff. A spec a reviewer has to go looking for
   is a spec that silently stops describing the code. The filename carries no
   number — build order is a roadmap concern, not a filename concern.

2. **Get it approved.** Code starts here, not before.
3. **Implement to the definition of done** — tests, doc comments, and the skill
   if it is a CLI command.
4. **When the implementation diverges from the spec, correct the spec in the
   same pull request.** A spec that no longer describes the code is worse than
   no spec — it is a confident lie, and the next agent will believe it.
5. **When the package is implemented and has survived review, the spec is
   retired — deleted, in the same change.** From that point the code, its doc
   comments, its tests and golden files, and the package's `warren.md` entry
   are the record; a parallel prose copy would only rot into the confident lie
   rule 4 exists to prevent. Before deleting, rehome anything load-bearing the
   spec still carries: open questions move to the spec of the package that
   will answer them, dependency-audit results live in the §9 ledger, and any
   behaviour the spec fixed that the code does not state gets written into doc
   comments or `warren.md` first. The kernel specs retired on 2026-08-01
   (errors, domain, log, di, lifecycle, config) followed exactly this path.

Two rules carry the weight:

- **The spec's public API section is the contract under review.** Reviewing
  prose and then discovering the actual signatures at merge time is how a spec
  becomes theatre. Write the Go. `warren.md` already fixes the public surface
  for most packages — a spec that contradicts it needs `warren.md` amended in
  the same change.
- **Decisions are made by research and argument, not by building throwaway
  code.** No spikes, no prototypes, no "let's try it and see". Read the
  evidence, bring the options and a recommendation to the human, agree it, then
  spec it and build it once.

If you are asked to build something with no spec, **write the spec first and say
so**. If a spec exists but is not approved, say that too rather than
implementing against an unagreed contract.

### What a skill is

Step 3 above and mistake 8 both make a skill a hard deliverable for a CLI
command, and until 2026-08-02 nothing defined the artefact. It is:

- **A Markdown file at `.claude/skills/<name>/SKILL.md`**, with `name` and
  `description` frontmatter. Under version control, so it is reviewed in the
  same diff as the command it describes — the only mechanism that keeps it
  true.
- **One per command group, not per subcommand.** The five `warren g`
  generators share one mental model; five near-identical files would rot
  independently.
- **Agent-facing.** User documentation is `--help` and the generated
  `README.md`. A skill exists so an agent reaches for the generator instead
  of hand-writing the file and forgetting the wiring — which is mistake 7.
- **About the failure modes, not the happy path.** What the command writes
  *and what it wires*; the guarantees it makes; every refusal diagnostic and
  what to do about it; and what is still left to do afterwards. A skill that
  only restates `--help` is not worth the file.

This ruling is recorded, with its reasoning and what it deliberately leaves
open, in `cli/SPEC.md` open question 1.

---

## Before you write code

| If you are about to… | First… |
|---|---|
| Build any feature | Write or find `<package>/SPEC.md`. See above. An implemented package has no spec — its code, tests, and `warren.md` entry are the contract. |
| Add a dependency | Read "Adding a dependency" below. This has a hard process. |
| Change a port's shape, a module boundary, or a public API | Update [warren.md](warren.md) and get it agreed |
| Touch anything structural | Read [warren.md](warren.md) |

**Do not create a new module unless its first real code lands in the same
change.** An empty module is a release obligation with no user.

**Do not add a package that `warren.md` does not describe** without agreeing the
manifest entry first. The manifest is the plan; a package that is not in it is
either scope creep or a missing decision.

---

## Adding a dependency

Warren's credibility rests on its `go.mod` being defensible — a hello-world
service whose `go.sum` looks like a Java project's means the multi-module split
has failed. There is a process and it is not optional:

1. **Read the repository and the documentation.** Not the README summary —
   check whether it is archived, when it last shipped, what it pulls in
   transitively, and whether its licence is Apache-2.0/MIT/BSD/ISC compatible.
   `gh api repos/<owner>/<repo>` gives stars, `pushed_at`, `archived`, and open
   issues in one call; `gh api repos/<owner>/<repo>/releases/latest` gives the
   real release date.
2. **Record what you found** in the spec that adopts it, with the observation
   date, and add it to the ledger in [warren.md §9](warren.md).
3. **Check placement.** Core: stdlib and dig, never anything else. Driver
   module: only its own driver. Test-only: test files only. Build-time only:
   the CLI module, which never enters a service's `go.mod`.
4. **Assign a mode** — Build, Wrap, or Vendor — and justify it against the wrap
   rule.

**A package with no written audit does not go into a `go.mod`.** Star counts are
not evidence. "It is popular" is not evidence.

Two live examples of why this matters, both found during the initial audit:
`google/wire` is archived, and `git-chglog` is archived. Both are still widely
recommended in blog posts, and neither README said so.

---

## Code conventions

### Naming

- **Never use `With` in a type name.** No `UserWithRelations`, `OrderWithItems`,
  `BusinessWithRelations`. Name the thing for what it *is* — `UserProfile`,
  `OrderDetail`, `EnrichedUser` — or model the relation explicitly. `With` in a
  type name describes a query's shape, not a concept, and it multiplies without
  limit.
  (`With` as a *function* prefix for options — `WithTimeout(d)` — is the
  standard Go functional-options idiom and is fine.)
- Interfaces are named for behaviour, not `-er` reflexively: `Repository`,
  `Publisher`, `UnitOfWork`, `Registrar`.
- No stuttering: `broker.Publisher`, never `broker.BrokerPublisher`.
- Exported identifiers read plainly. The project's metaphor is not a tax on the
  API: `warren.Module`, not `den.NewBurrow()`.

### Errors

- Return `warren/errors` semantic errors (`Invalid`, `NotFound`, `Conflict`,
  `Unauthenticated`, `PermissionDenied`, `Unavailable`, `Internal`). Domain code
  knows nothing about HTTP 404.
- Wrap with `%w` and add context, never with `%v`.
- **Error messages tell the user how to fix it.** This is a feature, not polish.
  "provider not found" is a bug in the error message; it must name what was
  missing, who requested it, where it was declared, and a copy-pasteable fix.
  See invariant 2 for the standard.

### General

- `context.Context` is the first parameter, always. Never stored in a struct.
- Accept interfaces, return concrete types — **except** constructors that return
  a port, which is Warren's whole pattern.
- No `init()`. No package-level mutable state. No `panic` in library code —
  with two named exceptions, both boot-time. `di.MustResolve`: the `Must`
  prefix is Go's documented panic convention, and the alternative to failing
  loudly at boot is a zero value that explodes later. `app.Chain`'s
  composition guards: a nil handler, a nil middleware, or a middleware
  returning nil panics at composition (boot step 5) with a message naming the
  position — Chain cannot return an error (§3.2 fixes its signature), and
  deferring the failure would be the request-time nil dereference the
  boot-ordering rule exists to prevent. The same guard covers the built-in
  middleware constructors — `app.Retrying(nil)`, `app.Authorized(nil)` — and
  their consumer-ring mirrors: `broker.Chain`, `broker.Pipeline`, and the
  stage constructors (`Deduplicate`, `DeadLetter`, `Retry`,
  `ConcurrencyLimit`) panic at composition, with named messages, on nil or
  nonsensical arguments. Nothing else panics, and nothing is added to this
  list without amending this line.
- Reflection belongs at boot, not in the request path (invariant 7).
- Exported identifiers have doc comments starting with the identifier's name.
- Formatting is the formatter's job. Never hand-format; never argue about it.

---

## Testing

- Unit tests: **no Docker, no network, no sleeps.** Anything else goes behind
  `//go:build integration`.
- `t.Parallel()` and table-driven subtests named for behaviour.
- **Every error message in a spec gets a golden-file test.** The diagnostics
  *are* the product (invariant 2); untested error text rots immediately.
- **Every generator needs a golden-file test.** Templates break silently
  otherwise.
- **Every port change updates the contract suite first**, then the drivers.
- The request path gets an allocation benchmark. Invariant 7 is a claim about
  performance and needs a number behind it.
- A bug fix starts with a failing test.
- Do not add a mocking framework. Hand-written fakes live in `warren/testing`.

---

## Commits

Conventional Commits, **scope is the module path**:

```
feat(broker/kafka): drain in-flight messages before revoking partitions
fix(di): name the requesting file in missing-provider errors
docs(warren): document the drain sequence
```

- Imperative mood, no trailing period, ≤72 characters.
- Breaking: `!` after scope **and** a `BREAKING CHANGE:` footer stating the
  migration.
- Every commit builds and passes its module's tests.

**Do not commit or push unless the human asked you to.**

---

## Commands

This is a **multi-module repository**, which has one consequence worth burning
in: **`go test ./...` does not cross module boundaries.** Run it from the root
and it tests one module and reports success — a green result that means almost
nothing.

Use the Makefile's targets (`make ci` is the gate) rather than the raw `go`
command from the root, and add every new module to the Makefile's `MODULES`
list when it is created — a module the list misses is a module CI never tests.

**Verify before claiming.** Run the command and quote what it printed. If a tool
is not installed, say so rather than asserting the code is clean.

---

## Mistakes agents make in this repo

Named specifically, because generic advice does not prevent them:

1. **Adding a dependency to the core module** because it seemed small. The core
   is stdlib plus dig. Split into port + submodule instead.
2. **Letting a `dig` type reach a public signature**, or letting a dig error
   message reach a user. The wrap boundary is the product.
3. **Running `go test ./...` from the root** and reporting the suite passed. It
   tested one module.
4. **Making a module declaration do work.** `NewModule` returns an inert value;
   the bootstrapper walks the graph first. Registering on construction breaks
   cycle detection and encapsulation.
5. **Reaching for the container at request time.** Boot builds closures; the
   request path calls them.
6. **Putting an implementation in a contract package**, or importing one adapter
   from another.
7. **Writing a new file that duplicates what a generator already produces.** Run
   the generator, then read its output.
8. **Adding a CLI command without its skill.** The command is not done until the
   skill exists.
9. **Assuming a package is healthy because it is well known.** Verify: `wire`
   and `git-chglog` are both archived.
10. **Naming a type `SomethingWithSomething`.** See Naming above.
11. **Marking work complete with an unverified claim.** Run the command and
    paste what it printed. "Should work" is not a result.
12. **Writing code for a feature that has no approved spec.** The spec is where
    a feature is made small enough to finish.
13. **Letting the spec and the code drift apart.** If the implementation had to
    differ, the spec is corrected in the same pull request — not later.
14. **Proposing a spike or a prototype.** Research it, put the options to the
    human, and agree the decision. Then build it once.

---

## When you are unsure

- **The decision is probably recorded.** Check [warren.md](warren.md) and the
  feature's spec before re-deriving it, and before re-opening it.
- **If a recorded decision is wrong, say so and propose changing it.** Do not
  quietly work around it — a worked-around rule is how layering decays, which is
  the problem Warren exists to solve.
- **If a rule here blocks a genuinely correct change**, raise it rather than
  disabling the check. `//nolint` without a specific linter and a reviewed
  reason is itself a lint failure.
- **Ask rather than guess on anything structural.** Module boundaries, port
  shapes, and public API are the human's call, not yours to discover.
