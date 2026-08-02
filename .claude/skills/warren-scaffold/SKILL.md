---
name: warren-scaffold
description: Use when creating a new Warren service with `warren new`, or when working out what a scaffolded project contains and why. Covers the generated tree, the platform module, and what does not exist yet.
---

# Scaffolding a Warren service

```
warren new <name> --module github.com/acme/<name>
```

`--module` is the Go module path of the new service and you should always
pass it; `--dir` defaults to the app's name. `--db` and `--broker` both
default to `memory`, which is the only driver that exists today.

The result compiles, vets and passes `go test ./...` **as generated**. If it
does not, that is a bug in the CLI, not something to work around.

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

## What does not exist yet

**There is no HTTP or gRPC adapter.** A scaffolded service has no server and
no port to curl. This is expected, and the generated `README.md` says where
the server goes when it ships. Drive use cases through the framework's
testing helpers instead — `warrentest.NewModuleTest` boots the real graph,
and `warrentest.Invoke` calls a handler through it.

Do not hand-roll a `net/http` server against the handlers to "get something
working": handlers import no transport package by invariant, and a
hand-rolled server is exactly the code the adapter will replace.

## Working in a scaffolded project before the framework is published

The framework is not on a module proxy yet, so a generated `go.mod` will not
resolve. Write a `go.work` in the app directory pointing at your checkout:

```
go 1.26.3

use (
	.
	/path/to/warren
)
```

Never commit a `replace` directive into the app's `go.mod` — invariant 8
forbids it in this repository, and it is the wrong habit to teach in a
generated project.

## Then what

Add features with `warren g` (see the `warren-generate` skill). Check the
architecture with `warren lint arch` (see `warren-lint`).
