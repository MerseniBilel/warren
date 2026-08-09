# Getting started

A complete Warren service, from nothing to a running HTTP API, in one page.

Every line here compiles and runs against the current tree — the output blocks
are real terminal output, not illustrations. If something in this file stops
being true, that is a bug: [warren.md](warren.md) is the design and
[AGENT.md](AGENT.md) is the rules, but **this** is the page that has to work.

> **Pre-release.** Warren is not tagged yet, so your `go.mod` needs a `replace`
> pointing at a local checkout. That goes away at v0.1.

> **Getting the `warren` binary.** Untagged means `go install
> github.com/MerseniBilel/warren/cli/cmd/warren@latest` has nothing to fetch,
> so build it from your checkout:
>
> ```
> cd cli && go build -o ~/.local/bin/warren ./cmd/warren
> ```
>
> The `cd cli` is required, not tidiness: the CLI is **its own module**, so
> from the repository root `go build ./cli/cmd/warren` answers `main module
> (github.com/MerseniBilel/warren) does not contain package …/cli/cmd/warren`.
> It appears to work in a tree where `make` has run, because the generated
> `go.work` joins the modules — and `go.work` is never committed, so it does
> not exist in a fresh clone.
>
> For the same reason `go run ./cmd/warren …` works only from inside `cli/`:
> `go run` resolves its package argument against the *current* module, so
> running it from the directory you want to scaffold into fails with `go:
> go.mod file not found in current directory or any parent directory`. A field
> test hit that on the literal first command. This page itself needs no CLI —
> it is written by hand throughout — but §"Scaffolding the next feature" does.

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
$ curl -i -X POST localhost:8080/notes -H 'Content-Type: application/json' -d '{"id":"n1","text":"hello"}'
HTTP/1.1 201 Created
Content-Type: application/json; charset=utf-8
X-Correlation-Id: 1c0da3cfa6f9-1

$ curl localhost:8080/notes/n1
{"id":"n1","text":"hello"}
```

**The `Content-Type` header is not decoration.** `curl -d` sends
`application/x-www-form-urlencoded`, which a JSON route cannot decode, so
Warren answers `415` naming both media types rather than guessing at your
body. `curl --json` is the shorter spelling if your curl is 7.82 or newer.
This page printed the `-d` form without the header for a while, and every
block below it was wrong in the same way — the framework's own
`TestFormEncodedPostIsRefused` had been asserting the 415 the whole time.

The failure paths, none of which you wrote:

```
$ curl -X POST localhost:8080/notes -H 'Content-Type: application/json' -d '{"id":"n2"}'
{"error":{"code":"INVALID","message":"field text is invalid",
          "details":{"text":"is required"},"correlation_id":"…-5"}}

$ curl localhost:8080/notes/nope
{"error":{"code":"NOT_FOUND","message":"note nope not found","correlation_id":"…-6"}}

$ curl -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/notes \
       -H 'Content-Type: application/json' -d '{"id":"n1","text":"again"}'
409

$ curl -o /dev/null -w '%{http_code}\n' localhost:8080/readyz
200
```

`/healthz` and `/readyz` came free, and they bypass your middleware — a probe
every two seconds should not be a span, an audit line and a rate-limit
decision.

---

## 7b. Logs you can join to a request

The service already puts a correlation ID on every response
(`X-Correlation-Id`) and in every error body. Getting it onto your **log
lines** is one line in `main`, and it is worth doing on day one:

```go
slog.SetDefault(slog.New(log.Handler(slog.NewJSONHandler(os.Stdout, nil))))
```

`log.Handler` resolves the correlation ID when a record is emitted, so a
request that logs nothing pays nothing for this. Then, in a handler:

```go
log.FromContext(ctx).InfoContext(ctx, "note saved", "id", n.ID)
```

**Use the `*Context` methods.** `InfoContext`, `ErrorContext`, `WarnContext`.
Passing the context twice looks redundant and is not — slog's plain `Info`
passes `context.Background()`, so it resolves nothing and the record silently
loses every field that would let you join it to the request:

```json
{"level":"INFO","msg":"note saved","id":"n1"}                          // .Info
{"level":"INFO","msg":"note saved","id":"n1","correlation_id":"…-1"}   // .InfoContext
```

Add `warren/observability` later and the same line starts carrying `trace_id`
and `span_id` too — pass `observability.LogAttrs()` as a second argument to
`log.Handler`.

---

## 8. Swapping the map for Postgres

The in-memory repository above is a real implementation of the port, and
replacing it changes **no use case and no controller** — only the module's
provider list. Add two requires:

```go
require github.com/MerseniBilel/warren/persistence/postgres v0.1.0
```

### The repository

Two rules, and the first is what stops events being silently lost:

```go
type postgresNotes struct{ db postgres.DB }

