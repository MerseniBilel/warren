# ADR-0005: Conventional Commits, changie fragments, one changelog per module

- **Status:** Accepted
- **Date:** 2026-07-27
- **Relates to:** ADR-0003 (multi-module layout)

## Context

ADR-0003 makes this a multi-module repository where modules version and release
independently. That creates two problems a single-module repo does not have:

1. **A commit must say which module it changed**, or release tooling cannot
   decide what to bump.
2. **A single `CHANGELOG.md` is wrong**, because a user of
   `warren/broker/kafka` should not read core's release notes to find their
   changes — and every PR editing one changelog file conflicts with every other.

`git-chglog`, the most commonly recommended Go changelog generator, was
**archived on 2026-01-18** (verified 2026-07-27). It is not an option.

## Decision

### Commit format — Conventional Commits 1.0.0

```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types.** `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`,
`chore`, `revert`. Only `feat`, `fix`, and `perf` produce changelog entries by
default.

**Scope is the module path**, relative and without the `warren/` prefix — it is
what tells tooling which module to bump:

```
core  di  lifecycle  config  log  errors  domain  app  validate  health
transport/http  transport/http/stdlib  transport/http/echo  transport/http/gin
transport/grpc  broker  broker/kafka  broker/memory  outbox  inbox
persistence  persistence/postgres  observability  auth  resilience  jobs
testing  cli  mcp  docs  skills
```

A commit touching more than one module either uses the broadest accurate scope
or, preferably, is split. Cross-cutting work with no single scope omits it.

**Breaking changes** use `!` after the scope **and** a `BREAKING CHANGE:`
footer explaining the migration. While pre-1.0 this bumps the minor version.

```
feat(broker/kafka)!: replace segmentio client with franz-go

BREAKING CHANGE: kafka.Config.Dialer is removed. Use kafka.Config.ClientOpts
with franz-go's kgo.Opt values. See docs/migrations/0.3-kafka-client.md.
```

**Subject line rules.** Imperative mood ("add", not "added" or "adds"), no
trailing period, ≤ 72 characters. The body explains *why*; the diff already
shows *what*.

**Every commit must build and pass its module's tests.** History is bisectable
or it is not history.

### Changelog — changie fragments

`miniscruff/changie` (894 stars, active 2026-07-25) is adopted. Each change
lands as its own file under `.changes/unreleased/`:

```yaml
# .changes/unreleased/added-20260727-120000.yaml
kind: Added
module: broker/kafka
body: Consumer group rebalance now drains in-flight messages before revoking.
```

- **No merge conflicts.** Two PRs never edit the same changelog file, which is
  the single most common source of pointless rebases.
- **Per-module output.** The `module` field lets one release run emit
  `broker/kafka/CHANGELOG.md` alongside core's.
- Root `CHANGELOG.md` aggregates, linking to per-module files.

A `feat`, `fix`, or `perf` commit **must** carry a fragment. CI checks this.
Other types must not.

### Release process

1. `changie batch <version>` consumes fragments into the changelog.
2. `changie merge` regenerates the aggregate `CHANGELOG.md`.
3. Tag per module: `git tag broker/kafka/v0.3.0` — the path-prefixed form Go
   modules require for submodules.
4. GoReleaser builds and publishes the `warren` CLI binaries.

## Consequences

### What this buys

- Release notes are a build artefact, not a chore, and they are accurate because
  they were written when the change was fresh.
- Changelog merge conflicts disappear.
- Scope discipline in commits makes `git log --grep` a usable per-module history.

### What this costs

- Contributors must write a fragment. Friction, mitigated by `make changelog`
  prompting interactively and by CI failing with the exact command to run.
- changie is a small single-maintainer project. Accepted: it is a build-time
  tool whose output is markdown we own, so the migration cost if it dies is a
  script, not a rewrite.

## Alternatives considered

**`git-chglog`** — archived 2026-01-18. Not available.

**`googleapis/release-please`** — actively maintained, 7,258 stars, automates
release PRs well. Rejected: it requires a Node toolchain in a Go project's CI,
and its multi-module Go support is weaker than its monorepo support for other
languages.

**Generate the changelog from commit messages alone, no fragments** — zero
contributor friction. Rejected because commit subjects are written for reviewers
and make poor release notes, and because it offers no way to say "this affects
users" versus "this is internal." Fragments let the author write for the reader.

**One aggregate `CHANGELOG.md`** — simpler. Rejected: it reintroduces the merge
conflicts fragments exist to prevent, and it makes a driver user read core's
notes to find theirs.

## Revisit when

- changie is archived or unmaintained for 12 months.
- Contributor feedback shows the fragment requirement is deterring PRs.
