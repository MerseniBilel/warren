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

`*dig.Container`, `*chi.Mux`, `*pgx.Conn`, `*kgo.Client` — none of these may
appear in any Warren exported function, method, struct field, or error type.
This is what makes every driver swappable.

Only `warren/di` may import `go.uber.org/dig`
([ADR-0001](docs/adr/0001-dependency-injection.md)). Enforced by `depguard` and
by `make lint-modules`.

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

Warren tracks the current Go major ([ADR-0007](docs/adr/0007-go-version-policy.md)).
No `toolchain` directive in any module.

---

## Before you write code

| If you are about to… | First… |
|---|---|
| Add a dependency | Read §"Adding a dependency" below. This has a hard process. |
| Change a port's shape, a module boundary, or a layering rule | Write an ADR ([docs/adr/README.md](docs/adr/README.md)) |
| Add a CLI command | Read [ADR-0008](docs/adr/0008-agent-integration.md) — a command needs a skill |
| Touch anything structural | Read [docs/architecture.md](docs/architecture.md) |
| Write a test | Read [docs/testing.md](docs/testing.md) |

**Do not create a new module unless its first real code lands in the same
change.** An empty module is a release obligation with no user.

---

## Adding a dependency

Warren's credibility rests on its `go.mod` being defensible. There is a process
and it is not optional:

1. **Read the repository and the documentation.** Not the README summary — check
   whether it is archived, when it last shipped, what it pulls in transitively,
   and whether its licence is Apache-2.0/MIT/BSD/ISC compatible.
2. **Add a row to the audit table** in [docs/dependencies.md](docs/dependencies.md)
   recording what you found, with the observation date.
3. **Check placement.** Core: never. Driver module: only its own driver. Test-only:
   test files only.
4. **Wrap it** if it will be long-lived, so it can be swapped.

**A package with no audit row does not go into a `go.mod`.** Star counts are not
evidence. "It is popular" is not evidence.

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
- Exported identifiers read plainly. Per PRD §2.3, the project's metaphor is not
  a tax on the API: `warren.Module`, not `den.NewBurrow()`.

### Errors

- Return `warren/errors` semantic errors (`NotFound`, `Conflict`, `Invalid`,
  `PermissionDenied`, `Internal`). Domain code knows nothing about HTTP 404.
- Wrap with `%w` and add context, never with `%v`.
- **Error messages tell the user how to fix it.** PRD §8 makes this a feature.
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

Conventional Commits, **scope is the module path**
([ADR-0005](docs/adr/0005-commits-and-changelog.md)):

```
feat(broker/kafka): drain in-flight messages before revoking partitions
fix(di): name the requesting file in missing-provider errors
docs(architecture): document the drain sequence
```

- Imperative mood, no trailing period, ≤72 characters.
- `feat`/`fix`/`perf` **require** a changelog fragment (`make changelog`).
  Nothing else may have one. CI checks both directions.
- Breaking: `!` after scope **and** a `BREAKING CHANGE:` footer stating the
  migration.
- Every commit builds and passes its module's tests.

**Do not commit unless the human asked you to.** Scope list and full rules are
in [CONTRIBUTING.md](CONTRIBUTING.md).

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
make changelog        # add a changelog fragment
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
5. **Adding a CLI command without its skill.** The command is not done
   ([ADR-0008](docs/adr/0008-agent-integration.md)).
6. **Assuming a package is healthy because it is well known.** Verify: `wire`
   and `git-chglog` are both archived.
7. **Naming a type `SomethingWithSomething`.** See Naming above.
8. **Marking work complete with an unverified claim.** Run the command and paste
   what it printed. "Should work" is not a result.

---

## When you are unsure

- **The decision is probably recorded.** Check [docs/adr/](docs/adr/) before
  re-deriving it, and before re-opening it.
- **If an ADR is wrong, say so and propose superseding it.** Do not quietly work
  around it — a worked-around rule is how the layering decayed in the first
  place, which is the problem Warren exists to solve.
- **If a rule here blocks a genuinely correct change**, raise it rather than
  disabling the check. `//nolint` without a reviewed reason is itself a lint
  failure.
