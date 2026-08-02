# `warren/cli` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-02); `warren new` + `version` implemented** — the scaffold compiles, vets, and passes its own tests against the framework, gated by a CI test that builds it. Decisions: cobra (contributor familiarity beat urfave/cli's better dependency numbers); **`dave/dst` REJECTED** — no published releases, untouched 2022-12→2026-04, pins x/tools from 2022 — the AST editor is stdlib splicing, which preserves comments by construction. Generators, `lint arch`, and the rest follow in the recorded order. |
| **Source** | [warren.md §8](../warren.md) |
| **Module** | own module (`warren/cli`) — build-time only, never in a service go.mod |
| **Mode** | Vendor |
| **Vendors** | `spf13/cobra`, `dave/dst`, `golang.org/x/tools/go/packages` |

## Problem

Warren's fourth claim is **architecture enforced in CI** — `warren lint arch`
fails the build when `domain/` imports `infrastructure/` (AGENT.md § What Warren
is). Without the CLI that claim is a convention in a README, and conventions
decay: the layering rule Warren exists to protect is exactly the rule that gets
worked around under deadline.

The claim is also load-bearing on the framework itself. §1 states the governing
constraint: **Warren obeys its own dependency rule**, and *"every boundary below
is enforced by the same `warren lint arch` that ships to users."* If the kernel
imported `net/http`, the architecture-linting pitch would be a lie. So this
module is not only a user-facing tool — it is the gate on Warren's own four
rings, and it must be the same code path in both cases or the dogfooding is
theatre.

Two more problems land here:

- **Wiring is fiddly and mechanical.** A new module, entity, command, repository
  or consumer touches several files and must be registered in `module.go` — the
  one file permitted to see all four layers (AGENT.md § The four rings). Doing
  that by hand is how services drift out of the layout the linter then rejects.
- **Diagnostics are the product.** `warren explain di` renders the same
  resolution story that `di.Container.Explain` produces (§2.2). The CLI's output
  quality and `di`'s error quality are one surface, not two.

## Goals

- Ship the three subsystems of §8 — **templates**, **AST editor**, **analyzer** —
  as one module, `warren/cli`, in the tooling ring (§1.1, §1.6).
- Ship the command surface of §8 in four groups: scaffold, generate, govern,
  evolve. §8 marks the govern group **"← the differentiators"**; it is the group
  the module exists for.
- Enforce, via `warren lint arch`, the dependency rule inside a user's
  application (AGENT.md § The four rings):

  ```
  interfaces ──▶ application ──▶ domain ◀── infrastructure
  domain imports NOTHING from the other three.
  ```

  Only `module.go` may see all four layers. A violation exits non-zero (§8).
- Enforce the same rule on Warren itself — the four rings, dependencies downward
  only (§1, §1.1).
- Hold the **one-way street**: the CLI imports the runtime to analyse it; the
  runtime never imports the CLI (§1.1, AGENT.md § The four rings). `warren/cli`
  never appears in a service's `go.mod`.
- Do **one** `go/packages` load and share it across `lint arch`, `doctor` and all
  three `graph` commands (§8). That sharing is why "the governance commands are
  cheap once the first exists."
- Keep the analyzer usable on **any Go project, Warren or not** (§8). warren.md
  makes this a strategic position, not an accident: *"If the framework stalls,
  that piece stands alone."*
- Obey the five generator rules of §8, and support `--dry-run` and `--force` on
  every generator.
- Meet the §2.2 diagnostic bar in CLI output. AGENT.md § Errors: an error message
  names what was missing, who requested it, where it was declared, and a
  copy-pasteable fix.

## Non-goals

- **Not a runtime dependency.** Build-time only, never in a service's `go.mod`
  (§1.1, §1.6, §8, AGENT.md § Adding a dependency step 3). Nothing in the kernel,
  contracts or adapter rings may import this module.
- **Not a marker-comment rewriter.** Wiring a provider into `module.go` is a real
  AST edit; regex and `// warren:generated` string surgery are excluded by
  generator rule 2 (§8).
- **Not the owner of the code it emits.** Generated code is committed and owned by
  the user; there is no untouchable `.gen.go` except protobuf output
  (generator rule 4). The generated Postgres repository in §6.1 is *"plain pgx,
  plain SQL, fully readable, yours to edit."*
