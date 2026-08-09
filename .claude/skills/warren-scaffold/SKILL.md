---
name: warren-scaffold
description: Use when creating a new Warren service with `warren new`, or when working out what a scaffolded project contains and why. Covers the generated tree, the platform module, and what does not exist yet.
---

# Scaffolding a Warren service

```
warren new <name> --module github.com/acme/<name>
```

`--module` is the Go module path of the new service and you should always
pass it; `--dir` defaults to the app's name. `--db` takes `memory` or
`postgres`, `--broker` takes `memory` or `kafka`, and both default to
`memory`; anything else is refused by name rather than silently ignored.

The result compiles, vets and passes `go test ./...` **as generated**, after
`go mod tidy` and nothing else. If it does not, that is a bug in the CLI, not
something to work around.

## What you get, and why it is shaped that way

```
cmd/<name>/main.go          warren.New(...).Run()
internal/config/            the app's own Config struct
internal/platform/          the shared infrastructure module
internal/modules/user/      a worked feature, all four layers
internal/modules/notification/   a worked consumer
```

**`internal/platform`** is where the broker, the unit of work, the outbox and
its relay, and health are wired. Swapping an in-process driver for a real
one is a change to that file and nothing else — that is the whole point of
the ports the rest of the app is written against. Read it before writing any
infrastructure by hand.

**A feature module** owns four layers, and `module.go` is the only file
permitted to see all of them. `domain` imports nothing from the others;
`warren lint arch` enforces that.

**Every module is declared with `sync.OnceValue`.** Modules are deduplicated
by identity, so a plain factory called by two importers would be two modules
sharing one name — a boot error. Copy the pattern; do not "simplify" it to a
plain function.

## What the scaffold serves

**A scaffolded service serves HTTP.** `cmd/<name>/main.go` wires
`whttp.Server(whttp.Port(8080))`, the `user` module has a `controller.go`
registering `POST /users`, and the health probes come with the adapter:

```
POST /users     the RegisterUser use case          201
GET  /healthz   liveness — runs no checks          200
GET  /readyz    readiness — lifecycle + checks     200 / 503
```

Every response carries `X-Correlation-Id`, and a validation failure is a 400
with per-field `details` — none of which the generated code writes.

**There is still no gRPC adapter**, and `warren new` has no `--persistence
postgres` path: a scaffolded repository is in-memory. Adding Postgres is
manual today — `postgres.Module(postgres.DSN(...))` in `main.go` and a
repository following the three rules in warren.md §6.1.

**Do not hand-roll a `net/http` server against the handlers.** Handlers
import no transport package by invariant; register routes from the feature's
`controller.go` with `transport.Post(r, "/path", c.handler)` and let the
adapter serve them.

## Logs you can join to a request

`main.go` installs `log.Handler`, so every record carries the correlation ID.
**Use the `*Context` methods** — `log.FromContext(ctx).InfoContext(ctx, …)`.
slog's plain `Info` passes `context.Background()` and silently drops every
correlation field.

## Resolving the framework

`go mod tidy` in the new directory. That is all: every framework module is
published, the generated `go.mod` requires the version the CLI itself was
built from, and there is no `replace` in it.

**Do not add one.** A `replace` pins the project to one machine's filesystem
and it is the wrong habit to teach in a generated project — invariant 8
forbids one committed to Warren's own repository for the same reason. If a
require does not resolve, the version is wrong; say so rather than working
around it.

**The exception is developing Warren itself.** `warren new … --framework
/path/to/warren` writes the replace directives that point the new app at a
checkout, so a framework change is exercised by a real service before it is
tagged. `warren new` says so in its output when the flag was used, because a
machine-specific `go.mod` nobody mentioned is the next person's bug report.

## Then what

Add features with `warren g` (see the `warren-generate` skill). Check the
architecture with `warren lint arch` (see `warren-lint`).