func NewPostgresNotes(db postgres.DB) domain.Repository { return &postgresNotes{db: db} }

func (r *postgresNotes) Save(ctx context.Context, n domain.Note) error {
	// 1. RequireTx FIRST. Outside a unit of work the row would autocommit —
	//    and if this type raised events, they would stay pending on an object
	//    about to go out of scope and be lost silently, with no outbox row.
	//
	//    Note is a plain record, not an aggregate, so RequireTx is the whole
	//    of the rule here. A repository that saves an AGGREGATE uses
	//    persistence.Write instead, which makes this same check AND enlists
	//    the aggregate when the write succeeds — one call, so there is no
	//    separate Track statement to forget. That is what
	//    `warren g repository --driver postgres` writes.
	if err := postgres.RequireTx(ctx, "save note"); err != nil {
		return err
	}
	_, err := r.db(ctx).Exec(ctx,
		`INSERT INTO notes (id, text) VALUES ($1, $2)
		 ON CONFLICT (id) DO UPDATE SET text = EXCLUDED.text`, n.ID, n.Text)
	return err
}

func (r *postgresNotes) Find(ctx context.Context, id string) (domain.Note, error) {
	var n domain.Note
	err := r.db(ctx).QueryRow(ctx, `SELECT id, text FROM notes WHERE id = $1`, id).Scan(&n.ID, &n.Text)
	if err != nil {
		if errors.Is(err, postgres.ErrNoRows) {   // re-exported: no pgx import
			return domain.Note{}, werrors.NotFound("note", id)
		}
		return domain.Note{}, err
	}
	return n, nil
}
```

`r.db(ctx)` is the only piece of framework machinery here, and it does one
thing: return the transaction if `UnitOfWork.Do` put one on the context, and
the pool otherwise. **Reads work outside a transaction; writes are refused** —
with a diagnostic that explains why.

3. A `Delete` must check rows-affected and return `NOT_FOUND` when it matched
   nothing. A bare `DELETE ... WHERE id = $1` returns nil for a missing row,
   and silent success hides bugs.

### The wiring

```go
warren.NewModule("notes",
	warren.Imports(pg),                       // pg is the postgres module value
	warren.Providers(NewPostgresNotes, ...),
	warren.Controllers(NewController),
)
```

A module that wants `postgres.DB` must **import** the postgres module — a
provider is private to its module unless exported. In `main`:

Declare it ONCE, in `internal/platform`, and let features import it. A module
factory that takes a module as an argument does not work: modules are
deduplicated by identity, so a factory called by two importers is two modules
sharing a name — a boot error. This is also the shape `warren g module`
generates, which expects `platform.Module()` to exist.

```go
// internal/platform/postgres.go
var Postgres = sync.OnceValue(func() warren.Module {
	return postgres.Module(
		postgres.DSN(os.Getenv("DATABASE_URL")),
		postgres.WithOutbox(),    // events land in warren_outbox, same commit
	)
})

// internal/modules/notes/module.go
var Module = sync.OnceValue(func() warren.Module {
	return warren.NewModule("notes",
		warren.Imports(platform.Postgres()),
		warren.Controllers(NewController),
	)
})

