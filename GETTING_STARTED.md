# Getting started

A complete Warren service, from nothing to a running HTTP API, in one page.

Every line here compiles and runs against the current tree — the output blocks
are real terminal output, not illustrations. If something in this file stops
being true, that is a bug: [warren.md](warren.md) is the design and
[AGENT.md](AGENT.md) is the rules, but **this** is the page that has to work.

> **Pre-release.** Warren is not tagged yet, so your `go.mod` needs a `replace`
> pointing at a local checkout. That goes away at v0.1.

---

## What you are building

A notes service: `POST /notes` and `GET /notes/{id}`, with validation, the
error table, health probes and a graceful drain. Four layers, one module.

```
internal/notes/
    domain/            the model and the ports — imports nothing else
    application/       the use cases — imports domain
    infrastructure/    the adapters — imports domain
    controller.go      the wiring — the ONLY file that sees all four
cmd/notes/main.go      the composition root
```

That layering is not a convention here. `warren lint arch` fails the build
when `domain/` imports `infrastructure/`.

---

## 1. The module

```
mkdir -p notes/internal/notes/{domain,application,infrastructure} notes/cmd/notes
cd notes
```

`go.mod`:

```go
module example.com/notes

go 1.26.3

require (
	github.com/MerseniBilel/warren v0.1.0
	github.com/MerseniBilel/warren/transport/http v0.1.0
)

// Until v0.1 is tagged:
replace github.com/MerseniBilel/warren => /path/to/warren
replace github.com/MerseniBilel/warren/transport/http => /path/to/warren/transport/http
```

Two requires. That is the whole dependency budget for an HTTP service — the
HTTP adapter is `net/http` and nothing else, and `dig` arrives indirectly and
is never yours to import.

---

## 2. The domain — `internal/notes/domain/note.go`

```go
package domain

import "context"

type Note struct {
	ID   string
	Text string
}

// Repository is the port. The application layer depends on this; only
// infrastructure implements it.
type Repository interface {
	Save(ctx context.Context, n Note) error
	Find(ctx context.Context, id string) (Note, error)
}
```

No framework import at all. That is the point.

---

## 3. The use case — `internal/notes/application/write.go`

```go
package application

import (
	"context"

	"github.com/MerseniBilel/warren/errors"

	"example.com/notes/internal/notes/domain"
)

// The tags are the whole contract with the edge: `json` for the body,
// `validate` for what must be present, `param` and `query` for the URL.
// Warren binds and validates before your code runs.
type WriteNote struct {
	ID   string `json:"id"   validate:"required"`
	Text string `json:"text" validate:"required"`
}

type NoteView struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type writeNote struct{ notes domain.Repository }

// Constructors wire; they never acquire. A connection or a goroutine belongs
// in a lifecycle hook, whose rollback the framework guarantees.
func NewWriteNote(notes domain.Repository) *writeNote { return &writeNote{notes: notes} }

func (h *writeNote) Handle(ctx context.Context, cmd WriteNote) (NoteView, error) {
	if _, err := h.notes.Find(ctx, cmd.ID); err == nil {
		return NoteView{}, errors.Conflict("note %s already exists", cmd.ID)
	}
	n := domain.Note{ID: cmd.ID, Text: cmd.Text}
	if err := h.notes.Save(ctx, n); err != nil {
		return NoteView{}, err
	}
	return NoteView{ID: n.ID, Text: n.Text}, nil
}
```

**Read what is absent.** No `http.ResponseWriter`, no status code, no JSON, no
router. `errors.Conflict` becomes 409 over HTTP, `AlreadyExists` over gRPC, and
an ack on a queue — because [one table](AGENT.md) owns that mapping and your
handler is not in it.

The read side is the same shape, and shows path binding:

```go
// `param:"id"` matches the "{id}" wildcard in the route pattern.
type ReadNote struct {
	ID string `param:"id"`
}

func (h *readNote) Handle(ctx context.Context, q ReadNote) (NoteView, error) {
	n, err := h.notes.Find(ctx, q.ID)
	if err != nil {
		return NoteView{}, err
	}
	return NoteView{ID: n.ID, Text: n.Text}, nil
}
```

---

## 4. The infrastructure — `internal/notes/infrastructure/memory.go`

```go
func NewMemoryNotes() domain.Repository {
	return &memoryNotes{m: map[string]domain.Note{}}
}

func (r *memoryNotes) Find(_ context.Context, id string) (domain.Note, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.m[id]
	if !ok {
		return domain.Note{}, errors.NotFound("note", id)
	}
	return n, nil
}
```

**Declare the return type as the PORT**, not the struct. A constructor
returning `*memoryNotes` does not satisfy a module that exports
`domain.Repository`, and the boot will tell you so.

---

## 5. The controller and the module — `internal/notes/controller.go`

