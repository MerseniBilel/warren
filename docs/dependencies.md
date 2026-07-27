# Dependency Policy and Audit

Warren's credibility rests on its `go.mod` being defensible. A framework that
drags forty transitive dependencies into a service has failed before it has
shipped a feature. This document is the standing policy for taking on a
dependency, and the record of every audit performed so far.

---

## 1. Policy

### 1.1 The rule

**No dependency is adopted until someone has read its repository and its
documentation and written down what they found.** Star counts are not evidence.
"Everyone uses it" is not evidence. The audit record in §3 is the evidence, and
a package without a row there does not go into a `go.mod`.

### 1.2 Audit checklist

Every candidate is assessed against all eight. A candidate that fails **any** of
the first four is rejected outright.

| # | Gate | Rejection threshold |
|---|---|---|
| 1 | **Alive** | Archived, or no commit in 18 months with open correctness bugs |
| 2 | **Licence** | Not Apache-2.0/MIT/BSD/ISC compatible. No copyleft in any module. |
| 3 | **Transitive weight** | Pulls a dependency tree we would not adopt individually |
| 4 | **Escapable** | We cannot wrap it behind our own interface and swap it later |
| 5 | Maintainer count | Single-maintainer projects are allowed but recorded as a risk |
| 6 | Release discipline | Tagged semver releases; a v0 with breaking changes is a risk note |
| 7 | Security posture | Known unpatched advisories, `govulncheck` clean |
| 8 | API stability | v1+ preferred; churn history reviewed |

### 1.3 Placement rule

A dependency's blast radius determines which module it may live in.

- **Core module (`github.com/MerseniBilel/warren`) — zero third-party
  dependencies.** Standard library only, permanently. This is a hard invariant
  enforced by CI (see `make lint-deps`). If a core feature seems to need a
  library, the feature is defined as a port in core and implemented in a
  submodule.
- **Driver modules** may take exactly the dependencies their driver requires.
  `broker/kafka` may import a Kafka client. It may not import an HTTP router.
- **Test-only dependencies** must be in the module's test files only, and must
  not appear in the module's exported API.

### 1.4 Re-audit cadence

The table in §3 is re-verified **quarterly** and before every minor release. The
`make audit-deps` target refreshes the observed columns. A dependency that has
crossed a rejection threshold since adoption opens a migration issue
immediately — not at the next major.

---

## 2. Method used for this audit

Repository metadata was read from the GitHub API (stars, last push, archive
status, open issue count, latest release tag and date). Documentation was read
from each project's own docs site or `pkg.go.dev`. Where a claim below is about
behaviour rather than metadata, it came from the project's documentation and is
attributed inline.

**Observation date: 2026-07-27.** Metadata figures are a snapshot; they age.
The judgements are what matter, and each one states its reasoning so it can be
re-checked rather than re-litigated from scratch.

---

## 3. Audit record

### 3.1 Toolchain

| Item | Version | Observed | Notes |
|---|---|---|---|
| Go | **1.26** (patch 1.26.5, 2026-07-07) | Go 1.26.0 released 2026-02-10 | Green Tea GC matured; `new` accepts expressions; self-referential generic type params. See [ADR-0007](adr/0007-go-version-policy.md). |
| golangci-lint | **v2.12.2** (2026-05-06) | Config schema `version: "2"` | v2 splits `formatters` out of `linters`. See [ADR-0006](adr/0006-lint-and-format.md). |

### 3.2 Dependency injection — the §13.1 decision

| Candidate | Stars | Last push | Latest release | Verdict |
|---|---|---|---|---|
| **`uber-go/dig`** | 4,486 | 2025-05-13 | v1.19.0 (2025-05-13) | **Adopted, wrapped** |
| `google/wire` | 14,417 | 2025-08-22 | — | **Rejected — ARCHIVED** |
| `uber-go/fx` | 7,614 | 2025-12-27 | v1.24.0 (2025-05-13) | Rejected — owns the lifecycle we need to own |
| `samber/do` | 2,780 | 2026-07-27 | v2.1.0 (2026-07-20) | Rejected — but the live fallback |