// cmd/app/main.go
warren.New(platform.Postgres(), notes.Module(), whttp.Server(whttp.Port(8080))).Run()
```

### The schema — a deploy step, never a boot step

**Warren never migrates at boot, and there is no option to make it.** Under a
rolling deploy that races every replica: N−1 block and miss their readiness
deadline, the winner applies DDL the still-serving old replicas were not
written against, and one bad file crash-loops every replica at once.

So it is two calls — Warren's tables and yours — from your deploy job:

```go
// cmd/migrate/main.go
ctx := context.Background()
dsn := os.Getenv("DATABASE_URL")
if err := postgres.Migrate(ctx, dsn, postgres.Schema); err != nil { log.Fatal(err) }
if err := postgres.Migrate(ctx, dsn, schema.FS);       err != nil { log.Fatal(err) }
```

`schema.FS` is your own `embed.FS` of numbered `.sql` files. Two things to
know:

- Files apply in **name order**, so zero-pad them: `00010` sorts after
  `00009`, `10` does not. `warren g repository --driver postgres` numbers its
  own files this way, continuing from the highest already in `db/migrations`.
- Both filesystems record into the **same** `warren_schema_migrations` table,
  keyed by bare filename. Name yours so they cannot collide with Warren's —
  `00001_notes.sql`, never `00001_warren_outbox.sql`, which would be recorded
  as applied without running.

The markers are goose's, so the same files work unchanged if your project
already runs goose, atlas or dbmate:

```sql
-- +goose Up
CREATE TABLE IF NOT EXISTS notes (id TEXT PRIMARY KEY, text TEXT NOT NULL);
```

Forget to run it and the boot tells you exactly that, naming the table and the
call.

### What `WithOutbox()` does and does not do

It writes an outbox row for every event your aggregates raise, **in the same
transaction as the state** — that atomicity is the whole pattern. It does not
publish them: draining the outbox to a broker is `outbox.NewRelay`, which you
wire when you have a broker. Until then the rows accumulate, which is the
correct behaviour but worth knowing.

---

## 9. What to reach for next

| You want | Use |
|---|---|
| Edge middleware (CORS, real IP) | `whttp.Middleware(...)` — the stdlib `func(http.Handler) http.Handler` shape, so `chi/v5/middleware` works unmodified |
| Authorization on one route | `transport.Guard(app.RequireScope("users:read"))` — runs **before** decode, so a denied caller's malformed body is a 401/403, not a 400 |
| Reading the caller | `id, ok := app.IdentityFromContext(ctx)` — seeded by your own edge middleware with `app.WithIdentity`. The ok-bool is the point: `IdentityFromContext(ctx).Subject` does not compile |
| Cross-cutting logic on every protocol | `app.Chain` / core middleware — wraps the handler, so it applies to HTTP, gRPC and consumers identically |
| Retrying a lost connection | `app.Retrying(broker.ExponentialBackoff(3))` — retries `UNAVAILABLE` only |
| **Contention on one aggregate** | `app.Retrying(p)` — no code list. A stale write is `CONTENTION`, which `Retrying` covers along with `UNAVAILABLE`. **Write it exactly like this**, and read the note under this table before you change the order: `app.Chain(h, app.Retrying(p), app.Transactional(uow))`. Your handler must also **RE-READ** the aggregate on each attempt — `Retrying` re-invokes the handler, not the transaction, so one closing over a stale aggregate contends for ever. `RetryingOn(p, errors.CodeConflict)` is a boot panic: a business refusal is refused identically on every attempt, and retrying it cost a measured 6 transactions and 1.25s against 7ms |
| Bounding a slow dependency | `app.Timeout(3*time.Second)` — inside `Retrying` bounds each attempt, outside bounds the sequence |
| File upload, download, SSE, WebSocket | `transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", h)` from your controller — note the pattern carries the method here |
| `pprof`, static assets, a webhook receiver | `whttp.Handle("GET /debug/pprof/", h)` — for handlers needing no module dependency |
| **Refusing a misspelled field** | `whttp.Codec(transport.StrictJSON())`. The default codec IGNORES unknown members, so a client sending `reorderPoint` for `reorder_point` gets a 201 and a record with the field it asked for left at zero. That default is deliberate — one codec decodes HTTP *and* events, and an INVALID on a consumer dead-letters without retry, so a producer adding a field would DLQ 100% of a consumer's traffic — but on an HTTP-only service strict is usually what you want |
| A test that boots the app | `warren/testing` — `NewModuleTest`, `Replace`, `Invoke` for a handler, and `Resolve[T]` for anything else the boot built (a repository, the publisher, a sweeper). `Resolve` returns the instance the boot made, not a second construction |
| A fast test suite | `whttp.DrainDelay(0)` — the 5s default is correct in production and costs 5s per test |
| Scaffolding the next feature | `warren new` and `warren g` — see the CLI's skills, and **build the binary first** (below) |

