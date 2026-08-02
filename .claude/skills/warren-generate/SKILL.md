---
name: warren-generate
description: Use when adding a feature module, aggregate, use case, repository, or event consumer to a Warren application. Covers `warren g` — what each generator writes, what it wires, and how to recover when one refuses.
---

# Generating code in a Warren application

`warren g` writes the code a feature needs **and wires it into the module
that owns it**. The wiring is the part a template alone cannot do, and the
part people forget — so prefer the generator over hand-writing the file,
even when the file looks trivial.

Run it from the project root (the directory holding `go.mod`), or pass
`--dir`.

## The five generators, in the order you use them

| Command | Writes | Wires |
|---|---|---|
| `warren g module <name>` | `internal/modules/<name>/` with `module.go`, `module_test.go`, and a `doc.go` for each of the three layers | the module into `warren.New(...)` in `cmd/*/main.go` |
| `warren g entity <module> <Name>` | the aggregate, its ID, its first event, and its repository **port**, in `domain/` | nothing — an aggregate is not a provider |
| `warren g repository <module> <Name>` | the in-process implementation in `infrastructure/` | the constructor into `warren.Providers(...)` |
| `warren g command <module> <Name>` | an `app.Handler` and its test, in `application/` | the constructor into `warren.Providers(...)` |
| `warren g consumer <module> <EventName>` | the handler in `application/`, and the subscription beside `module.go` | both the handler **and** the subscription (`warren.Consumers(...)`) |

`entity` before `repository`: the repository implements the port the
aggregate declares, so the port has to exist first.

`--topic` overrides a consumer's topic, which otherwise comes from the event
name in dotted lower case — `OrderPlaced` subscribes to `order.placed`.

Flags on every generator: `--dir`, `--dry-run`, `--force`.

## What the generators guarantee

- **Atomic.** Every target is checked and every edit computed before
  anything is written. A refused run leaves no partial state — not even a
  `module.go` edit.
- **Never silently overwritten.** A colliding name is an error listing every
  file involved. There is no prompt, because a prompt cannot run in CI.
- **Comment-preserving.** Wiring is spliced into the bytes of the file at a
  point located through the AST; the file is never reprinted, so the
  comments and formatting in a hand-edited `module.go` survive.
- **`--force` does not duplicate wiring.** It overwrites the files, but an
  argument already present in `warren.Providers(...)` is left alone.

## When a generator refuses

**"these files already exist"** — the generator ran before, or the name
collides with something hand-written. Nothing was written. Either delete the
listed files, pick another name, or pass `--force` to overwrite them. Do not
work around it by hand-writing the file under a different name; you will end
up with two half-wired versions.

**"no such module"** — the diagnostic lists the modules the project has.
Create it with `warren g module <name>` first.

**"no module declaration found"** / **"no `warren.New(...)` call found"** —
the file being edited is not shaped like a scaffolded project. This happens
in a tree that was hand-built or heavily refactored. Add the provider by
hand and carry on; the generator is not able to guess where it goes.

## After generating

The generated code compiles and passes `go vet`, `go test` and
`warren lint arch` as written, but it is a **skeleton with intent**, not a
finished feature:

- an aggregate ships with one field and one event — add the fields the
  invariants need, and put every state change behind a method;
- a use case returns its input and does nothing — inject the repository into
  its `New...Handler` and the container will supply it;
- a consumer's event type is the module's own local view, decoded from the
  wire. Do **not** replace it with an import of the publishing module's Go
  type: the contract between modules is the wire format, and that is what
  makes extracting a module into its own service a wiring change rather
  than a rewrite.

Then run the module's generated `module_test.go`. It boots the whole graph
and catches the one wiring mistake the compiler cannot see: a constructor
asking the container for a type no module exports.

## `warren g command` does not expose the route

The generator writes the use case and provides it from the module, so the
container can build it — but nothing serves it until you say so. It prints
the three lines to add, and they go in the feature's `controller.go`:

```go
type Controller struct {
    suspendUser app.Handler[application.SuspendUser, application.SuspendUserResult]
}

func NewController(
    suspendUser app.Handler[application.SuspendUser, application.SuspendUserResult],
) *Controller { … }

func (c *Controller) Register(r transport.Registrar) {
    transport.Post(r, "/suspend_user", c.suspendUser)
}
```

The generator cannot write them: it would have to edit a struct, a
constructor and a method body it did not create. Forgetting them is not
silent — the handler simply has no route — but it is the one part of `g
command` that is not wiring you get for free.

**The pattern is the PATH ALONE.** `transport.Post` already names the method;
`transport.Post(r, "POST /x", h)` fails the boot. Only `transport.Raw` takes
`"METHOD /path"`, because it names no method of its own.

## `warren g repository --driver postgres`

Writes plain SQL over `postgres.DB`, the table's migration, and
`cmd/migrate/main.go`. It exists because a Postgres repository has three
rules **no compiler enforces**, and getting any of them wrong loses data
silently:

1. **`postgres.RequireTx(ctx, "save x")` first on every write.** Outside a
   unit of work the row autocommits while the aggregate's events stay pending
   on an object about to go out of scope — lost, with no error anywhere.
2. **`r.db(ctx)` for the handle**, never a stored pool. It returns the
   ambient transaction if one is in scope and the pool otherwise, which is
   what lets a read work outside a unit of work while a write does not.
3. **`persistence.Track(ctx, agg)` after a successful write.** That enlists
   the aggregate so its events reach the outbox in the same commit.

A `Delete` must also check rows-affected and return `NOT_FOUND` when it
matched nothing: a bare `DELETE … WHERE id = $1` returns nil for a missing
row, and the persistence contract suite deletes twice and requires the second
to fail.

**It generates, it does not wire.** The repository needs `postgres.DB`, which
means `main.go` must add `postgres.Module(postgres.DSN(...))` and the feature
module must `warren.Imports` it — a provider is private to its module. The
generator prints those steps; it cannot perform them.

## Migrations are a deploy step

`cmd/migrate` is generated alongside, and applies Warren's tables then the
project's:

```
DATABASE_URL=... go run ./cmd/migrate
applied  warren       (warren_outbox, warren_inbox)
applied  db/migrations
```

**Warren never migrates at boot and offers no option to.** Under a rolling
deploy that races every replica: all but one block past their readiness
deadline and get killed, the winner applies DDL the still-serving old
replicas were not written against, and one bad file crash-loops everything at
once.

**Name migration files for your own aggregates.** Yours and Warren's record
into one `warren_schema_migrations` table keyed by bare filename, so a file
of yours called `00001_warren_outbox.sql` would be marked applied without
ever running. `00001_orders.sql` cannot collide.
