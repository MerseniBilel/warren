# Contributing to Warren

Warren's central claim is that structure which is not enforced decays. That
applies to this repository first. Most rules here are executable — `make ci`
runs every gate CI runs, so a passing local run means a passing pull request.

---

## Getting set up

```bash
git clone https://github.com/MerseniBilel/warren
cd warren
make tools        # pinned golangci-lint, govulncheck, changie
make work         # generates go.work across all modules (git-ignored)
make ci           # everything CI runs
```

**Go 1.26 is required**, not merely supported. Warren tracks the current Go
major release ([ADR-0007](docs/adr/0007-go-version-policy.md)); there is no
compatibility path to older releases.

### Multi-module workspace

This repository holds many Go modules ([ADR-0003](docs/adr/0003-repo-layout.md)).
Two consequences bite immediately:

- **`go test ./...` does not cross module boundaries.** Use `make test`.
- **Never commit a `replace` directive.** It breaks `go get` for users, and it
  breaks it silently. `make work` generates `go.work` for local cross-module
  work; it is git-ignored. `make lint-modules` fails on a committed `replace`.

---

## Before you write code

**Find or write the spec.** Warren is built spec-first: every feature has a
`spec.md` under [plan/](plan/), and it is written and approved *before* the
code. The spec is where a feature is made small enough to finish, and where the
public API is agreed while changing it is still free.

```
plan/<milestone>/<nn>-<feature>/spec.md     # from plan/TEMPLATE.spec.md
```

Two rules carry the weight:

- **The spec's §4 Public API is the contract under review.** Write the Go.
  Reviewing prose and discovering the signatures at merge time is how a spec
  becomes theatre.
- **If the implementation diverges, correct the spec in the same pull request.**
  A spec that no longer describes the code is worse than no spec.

[plan/README.md](plan/README.md) has the index, the status vocabulary, and what
is being built now.

**Read [docs/architecture.md](docs/architecture.md).** Warren has a dependency
rule, and violating it fails the build rather than producing a review comment.

**If your change is structural, write an ADR first.** Structural means: a new
dependency in a public API, a change to a port's shape, a module boundary, or
overturning an existing decision. See [docs/adr/README.md](docs/adr/README.md)
for when one is warranted and the template. An ADR before the code is a design
review; an ADR after the code is paperwork.

### Adding a dependency

Warren has a strict dependency policy
([docs/dependencies.md](docs/dependencies.md)). In short:

1. **The core module takes no third-party dependencies. Ever.** Not
   temporarily, not for convenience. If a core feature seems to need a library,
   split it: the port goes in core, the implementation goes in a submodule.
2. **Audit before adopting.** Read the repository and the documentation, then
   add a row to the audit table in `docs/dependencies.md` with what you found.
   A package with no audit row does not go into a `go.mod`. Star counts are not
   evidence.
3. A driver module depends only on its own driver.

`make lint-modules` enforces rule 1 mechanically.

---

## Commits