> **`app.Chain`'s first middleware is the OUTERMOST one.** So
> `app.Chain(h, app.Retrying(p), app.Transactional(uow))` gives each retry
> its own transaction, and swapping those two arguments wraps one transaction
> around the whole retry loop.
>
> **The reverse is now REFUSED at boot**, with a panic naming the fix. It
> used to compile, boot, pass every generated test and serve 201s — a field
> test wrote it straight from an earlier version of this table and measured
> *eight handler attempts for eight concurrent requests*: zero retries, and
> two callers got a 409 for stock that existed. With the arguments the right
> way round, the same test succeeded 8 of 8.
>
> An architect ruling settled that it is never legitimate. On Postgres it is
> worse than wasteful: the version check runs inside the handler, so the
> retry DOES run — in the same open transaction — and commits the failed
> attempt's staged writes alongside the successful one's. To retry one flaky
> outbound call rather than the whole handler, retry it in the adapter behind
> its port (warren.md §7.3).
>
> Note the failure is not always the one you would predict. Against the
> in-memory driver the version check runs at COMMIT, which is outside the
> retry loop in the wrong ordering — so `Retrying` sees no error at all and
> retries **zero** times, rather than spending its budget on a doomed
> transaction.

---

## Who is calling — identity and guards

**Warren does not authenticate. You do, in fifteen lines at the edge, and
Warren carries the result.** There is no JWT or OIDC verifier in v0.1 — that
is `warren/auth` in v0.2 — and nothing you write here changes when it lands.

Your middleware parses whatever credential you use and seeds the identity:

```go
func bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := verify(r.Header.Get("Authorization")) // your token library
		if err != nil {
			// The framework's own error envelope — same shape, same
			// correlation id, and it redacts INTERNAL for you.
			whttp.WriteError(w, r, errors.Unauthenticated("invalid credential"))
			return
		}
		id := app.Identity{
			Subject: claims.Subject,
			Issuer:  claims.Issuer,
			Scopes:  strings.Fields(claims.Scope),
			Claims:  claims.Rest, // map[string]any, whatever else the token said
		}
		next.ServeHTTP(w, r.WithContext(app.WithIdentity(r.Context(), id)))
	})
}

whttp.Server(whttp.Middleware(bearer))
```

Note what it does NOT do: it does not reject an unauthenticated request. No
credential means **no identity**, and the route's own guard decides whether
that is allowed — so a public route stays public without a second list to
keep in step.

Guard the routes that need it, in `Register`:

```go
transport.Post(r, "/documents", c.create, transport.Guard(app.RequireScope("docs:write")))
transport.Get(r, "/documents/{id}", c.get, transport.Guard(app.RequireAuthenticated()))
```

`Guard` runs **before decode**, so an unauthorized caller's malformed body is
a 401 or 403, never a 400 — and `RequireScope` never merges the two: no
identity is 401, a known caller lacking the scope is 403.

Read the caller in a handler:

```go
id, ok := app.IdentityFromContext(ctx)
if !ok {
	return zero, errors.Unauthenticated("no caller identity")
}
doc.Owner = id.Subject
tenant, _ := app.Claim[string](id, "tid")   // JSON numbers are float64
```

The `ok` is not optional politeness — `app.IdentityFromContext(ctx).Subject`
does not compile, deliberately, so an absent caller cannot become a row owned
by `""`.

In tests, one line:

```go
res, err := warrentest.Invoke[application.CreateDoc, application.DocView](
	warrentest.AsCaller(ctx, "u-1", "docs:write"), app, cmd)
```

`AsCaller` sets a **subject and scopes, and no claims** — so a handler
reading `app.Claim[string](id, "tid")` gets `("", false)` from it, and a
multi-tenant test needs the Identity built out:

```go
ctx := app.WithIdentity(ctx, app.Identity{
	Subject: "u-1",
	Scopes:  []string{"docs:write"},
	Claims:  map[string]any{"tid": "acme"},
})
```

This page used to show both in one section without saying so, and every
tenant-aware test written from it had to find out the hard way.

**A custom policy is an ordinary type** — this is a tenant check, and it can
read path parameters because guards see them on every route shape:

```go
type sameTenant struct{}

func (sameTenant) Authorize(ctx context.Context) error {
	id, ok := app.IdentityFromContext(ctx)
	if !ok {
		return errors.Unauthenticated("no caller identity")
	}
	p := transport.ParamsFromContext(ctx)
	if p == nil {
		// No params on this context at all — an event route, or a unit test.
		// DENY. The tempting reading is "nothing to compare, not applicable,
		// allow", and that is a silent cross-tenant bypass: a policy that
		// cannot check must not pass.
		return errors.PermissionDenied("act in this tenant")
	}
	want, _ := app.Claim[string](id, "tid")
	got, _ := p.Path("tenant")
	if want != got {
		return errors.PermissionDenied("act in this tenant")
	}
	return nil
}
```

