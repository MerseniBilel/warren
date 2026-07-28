# Spec: `warren new`

| | |
|---|---|
| **Module** | `warren/cli` |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [09-cli-foundation](../09-cli-foundation/spec.md), [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md), [08-transport-http](../08-transport-http/spec.md), [05-config](../05-config/spec.md) |
| **Blocks** | Nothing — but it is the milestone's front door |
| **PRD** | §5.1, §7.1, §7.6, §8 |
| **ADRs** | [ADR-0002](../../../docs/adr/0002-http-router-port.md), [ADR-0008](../../../docs/adr/0008-agent-integration.md) |
| **Date** | 2026-07-28 |

---

## 1. Problem

This is the first thing anyone runs, and it is where the framework is judged.
PRD §8 sets the bar: **zero to a running service in under two minutes**, and
zero to a first endpoint with a passing test in under ten.

It is also where PRD §1.3's rule is tested hardest: *"if a developer cannot read
the generated code and understand it in five minutes, the feature is wrong."*
The generated `main.go` is the framework's argument. A reader who cannot tell
what starts the server will not use Warren, however good the container is.

## 2. Goals

1. **Scaffold a project that builds and runs immediately** — `warren new x && cd
   x && go run ./cmd/api` serves a request, with no editing.
2. **Under two minutes including `go mod download`**, on a cold module cache.
3. **The generated layout is PRD §5.1's**, exactly — so every later generator
   knows where things go.
4. **A reader understands `main.go` in five minutes.** No hidden control flow, no
   framework-owned file the user is forbidden to touch.
5. **The router is the user's choice** at generation time, defaulting to chi.

## 3. Non-goals

- **v0.1 supports `--layout modular-monolith` only.** `microservice` and
  `library` are v0.2; shipping three layouts that all need updating with every
  generator is how the CLI stops being maintainable.
- **No `--db`, no `--broker` at v0.1.** Persistence is v0.2 and brokers are v0.3.
  The flags are reserved and rejected with a message naming the milestone, not
  silently ignored.
- **No `--preset`** (PRD §7.1). Org presets from a git repo are v0.5.
- **No `--interactive` at v0.1.** Flags first; the prompt flow is a wrapper over
  a settled flag set, and building it first means rebuilding it.
- **No Docker or k8s manifests.** `--with docker` is v0.2.

## 4. Public API

```bash
warren new <name> [flags]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--module` | string | inferred from `<name>` | Go module path, e.g. `github.com/acme/myapp` |
| `--layout` | string | `modular-monolith` | v0.1 accepts this value only |
| `--transport` | string | `http` | v0.1 accepts this value only |
| `--router` | string | `chi` | `chi` or `stdlib` (ADR-0002) |
| `--dir` | string | `./<name>` | Target directory |
| `--dry-run` | bool | `false` | Print the tree and file contents; write nothing |
| `--force` | bool | `false` | Write into a non-empty directory |

## 5. Behaviour

Generated tree — PRD §5.1, trimmed to what v0.1 can fill honestly:

```
myapp/
├── cmd/api/main.go              # composes modules; the file to read first
├── internal/
│   ├── modules/
│   │   └── health/              # one real module, so the layout is shown, not described
│   │       ├── application/
│   │       ├── interfaces/
│   │       └── module.go
│   ├── shared/platform/
│   └── config/config.go
├── test/                        # integration tests (testcontainers arrive in v0.2)
├── warren.yaml
├── go.mod
├── Makefile
├── .gitignore
└── README.md
```

- **No empty directories.** A scaffold of empty folders teaches nothing and
  makes the project look larger than it is. `domain/` and `infrastructure/`
  appear when `warren g module` creates a module that needs them.
- **The `health` module is real and works.** `GET /healthz` returns 200 through
  the full stack: module → controller → handler → HTTP adapter. It is the
  smallest complete example of every concept, and it is what the user reads to
  learn the pattern.
- **`main.go` is the shape of PRD §14.3** and handles the error from `Run`.
  Every line is something the user can delete.
- **`go.mod` is written, not `go mod init`-ed**, then `go mod tidy` runs. The
  dependency list is short by design and worth looking at: with `--router
  stdlib` a v0.1 project has **no third-party runtime dependency at all**, and
  that is the claim in [architecture.md](../../../docs/architecture.md) made
  checkable.
