# CLAUDE.md

**Read [AGENT.md](AGENT.md) first — it is the canonical instruction file for
this repository**, and it applies here in full: the invariants, the modes, the
two orderings, the spec process, the code conventions, and the commit rules.
This file adds only what is specific to Claude Code.

---

## Quick orientation

Warren is a DDD-first framework and CLI for Go. Multi-module repository,
Go 1.27 on its release — toolchain 1.26.x until then, and nothing may depend on
a 1.27 feature before it ships (AGENT.md invariant 9). Apache-2.0, module path
`github.com/MerseniBilel/warren`.

**The repository was reset in July 2026** — everything except the licence and
`warren.md` was deleted because the previous implementation had drifted from
the design — **and the tooling was rebuilt on 2026-08-01.** The core `go.mod`,
the Makefile, CI, `.golangci.yml`, and `scripts/invariants.sh` all exist now;
`make ci` (fmt · vet · lint · invariants · test) is the gate, and new modules
must be added to the Makefile's `MODULES` list when they are created.

Practically: run `make ci` and quote what it printed. The invariants script
only covers what grep can see (core deps, dig confinement, `replace`, `XWithY`
names); the rest of AGENT.md binds in review — say which parts you verified by
tool and which by reading.

| Question | Where |
|---|---|
| What are we building, package by package? | [warren.md](warren.md) |
| How do the four rings fit together? | [warren.md §1](warren.md) |
| What does each kernel package expose? | [warren.md §2](warren.md) |
| Which library did we pick, and in what mode? | [warren.md §9](warren.md) |
| What are the rules? | [AGENT.md](AGENT.md) |

The invariants that fail CI, in short — full versions in AGENT.md:

1. Core module: **standard library + `go.uber.org/dig`**, nothing else, ever.
2. **`dig` is imported by `warren/di` alone**, and no dig type — nor any dig
   error message — reaches a user.
3. No driver type (`chi`, `pgx`, `kgo`, `grpc`) in a public signature.
4. Adapters never import each other; contract packages hold zero
   implementations — single carve-out: §3.5's registrars are concrete structs
   with generic methods (Go 1.27), driver-free.
5. Handlers import no transport package.
6. No reflective *dispatch* on the request path — every reflective decision
   is made at boot, and the container is not consulted per request.
7. No committed `replace` directive.

---

## Working here

**Write the spec before the feature — and retire it after.** Every feature
gets a `SPEC.md` **in the package directory it describes**, written and
approved *before* any code, corrected in the same pull request whenever the
implementation diverges — and **deleted once the package is implemented and
reviewed**, with its load-bearing leftovers rehomed first (open questions to
the spec that will answer them, audits to the §9 ledger). An implemented
package's contract is its code, tests, golden files, and `warren.md` entry.
If you are asked to build something that has no spec, write the spec first and
say so. See [AGENT.md § Spec-driven development](AGENT.md).

**`warren.md` already fixes the public API for most packages.** Start a spec
from that surface rather than inventing one. If the spec needs to contradict the
manifest, amend `warren.md` in the same change and flag it — that is an
architecture decision, not an implementation detail.

**Do not propose a spike or a prototype.** Decisions here are made by research
and argument: read the evidence, put the options and a recommendation to the
human, agree it, then build it once. Throwaway code is not how this project
decides things.

**Bring structural choices to the human.** Module boundaries, port shapes, and
public API are decisions to be taken together, not discovered by writing code
and seeing what happens.

**Multi-module means `go test ./...` lies.** From the root it tests one module
and exits zero. Use the Makefile targets — they iterate the `MODULES` list —
and add every new module to that list when it is created.

**Verify before claiming.** Run the command and quote what it printed. "Should
work" is not a result, and neither is a tool you did not actually invoke.

---

## Diagrams

The architecture diagrams live as ASCII inside [warren.md](warren.md) — the four
rings (§1.1), scoped containers (§1.2), the boot sequence (§1.3), the transport
spine (§1.4), and the messaging runtime (§1.5). Edit them there; warren.md stays
the source of truth for the architecture.

Each **approved** spec also has a usage-flow diagram in `docs/assets/<pkg>-usage.puml`,
rendered to a PNG of the same name; `docs/assets/approved-usage.puml` combines
them into one overview. A spec's diagram is created when the spec is approved,
not before — and it **outlives the spec's retirement**: after a package is
implemented its diagram cites the package's `warren.md` section instead.
Regenerate after any change to a source, and commit the images:

```bash
java -jar ~/.local/bin/plantuml.jar -tpng -o . docs/assets/*.puml
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

Record what you found in the spec that adopts the package, assign it a mode
(Build / Wrap / Vendor), and add it to the ledger in
[warren.md §9](warren.md).

---

## Things to avoid here

- **Do not commit or push unless asked.**
- **Do not add a dependency to the core module.** It is stdlib plus dig. A core
  feature that seems to need a library becomes a port in core and an
  implementation in a submodule — every time.
- **Do not let `dig` leak.** Not into a signature, not into an error string.
  Warren's diagnostics are the reason dig is wrapped rather than re-exported.
- **Do not add a mocking framework, a logging library, or an assertion library
  to the core.**
- **Do not name a type `XWithY`** — see AGENT.md § Naming.
- **Do not disable a linter to make a change pass.** `//nolint` needs a specific
  linter and a stated reason.
- **Do not create empty modules** ahead of the code that will fill them.
- **Do not add a package `warren.md` does not describe** without agreeing the
  manifest entry first.