```go
package notes

type Controller struct {
	write app.Handler[application.WriteNote, application.NoteView]
	read  app.Handler[application.ReadNote, application.NoteView]
}

func NewController(
	write app.Handler[application.WriteNote, application.NoteView],
	read app.Handler[application.ReadNote, application.NoteView],
) *Controller {
	return &Controller{write: write, read: read}
}

// Register declares the routes. One method, and every protocol you add later
// reads from the same table.
//
// The pattern is the PATH ALONE — Post already names the method. Post
// defaults to 201, Get to 200, Delete to 204.
func (c *Controller) Register(r transport.Registrar) {
	transport.Post(r, "/notes", c.write)
	transport.Get(r, "/notes/{id}", c.read)
}

// Declared ONCE. Modules are deduplicated by identity, so a plain factory
// called by two importers would be two modules sharing a name — a boot error.
// sync.OnceValue reads like a function and yields one identity.
var Module = sync.OnceValue(func() warren.Module {
	return warren.NewModule("notes",
		warren.Providers(
			infrastructure.NewMemoryNotes,
			func(r domain.Repository) app.Handler[application.WriteNote, application.NoteView] {
				return application.NewWriteNote(r)
			},
			func(r domain.Repository) app.Handler[application.ReadNote, application.NoteView] {
				return application.NewReadNote(r)
			},
		),
		warren.Controllers(NewController),
	)
})
```

Three rules worth burning in:

- **`Controllers`, not `Providers`.** Only `Controllers` and `Consumers` are
  registered at boot step 5. A controller under `Providers` would register no
  routes — so the boot refuses it by name rather than letting you ship a dead
  service.
- **A provider is private to its module** unless the module also
  `Exports[T]()` it. This is real encapsulation, not one global container.
- **`NewModule` returns an inert value.** Nothing registers on construction;
  the bootstrapper walks the whole graph first. That ordering is what makes
  cycle detection and encapsulation checkable rather than emergent.

---

## 6. `main.go`

```go
func main() {
	app := warren.New(
		notes.Module(),
		whttp.Server(
			whttp.Port(8080),
			whttp.ReadTimeout(30*time.Second),
		),
	)
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
```

`Run` boots and blocks until SIGINT or SIGTERM, then runs the shutdown
sequence: **readiness closes first**, the load balancer is given `DrainDelay`
to notice, in-flight requests finish, and only then do pools close. That
ordering is the one most hand-rolled Go services get backwards.

---

## 7. Run it

```
$ go build ./... && go vet ./... && go run ./cmd/notes
INFO http server listening addr=[::]:8080 module=warren/transport/http tls=false
```

```
$ curl -i -X POST localhost:8080/notes -d '{"id":"n1","text":"hello"}'
HTTP/1.1 201 Created
Content-Type: application/json; charset=utf-8
X-Correlation-Id: 1c0da3cfa6f9-1

$ curl localhost:8080/notes/n1
{"id":"n1","text":"hello"}
```

The failure paths, none of which you wrote:

```
$ curl -X POST localhost:8080/notes -d '{"id":"n2"}'
{"error":{"code":"INVALID","message":"field text is invalid",
          "details":{"text":"is required"},"correlation_id":"…-5"}}

$ curl localhost:8080/notes/nope
{"error":{"code":"NOT_FOUND","message":"note nope not found","correlation_id":"…-6"}}

$ curl -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/notes -d '{"id":"n1","text":"again"}'
409

$ curl -o /dev/null -w '%{http_code}\n' localhost:8080/readyz
200
```

`/healthz` and `/readyz` came free, and they bypass your middleware — a probe
every two seconds should not be a span, an audit line and a rate-limit
decision.

---

## 8. What to reach for next

| You want | Use |
|---|---|
| Edge middleware (CORS, real IP) | `whttp.Middleware(...)` — the stdlib `func(http.Handler) http.Handler` shape, so `chi/v5/middleware` works unmodified |
| Authorization on one route | `transport.Guard(policy)` — runs **before** decode, so a denied caller's malformed body is a 403, not a 400 |
| Cross-cutting logic on every protocol | `app.Chain` / core middleware — wraps the handler, so it applies to HTTP, gRPC and consumers identically |
| File upload, download, SSE, WebSocket | `transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", h)` from your controller — note the pattern carries the method here |
| `pprof`, static assets, a webhook receiver | `whttp.Handle("GET /debug/pprof/", h)` — for handlers needing no module dependency |
| A test that boots the app | `warren/testing` — `NewModuleTest`, `Replace`, `Invoke` |
| Scaffolding the next feature | `warren new` and `warren g` — see the CLI's skills |

---

## When boot fails

It will, and that is the design: **every error the framework can detect
surfaces at boot, never on request 1.** The diagnostics are meant to be
copy-pasteable. A real one, in full:

```
✗ cannot resolve dependency

    domain.Repository
      └─ required by *application.writeNote
           └─ required by *notes.Controller
                └─ declared in internal/notes/controller.go:44

  No provider found in scope "notes" or its imports.

  Did you mean:
    • infrastructure.NewMemoryNotes is registered in scope "storage" but not exported.
      Add to storage's module: warren.Exports[domain.Repository]()
    • Or provide it locally:  warren.Providers(infrastructure.NewMemoryNotes)
```

If a diagnostic ever tells you something that does not fix your problem, that
is a bug worth reporting — the error messages are a deliverable here, and they
have golden-file tests to prove it.
