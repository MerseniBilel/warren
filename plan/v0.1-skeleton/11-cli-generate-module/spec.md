# Spec: `warren generate module`

| | |
|---|---|
| **Module** | `warren/cli` |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [09-cli-foundation](../09-cli-foundation/spec.md), [10-cli-new](../10-cli-new/spec.md), [06-module-and-bootstrap](../06-module-and-bootstrap/spec.md) |
| **Blocks** | Every v0.2 generator — they all wire into a module this creates |
| **PRD** | §5.1, §7.2, §7.6, §14.1 |
| **ADRs** | [ADR-0008](../../../docs/adr/0008-agent-integration.md) |
| **Date** | 2026-07-28 |

---

## 1. Problem

Adding a bounded context by hand means creating four layer directories, writing
`module.go`, and registering it in `main.go` — and getting the layering right
every time, six months in, when nobody remembers the convention. That is the
erosion PRD §1.1 describes.

This command is also where PRD §7.6 rule 2 gets its first real test:
**registering into an existing file is an AST edit, not a marker-comment hack.**
Every v0.2 generator (`g entity`, `g command`, `g repository`) wires into the
`module.go` this creates, so if the AST editing is wrong here, it is wrong
eighteen times.

## 2. Goals

1. **Create a module in PRD §5.1's layout** and register it in `cmd/api/main.go`
   with a `go/ast` edit.
2. **The result builds and its test passes** immediately, with no editing.
3. **Idempotent** — re-running is a no-op, and it does not duplicate the
   registration.
4. **The generated `module.go` is PRD §14.1's shape**, so the illustrative code
   and the real output agree.

## 3. Non-goals

- **No entity, command, query, or repository generation.** Those are v0.2 and
  each is its own spec and skill. This creates the container they land in.
- **No `--layout simple`** (a module with no domain layer). PRD §12 names it as
  the mitigation for "DDD is a hard sell"; it needs the domain layer to exist
  first in order to be optional. v0.2.
- **No cross-module wiring.** A module does not import another module's
  internals — that restriction is what makes `extract module` possible (PRD
  §5.3) and it is not relaxed for convenience.
- **No removal.** `warren remove module` deletes user code; that needs a
  different level of care and is not v0.1.

## 4. Public API

```bash
warren generate module <name> [flags]     # aliases: warren g module, warren g mo
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--transport` | string | `http` | Transports to scaffold an interface layer for |
| `--dry-run` | bool | `false` | Print the diff; write nothing |
| `--force` | bool | `false` | Overwrite conflicting files |

## 5. Behaviour

Writes:

```
internal/modules/<name>/
├── domain/                       # entities, VOs, events, repository interfaces
│   └── doc.go                    # states the layering rule for this directory
├── application/
│   └── doc.go
├── infrastructure/
│   └── doc.go
├── interfaces/
│   └── http_controller.go        # only when --transport includes http
└── module.go
```

Edits:

```
cmd/api/main.go                   # adds <name>.Module() to warren.New(...)
```

- **`module.go` is PRD §14.1's shape**, with `Providers`, `Controllers`, and
  `Exports` present — `Providers` and `Exports` empty with a comment showing the
  form. An empty option with an example beats a missing one: the next generator
  needs the line to exist, and the user needs to see what goes in it.
- **Each `doc.go` states that directory's rule** — one sentence, in the package
  comment, where a developer is standing when they need it. Documentation that
  lives in the repository the developer is not reading does not prevent a
  layering violation.
- **The AST edit into `main.go`** adds the import and the `warren.New` argument,
  reprinted with `go/format`. If `warren.New(...)` cannot be found, the command
  fails with the §6 message and writes nothing — including no module directory,
  since a module that is not registered is worse than no module.
- **Idempotence**: existing files with identical contents are skipped; the
  `main.go` edit checks for an existing registration and does nothing if present.
  Running twice prints "no changes."
- **Name validation**: a valid Go identifier, lowercase, not a Go keyword, not
  already taken. The name becomes a package name, a directory, and a container
  scope, so a bad one fails in three places later.
