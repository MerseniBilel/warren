# `warren/openapi` — SPEC

| | |
|---|---|
| **Status** | Draft — not approved |
| **Source** | [warren.md §4.3](../warren.md) |
| **Module** | own module (`warren/openapi`) |
| **Mode** | Build |
| **Wraps** | — |

> **This spec is thin on purpose.** §4.3 is three sentences and fixes no Go
> surface. Everything below either quotes it or marks the gap as a question.
> Padding it into something that looks complete would be inventing the design,
> which is the human's call (AGENT.md § When you are unsure).

## Problem

Hand-written OpenAPI drifts. Annotation comments drift more quietly, because the
compiler never reads them. Warren already holds both halves of the answer at
boot: the route registrations built at step 5 (§1.3), and the DTO struct tags —
`json:` and `validate:` — that the transport adapters already use to decode and
validate (§2.7). The document should be derived from those, not maintained
alongside them.

**The claim, in §4.3's words: "No annotations, no separate spec file to drift."**
That is the whole product of this package. A design that requires either has
failed.

## Goals

Everything §4.3 states, and nothing more:

- Read **route registrations** plus **DTO struct tags, including `validate:`
  constraints**, and emit **OpenAPI 3.1**.
- Serve **Scalar/Swagger UI at `/docs`**.
- Support **`warren openapi export > openapi.yaml`** for CI.

## Non-goals

- **No annotations.** No magic comments, no `// @Summary`, no doc-comment DSL.
- **No checked-in source spec.** The Go is the source; an `openapi.yaml` in the
  repository is an *output*. Whether that output is committed at all, and
  whether CI regenerates it, is Open question 9.
- **No import of a transport adapter.** Adapters never import each other
  (invariant 4, §1.6), and §1.6 gives this package its own module with no
  third-party dependency. Route registrations must therefore reach it through
  the contracts ring (`warren/transport`, §3.5) or through the CLI's analyzer
  (§8) — warren.md does not say which. See Open questions 2.
- **Not a validator or a request router.** `warren/validate` owns constraint
  enforcement (§2.7); this package only *describes* the constraints it finds.

## Public API

**warren.md fixes no Go surface for this package.** §4.3 names two entry points
and no types, functions, or options. Writing signatures here would contradict
AGENT.md's rule that the spec's public API section is the contract under
review — a contract nobody has agreed yet.

What §4.3 does fix:

| Entry point | Owner | Source |
|---|---|---|
| `GET /docs` — Scalar/Swagger UI | this package, served over HTTP | §4.3 |
| `warren openapi export > openapi.yaml` | `warren/cli` (§8), reading this package | §4.3 |

Every sibling adapter exposes a `warren.Module` constructor plus functional
options — `http.Server(...)`, `grpc.Server(...)`, `postgres.Module(...)`,
`config.Module[T](...)`. Whether this package follows that pattern is Open
question 1, not a decision this spec takes.

## Behaviour

**If generation is a runtime concern, it runs at boot, not per request.** The
inputs exist after step 5 of §1.3, when controllers have built the route tables
in memory, and reflection over struct tags is a boot-time activity
(invariant 7). But warren.md never says when generation runs, and Open question
2(b) proposes the CLI's static `go/packages` analyzer instead — which is not
boot at all. Serving `/docs` from an already-built document follows only from
the runtime reading.

**What it reads**:

- Route registrations — method, path, and the `Req`/`Res` types behind each
  `r.HTTP()` registration (§3.5).
- DTO struct tags — `json:` for names and shapes, `validate:` for constraints.
  §2.7's example is the whole input:

  ```go
  type RegisterUser struct {
      Email string `json:"email" validate:"required,email"`
      Name  string `json:"name"  validate:"required,min=2,max=64"`
  }
  ```

  `required` is a required property; `email` is a format; `min`/`max` are length
  constraints. The full tag-to-schema mapping is Open question 6.

**What it emits**: OpenAPI 3.1.

**Must never**: import `warren/transport/http` or any other adapter
(invariant 4); require an annotation; require a hand-maintained spec file; run
generation on the request path.

## Error mapping

**None.** This package owns no transport column of §2.6. `warren/transport/http`
owns HTTP, `warren/transport/grpc` owns gRPC, the broker adapters own the
consumer column. Whether the *documented* responses should include the status
codes those mappings produce — a 404 on every route whose handler can return
`NotFound` — is Open question 5, and it is a documentation question, not a
mapping one.

## Escape hatch

