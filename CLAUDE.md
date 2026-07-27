# CLAUDE.md

**Read [AGENT.md](AGENT.md) first — it is the canonical instruction file for
this repository.** It holds the invariants, code conventions, commit rules, and
the dependency-adoption process, and it applies in full here. This file adds
only what is specific to Claude Code.

---

## Quick orientation

Warren is a DDD-first framework and CLI for Go. Multi-module repository, Go 1.26,
Apache-2.0, module path `github.com/MerseniBilel/warren`.

Nothing is implemented yet. The repository currently holds the foundation:
architecture decisions, quality gates, and conventions. Framework code starts at
v0.1 (PRD §10).

The five invariants that fail CI, in short — full versions in AGENT.md:

1. Core module: **standard library only**, permanently.
2. No driver type (`dig`, `chi`, `pgx`, `kgo`) in a public signature.
3. `domain` imports nothing from the other layers.
4. Handlers import no transport package.
5. No committed `replace` directive.

---

## Working here

**Use the make targets, not raw `go` commands.** This is a multi-module repo, so
`go test ./...` silently tests one module and reports success. `make test`
iterates all of them. Same for `go vet` (`make vet`) and lint (`make lint`).

**`make ci` is the gate.** If it passes locally it passes in CI. When it does
not, that is a bug in the Makefile worth reporting.

**Verify before claiming.** Run the command and quote what it printed. If
`golangci-lint` is not installed, say so rather than asserting the code lints
cleanly — `make lint-config` falls back to schema validation and tells you
which path it took.

**Prefer reading an ADR to re-deriving a decision.** [docs/adr/](docs/adr/) is
short and indexed; the reasoning is there, including what was rejected and why.

---

## Research before choosing a package

This repository has a standing rule that **no dependency is adopted until
someone has read its repository and documentation and written down what they
found** — see [docs/dependencies.md](docs/dependencies.md).

When evaluating a package, actually check it. `gh api repos/<owner>/<repo>` gives
stars, `pushed_at`, `archived`, and open-issue count in one call, and `gh api
repos/<owner>/<repo>/releases/latest` gives the real release date. The initial
audit found two widely-recommended packages archived (`google/wire`,
`git-chglog`); neither README said so.

Then add the row to `docs/dependencies.md`. A package with no audit row does not
go into a `go.mod`.

---

## Skills

Every Warren CLI command ships a skill under `skills/`
([ADR-0008](docs/adr/0008-agent-integration.md)). When you add or change a
command, update its skill in the same change — the skill is part of the
command's definition of done, alongside its tests.

Skill authoring format and the required sections are in
[docs/agent-integration.md](docs/agent-integration.md).

---

## Things to avoid here

- **Do not commit or push unless asked.** Conventions are enforced
  (`scripts/check-commits.sh`), so a casual commit will likely fail CI anyway.
- **Do not add a mocking framework**, a logging library, or an assertion library
  to the core. Core is stdlib-only.
- **Do not name a type `XWithY`** — see AGENT.md § Naming.
- **Do not disable a linter to make a change pass.** `//nolint` needs a specific
  linter and a stated reason, or `nolintlint` fails it.
- **Do not create empty modules** ahead of the code that will fill them.

---

## Reference

| Question | File |
|---|---|
| What are the rules? | [AGENT.md](AGENT.md) |
| Why is it built this way? | [docs/architecture.md](docs/architecture.md) |
| Why this dependency? | [docs/dependencies.md](docs/dependencies.md) |
| Why was this decided? | [docs/adr/](docs/adr/) |
| How do I test this? | [docs/testing.md](docs/testing.md) |
| How do I contribute? | [CONTRIBUTING.md](CONTRIBUTING.md) |
| What is the product? | [prd.md](prd.md) |
