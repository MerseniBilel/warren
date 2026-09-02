# Contributing to Warren

Warren is a DDD-first application framework and CLI for Go. This page is the
short version: how to get a green build, what the layout means, and the three
rules that actually get changes rejected. The long version is
[AGENT.md](AGENT.md) — it is written for coding agents and it binds humans
identically.

## Get it building

```bash
git clone https://github.com/MerseniBilel/warren
cd warren
make ci
```

Go **1.27** and nothing older; Warren tracks the current major release from the
day it ships and keeps no compatibility path. `make ci` runs fmt, vet,
golangci-lint, the invariants script and `go test -race` across every module,
and it is the gate — if it is green your change is testable, and if it is not,
say what it printed rather than what you expect it to print.

Two consequences of the multi-module layout surprise people. **`go test ./...`
from the root tests ONE module and exits zero** — it is not a green build; use
the Makefile targets, which iterate `MODULES`. And **`go.work` is generated,
never committed**: a committed `go.work` or `replace` breaks `go get` for
users, silently (invariant 8).

You do not need Docker. The suites wanting a real Postgres or Kafka sit behind
`//go:build integration`, run from `make integration`, and are not in
`make ci`.

## The layout

Eight modules in four rings — kernel, contracts, adapters, tooling — set out
in [AGENT.md](AGENT.md) § The four rings. Which ring you are in decides what
you may import, dependencies point downward only, and adapters never import
each other. `scripts/invariants.sh` checks the part grep can see and CI runs
it; the rest is enforced in review, and by `warren lint arch` — the same
command Warren ships to users.

## Adding an adapter

The common contribution, and the one with the least ceremony. A new driver for
an existing port — another broker, another store — is **not** an architecture
change and needs no spec:

1. New module directory, e.g. `broker/nats/`, with its own `go.mod` declaring
   `go 1.27.0` and depending on the core module plus its own driver. Only its
   own driver.
2. Implement the port from the contracts ring. No driver type in any exported
   signature — `*kgo.Client`, `*pgxpool.Pool` and friends reach users through
   named escape hatches or not at all.
3. Run the exported contract suite. `persistence.RunContract` and
   `brokertest.Run` exist so a community adapter is held to the same standard
   as ours.
4. **Add the module to the Makefile's `MODULES` list.** A module the list
   misses is a module CI never tests.
5. Write down what you found out about the dependency — is it archived, when
   did it last ship, what does it pull in — in the [warren.md](warren.md) §9
   ledger. A package with no written audit does not go into a `go.mod`; the
   initial audit found two widely-recommended packages archived and neither
   README said so.

## What gets a change rejected

- **A dependency in the core module.** It is the standard library plus `dig`.
  A core feature that seems to need a library becomes a port in core and an
  implementation in a submodule — every time.
- **A `dig` type or a `dig` error message reaching a user.** The wrap boundary
  is the product: "a missing provider prints a copy-pasteable fix" is
  unreachable while surfacing someone else's diagnostics.
- **An error message with no test.** The diagnostics *are* the deliverable
  here, so every one of them gets a golden file. Untested error text rots
  inside a month, and we have the commits to prove it.

Two smaller ones: never name a type `SomethingWithSomething`, and never
disable a linter to make a change pass — `//nolint` needs a specific linter
and a stated reason.

## Do I need a spec?

Only for a **new package**, a **public API change**, or a change that **stops
working code from working**. Bug fixes, tests, documentation, diagnostics and
new adapters go straight to a pull request. The full rule, and why it was
narrowed on 2026-09-02, is in [AGENT.md](AGENT.md) § Spec-driven development.

## Commits

Conventional Commits, scope is the module path, imperative, ≤72 characters:

```
feat(broker/kafka): drain in-flight messages before revoking partitions
fix(di): name the requesting file in missing-provider errors
```

**Every measurement in a commit message must be one you actually ran.** This
is not a style note. On 2026-08-31 a commit claimed "Blast radius, measured
rather than estimated: 31 packages green across all eight modules" and "the
control was run", and neither was true of half its diff; `main` was red for
two days behind a CI run that had already printed the failure.

## Maintainers: one setting that is not in this repository

`main` has no branch protection, so a red `test` job does not block a merge —
which is how the above reached `main`. It cannot be committed; run it once:

```bash
gh api -X PUT repos/MerseniBilel/warren/branches/main/protection \
  --input - <<'JSON'
{
  "required_status_checks": { "strict": true, "contexts": ["test", "lint"] },
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null
}
JSON
```
