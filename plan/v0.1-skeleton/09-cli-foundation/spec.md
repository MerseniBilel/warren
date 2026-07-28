# Spec: CLI foundation

| | |
|---|---|
| **Module** | `warren/cli` |
| **Milestone** | v0.1 |
| **Status** | Draft |
| **Depends on** | [01-errors](../01-errors/spec.md) |
| **Blocks** | [10-cli-new](../10-cli-new/spec.md), [11-cli-generate-module](../11-cli-generate-module/spec.md), and every generator thereafter |
| **PRD** | §7, §7.6, §8, §9 |
| **ADRs** | [ADR-0008](../../../docs/adr/0008-agent-integration.md) |
| **Date** | 2026-07-28 |

---

## 1. Problem

PRD §7 defines roughly twenty commands. Written one at a time, they will
disagree with each other: different flag names, different conflict handling,
different diff output, half of them without golden tests, and — per ADR-0008's
warning — three skills across twenty commands.

This spec builds the shared machinery once: command registration, the template
engine, the file writer that implements PRD §7.6's five generator rules, the
AST editor that wires generated code into `module.go`, the golden-file harness,
and skill generation from Cobra metadata.

**Nothing here is user-visible on its own.** It is the foundation two generators
in v0.1 and eighteen later ones stand on, and building it after three
generators exist means rewriting three generators.

## 2. Goals

1. **PRD §7.6's five rules hold for every generator by construction**, not by
   each author remembering them: idempotent, surgical AST wiring, never
   overwrite silently, generated code is owned by the user, every template is
   forkable.
2. **`--dry-run` and `--force` on every generator**, implemented once.
3. **A golden-file harness that makes a generator's test a three-line file**, so
   PRD §9's "golden-file tests for every CLI generator" is the path of least
   resistance rather than a discipline.
4. **Skills generated from Cobra metadata** with a drift check, so ADR-0008's
   rule is mechanically enforced (`make skills-gen` / `make skills-check`
   already exist in the [Makefile](../../../Makefile) and this is what they
   call).
5. **Errors that name the fix** (PRD §8), uniformly.

## 3. Non-goals

- **No commands here.** This spec ships the framework and a `warren version`
  as its smoke test. Commands are separate specs with separate skills.
- **No `warren dev` / hot reload.** v0.2 at the earliest; it is a file-watching
  product of its own and PRD §12 names scope explosion as the top risk.
- **No plugin system.** A third party extending the CLI is a v1.0 conversation.
- **No `templates eject` at v0.1.** PRD §7.6 rule 5 requires it eventually; the
  template resolution order below makes it a small addition later.

## 4. Public API

The CLI is a program, not a library, so most of this is internal. What is
exported is what generators are written against:

```go
package gen // warren/cli/gen

// Generator is one code-producing command's engine. Every generator implements
// this, and the runner supplies dry-run, conflict handling, and formatting.
type Generator interface {
    // Plan computes every file this generator would write or modify, without
    // touching the filesystem. Dry-run is Plan plus a renderer, which is why
    // dry-run cannot drift from the real thing.
    Plan(ctx context.Context, in Input) (Plan, error)
}

type Plan struct {
    Files []FileOp
    Edits []SourceEdit // AST edits to existing files
}

type FileOp struct {
    Path     string
    Contents []byte
    Mode     WriteMode // Create, Skip (exists and identical), Conflict
}

// SourceEdit is a structured change to an existing Go file, applied through
// go/ast. PRD §7.6 rule 2: wiring is an AST edit, never a marker-comment hack.
type SourceEdit struct {
    Path        string
    Description string // human-readable, shown in --dry-run
    Apply       func(*ast.File) error
}

// Runner executes a Plan. One implementation, so every generator handles
// conflicts, dry-run, and formatting identically.
type Runner struct { /* unexported */ }

func (r *Runner) Execute(ctx context.Context, p Plan, opts Options) (Result, error)

type Options struct {
    DryRun bool
    Force  bool
    Root   string
}

// Templates resolves a named template, checking the project's own override
// directory before the embedded default. This is what makes PRD §7.6 rule 5
// (fork any template) a lookup order rather than a feature.
type Templates struct { /* unexported */ }

func NewTemplates(embedded fs.FS, projectRoot string) *Templates
func (t *Templates) Render(name string, data any) ([]byte, error)
```

```go
package project // warren/cli/project

// Project is a loaded warren.yaml plus the facts derived from the filesystem.
// Every command that runs inside a project starts here.
type Project struct {
    Root       string
    ModulePath string   // from go.mod
    Layout     string   // modular-monolith | microservice | library
    Modules    []string // discovered module directories
    Config     Config   // warren.yaml
}

func Load(dir string) (*Project, error) // walks up to find warren.yaml
```

## 5. Behaviour

### The five generator rules, implemented once

| PRD §7.6 rule | How the foundation holds it |
|---|---|
| 1. Idempotent | `Plan` compares contents against what exists. Identical → `Skip`. Re-running prints "no changes" and exits 0 |
| 2. Surgical wiring | `SourceEdit` operates on `go/ast` and reprints with `go/format`. There is no marker-comment API to reach for |
| 3. Never overwrite silently | `Conflict` is the default for a differing existing file. Without `--force` the command exits non-zero with a diff |
| 4. Generated code is owned | Everything is written once and committed. No `.gen.go`, no regeneration on build, no "do not edit" headers except on protobuf output |
| 5. Templates are forkable | `Templates.Render` checks `<root>/.warren/templates/` before the embedded FS |

### Other behaviour

- **`--dry-run` prints a unified diff** for new files and for AST edits, and
  exits 0. It renders the same `Plan` the real run executes — a dry-run that
  builds its output separately is a dry-run that lies.