Warren uses [Conventional Commits 1.0.0](https://www.conventionalcommits.org/).
`make ci` and CI both check this; see
[ADR-0005](docs/adr/0005-commits-and-changelog.md) for the reasoning.

```
<type>(<scope>): <subject>

<body>

<footer>
```

### Types

`feat` `fix` `perf` `refactor` `docs` `test` `build` `ci` `chore` `revert`

Only `feat`, `fix`, and `perf` produce changelog entries.

### Scope is the module path

The scope tells release tooling which module to version, so a wrong scope means
a wrong release. Use the module path without the `warren/` prefix:

```
core  di  lifecycle  config  log  errors  domain  app  validate  health
transport/http  transport/http/stdlib  transport/http/echo  transport/http/gin
transport/grpc  transport/gateway  openapi
broker  broker/kafka  broker/rabbitmq  broker/nats  broker/memory  outbox  inbox
persistence  persistence/postgres  persistence/mysql  persistence/mongo
observability  auth  resilience  jobs  testing  cli  mcp
docs  skills  ci  build  deps  release
```

A commit spanning several modules either uses the broadest accurate scope or —
better — is split into several commits.

### Rules

- **Imperative mood.** "add", not "added" or "adds".
- **No trailing period**, and **72 characters** maximum on the subject.
- **The body explains why.** The diff already shows what.
- **Every commit builds and passes its module's tests.** History is bisectable
  or it is not history.

### Breaking changes

Mark with `!` after the scope **and** a `BREAKING CHANGE:` footer that states
the migration — not merely that one is needed.

```
feat(broker/kafka)!: replace segmentio client with franz-go

BREAKING CHANGE: kafka.Config.Dialer is removed. Use kafka.Config.ClientOpts
with franz-go's kgo.Opt values. See docs/migrations/0.3-kafka-client.md.
```

While Warren is pre-1.0 this bumps the **minor** version, per Go's semantic
import versioning for `v0.x`.

---

## Changelog

Every `feat`, `fix`, or `perf` commit carries a changelog fragment. Nothing
else does. CI checks both directions.

```bash
make changelog     # prompts for project, kind, body, issue number
git add .changes/unreleased/
```

Each change becomes its own file, so two pull requests never conflict over the
changelog — which is the point. Write the entry for someone reading release
notes, not for someone reading the diff:

| Instead of | Write |
|---|---|
| `fix consumer bug` | `Kafka consumers no longer lose in-flight messages when a rebalance occurs mid-batch.` |
| `add scope support` | `Modules now isolate their providers: a provider registered in one module is no longer resolvable from another unless exported.` |

---

## Tests

Read [docs/testing.md](docs/testing.md) for what belongs in each tier. The
essentials:

- **Unit tests run with no Docker, no network, and no sleeps.** `make test`
  runs them with `-race -shuffle=on`.
- **Integration tests are behind the `integration` build tag** and use
  testcontainers. `make test-integration`.
- **Every CLI generator has a golden-file test.** Templates break silently
  otherwise — this is not optional (PRD §9).
- **A bug fix starts with a failing test.** If the test passes before the fix,
  it is testing something other than the bug.

`make golden-update` regenerates golden files. Read that diff before committing
it: an unexpected change there is a bug, not noise.

---

## Adding a CLI command

A command is not complete until all five exist
([ADR-0008](docs/adr/0008-agent-integration.md)):

1. The command, registered in `cli/`
2. Unit tests, plus a golden-file test if it generates anything
3. **A skill** in `skills/`, so agents drive it correctly
4. Documentation in `docs/`
5. A changelog fragment

Item 3 is the one people skip. It is not a follow-up task — a follow-up task is
how twenty commands end up with three skills.

---

## Pull requests

- **Branch from `main`.** Name it `<type>/<short-description>`.
- **`make ci` passes locally** before you open it.
- **Explain the why in the description.** Link the issue or the ADR.
- Draft PRs skip the integration suite; mark ready when you want the full run.

### What review looks for

1. **Does it respect the dependency rule?** Non-negotiable, and automated.
2. **Does the public API leak an implementation?** No `dig`, `chi`, `pgx`, or
   `kgo` type may appear in a Warren public signature. This is what makes every
   driver swappable.
3. **Would a user understand the generated code in five minutes?** PRD §1.3:
   if not, the feature is wrong.
4. **Does the error message tell the user how to fix it?** PRD §8 makes this a
   feature, not polish.
5. Tests, naming, and docs.

---

## Reporting bugs and asking questions

Open an issue with the Warren version, the Go version, a minimal reproduction,
and what you expected. For a DI or architecture problem, include the output of
`warren graph di` or `warren lint arch` — it is usually the whole answer.

## Licence

Contributions are licensed under [Apache-2.0](LICENSE). By contributing you
agree your work is released under it.