- **Not a closed template set.** Every template is forkable, and `warren
  templates eject` exists so an organisation can fork them (§8, generator rule 5).
- **Not per-command analysis.** One `go/packages` load is shared; a governance
  command that loads the world again defeats the design (§8).
- **Not a lifecycle participant.** The CLI does not boot the framework's run loop;
  it is not a `warren.Module` and registers no hooks (§1.1 — tooling is its own
  ring, above adapters).
- **Not a code owner for protobuf.** `warren g proto` delegates generation to
  `buf` (§4.2); `buf` is Vendor, tooling only (§9).

## Dependency audit

warren.md fixes the libraries and the mode. §9 ledger row:

| Area | Library | Mode | Note |
|---|---|---|---|
| CLI | `cobra`, `dave/dst`, `x/tools` | Vendor | build-time only |

§1.6 records the module line `warren/cli/ MODULE cobra + dst + x/tools
(build-time only)`, and §8 names the third one precisely as
`x/tools/go/packages`.

**Why Vendor is right here.** The wrap rule is: *if changing a library would
force edits across hundreds of user files, it must be behind a port* (§ Modes,
AGENT.md). For these three the blast radius of a swap is zero user files,
because no user file can import them — the module is build-time only and never
enters a service's `go.mod`. The cost of a swap is confined to this module, which
is exactly the trade Vendor names: *"imported and used directly; swapping it
would be a breaking change we accept."* Wrapping cobra behind a port would buy
nothing and would add a layer between the command definitions and their tests.

| Library | Role in §8 |
|---|---|
| `spf13/cobra` | The command surface — four groups, subcommands, flags |
| `dave/dst` | The AST editor. Chosen because it is **comment-preserving**; a wiring edit must not reflow or drop the comments in a user's `module.go` |
| `golang.org/x/tools/go/packages` | The analyzer's single load, shared by `lint arch`, `doctor`, and the three `graph` commands |

**The audit is outstanding, and it blocks the `go.mod`.** warren.md records a
mode and a one-line note for each of these and nothing else — no observation
date, no archived check, no last-shipped date, no transitive-dependency count,
no licence check. AGENT.md § Adding a dependency requires all of it, recorded in
the spec that adopts the package, with the observation date, before it lands in
a `go.mod`: *"A package with no written audit does not go into a `go.mod`. Star
counts are not evidence. 'It is popular' is not evidence."*

The warning is specific and it applies to all three: the initial audit found
`google/wire` **archived** and `git-chglog` **archived**, both still widely
recommended, and neither README said so (AGENT.md § Adding a dependency,
mistake 9). `dave/dst` in particular is a single-maintainer library in a
category — Go AST rewriting — where projects go quiet; that is a fact to
establish, not to assume in either direction.

Per library, this section must be filled in before implementation with:
`archived?`, `pushed_at`, latest release **date** (from
`gh api repos/<owner>/<repo>/releases/latest`, not the README), open issue
count, transitive dependencies pulled in, licence, and the observation date. The
result is then reconciled with the §9 ledger row.

## Architecture

### The tooling ring, and the one-way street

```
┌─────────────────────────────────────────────────────────┐
│  TOOLING            warren/cli                          │  build-time only
│                     templates · AST editor · analyzer   │  never in a service go.mod
└─────────────────────────────────────────────────────────┘
```

*"Tooling is a one-way street: the CLI imports the runtime to analyse it; the
runtime never imports the CLI"* (§1.1, repeated verbatim in AGENT.md § The four
rings). Two consequences that the implementation must hold:

1. `warren/cli` may import the core module and the adapter modules — that is how
   it knows what a module declaration, a controller, or a consumer looks like.
2. No package in any other module may import `warren/cli`, and no `go.mod`
   produced by `warren new` may list it. Both are testable; see **Testing**.

### Three subsystems

| Subsystem | Purpose |
|---|---|
| **Templates** | `embed.FS`, ejectable via `warren templates eject` for per-org forks |
| **AST editor** | `dst` (comment-preserving) — wiring a provider into `module.go` is a real AST edit, not marker-comment string surgery |
| **Analyzer** | One `go/packages` load, shared by `lint arch`, `doctor`, and all three `graph` commands |

**Templates.** Embedded with `embed.FS`, so the binary is self-contained. Eject
copies them out for an organisation to fork (generator rule 5: every template is
forkable).