- **The generated controller is complete and does nothing useful**: one route
  returning a stub, wired end to end. A generated file that does not compile
  because it is a template with holes in it is the worst possible first
  impression.
- **`--dry-run` prints the tree, every file's contents, and the `main.go` diff.**

## 6. Errors

| Condition | Code | Message |
|---|---|---|
| Not in a Warren project | `CodeFailedPrecondition` | The directory searched, and `warren new <name>` |
| Module already exists | `CodeConflict` | The path, that re-running is safe, and that `--force` overwrites |
| Invalid name | `CodeInvalid` | What is wrong (keyword, uppercase, punctuation) and a valid example |
| `warren.New(...)` not found in `main.go` | `CodeInvalid` | The file searched, what was looked for, and the exact line to paste in manually — since the user may have restructured `main.go`, which is their right (PRD §7.6 rule 4) |
| `main.go` does not parse | `CodeInvalid` | The parser's line and column. Warren does not edit a file it cannot parse |
| Generated code does not parse | `CodeInternal` | A Warren bug: the template name and the issue-report line |

**The `warren.New` not-found case is the interesting one.** Generated code is
owned by the user, so `main.go` will be edited, sometimes heavily. The command
must degrade to "here is the line to add" rather than either failing opaquely or
guessing.

## 7. Configuration

Reads `warren.yaml` for `paths.modules` and for a template override directory.
Writes nothing to it — a module is discovered from the filesystem, not from a
registry that can disagree with reality.

## 8. Testing

- **Golden-file test of the module tree**, with and without `--transport http`.
- **Golden-file test of the `main.go` diff**, from a pristine scaffold and from
  a `main.go` that already has two modules — the second is the case that breaks.
- **Idempotence**: run twice, assert no changes and no duplicate registration.
- **The generated module builds and its test passes**, as an integration test
  over a real scaffolded project.
- **The new route responds**, proving the module is genuinely registered rather
  than only present in the source.
- **`warren.New` not found**: a hand-restructured `main.go` produces the §6
  message, and **no files are written** — asserted by hashing the tree.
- **Malformed `main.go`** fails cleanly with no partial write.
- **Name validation table**: keywords, uppercase, hyphens, digits-first, empty,
  and a name colliding with an existing directory.
- **Cross-platform** on all three OSes.

## 9. Invariants touched

- **Invariant 3 (the dependency rule)** — this command creates the four layers.
  If the generated layout or the `doc.go` rules are wrong, every project built
  on Warren inherits the error, and `warren lint arch` (v0.4) will be validating
  a layout that was wrong from the start.

## 10. Definition of done

- [ ] Flags match §4
- [ ] Golden files for the tree and for both `main.go` diff cases
- [ ] Idempotence test green
- [ ] Generated module builds, tests pass, route responds
- [ ] Failure cases write nothing, verified by tree hashing
- [ ] Tests green on Linux, macOS, and Windows
- [ ] **Skill at `skills/warren-generate-module/SKILL.md`**, including the "do not hand-edit `module.go` afterwards" warning that [AGENT.md](../../../AGENT.md) lists as mistake 4
- [ ] `make skills-check` green
- [ ] `make ci` green
- [ ] `docs/` guide: adding a bounded context
- [ ] Changelog fragment

## 11. Open questions

1. **Should `doc.go` files be generated at all?** They keep directories
   non-empty and put the layering rule where it is needed. They are also four
   files nobody asked for, and Go developers may read them as clutter. Leaning
   yes — the rule needs to be somewhere the developer is standing.
2. **Does the module register in `cmd/api/main.go` only, or in every entrypoint?**
   v0.1 generates one. `cmd/worker` arrives in v0.3 and the answer must be
   "the ones the user chooses", which means a flag. Reserve `--entrypoint` now
   so it is not a breaking addition.
3. **Is `internal/modules/` the right path?** PRD §5.1 says so. `internal/`
   forbids importing across repository boundaries, which is right for a monolith
   and is exactly what `extract module` (v0.5) has to undo. Confirm the
   extraction story survives it before this path is baked into every project.
