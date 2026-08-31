# `warren/persistence` — SPEC (the memory driver accepts what Postgres refuses)

| | |
|---|---|
| **Status** | **PROPOSED (2026-08-31) — NOT APPROVED.** Two guards in one file, no new public API. **Changes shipped behaviour**, and the blast radius is measured below rather than estimated. |
| **Source** | Field test #14 (`fieldtest14/REPORT.md`, finding 5), verified against `persistence/memory.go:453,523`, `persistence/persistence.go:200-250`, `persistence/postgres/postgres.go`, and a patched-copy probe of all eight modules |
| **Module** | core (`persistence/`) — stdlib + dig |
| **Mode** | Build |
| **Retires** | Deleted when implemented and reviewed. Residue rehomes to `MemoryRepository`'s doc comment and warren.md §3.4. |

## Problem

`MemoryRepository.Save` with no unit of work on the context writes the row,
returns `nil`, and leaves the aggregate's pending events on an object about to
go out of scope. Measured by the field test:

```
memory Save outside a unit of work: err = <nil>
row readable afterwards: true (err=<nil>)
events still pending on the saved aggregate: 1
  -> BookBorrowed was written to no outbox and published nowhere, with no error
```

The Postgres driver refuses the same call. So a handler that forgets
`app.Transactional` writes the row, answers 201, drops the event, and **every
memory-driver test passes.** The mistake appears on the switch to Postgres —
after the tests that were supposed to catch it have gone green.

The direction is the worst available one: the driver the scaffold defaults to,
and that `warrentest.WithMemoryPersistence()` binds, is more permissive than
production.

## Ruling — refuse, using the error that already exists for this

`persistence.Write` (`persistence.go:200-203`) already contains the refusal:

```go
if !InTransaction(ctx) {
    return ErrNoTransaction(op)
}
```

`MemoryRepository.Save` and `.Delete` do not call it; they hand-roll the path
and take an "outside a transaction the write is immediate" branch
(`memory.go:497-500`, `:541`).

**This is not new public API.** `ErrNoTransaction` is already exported, with a
doc comment stating exactly why:

> It is exported for the same reason `ErrNestedOptions` is: **every driver must
> produce the SAME message**, and a second copy in another module drifts from
> this one within a month.

and its own error text already says whose job the check is:

> A repository's write does not make this check itself. It calls
> `persistence.Write`, which makes it — and enlists the aggregate when the
> write succeeds, so its events cannot be left behind.

The memory driver is a driver. It is the only one that does not produce it.

### Four supporting arguments, all from this repository's own text

1. **`memory.go:461-476` already settles the principle**, in a comment written
   while fixing a *different* memory-vs-Postgres divergence (version
   advancement): *"Two drivers implementing one port differently is exactly
   what the port exists to prevent, and no contract test could see it because
   the suite only looked after commit."*
2. **`66333e7` is this same class one layer up** — *"the exact class
   `persistence.Write` was introduced to abolish, reintroduced by the template
   whose own comment documents the abolition."*
3. **The contract suite already assumes the refusal.** All seventeen
   `Save`/`Delete` call sites in `RunContract` and `RunVersionedContract`
   (`contract.go:60,78,103,118,123,128,149,158,171,174,195,217,265,343,371`)
   are wrapped in `uow.Do` — they have to be, because Postgres refuses
   otherwise. The suite is already driver-symmetric; only the driver is not.
4. **warren.md's defence does not survive reading its own paragraph.**
   `warren.md:1483-1489` describes a defect that *was* fixed — a sink-less unit
   of work that "drained the aggregates, discarded what it drained, and
   committed… no error or log line said so" — and `warren.md:1490-1491` then
   licenses structurally the same silent loss on the untransacted path.

   Be precise about the target: **`persistence.go:142`'s comment is correct
   and stays.** `Track` outside a `Do` genuinely is a no-op that loses nothing,
   because `PullEvents` is never called. The wrong sentence is warren.md's,
   which generalises that true statement about `Track` into a licence for the
   untransacted *write*. The engineer's rebuttal is exact: in a
   request-scoped handler there is no later `Do`, so "loses nothing" is true
   of the API and false of the program.

This cannot take the `app.Chain` transparency route — document it, do not fix
it — because the document that would carry it is the one that is wrong.
Rewriting `warren.md:1490` to "loses the events, silently, and we chose not to
stop it" is a worse artefact than no sentence at all.

Reads are unaffected: `ErrNoTransaction` already says *"Reads need no
transaction; only writes are refused."*

## Ordering: the argument check precedes the context check

**The rule: a check about the ARGUMENT runs before a check about the CONTEXT.**

In `Save` that means `errAggregateHasNoID` first, then the guard. In `Delete`
there is no argument-shaped check, so the guard is first.

The reason is not the test count. A `Save` of an aggregate with no ID is wrong
**in every context** — inside a `Do` as much as outside it — so checking it
first means one bad aggregate always produces one diagnostic, regardless of
whether the caller wrapped the call. Guard-first would make a single mistake
report two different errors depending on context, which is the less
predictable API and the harder one to search for.