`ParamsFromContext` returns nil rather than an empty value on purpose: an
empty one would answer `("", false)` for every name, which reads as "no such
parameter" and lets a policy conclude it has nothing to check. The nil forces
the choice to be written down.

**Where this file goes matters.** It imports `warren/transport`, so
`warren lint arch` will refuse it in `application/` or `domain/` — a handler
imports no transport package, and that holds through a helper too. Put a
policy that reads path parameters in the feature's own `controller.go`,
which is unlayered and is exactly where a use case meets a protocol, or in a
package of your own outside `internal/modules/` that only the controller
imports. A field test put it beside its tenant reader in one `internal/auth`
and had to split the package once the linter told it so.

**A path parameter can contain a `/`.** Go's `ServeMux` unescapes `%2F`
inside a segment, so `GET /tenants/evil/stock/acme%2fWIDGET` yields
`sku = "acme/WIDGET"`. That is harmless when the tenant is a whole segment,
as above — and it is a cross-tenant read the moment a policy composes two
values into one key, because the caller controls where the separator falls.
Compare parameters individually, never concatenated.

**Identity does not cross the broker in v0.1.** A consumer's context carries
the correlation ID but no caller, so an audit trail built from events must
carry the actor in the event payload itself. Composing `app.Authorized` into
a consumer chain denies every message and dead-letters it — fail-closed, and
deliberate, but not what you wanted.

---

## Where a message goes when it cannot be handled

A consumer's error decides its own fate, by its `warren/errors` code —
[§2.6](warren.md)'s table, and nothing in your handler chooses it:

| Code | What happens |
|---|---|
| `NOT_FOUND`, `CONFLICT` | acked. The work is already done, or was never possible. |
| `CONTENTION` | nacked and redelivered — or dead-lettered once the retries are spent, if the broker cannot redeliver, which `broker/memory` cannot and the scaffold defaults to. The work was **not** done — a conditional write matched no row — so acking it would destroy a message whose effect never happened. This row is why a repository must return `errors.Contention` and not `errors.Conflict` for a lost version race. |
| `UNAVAILABLE` | retried with backoff, then nacked so the broker redelivers — **or dead-lettered, if the broker cannot redeliver**. See below. |
| everything else | retried, then **dead-lettered**. |

A dead letter is published to `<topic>.dlq` — override with
`broker.WithDeadLetter("...")` — carrying four headers that say what
happened: `warren-origin-topic`, `warren-error-code`, `warren-error`,
`warren-attempts`. **It is also logged at ERROR**, deliberately: that line is
the alert, and it is the one consumer event that should wake someone up.

**A DLQ topic is an ordinary topic.** There is no special API — you consume it
the way you consume anything else, which is how you inspect it:

```go
broker.Pipeline("notes.dlq_watch", "note.written.dlq", handle, store, pub,
    broker.WithoutDedupe(),   // the inbox already saw these ids
)
```

**Replaying onto the ORIGIN topic needs a new message id.** That comment
above is the whole reason: the inbox marked the id seen when the message was
dead-lettered, so republishing the preserved envelope unchanged is
**silently discarded** — `Publish` returns nil, the handler never runs, and
nothing says so. A field test hit exactly that.

The suppression is right for a *redelivery* — the message is already
preserved, and handling it twice is what the inbox exists to prevent. It is
wrong only for a deliberate replay, and the difference is the id:

```go
m := dead                       // the envelope off the .dlq topic
m.ID = dead.ID + "-replay-1"    // a NEW id, or the inbox drops it
_ = pub.Publish(ctx, m.Headers["warren-origin-topic"], m)
```

Keep the original id in a header if you want to trace the two together.

**On the in-process broker, `<topic>.dlq` has no subscriber unless you write
one** — so the ERROR log is your only record. That is the honest limit of a
broker with no durable log, and it is the reason `broker/memory` reports
`Redelivers() false`: a nack there is a drop, so an `UNAVAILABLE` whose
retries are spent is dead-lettered rather than silently lost. A durable
broker keeps §2.6's row exactly as written.

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