**None, and none is needed.** Mode is Build: there is no wrapped library to
escape to. A user who wants a document this package will not produce edits the
emitted YAML downstream, which costs nothing at runtime because the document is
an artefact rather than a contract the server enforces.

## Testing

- **Unit tests: no Docker, no network, no sleeps** (AGENT.md § Testing).
  Generation is a pure function from registrations and types to a document.
- **Golden-file tests are the primary test form here.** The emitted document for
  a fixed set of routes and DTOs is compared byte-for-byte against a committed
  golden file — this is the same rule that covers generators and error text, and
  a spec generator breaks silently without it. Every `validate:` constraint that
  the tag mapping supports gets a row.
- **Integration tests behind `//go:build integration`** — serving `/docs` over a
  real listener, and any end-to-end run of `warren openapi export`.
- **Schema validity.** The output is checked against the OpenAPI 3.1 meta-schema,
  not merely against a golden file, so a golden update cannot silently bless an
  invalid document.
- **No allocation benchmark on a request path**, because there is no request
  path: generation runs at boot, and `/docs` serves a prebuilt document.

## Definition of done

1. OpenAPI 3.1 emitted from route registrations plus `json:`/`validate:` tags,
   with zero annotations and no source spec file anywhere in the repository.
2. `/docs` serves Scalar or Swagger UI (which one is Open question 5) over the
   generated document.
3. `warren openapi export` writes the document to stdout, suitable for
   `> openapi.yaml` in CI.
4. Golden-file tests for the emitted document, plus meta-schema validation.
5. `go.mod` for the module contains the core module and nothing else that is not
   stdlib — §1.6 records no third-party dependency for this package. If one is
   needed (YAML, JSON Schema, the UI bundle), it goes through AGENT.md § Adding
   a dependency first and is recorded in the §9 ledger.
6. No import of `warren/transport/http` or any other adapter.
7. This spec corrected in the same pull request wherever the implementation
   diverged.

## Open questions

For the human. §4.3 is three sentences; most of a normal spec is genuinely
undecided here, and none of it should be guessed.

1. **What is the public API?** Is this a `warren.Module` in `main.go`
   (`openapi.Module(...)`, matching every sibling), a library called by the CLI,
   or both? Nothing in warren.md says, and §10's `main.go` does not include it.
2. **How does it obtain route registrations without importing an adapter?** This
   is the structural question of the package. Two candidates: (a) the runtime
   exposes registrations through the contracts ring (`warren/transport`, §3.5),
   which keeps invariant 4 intact but means the core module grows a
   documentation-shaped API; (b) the CLI's shared `go/packages` analyzer (§8)
   derives them statically, which fits "build-time only" but cannot serve
   `/docs` from a running process. §4.3 asks for **both** `/docs` and
   `warren openapi export`, which may mean both paths are needed.
3. **Which ring does this package belong to?** §1.6 gives it a module; the §1.1
   ring diagram does not list it. It has no driver, so it is not obviously an
   adapter; it serves an HTTP endpoint, so it is not tooling either.
4. **`/docs` is an HTTP path.** Serving it needs a router, which lives in
   `warren/transport/http` — an adapter this package may not import (invariant
   4). Does the HTTP adapter mount a document this package produces, inverting
   the dependency? That would satisfy the invariant, but it is a boundary
   decision.
5. **Scalar or Swagger UI?** §4.3 writes "Scalar/Swagger UI" without choosing.
   Both ship a JavaScript bundle that has to be embedded, which is a dependency
   decision under AGENT.md § Adding a dependency and would be the module's first.
6. **The `validate:` tag → JSON Schema mapping is not specified.** `required`,
   `email`, `min`, `max`, and `oneof` appear in warren.md examples; the
   validator supports far more. Which subset is supported, and what happens on
   an unmappable constraint — omit, warn, or fail the build?
7. **gRPC and events.** §4.3 says "route registrations". §3.5's `Registrar` has
   three faces — `HTTP()`, `GRPC()`, `Events()`. OpenAPI describes HTTP; does
   this package ignore the other two, and is an AsyncAPI equivalent for
   `Events()` in scope later?
8. **`warren openapi export` is not in §8's command surface,** which lists
   `new`, `g`, `lint`, `doctor`, `graph`, `explain`, `add`, `migrate`, and
   `extract`. The manifest should be reconciled before the CLI spec commits to
   it.
9. **Is the exported document checked in?** "for CI" suggests a diff check
   against a committed file, which would reintroduce exactly the artefact §4.3
   says should not exist. Generate-and-compare in CI, or publish-only?