The measurement agrees, which is confirmation rather than justification:
guard-first breaks one test, argument-first breaks none.

## Measured blast radius

A patched copy of the checkout was built in the scratchpad and run against a
pre-change baseline. **Nothing in the repository was modified.**

| | baseline | after (argument-first) | after (guard-first) |
|---|---|---|---|
| framework, all 8 modules | 0 failures / 31 pkgs ok | **0 failures** | 1 failure |
| field-test app `library` | 0 failures / 6 pkgs ok | **0 failures** | 0 failures |

- **`persistence/`'s own contract suite: 0 failures**, for the reason in
  argument 3 above.
- **`persistence/postgres`: unaffected.** No source change, no golden change.
  Its `errNoTransaction` is its own unexported function.
- **`testing/warrentest.go`: unaffected.** `WithMemoryPersistence`
  (`warrentest.go:151`) binds ports into the container and makes no writes.
  No `warren.md`-documented helper breaks.
- **The CLI is clean, and proven so rather than read so.** No template or
  golden produces an untransacted write: the test templates delegate to
  `RunContract`, and every generated production call site sits behind
  `app.Transactional`. `cli/internal/scaffold/compile_test.go:41` and
  `cli/internal/generate/compile_test.go:130` point `FrameworkPath` at the
  checkout, so `TestScaffoldCompilesAndPasses/memory`, `/postgres` and
  `TestGeneratedCodeCompilesAndPasses` all built, vetted and tested generated
  trees **against the patched framework** and passed.
- The field test app's only untransacted `Save` is its deliberate probe
  (`atomicity_test.go:66`), which logs and returns early, so it passes either
  way — but its name becomes a lie and it needs rewriting, not fixing.

**Zero failures is not zero edits.** Two comment corrections are required in
the same change:

- `memory.go:497-500` and `:541` describe an immediate-write path ordinary
  callers can no longer reach.
- `persistence_test.go:615-618`'s tail still passes — a no-ID aggregate is
  still refused on both paths — but its comment *"the immediate-write path is
  the same bug"* names a path that is gone.

## The mixed-driver case: ruled, not fixed

`InTransaction` keys on `collectorKey{}` (`persistence.go:298-301`), while the
memory driver's staging keys on `stagingKey{}` (`memory.go:99`). Both
`MemoryUnitOfWork.Do` (`memory.go:99-100`) and `postgres.UnitOfWork.Do`
(`postgres/uow.go:111`) call `persistence.Collect`. So a `MemoryRepository`
used inside a **Postgres** `Do` passes the new guard, finds no staging, and
still takes the immediate-write branch — its write is outside that
transaction's staging set and survives a rollback.

**Ruling: keep `InTransaction`. This is not a defect of this change, and it is
not a defect at all.** Two stores in one transaction is a distributed
transaction, which Warren deliberately does not do — the outbox exists because
it does not. Note what *does* work correctly in that case: the collector is
present, so `Track` enlists and the events are drained on commit. Only
rollback atomicity across two stores is absent, and it is absent inherently.

Switching the guard to `stagingKey` presence would refuse a legitimate pattern
— `warrentest.Replace[SomePort](memoryRepo)` inside a Postgres application —
and would produce a *driver-specific* error, which is the one thing
`ErrNoTransaction`'s exported-so-every-driver-agrees rationale forbids.

What is owed is one sentence in `MemoryRepository`'s doc comment saying that a
memory write inside another driver's transaction is not rolled back with it.
Nothing in the suite exercises the mix today, and nothing should be built to.

## Definition of done

- [ ] `MemoryRepository.Save` outside a `Do` returns `ErrNoTransaction("Save")`
      — byte-identical to Postgres's. Golden test asserting both drivers
      produce the same text.
- [ ] `MemoryRepository.Delete` outside a `Do` likewise.
- [ ] `FindByID` outside a `Do` still succeeds.
- [ ] A no-ID aggregate outside a `Do` reports the missing `NewAggregateRoot`,
      not the missing transaction — the ordering rule, pinned.
- [ ] `RunContract` and `RunVersionedContract` pass unchanged against both
      drivers.
- [ ] `warren.md:1490-1491` corrected; `persistence.go:142` left alone.
- [ ] `memory.go:497-500`, `:541` and `persistence_test.go:615-618` comments
      corrected in the same change (AGENT.md rule 4).
- [ ] `MemoryRepository`'s doc comment gains the mixed-driver sentence.

## Separate finding, surfaced by the probe and unrelated to this change

Three golden diagnostics embed the **checkout directory's basename**:
`testdata/export_without_provider.golden:3` reads
`module "user" (warren/warren_test.go:NN)`, and
`testdata/controller_registers_nothing.golden` is the same shape. Verified in
the repository as it stands. `TestExportWithoutProvider`,
`TestAComponentThatTakesTheLifecycleAndIsNeverBuiltFailsTheBoot` and
`TestControllerWithoutRegisterFailsTheBoot` therefore fail in any checkout not
named `warren` — a fork, a CI job that clones into `src`, or a contributor with
`warren-fix` on disk. Not caused by anything here; worth its own one-line fix
(normalise the module-relative path before rendering).