**The finding that changes the PRD.** PRD §13.1 proposes prototyping three
approaches in week 1: `dig`, generics-based explicit registration, and `wire`
codegen. **`google/wire` is archived.** The repository is read-only as of
2025-08-22 with 108 issues left open. The compile-time-codegen option is
therefore not a live choice, and week 1 is a two-way comparison, not three.

**Why `dig` anyway, given it is quiet.** `dig`'s last commit is 2025-05-13 —
fourteen months before this audit. That is a genuine risk and it is recorded as
one. It is still the right pick:

- It is feature-complete for exactly what Warren needs, and several of those
  features map one-to-one onto PRD §7.4 commands that are otherwise expensive:
  `Visualize()` emits DOT and gives us `warren graph di` nearly free;
  `DryRun(true)` gives us boot-time graph validation without constructing
  anything, which is how PRD §4.1 principle 2 ("errors at startup, never at
  request time") gets implemented; `Scope`, value groups, and `Decorate` are
  what module imports/exports compile down to.
- 2,040 packages import it. It is the engine under `fx`, which Uber states is
  "the backbone of nearly all Go services" there. Dormancy here reads as
  finished rather than abandoned — no open correctness bugs of consequence.
- **It is wrapped, never exposed.** `warren/di` is the only package in the repo
  permitted to import `dig`. No `dig` type appears in any Warren public
  signature. This is what makes the risk survivable: if `dig` needs replacing,
  the blast radius is one package, and `samber/do` (actively maintained, v2.1.0
  last week) or a hand-written reflective container are both viable behind the
  same interface.

`fx` is rejected for the reason PRD §6.6 already gives — it owns application
lifecycle, and Warren's lifecycle semantics (ordered start/stop, readiness
gating, graceful drain) are a product feature we cannot delegate. `fx`'s error
messages are also a well-known DX complaint, and PRD §8 makes DI error quality a
headline feature.

> **Risk registered:** `dig` dormancy. Re-check at every quarterly audit. If a
> Go release breaks it and no fix lands within 60 days, execute the swap behind
> `warren/di`. Tracked in [ADR-0001](adr/0001-dependency-injection.md).

### 3.3 HTTP router — pluggable, with a recommended default

This is a product requirement, not just a dependency choice: users pick their
router, and Warren recommends one.

| Candidate | Stars | Last push | Latest release | Engine | Verdict |
|---|---|---|---|---|---|
| **`go-chi/chi`** | 22,585 | 2026-07-06 | v5.3.1 (2026-07-06) | `net/http` | **Default adapter** |
| **`net/http.ServeMux`** | stdlib | — | Go 1.26 | `net/http` | **Zero-dependency adapter** |
| `labstack/echo` | 32,565 | 2026-07-23 | v5.3.1 (2026-07-21) | `net/http` | Supported adapter |
| `gin-gonic/gin` | 88,980 | 2026-07-16 | v1.12.0 (2026-02-28) | `net/http` | Supported adapter |
| `gofiber/fiber` | 40,018 | 2026-07-27 | v3.4.0 (2026-07-02) | **fasthttp** | Community adapter, caveated |

**The structural finding.** Fiber is built on `valyala/fasthttp`, not
`net/http` — its own documentation leads with this. fasthttp deliberately does
not implement `net/http` interfaces, so a Fiber handler is not an
`http.Handler`, and no `net/http` middleware works with it. This is not a
detail we can paper over: it decides the shape of the port.

**Consequence for the design.** Warren's HTTP port is shaped on `net/http`,
because that is the only contract the ecosystem actually shares — chi, Echo,
Gin, and stdlib all satisfy it, and so do OpenTelemetry's instrumentation, every
tracing middleware, and every auth library a user already owns. Committing to a
lowest-common-denominator abstraction that also accommodated fasthttp would tax
the 95% case to serve the 5%, and would break PRD §4.1 principle 4 (the raw
`*http.Request` is always reachable). Fiber support is therefore a separate
adapter that re-implements the middleware chain against fasthttp, is documented
as not sharing the `net/http` middleware ecosystem, and is community-owned.

**Why chi is the recommendation.** It has zero external dependencies and is
roughly a thousand lines — the cheapest thing we can put in a default `go.mod`.
It is 100% `net/http`-compatible, so its middleware and everyone else's are the
same middleware. It is actively released (v5.3.1, three weeks before this
audit). Gin has more stars by a wide margin, but it carries a real dependency
tree and its own context type, and 733 open issues against 112 suggests a
different maintenance posture. Echo is a strong second and is a fully supported
adapter.

