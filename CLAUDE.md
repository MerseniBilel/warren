# CLAUDE.md

**Read [AGENT.md](AGENT.md) first — it is the canonical instruction file for
this repository.** It holds the invariants, code conventions, commit rules, and
the dependency-adoption process, and it applies in full here. This file adds
only what is specific to Claude Code.

---

## Quick orientation

Warren is a DDD-first framework and CLI for Go. Multi-module repository, Go 1.26,
Apache-2.0, module path `github.com/MerseniBilel/warren`.

Nothing is implemented yet. The repository holds the product definition, the
architecture, the roadmap, and the quality gates.

| Question | File |
|---|---|
| What are we building, and how does it fit together? | [docs/architecture.md](docs/architecture.md) |
| What does the module map look like? | [docs/assets/architecture.png](docs/assets/architecture.png) · source: [docs/assets/architecture.puml](docs/assets/architecture.puml) |
| What happens at boot and at shutdown? | [docs/assets/lifecycle.png](docs/assets/lifecycle.png) |
| What is being built next? | [docs/roadmap.md](docs/roadmap.md) |
| What are the rules? | [AGENT.md](AGENT.md) |

The five invariants that fail CI, in short — full versions in AGENT.md:

1. Core module: **standard library only**, permanently.
2. No driver type (`chi`, `pgx`, `kgo`) in a public signature, and **no
   third-party DI container anywhere** — Warren writes its own.
3. `domain` imports nothing from the other layers.
4. Handlers import no transport package.
5. No committed `replace` directive.

---

## Working here

**Write the spec before the feature.** Every feature gets
`specs/<nn>-<feature>.md`, written and approved *before* any code, and corrected
in the same pull request whenever the implementation diverges. If you are asked
to build something that has no spec, write the spec first and say so. See
[AGENT.md § Spec-driven development](AGENT.md).

**Do not propose a spike or a prototype.** Decisions here are made by research
and argument: read the evidence, put the options and a recommendation to the
human, agree it, then build it once. Throwaway code is not how this project
decides things.

**Bring structural choices to the human.** Module boundaries, port shapes, and
public API are decisions to be taken together, not discovered by writing code
and seeing what happens.

**Use the make targets, not raw `go` commands.** This is a multi-module repo, so
`go test ./...` silently tests one module and reports success. `make test`
iterates all of them. Same for `go vet` (`make vet`) and lint (`make lint`).

**`make ci` is the gate.** If it passes locally it passes in CI. When it does
not, that is a bug in the Makefile worth reporting.

**Verify before claiming.** Run the command and quote what it printed. If
`golangci-lint` is not installed, say so rather than asserting the code lints
cleanly — `make lint-config` falls back to schema validation and tells you which
path it took.

---

## Diagrams

`docs/assets/architecture.puml` holds both diagrams and is the source of truth.
Regenerate after any change to it, and commit the images:

```bash
java -jar ~/.local/bin/plantuml.jar -tpng -o . docs/assets/architecture.puml
java -jar ~/.local/bin/plantuml.jar -tsvg -o . docs/assets/architecture.puml
```

The `plantuml` wrapper on this machine fails with an unbound-variable error when
given no extra arguments; call the jar directly, as above.

---

## Research before choosing a package

**No dependency is adopted until someone has read its repository and
documentation and written down what they found.**

`gh api repos/<owner>/<repo>` gives stars, `pushed_at`, `archived`, and the open
issue count in one call; `gh api repos/<owner>/<repo>/releases/latest` gives the
real release date. The initial audit found two widely-recommended packages
archived — `google/wire` and `git-chglog` — and neither README said so.

Record what you found in the spec that adopts the package, and add it to the
table in [docs/architecture.md §3](docs/architecture.md).

---

## Things to avoid here

- **Do not commit or push unless asked.**
- **Do not add a mocking framework**, a logging library, a DI library, or an
  assertion library to the core. Core is stdlib-only.
- **Do not name a type `XWithY`** — see AGENT.md § Naming.
- **Do not disable a linter to make a change pass.** `//nolint` needs a specific
  linter and a stated reason, or `nolintlint` fails it.
- **Do not create empty modules** ahead of the code that will fill them.