**AST editor.** `dst` is chosen for one property: it preserves comments across a
rewrite. `module.go` is the one file that may see all four layers (AGENT.md), it
is hand-edited by users, and it is the file every generator has to wire into —
losing a comment there on every `warren g` invocation would make the generators
unusable. Marker comments and regular expressions are excluded by generator
rule 2.

**Analyzer.** One `go/packages` load per invocation, shared by every governance
command. Two things follow:

- The governance commands are cheap once the first exists (§8) — `doctor` and
  the `graph` trio are new questions asked of an index that already exists, not
  new passes over the source.
- It *"works on any Go project, Warren or not. If the framework stalls, that
  piece stands alone"* (§8). This is a design constraint, not a nice-to-have:
  the analyzer must not require a `warren.Module` to exist before it can answer
  a question about imports, and it is tested against a non-Warren project
  (see **Testing**).

### What `warren lint arch` checks

Two rule sets, one code path.

**In a user's application** (AGENT.md § The four rings):

```
interfaces ──▶ application ──▶ domain ◀── infrastructure
domain imports NOTHING from the other three.
```

Only `module.go` may see all four layers. `domain/` importing `infrastructure/`
is the named failing case (AGENT.md § What Warren is, claim 4). A violation
exits non-zero (§8).

**In Warren itself** (§1, §1.1): the four rings, dependencies pointing downward
only — kernel knows nothing of HTTP/SQL/Kafka; adapters are leaves and never
import each other; and the tooling ring is one-way. (Contracts holding zero
implementations is an architectural property of the ring, not an import-shaped
rule — whether `lint arch` can check it at all is Open question 9.) §1 is explicit that this is *the same* command:
*"Every boundary below is enforced by the same `warren lint arch` that ships to
users."*

Whether `lint arch` also checks the invariants that are not import-graph shaped —
no driver type in a public signature (invariant 3), `dig` imported only by
`warren/di` (invariant 2 — `scripts/invariants.sh` greps it today, and the di
package proposed it as a candidate for `warren lint arch`), no reflection on
the request path (invariant 7) — is an open question below.

## Command surface

Reproduced from §8, grouped as §8 groups them. Flags are as given; nothing is
added.

### scaffold

```bash
warren new myapp --module github.com/acme/myapp \
  --layout modular-monolith --transport http,grpc --db postgres --broker kafka
```

| Flag | Value shown in §8 |
|---|---|
| `--module` | the Go module path of the generated service (`github.com/acme/myapp`) |
| `--layout` | `modular-monolith` |
| `--transport` | comma-separated: `http,grpc` |
| `--db` | `postgres` |
| `--broker` | `kafka` |

The layout `warren new` produces is *"documented separately"* (§1) — warren.md
describes the framework's own internals, not the generated application's tree.
See Open questions.

### generate

```bash
warren g module     user
warren g entity     user/User --fields "email:Email,name:string"
warren g command    user/RegisterUser --transport http,grpc
warren g repository user/User --driver postgres
warren g consumer   user --event billing.customer.created
```

| Command | Argument | Flags |
|---|---|---|
| `warren g module` | `user` | — |
| `warren g entity` | `user/User` | `--fields "email:Email,name:string"` |
| `warren g command` | `user/RegisterUser` | `--transport http,grpc` |
| `warren g repository` | `user/User` | `--driver postgres` |
| `warren g consumer` | `user` | `--event billing.customer.created` |

Stated elsewhere in warren.md but **absent from §8's command surface**:

| Command | Source | What it does there |
|---|---|---|
| `warren g proto user --service UserService` | §4.2 | generates the `.proto`, runs `buf generate`, and wires the generated service to existing handlers |
| `warren openapi export > openapi.yaml` | §4.3 | emits the OpenAPI 3.1 document for CI |
| `warren templates eject` | §8 (Templates row) | copies the embedded templates out for a per-org fork |

These are real commands the manifest commits to; which group each belongs to is
an open question below.

### govern ← the differentiators

```bash
warren lint arch                # dependency-rule violations, non-zero exit
warren doctor                   # drift, dead providers, missing wiring
warren graph modules|di|events
warren explain di UserRepository
```

### evolve

```bash
warren add rabbitmq
warren migrate layout --module task --to modular
warren extract module billing --into ../billing-service
```

## Generator rules

Verbatim from §8, with the consequence each one carries:

1. **Idempotent** — re-running is a no-op or a marked diff. A generator is safe
   to re-run after a hand edit; it does not accumulate duplicates.
2. **Surgical wiring** — AST edits, never regex. This is what the `dst`
   subsystem exists for.
3. **Never overwrite silently** — conflicts prompt or fail. A generator never
   destroys work the user did not ask it to touch.
4. **Generated code is committed and owned** — no untouchable `.gen.go` except
   protobuf output. The §6.1 repository is the worked example: plain pgx, plain
   SQL, yours to edit.
5. **Every template is forkable** — the reason `warren templates eject` exists.

**All generators support `--dry-run` and `--force`** (§8). `--dry-run` and
`--force` are the two flags that interact with rule 3, and their exact semantics
are pinned in Open questions rather than guessed.

## Behaviour

### `warren lint arch`

Reports dependency-rule violations and **exits non-zero** (§8) — that non-zero
exit is the whole of claim 4 (AGENT.md § What Warren is). Runs against a user's
project and against Warren's own repository, same code path (§1).

Error text: warren.md fixes none. The bar is AGENT.md § Errors — name the
offending import, where it is, and how to fix it — with the §2.2 block as the
quality reference. The exact text is an open question and must be agreed before
its golden-file test can be written.

### `warren doctor`

Reports *"drift, dead providers, missing wiring"* (§8). Nothing further is
stated: what drift is measured against, what makes a provider dead, what wiring
counts as missing, and whether `doctor` exits non-zero are all open questions.

Note the interaction with `di`: boot step 3 validates *"every dep resolvable?
ambiguous? unused? → fail"* (§1.3). If an unused provider already fails boot,
`doctor`'s "dead providers" is either a second definition or an earlier warning —
the unused-provider question parked in warren.md §2.2 raises the same tension
from the other side (di's Validate checks resolvable and ambiguous only until
it is settled; the root package sharpened it — an unconsumed provider is
simply never built). The two must be settled together.

### `warren graph modules|di|events`

Three graphs over the shared analyzer load: the module import graph (§1.2), the
DI graph, and the event graph. Neither the contents of the three graphs nor the
output format is stated anywhere in warren.md — §8 gives the command line and
nothing else. See Open question 4.

### `warren explain di UserRepository`

`di.Container.Explain(target any) Resolution` exists to power this command
(§2.2). The output bar is §2.2's diagnostic block — the same text AGENT.md
invariant 2 calls *"the deliverable"*:

```
✗ cannot resolve dependency

    domain.UserRepository
      └─ required by *user.RegisterUserHandler
           └─ required by *user.UserController
                └─ declared in internal/modules/user/module.go:14

  No provider found in scope "user" or its imports.

  Did you mean:
    • postgres.NewUserRepository is registered in scope "billing" but not exported.
      Add to billing's module: warren.Exports[domain.UserRepository]()
    • Or provide it locally:  warren.Providers(postgres.NewUserRepository)
```

The CLI's rendering and `di`'s error rendering are one product surface.
`Resolution` is settled and implemented — a self-rendering tree (warren.md
§2.2) — and this command's output must reach that bar: the chain, the declaration site as `file:line`, the
verdict, and copy-pasteable fixes.

There is a mechanism question here that neither document settles: `Explain` is a
method on a **live** container, and the CLI is a static analyser. See Open
questions.

### `warren g repository user/User --driver postgres`

