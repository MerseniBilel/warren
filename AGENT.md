# AGENT.md — Instructions for AI agents working in this repository

This is the canonical instruction file for any coding agent. Tool-specific
files (`CLAUDE.md`, `.cursorrules`, `.github/copilot-instructions.md`) point
here rather than restating it — two copies of the same rules drift within a
month, and then neither is trusted.

**Read this before your first edit.** Warren enforces its architecture in CI, so
a change that ignores these rules fails the build rather than earning a review
comment.

---

## What Warren is

A DDD-first application framework and CLI for Go backends. Three claims define
it, and everything here protects one of them:

1. **Transport-agnostic use cases.** One `Handler[Req, Res]`; HTTP, gRPC, CLI,
   and message consumers are thin adapters over it.
2. **DDD as real types**, not folder naming conventions.
3. **Architecture enforced in CI** — `warren lint arch` fails the build when
   `domain/` imports `infrastructure/`.

Warren is **not** a web framework, an ORM, or a deployment platform. It composes
existing routers and drivers behind stable ports.

---

## Non-negotiable invariants

Breaking any of these fails CI. They are not style preferences.

### 1. The core module has zero third-party dependencies

The root module (`github.com/MerseniBilel/warren`) imports **only the standard
library**. Permanently — not "for now," not "except this one."

If a core feature seems to need a library: **define the port in core, implement
it in a submodule.** That is the move, every time.

Checked by `make lint-modules`.

### 2. No driver type in a public signature

`*chi.Mux`, `*pgx.Conn`, `*kgo.Client` — none of these may appear in any Warren
exported function, method, struct field, or error type. This is what makes every
driver swappable.

**Warren writes its own DI container**, so no third-party DI library
(`go.uber.org/dig`, `go.uber.org/fx`, `samber/do`) may be imported anywhere. See
[docs/architecture.md §3](docs/architecture.md). Enforced by `depguard` and by
`make lint-modules`.

### 3. The dependency rule

```
interfaces ──▶ application ──▶ domain ◀── infrastructure
domain imports NOTHING from the other three.
```

Only `module.go` may see all four layers.

### 4. Handlers import no transport

If your change makes a use case import `net/http`, `grpc`, or a broker client,
the change is wrong. This is the framework's whole point.

### 5. No committed `replace` directive

It breaks `go get` for users, silently. Use `make work` (generates a git-ignored
`go.work`) for cross-module development.

### 6. Go 1.26

Warren tracks the current Go major release. No `toolchain` directive in any
module, and no compatibility path to older releases.

---

## Spec-driven development — write the spec first

**No feature is implemented before its spec exists and is approved.** This is a
hard process rule, not a suggestion, and it applies to you exactly as it applies
to a human contributor.

The order is always:

```
docs/architecture.md → docs/roadmap.md → <package>/SPEC.md → code
```

1. **Write the spec** for the feature you are about to build, in `SPEC.md`
   **inside the package directory it describes** — `errors/SPEC.md`,
   `di/SPEC.md`, `transport/http/SPEC.md`. It states: the problem, goals,
   non-goals, the **public API as Go**, behaviour, every error message,
   testing, and a definition of done.

   The spec lives with its code, not in a central `specs/` tree, for one
   reason: a change to the code and the correction to its spec then appear in
   the same directory and the same diff. A spec a reviewer has to go looking for
   is a spec that silently stops describing the code. Build order lives in
   [docs/roadmap.md](docs/roadmap.md), so the filename carries no number.

   [`errors/SPEC.md`](errors/SPEC.md) is the worked example.
2. **Get it approved.** Code starts here, not before.
3. **Implement to the definition of done** — tests, doc comments, the skill if
   it is a CLI command.
4. **When the implementation diverges from the spec, correct the spec in the
   same pull request.** A spec that no longer describes the code is worse than
   no spec — it is a confident lie, and the next agent will believe it.

Two rules carry the weight:

- **The spec's public API section is the contract under review.** Reviewing
  prose and then discovering the actual signatures at merge time is how a spec
  becomes theatre. Write the Go.
- **Decisions are made by research and argument, not by building throwaway
  code.** No spikes, no prototypes, no "let's try it and see". Read the
  evidence, bring the options and a recommendation to the human, agree it, then
  spec it and build it once.

If you are asked to build something with no spec, **write the spec first and say
so**. If a spec exists but is not approved, say that too rather than
implementing against an unagreed contract.

---

## Before you write code

| If you are about to… | First… |
|---|---|
| Build any feature | Write or find `<package>/SPEC.md`. See §"Spec-driven development" above. |
| Add a dependency | Read §"Adding a dependency" below. This has a hard process. |
| Change a port's shape, a module boundary, or a layering rule | Update [docs/architecture.md](docs/architecture.md) and get it agreed |
| Touch anything structural | Read [docs/architecture.md](docs/architecture.md) |
| Ask "what are we building next" | Read [docs/roadmap.md](docs/roadmap.md) |

**Do not create a new module unless its first real code lands in the same
change.** An empty module is a release obligation with no user.

---

## Adding a dependency

Warren's credibility rests on its `go.mod` being defensible. There is a process
and it is not optional:

1. **Read the repository and the documentation.** Not the README summary — check
   whether it is archived, when it last shipped, what it pulls in transitively,
   and whether its licence is Apache-2.0/MIT/BSD/ISC compatible. `gh api
   repos/<owner>/<repo>` gives stars, `pushed_at`, `archived`, and open issues in
   one call.