- **`git init` runs** unless `--no-git`, with an initial commit. A scaffold that
  is not a git repo makes the user's first action `git init` before they can
  see a diff of their own work.
- **The generated `README.md` states what to run next**, in three commands.
- **The generated `Makefile` has `run`, `test`, and `lint`.** Not a copy of
  Warren's own — the user's project is not a multi-module framework repository.
- **A non-empty target directory is refused** without `--force`, listing what is
  there.

## 6. Errors

| Condition | Code | Message |
|---|---|---|
| Target directory exists and is not empty | `CodeConflict` | The path, the first few entries found, and that `--force` writes in anyway |
| Module path not inferrable and `--module` absent | `CodeInvalid` | That `<name>` is not a valid module path, with `--module github.com/you/<name>` as the fix |
| Invalid module path | `CodeInvalid` | What is wrong with it and a valid example |
| `--layout microservice` | `CodeUnimplemented` | That v0.1 supports `modular-monolith` only, and which milestone adds it |
| `--db` / `--broker` given | `CodeUnimplemented` | The milestone that adds it, so the flag reads as "not yet" rather than "not planned" |
| `--router` not `chi` or `stdlib` | `CodeInvalid` | The two valid values, and that Echo and Gin arrive in v0.2 |
| `go mod tidy` failed | `CodeUnavailable` | The command's own output, whether it looks like a network failure, and that the project is written and can be tidied by hand |
| Not writable | `CodeInvalid` | The path and the permission problem |

## 7. Configuration

Writes `warren.yaml` per
[09-cli-foundation §7](../09-cli-foundation/spec.md). Reads nothing.

## 8. Testing

- **Golden-file test of the entire generated tree**, for both routers. PRD §9
  makes this mandatory; the tree is the product.
- **The generated project builds**: an integration test running `go build ./...`
  in the output. Golden files prove the bytes are stable; only a build proves
  they are correct.
- **The generated project's own tests pass** — `go test ./...` inside the
  output.
- **`GET /healthz` returns 200** from the generated binary, started and stopped
  by the test.
- **`--router stdlib` yields a `go.mod` with no third-party requires.** A direct
  assertion on the file, because it is a headline claim.
- **`--dry-run` writes nothing**, verified by hashing the target directory
  before and after.
- **Cross-platform**: path separators and line endings on all three OSes.
- **Timing**: a recorded benchmark of scaffold-to-first-response, so PRD §8's
  two-minute target is measured on every CI run rather than asserted once.

## 9. Invariants touched

- **Invariant 1** — the generated project's `go.mod` is where the zero-dependency
  claim becomes visible to a user. The stdlib-router test above is the proof.
- **Invariant 3** — the generated layout *is* the dependency rule. Getting the
  directories wrong here means `warren lint arch` (v0.4) validates the wrong
  thing.

## 10. Definition of done

- [ ] Flags match §4
- [ ] Golden-file test of the full tree, both routers
- [ ] Generated project builds, tests pass, and serves `/healthz`
- [ ] Timing benchmark against the two-minute target, recorded in the docs
- [ ] Tests green on Linux, macOS, and Windows
- [ ] **Skill at `skills/warren-new/SKILL.md`** ([ADR-0008](../../../docs/adr/0008-agent-integration.md)) — the command is not done without it
- [ ] `make skills-check` green
- [ ] `make ci` green
- [ ] `docs/` tutorial: the zero-to-endpoint narrative, using this command
- [ ] Changelog fragment

## 11. Open questions

1. **Is the `health` module the right first example?** It is trivially small and
   genuinely useful, and it does not show a domain layer — which is Warren's
   whole thesis. A `user` module would show more and would be code the user
   deletes. Leaning `health`, plus a tutorial that immediately runs
   `warren g module user`.
2. **Does `warren new` run `go mod tidy` at all?** It is the slowest step, the
   only network dependency, and the difference between "it runs" and "it almost
   runs". Leaning yes, with the failure handled per §6 so an offline user still
   gets a usable project.
3. **`--dir` versus positional path.** `warren new ./services/api` is natural and
   makes the module name ambiguous. Keeping both is how a CLI grows two ways to
   do one thing.
4. **Should the generated project pin a Warren version?** Yes for
   reproducibility; it also means every scaffold is stale the day after a
   release, and `warren upgrade` (v0.5) has to handle it either way.