**Why stdlib `ServeMux` is a first-class adapter.** Go 1.22 gave `ServeMux`
method matching (`GET /users/{id}`), wildcards, `{path...}`, `r.PathValue`, and
defined precedence rules. Its documented gaps versus third-party routers are
middleware chaining, route groups, and sub-routers — and **all three are things
Warren provides itself**, above the router, because they must work identically
for gRPC and consumers. For a service that wants zero HTTP dependencies, stdlib
is now genuinely sufficient, and Warren can make that true.

See [ADR-0002](adr/0002-http-router-port.md).

### 3.4 CLI

| Candidate | Stars | Last push | Latest release | Verdict |
|---|---|---|---|---|
| **`spf13/cobra`** | 44,323 | 2026-07-11 | v1.10.2 (2025-12-04) | **Adopted** |
| `urfave/cli` | 24,177 | 2026-07-21 | — | Rejected |
| `alecthomas/kong` | 3,143 | 2026-07-27 | v1.x | Rejected, respectfully |

Cobra confirms PRD §7. It gives shell completions for four shells and man/docs
generation out of the box, which matters because PRD §7 defines roughly twenty
commands and every one needs completions. Its idioms are what Go developers
already recognise from `kubectl`, `hugo`, and `gh` — the CLI should feel
familiar on first contact.

Kong is the better-designed library in isolation: declarative struct tags, less
boilerplate, and it would suit Warren's generator flags well. It is rejected on
ecosystem, not merit — completions and docs generation would be ours to build,
and the CLI is not where Warren should spend novelty budget. Recorded here so
the choice is not silently re-opened.

### 3.5 Configuration

| Candidate | Stars | Last push | Latest release | Verdict |
|---|---|---|---|---|
| **`knadh/koanf`** | 4,130 | 2026-07-25 | v2.3.5 (2026-05-30) | **Adopted in `config`** |
| `spf13/viper` | 30,394 | 2026-01-12 | — | Rejected |

PRD §6.6 rejects Viper for dependency weight and global state, and directs us to
write our own loader. Koanf is the better reading of that intent: it is
explicitly built as the anti-Viper, and critically **its providers and parsers
are separate Go modules**, so the core takes only what it uses. Its documented
advantages over Viper are ones Warren cares about directly — it does not
lowercase keys, and it does not couple parsing to file extensions.

Note the placement rule: koanf is used by the **`warren/config` submodule**, not
by the core module. Core defines the config port; `config` implements it.

### 3.6 Persistence

| Item | Choice | Stars | Last push | Notes |
|---|---|---|---|---|
| Postgres driver | **`jackc/pgx` v5.10.0** | 14,068 | 2026-07-26 | Native protocol, not `database/sql`. Required for `COPY`, `LISTEN/NOTIFY`, and typed arrays — all of which the outbox relay wants. |
| Migrations | **`pressly/goose`** | 11,235 | 2026-07-25 | Embeddable as a library, so `warren` can run migrations in-process. |
| Migrations (alt) | `golang-migrate/migrate` | 18,753 | 2026-07-05 | More stars; CLI-first ergonomics fit us less well. |
| Schema-as-code | `ariga/atlas` | 8,610 | 2026-06-25 | Deferred. Powerful but opinionated and partly commercial; revisit post-1.0. |

### 3.7 Messaging

| Candidate | Stars | Last push | Open issues | Verdict |
|---|---|---|---|---|
| **`twmb/franz-go`** v1.21.5 | 2,964 | 2026-07-27 | **6** | **Adopted for `broker/kafka`** |
| `segmentio/kafka-go` | 8,591 | 2026-04-23 | **264** | Rejected |

The issue counts are the story. franz-go carries 6 open issues against
kafka-go's 264, with three months' more recency. franz-go supports the full
modern protocol surface — transactions, exactly-once semantics, and consumer
group rebalance protocols — which the transactional outbox in PRD §6.3 depends
on. Star count favours kafka-go and is the wrong signal here.

### 3.8 Validation, observability, testing