2. **Record what you found** in the spec that adopts it, with the observation
   date, and add it to the table in [docs/architecture.md §3](docs/architecture.md).
3. **Check placement.** Core: never. Driver module: only its own driver.
   Test-only: test files only.
4. **Wrap it** so it can be swapped.

**A package with no written audit does not go into a `go.mod`.** Star counts are
not evidence. "It is popular" is not evidence.

Two live examples of why this matters, both found during the initial audit:
`google/wire` is archived, and `git-chglog` is archived. Both are still widely
recommended in blog posts.

---

## Code conventions

### Naming

- **Never use `With` in a type name.** No `UserWithRelations`,
  `OrderWithItems`, `BusinessWithRelations`. Name the thing for what it *is* —
  `UserProfile`, `OrderDetail`, `EnrichedUser` — or model the relation
  explicitly. `With` in a type name describes a query's shape, not a concept,
  and it multiplies without limit.
  (`With` as a *function* prefix for options — `WithTimeout(d)` — is the
  standard Go functional-options idiom and is fine.)
- Interfaces are named for behaviour, not for `-er` reflexively: `Repository`,
  `Publisher`, `UnitOfWork`.
- No stuttering: `broker.Publisher`, never `broker.BrokerPublisher`.
- Exported identifiers read plainly. The project's metaphor is not a tax on the
  API: `warren.Module`, not `den.NewBurrow()`.

### Errors

- Return `warren/errors` semantic errors (`NotFound`, `Conflict`, `Invalid`,
  `PermissionDenied`, `Internal`). Domain code knows nothing about HTTP 404.
- Wrap with `%w` and add context, never with `%v`.
- **Error messages tell the user how to fix it.** This is a feature, not polish.
  "provider not found" is a bug in the error message; it must name what was
  missing, who requested it, and a copy-pasteable fix.

### General

- `context.Context` is the first parameter, always. Never stored in a struct.
- Accept interfaces, return concrete types — **except** constructors that return
  a port, which is Warren's whole pattern.
- No `init()`. No package-level mutable state. No `panic` in library code.
- Reflection belongs at boot, not in the request path.
- Exported identifiers have doc comments starting with the identifier's name.
- Formatting is `make fmt`'s job. Never hand-format; never argue about it.

---

## Commits

Conventional Commits, **scope is the module path**:

```
feat(broker/kafka): drain in-flight messages before revoking partitions
fix(di): name the requesting file in missing-provider errors
docs(architecture): document the drain sequence
```

- Imperative mood, no trailing period, ≤72 characters.
- Breaking: `!` after scope **and** a `BREAKING CHANGE:` footer stating the
  migration.
- Every commit builds and passes its module's tests.

**Do not commit unless the human asked you to.**

---

## Testing

- Unit tests: **no Docker, no network, no sleeps.** Anything else goes behind
  `//go:build integration`.
- `t.Parallel()` and table-driven subtests named for behaviour.
- **Every generator needs a golden-file test.** Templates break silently
  otherwise.
- **Every port change needs the contract suite updated first**, then the drivers.
- A bug fix starts with a failing test.
- Do not add a mocking framework. Hand-written fakes live in `warren/testing`.

---

## Commands

```bash
make ci               # exactly what CI runs — the one that matters
make check            # everything except integration tests
make test             # unit tests, race, shuffled
make lint             # golangci-lint across all modules
make lint-modules     # the invariants above
make fmt              # apply formatters
make work             # go.work for cross-module edits (git-ignored)
```

This is a **multi-module repository**: `go test ./...` does **not** cross module
boundaries. Use the make targets.

---

## Mistakes agents make in this repo

Named specifically, because generic advice does not prevent them:

1. **Adding a dependency to the core module** because it seemed small. The core
   is stdlib-only. Split into port + submodule instead.
2. **Running `go test ./...` from the root** and reporting the suite passed. It
   tested one module. Use `make test`.
3. **Writing a new file that duplicates what a generator already produces.** Run
   the generator, then read its output.
4. **Hand-editing `module.go` after a generator already wired it.** Check first.
5. **Adding a CLI command without its skill.** The command is not done until the
   skill exists.
6. **Assuming a package is healthy because it is well known.** Verify: `wire`
   and `git-chglog` are both archived.
7. **Naming a type `SomethingWithSomething`.** See Naming above.
8. **Marking work complete with an unverified claim.** Run the command and paste
   what it printed. "Should work" is not a result.
9. **Writing code for a feature that has no approved spec.** The spec is where a
   feature is made small enough to finish. Write `<package>/SPEC.md` first.
10. **Letting the spec and the code drift apart.** If the implementation had to
    differ, the spec is corrected in the same pull request — not later.
11. **Proposing a spike or a prototype.** Research it, put the options to the
    human, and agree the decision. Then build it once.

---

## When you are unsure

- **The decision is probably recorded.** Check [docs/architecture.md](docs/architecture.md)
  and the feature's spec before re-deriving it, and before re-opening it.
- **If a recorded decision is wrong, say so and propose changing it.** Do not
  quietly work around it — a worked-around rule is how the layering decayed in
  the first place, which is the problem Warren exists to solve.
- **If a rule here blocks a genuinely correct change**, raise it rather than
  disabling the check. `//nolint` without a reviewed reason is itself a lint
  failure.
- **Ask rather than guess on anything structural.** Module boundaries, port
  shapes, and public API are the human's call, not yours to discover.