- **Generated Go is formatted with `go/format`** before writing, so template
  whitespace is never a review comment.
- **Nothing is written until every file in the `Plan` succeeds.** A generator
  that fails halfway leaves a project that neither builds nor regenerates.
  Writes go to temporaries and are renamed.
- **A command that needs a project fails immediately outside one**, naming the
  directory it searched and suggesting `warren new`.
- **Exit codes**: 0 success, 1 user error, 2 internal error. `warren lint arch`
  (v0.4) depends on this being conventional.
- **Colour is disabled** when stdout is not a TTY or `NO_COLOR` is set.
- **`forbidigo` allows `fmt.Print` under `cli/`** — the exclusion is already in
  [`.golangci.yml`](../../../.golangci.yml). Writing to stdout is this module's
  job; it is a program, not a library.

### Skill generation

Per [ADR-0008](../../../docs/adr/0008-agent-integration.md) and
[docs/agent-integration.md](../../../docs/agent-integration.md):

- `make skills-gen` walks the Cobra command tree and regenerates the sections
  marked `GENERATED` in each `skills/warren-<command>/SKILL.md`.
- `make skills-check` regenerates into a temporary directory and diffs. Drift
  fails CI.
- Hand-written sections — when to use, when *not* to use, what it writes, what
  it wires, failure modes — are never touched by the generator.
- **A command with no skill directory fails `skills-check`.** That is the
  mechanism behind "the skill is part of the command's definition of done";
  without it the rule is an intention.

## 6. Errors

| Condition | Code | Message |
|---|---|---|
| Not inside a Warren project | `CodeFailedPrecondition` | The directory searched, that it walked up to the filesystem root, and `warren new <name>` |
| File exists with different contents | `CodeConflict` | The path, a diff, and that `--force` overwrites — never overwriting on its own |
| AST edit target not found | `CodeInvalid` | The file, what was being looked for (`func Module()`), and the manual edit to make instead |
| `warren.yaml` malformed | `CodeInvalid` | Path, line, column, and the parser's message |
| Template render failed | `CodeInternal` | Template name, whether it resolved to a project override or the embedded default, and the underlying error |
| Go module path cannot be determined | `CodeFailedPrecondition` | That `go.mod` is missing or unreadable, and `go mod init <path>` |
| Generated code does not parse | `CodeInternal` | The template, the offending line, and that this is a Warren bug with the issue-report line to run — a generator emitting invalid Go is never the user's fault |

## 7. Configuration

`warren.yaml`, read by `project.Load`:

```yaml
version: 1
module: github.com/acme/myapp
layout: modular-monolith        # microservice | library
paths:
  modules: internal/modules
generators:
  templates: .warren/templates  # project overrides; optional
arch:                           # consumed by `warren lint arch` in v0.4
  rules: default
```

The `arch` block is defined now and unused until v0.4. ADR-0004 requires
relaxations to be visible in `warren.yaml`, so the file's shape needs to
anticipate it rather than be restructured later.

## 8. Testing

- **Golden-file harness tests itself**: a fixture generator, an expected tree,
  and an assertion that `make golden-update` regenerates it byte-identically.
- **Idempotence**: every generator built on this runs twice; the second run
  writes nothing and exits 0. Enforced as a shared test helper so a new
  generator gets the check for free.
- **Conflict handling**: existing-identical → skip; existing-different → exit
  non-zero with a diff; with `--force` → overwrite.
- **Atomicity**: a `Plan` whose third file fails leaves the first two unwritten.
- **AST editing**: a `module.go` with existing providers gains one in the right
  place; running twice does not duplicate it; a malformed `module.go` produces
  the §6 message rather than a corrupted file.
- **Template override**: a project template shadows the embedded one; a missing
  override falls back cleanly.
- **Cross-platform**: path separators and line endings on Windows and macOS.
  The CI matrix already runs the core module on all three
  ([ci.yml](../../../.github/workflows/ci.yml)) for exactly this reason.
- **Skill drift**: adding a flag to a command without regenerating fails
  `skills-check`. Tested by doing it.

## 9. Invariants touched

- **Invariant 1** — `warren/cli` is a submodule and may depend on cobra. The
  core module must not depend on `warren/cli`, which is the natural direction
  anyway.
- **Invariant 2** — no cobra type in any exported signature, so the generator
  API is not welded to a CLI library.

## 10. Definition of done

- [ ] Public API matches §4
- [ ] All five PRD §7.6 rules covered by tests, named for the rule they enforce
- [ ] Golden harness in use by the fixture generator
- [ ] `make skills-gen` and `make skills-check` work end to end
- [ ] `warren version` builds, runs, and has a skill
- [ ] Tests green on Linux, macOS, and Windows
- [ ] cobra audit row confirmed current in `docs/dependencies.md`
- [ ] `make ci` green
- [ ] `docs/` page: writing a generator, including the five rules
- [ ] Changelog fragment

## 11. Open questions

1. **Does `SourceEdit.Apply` taking a `*ast.File` closure make edits testable
   enough?** A declarative edit description would be inspectable and printable;
   a closure is far easier to write. Leaning closure plus a mandatory
   `Description` — but that means `--dry-run` shows prose for AST edits and a
   real diff for files, which is inconsistent. Consider rendering the edit by
   applying it to a copy and diffing the printed source.
2. **Should `warren.yaml` be required?** A project without one cannot be
   located, and requiring it means `warren new` must always write it. Leaning
   required — the alternative is inferring the project root from `go.mod`, which
   is ambiguous in a multi-module repo, the exact situation Warren encourages.
3. **Where do embedded templates live** — one `embed.FS` per command or one for
   the module? Per-command keeps a generator self-contained; shared keeps
   partials (licence headers, package docs) in one place. Probably one FS with a
   directory per command.