| Item | Choice | Version | Last push | Notes |
|---|---|---|---|---|
| Validation | `go-playground/validator` | v10.30.3 (2026-05-29) | 2026-07-27 | Confirms PRD §6.1. Struct-tag validation; used only by the `validate` submodule. |
| Observability | `open-telemetry/opentelemetry-go` | v1.44.0 (2026-05-27) | active | Confirms PRD §6.5. |
| Assertions | **stdlib + `testify/require` where it pays** | v1.11.1 (2025-08-27) | 2026-07-21 | See [testing.md §2](testing.md). Core module stays testify-free; it is a test dependency in submodules only. |
| Integration | `testcontainers/testcontainers-go` | v0.43.0 (2026-06-19) | 2026-07-25 | Confirms PRD §9. Still v0 — API churn is a registered risk; wrapped behind `warren/testing`. |

### 3.9 Agent integration

| Item | Choice | Stars | Notes |
|---|---|---|---|
| MCP server | **`modelcontextprotocol/go-sdk`** v1.7.0+ | 4,872 | The official SDK, maintained with Google. v1.7.0+ targets MCP spec 2026-07-28 with back-compatibility to 2024-11-05. Distinct from the older community `mcp-go`. Note: roots, sampling, and logging are deprecated in the 2026-07-28 spec — do not build on them. |

### 3.10 Release and changelog tooling

| Candidate | Stars | Last push | Verdict |
|---|---|---|---|
| **`miniscruff/changie`** | 894 | 2026-07-25 | **Adopted** |
| **`goreleaser/goreleaser`** | 15,961 | 2026-07-27 | **Adopted** for CLI binary releases |
| `googleapis/release-please` | 7,258 | 2026-07-24 | Rejected — Node toolchain, weaker multi-module Go story |
| `git-chglog/git-chglog` | 2,869 | 2026-01-18 | **Rejected — ARCHIVED** |

**Second archived-project finding.** `git-chglog`, the most commonly recommended
Go changelog generator, was archived on 2026-01-18.

Changie is adopted because it solves the problem multi-module repos actually
have: each change lands as a **separate unreleased fragment file**, so two PRs
touching the changelog never conflict, and fragments carry a module label that
lets us emit one changelog per module. It is a small project (894 stars,
single-maintainer) — recorded as a risk, but the blast radius is a build tool,
not a runtime dependency, and its output is plain markdown we already own.

See [ADR-0005](adr/0005-commits-and-changelog.md).

### 3.11 Prior art reviewed, not adopted

| Project | Stars | Why it matters |
|---|---|---|
| `fe3dback/go-arch-lint` | 526 | The closest existing thing to `warren lint arch`. Reviewed for design, not adopted: it is standalone and YAML-driven, whereas Warren's rules must derive from module structure the framework already knows. Confirms the niche is real and under-served. |

---

## 4. Adopted set at a glance

Everything Warren will depend on, and where it is allowed to live.

```
core (github.com/MerseniBilel/warren)     → stdlib only. Enforced.
warren/di                                 → go.uber.org/dig
warren/config                             → github.com/knadh/koanf/v2
warren/validate                           → github.com/go-playground/validator/v10
warren/transport/http                     → github.com/go-chi/chi/v5   (default)
warren/transport/http/stdlib              → stdlib only
warren/transport/http/{echo,gin}          → respective router
warren/transport/grpc                     → google.golang.org/grpc
warren/persistence/postgres               → github.com/jackc/pgx/v5 + pressly/goose
warren/broker/kafka                       → github.com/twmb/franz-go
warren/observability                      → go.opentelemetry.io/otel
warren/cli                                → github.com/spf13/cobra
warren/mcp                                → github.com/modelcontextprotocol/go-sdk
warren/testing                            → testcontainers-go, testify (test scope)
```

## 5. Open items

| Item | Blocking | Resolution path |
|---|---|---|
| `dig` dormancy | No | Quarterly re-check; swap plan documented in ADR-0001 |
| PRD §13.1 week-1 prototype | v0.1 | Now a two-way comparison — `dig` wrapper vs. generics-based explicit registration |
| testcontainers-go still v0 | No | Wrapped behind `warren/testing`; pin exact version |
| Fiber adapter ownership | No | Community-owned; not in the v0.x support matrix |
| Atlas vs. goose long-term | No | Revisit post-1.0 |