§6.1 fixes the struct shape and the `FindByID` body, which the golden file must
match. It is an excerpt, not a complete artefact: `Save` and `Delete` (required
by §3.3's port), the package clause, the imports, and the target file path are
all unspecified — see Open questions.

```go
type UserRepository struct{ db postgres.DB }   // DB resolves tx-from-context or pool

func (r *UserRepository) FindByID(ctx context.Context, id domain.UserID) (*domain.User, error) {
    row := r.db(ctx).QueryRow(ctx,
        `SELECT id, email, name, status FROM users WHERE id = $1`, id)
    var u domain.User
    if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Status); err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, errors.NotFound("user", id)
        }
        return nil, err
    }
    return &u, nil
}
```

Plain pgx, plain SQL, readable, editable. `r.db(ctx)` returns the ambient
transaction if one exists, else the pool. Note what the template must produce
without being told twice: `context.Context` first (AGENT.md § General), and
`errors.NotFound("user", id)` rather than an HTTP status — the generated
infrastructure code speaks the semantic error vocabulary (§2.6).

### `warren g proto user --service UserService`

Generates the `.proto`, runs `buf generate`, and wires the generated service to
existing handlers (§4.2). Two notes: `buf` is an **external tool** (§9, Vendor,
tooling only), so anything invoking it is not a unit test (see **Testing**); and
protobuf output is the single exception to generator rule 4's "no untouchable
`.gen.go`".

### `warren extract module billing --into ../billing-service`

This command is made viable by an architectural decision taken elsewhere. §5.4:
the memory broker is the default in tests and in modular monoliths, and *"this
is what makes `warren extract module` viable: modules communicate through the
broker port from day one, so extraction swaps the driver rather than rewriting
call sites."* Extraction is therefore a wiring change — swap `broker/memory` for
a real driver — not a call-site rewrite. What the command produces in the source
repository is not stated; see Open questions.

### AST wiring

Every generator that adds a provider, controller or consumer wires it into the
module declaration (§2.1's `warren.NewModule(...)` form) by AST edit. `module.go`
is the only file permitted to see all four layers (AGENT.md), so it is the only
file wiring is written into. Comments in it survive the edit — that is why `dst`
and not `go/ast`.

## Testing

- **A golden-file test for every generator.** Non-negotiable and stated twice:
  §7.5 — *"Every CLI generator has golden-file tests — templates break silently
  otherwise"* — and AGENT.md § Testing — *"Every generator needs a golden-file
  test."* Every command in the generate group, plus `warren new` and
  `warren g proto`.
- **`warren g repository user/User --driver postgres` reproduces §6.1 byte for
  byte.** That block is already a published contract.
- **A golden-file test for every error message in this spec**, once its text is
  agreed (AGENT.md § Testing: *"the diagnostics are the product; untested error
  text rots immediately"*). That includes `lint arch` violations, `doctor`
  findings, and `explain di` output.
- **The analyzer is tested against a Go project that is not a Warren project.**
  §8 claims it *"works on any Go project, Warren or not"* and that it *"stands
  alone"* if the framework stalls. An untested claim of that shape is worth
  nothing — the fixture is a plain Go module with no `warren.Module` anywhere.
- **`lint arch` is tested against Warren's own repository.** §1 makes the
  framework's own rings subject to the same command; the dogfood test is the
  proof. Plus fixtures with a known violation (`domain/` importing
  `infrastructure/`) asserting a **non-zero exit**.
- **One-way street test.** Assert no package outside `warren/cli` imports
  `warren/cli`, and that a `go.mod` produced by `warren new` does not list it
  (§1.1, §1.6).
- **Idempotency test per generator.** Run twice; the second run is a no-op or a
  marked diff (rule 1) — the exact assertion depends on what "a marked diff"
  means (Open questions).
- **Conflict test per generator.** An existing file that the generator would
  touch must never be silently overwritten (rule 3), and `--dry-run` must write
  nothing at all.
- **AST-editor test.** Wiring a provider into a `module.go` that carries comments
  preserves every comment and reformats nothing else. This is the property `dst`
  was chosen for, so it gets its own test rather than being assumed.
- **Single-load test.** A governance command performs one `go/packages` load;
  asserting this keeps §8's cheapness claim honest as commands are added.
- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing).
  `warren g proto` shells out to `buf`, and `warren new` may resolve modules —
  anything that needs an external binary or the network goes behind
  `//go:build integration`. `t.Parallel()` and table-driven subtests named for
  behaviour.

## Definition of done

**Module-wide**

- [ ] Spec approved.
- [ ] `spf13/cobra`, `dave/dst` and `golang.org/x/tools/go/packages` audited per
      AGENT.md § Adding a dependency — archived?, `pushed_at`, real latest-release
      date, open issues, transitive deps, licence — **recorded in this spec with
      the observation date** and reconciled with the §9 ledger row.
- [ ] `warren/cli` is its own module (§1.6) and appears in no other module's
      `go.mod`; no package outside it imports it.
- [ ] No committed `replace` directive (invariant 8); Go 1.26, no `toolchain`
      directive (invariant 9).
- [ ] Templates embedded via `embed.FS` and ejectable.
- [ ] AST editor performs real `dst` edits; no regex or marker-comment wiring
      anywhere in the module.
- [ ] Analyzer does one `go/packages` load shared by `lint arch`, `doctor`, and
      the three `graph` commands, and passes the non-Warren-project test.
- [ ] `warren lint arch` passes on Warren's own repository and fails, non-zero,
      on each violation fixture.
- [ ] Every exported identifier has a doc comment starting with the identifier's
      name; `context.Context` is the first parameter everywhere and is stored in
      no struct (AGENT.md § General).
- [ ] `warren.md` amended in the same change if anything here diverged from §8.

**Per command — every one of `new`, `g module`, `g entity`, `g command`,
`g repository`, `g consumer`, `g proto`, `lint arch`, `doctor`,
`graph modules`, `graph di`, `graph events`, `explain di`, `add`,
`migrate layout`, `extract module`, `templates eject`, `openapi export`:**

- [ ] Implemented with exactly the flags recorded in **Command surface** — no
      flag invented, none dropped.
- [ ] Golden-file test (generators) or golden error-text test (governance
      commands).
- [ ] Error messages tell the user how to fix the problem, at the §2.2 bar:
      what failed, where, and a copy-pasteable fix (AGENT.md § Errors). A CLI
      that prints "invalid layout" and exits has failed this line.
- [ ] `--dry-run` and `--force` supported and tested (generators).
- [ ] Idempotency and conflict behaviour tested (generators).
- [ ] **Its skill exists.** *"The command is not done until the skill exists"* —
      AGENT.md states this twice: § Spec-driven development step 3 (*"implement
      to the definition of done — tests, doc comments, and the skill if it is a
      CLI command"*) and mistake 8 (*"Adding a CLI command without its skill"*).
      A command whose skill is missing is not shippable, however green its tests
      are. What the artefact must contain is Open question 1 and blocks this
      checkbox.

## Open questions

1. **What is a "skill"?** AGENT.md makes it a hard per-command deliverable in two
   places (§ Spec-driven development step 3, mistake 8) and never defines the
   artefact; warren.md never mentions skills at all. Needed before the Definition
   of done above can be satisfied: where does a skill live (a directory in this
   repo? `.claude/skills/`? shipped in the binary?), what format, what must it
   contain, is it one per command or one per group (does `warren g` have a skill,
   or do its five subcommands have five?), and is it published to users or purely
   an agent-facing artefact?

2. **How does §8's design get settled?** warren.md's closing line says
   *"Section 8 (CLI) and the outbox relay design are the parts most likely to
   change once prototyped."* AGENT.md forbids exactly that: *"No spikes, no
   prototypes, no 'let's try it and see'"* (§ Spec-driven development), repeated
   as mistake 14. The two documents disagree about how this package's design is
   decided. Which governs — and if AGENT.md does, what replaces the prototype as
   the mechanism for the parts of §8 that are genuinely undecided?

3. **What does `warren doctor` actually report?** §8 gives three words: *"drift,
   dead providers, missing wiring."* Drift between what and what — generated code
   versus template, code versus `warren.md`, `module.go` versus the files on
   disk? What makes a provider dead, and is it the same condition as boot step
   3's "unused?" (§1.3 — parked in warren.md §2.2)? What is missing wiring?
   Does `doctor` exit non-zero like `lint arch`, or is it advisory?

4. **What do the three `graph` commands output?** `warren graph
   modules|di|events` — format (DOT, Mermaid, JSON, text), destination (stdout or
   file), and whether there are flags at all. warren.md is silent.

5. **What does `warren migrate layout --module task --to modular` do?** The
   mechanism is entirely unstated. Two vocabulary problems come with it: `--module
   task` uses `--module` for something that is not a Go module path, while
   `warren new --module github.com/acme/myapp` uses it for exactly that; and
   `--to modular` does not match `warren new --layout modular-monolith`. What are
   the layout names, what is the full set, and is one of the two flags misnamed?

6. **What does `warren add rabbitmq` do beyond its name?** Add the module to
   `go.mod`, wire it into `main.go`, add config keys, all three? What is the set
   of valid arguments — adapter module names from §1.6? Does it replace an
   existing adapter (`add rabbitmq` in a project scaffolded with `--broker kafka`)
   or add alongside?

7. **What does `warren extract module billing --into ../billing-service`
   produce?** §5.4 explains why extraction is *viable* — the broker port — but not
   what the command does to the source repository. Does it delete the module,
   leave a shim, rewrite the memory-broker wiring into a real driver on both
   sides, create a `go.mod` in the target, or emit a plan for a human?

8. **Which group do `warren g proto`, `warren openapi export` and `warren
   templates eject` belong to?** All three are committed to elsewhere in
   warren.md (§4.2, §4.3, §8) and none appears in §8's four-group command
   surface. `warren openapi export` in particular reads as a fifth group or a
   top-level noun. §8 needs amending either way.

9. **Does `warren lint arch` check anything that is not import-shaped?** The layer
   rule and the ring rule are both import-graph questions. Invariants 2, 3 and 7
   are not — no `dig` type in a public signature, no driver type in a public
   signature, no reflection on the request path — and the dig-boundary check
   (today a grep in `scripts/invariants.sh`) was proposed as a candidate for
   `warren lint arch`. Is `lint arch` one check or a suite, and is there a
   `warren lint` family with other members?

10. **How does `lint arch` learn a project's layer layout?** The rule is
    `interfaces → application → domain ← infrastructure`, but a real project's
    directories are named by its authors. Convention from `warren new`'s layout,
    a config file, flags, or package-level markers? And what happens when it is
    run on a project that was not scaffolded by `warren new` — does it degrade to
    the four-ring check, or refuse?

11. **What is the exact text of a `lint arch` violation?** warren.md fixes none,
    and AGENT.md's standard is high: name what, where, and the copy-pasteable
    fix. The block needs agreeing before its golden-file test exists — same
    treatment §2.2's diagnostic got.

12. **How does `warren explain di` reach `Explain`?** §2.2 says
    `Explain(target any) Resolution` *"powers `warren explain di`"*, but `Explain`
    is a method on a live `Container`, populated at boot steps 1–4, while the CLI
    is a static `go/packages` analyser. Does the CLI build the app (compile and
    run the user's wiring) to ask a live container? Does it reconstruct the graph
    statically and share only the rendering code with `di`? Or does `di` expose a
    renderer the CLI drives? This decides whether `explain di` works on a project
    that does not currently boot — arguably the case where it is most needed.

13. **Where does `warren templates eject` write, and how are ejected templates
    picked up afterwards?** §8 says "ejectable ... for per-org forks" and stops
    there. Target directory, discovery mechanism (flag, config file, convention),
    precedence over the embedded `embed.FS`, and what happens when an ejected
    template is stale against a newer CLI.

14. **What is "a marked diff"?** Generator rule 1 allows re-running to produce
    either a no-op or a marked diff. Marked how — a conflict marker in the file,
    a printed diff the user applies, a `.orig` file? This changes what the
    idempotency test asserts.

15. **When does a conflict prompt and when does it fail?** Rule 3 says *"conflicts
    prompt or fail"*, which is two behaviours. Prompting is impossible in CI and
    in a non-TTY; is the rule "prompt when interactive, fail otherwise"? And what
    exactly does `--force` override — only the prompt, or rule 3 entirely?

16. **What layout does `warren new` generate?** §1 says the layout of applications
    built with the framework *"is `warren new`'s concern, documented separately"*,
    and no such document exists in the repository. It is also the input to
    question 10. What is the tree, and what are the valid `--layout` values beyond
    `modular-monolith`?

17. **Does the analyzer have a Go API, and is it separately consumable?** §8's
    *"if the framework stalls, that piece stands alone"* implies it is usable
    outside the `warren` command, but §1.6 lists exactly one CLI module. Is the
    analyzer an exported package of `warren/cli`, a separate module, or internal
    with the standalone claim meaning only "portable, not Warren-coupled"? This
    is the one place this module might need a Go public API section, which
    AGENT.md § Spec-driven development otherwise requires of every spec.

18. **Does the analyzer require the target project to compile?** `go/packages`
    load modes range from syntax-only to full type checking. `lint arch` on a
    broken build, and `doctor` on a project with a missing provider, are both
    plausible primary use cases — and the stricter load mode makes both
    impossible.

19. **Is `buf` a hard prerequisite?** §4.2 has `warren g proto` run `buf
    generate`; §9 lists `buf` as Vendor, tooling only. Is it expected on `PATH`,
    version-pinned, or invoked through a `buf.gen.yaml` the generator writes? What
    is the error when it is absent — and per AGENT.md § Errors, that message must
    say how to install it.
