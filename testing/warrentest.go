// Package warrentest boots a Warren module for a test with dependencies
// substituted, invokes handlers by request and response type, and asserts on
// what was published.
//
// Its import path is .../warren/testing; its package name is warrentest, so
// a test file never has to alias the standard testing package.
//
// It is the standard library plus Warren: no assertion library, no
// testcontainers. Container fixtures, when they exist, live in their own
// module so Docker never enters this one's graph — AGENT.md's no-Docker rule
// then holds structurally rather than by convention.
package warrentest

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/memory"
	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/outbox"
	"github.com/MerseniBilel/warren/persistence"
	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
)

// App is a booted module graph under test.
type App struct {
	app      *warren.App
	module   string
	recorder *Recorder
	closed   bool
}

// Option configures NewModuleTest.
type Option struct{ apply func(*config) }

type config struct {
	subs        []warren.Substitution
	extra       []warren.Module
	inModule    string
	broker      bool
	persistence bool
}

// Replace substitutes the provider of T with v, in every module that
// provides it. A substitution matching nothing fails the boot naming T — a
// fake that was silently ignored is worse than no fake.
func Replace[T any](v T) Option {
	return Option{apply: func(c *config) { c.subs = append(c.subs, warren.Substitute[T](v)) }}
}

// Provide binds v as T in the root scope, where every module can see it —
// for a dependency the graph does not already have.
func Provide[T any](v T) Option {
	return Option{apply: func(c *config) { c.subs = append(c.subs, warren.Bind[T](v)) }}
}

// WithModules adds modules the module under test needs.
func WithModules(extra ...warren.Module) Option {
	return Option{apply: func(c *config) { c.extra = append(c.extra, extra...) }}
}

// InModule directs Invoke at a named scope when several modules are booted.
// The default is the module passed to NewModuleTest.
func InModule(name string) Option {
	return Option{apply: func(c *config) { c.inModule = name }}
}

// WithValidator compiles the module's routes against v instead of the
// standard-library validator — how a module whose requests carry tags core
// refuses is tested:
//
//	warrentest.NewModuleTest(t, stock.Module(), warrentest.WithValidator(playground.New()))
//
// Without it, installing warren/validate/playground — which core's own
// diagnostic tells you to do — booted in production and failed every module
// test in that module, because NewModuleTest calls Start itself and never
// touches App.Validator.
//
// IT DOES NOT MAKE Invoke VALIDATE, and the name invites the opposite reading.
// A validator satisfies the BOOT-TIME check that every `validate:` tag in the
// graph is one the installed implementation understands. The tags themselves
// run on the TRANSPORT EDGE — planRule compiles a rule per route at boot step
// 5, and the invoker calls it before the handler — and [Invoke] calls
// Handle directly, so nothing on that edge runs.
//
// The consequence, measured by field test #15: a test asserting that
// `validate:"min=2"` rejects a one-character name passes with err == nil,
// through this harness, with the validator installed, while the same request
// over HTTP correctly answers 400. A green test that cannot fail is worse
// than no test.
//
// To test a validation rule, go through the transport:
//
//	s := servertest.New(t, user.Module())
//	res := s.Post(t, "/members", map[string]string{"name": "A"})
//	// res.Status == 400
//
// Making Invoke apply the same rule is scheduled for v0.3; until it lands this
// doc comment is the contract.
func WithValidator(v validate.Validator) Option {
	return Option{apply: func(c *config) {
		c.subs = append(c.subs, warren.Bind[validate.Validator](v))
	}}
}

// WithTelemetry binds t as the application's instrumentation, so boot step 5
// wraps app.Traced and app.Metered around every handler — which is how a test
// asserts on the SPANS a route produces rather than on its response.
//
// It binds rather than calling App.Telemetry for the same reason
// WithValidator does: NewModuleTest calls Start itself, so an App method has
// no moment to be called in. Boot resolves app.Telemetry from the container
// when the App field is unset, so a binding reaches the same place.
//
//	rec := &spanRecorder{}
//	a := warrentest.NewModuleTest(t, user.Module(), warrentest.WithTelemetry(rec))
func WithTelemetry(t app.Telemetry) Option {
	return Option{apply: func(c *config) {
		c.subs = append(c.subs, warren.Bind[app.Telemetry](t))
	}}
}

// WithMemoryBroker binds the in-process broker as Publisher and Subscriber
// in the root scope, wrapped in a recorder Published and AssertPublished
// read.
func WithMemoryBroker() Option {
	return Option{apply: func(c *config) { c.broker = true }}
}

// WithMemoryPersistence binds Warren's in-process persistence in the root
// scope: the unit of work, the outbox store and the inbox store. It is the
// counterpart to WithMemoryBroker, and together they boot a module graph with
// no database, no broker and no Docker.
//
// It binds, rather than substituting, for the same reason WithMemoryBroker
// does: a module graph that already provides these ports is the normal case,
// and an ambiguous-binding failure there would be useless. See warren.Bind.
//
// The unit of work is wired to the outbox store with OnCommit, so an
// aggregate's drained events reach the outbox exactly as they do in
// production. Without that wiring the option would be a trap: aggregates
// would commit, no outbox row would be written, and AssertPublished would
// fail with a harness-shaped error that reads like an application bug.
//
// WHAT IT CANNOT DO, and this is the part worth reading. It binds PORTS. It
// does not remove a module, and it cannot stop one dialling. A graph that
// imports a driver module — postgres.Module, kafka.Broker — still builds that
// driver's pool or client at boot step 4 and still runs its OnStart, because
// a driver connects at boot deliberately, so that a misconfigured service
// fails its boot rather than request 1. postgres.Module declares
// warren.Eager[*pool](), so the pool is built whether or not anything injects
// it, and binding a fake postgres.DB does not prevent the dial.
//
// So this option makes a module testable when that module depends on the
// PORTS. It does not make a module testable when that module imports the
// driver. For the second case, declare a test module that imports your
// memory-wired platform instead — which is what `warren new` generates.
func WithMemoryPersistence() Option {
	return Option{apply: func(c *config) { c.persistence = true }}
}

// NewModuleTest boots m for the duration of the test and registers cleanup,
// so Close is belt-and-braces. A module with an unresolvable dependency
// fails HERE — boot step 3 — with Warren's own diagnostic, not later at
// Invoke.
func NewModuleTest(t *testing.T, m warren.Module, opts ...Option) *App {
	t.Helper()

	var cfg config
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	// A module test drives handlers DIRECTLY — Invoke resolves them out of
	// the container and calls them. It serves no traffic, so the routes its
	// controllers register have no adapter, and boot would refuse them:
	// "registered routes have no adapter serving them" is exactly right in
	// production and exactly wrong here.
	//
	// So the harness claims every protocol on its own behalf. What it gives
	// up is the boot check that would have caught a route nobody serves —
	// which is a production concern, and which the application's own main
	// still gets.
	claimAll := warren.NewModule("warrentest/claims",
		warren.Providers(func(tbl *transport.Table) *protocolClaims {
			for _, p := range []transport.Protocol{
				transport.ProtocolHTTP, transport.ProtocolGRPC, transport.ProtocolEvent,
			} {
				tbl.Claim(p, "warren/testing")
			}
			return &protocolClaims{}
		}),
		warren.Eager[*protocolClaims](),
	)

	modules := append([]warren.Module{m, claimAll}, cfg.extra...)
	a := &App{module: m.Name()}
	if cfg.inModule != "" {
		a.module = cfg.inModule
	}
	if cfg.broker {
		a.recorder = &Recorder{inner: memory.New()}
		cfg.subs = append(cfg.subs,
			warren.Bind[broker.Publisher](a.recorder),
			warren.Bind[broker.Subscriber](a.recorder),
		)
	}
	if cfg.persistence {
		uow := persistence.NewMemoryUnitOfWork()
		store := outbox.NewMemoryStore()
		// The line that keeps the option honest: without it an aggregate
		// commits and no outbox row is written, so AssertPublished fails in a
		// way that looks like the application's fault.
		uow.OnCommit(outbox.Sink(store, outbox.JSONEncoder()))
		cfg.subs = append(cfg.subs,
			warren.Bind[persistence.UnitOfWork](uow),
			// The concrete type too: a memory repository takes
			// *persistence.MemoryUnitOfWork, not the interface.
			warren.Bind[*persistence.MemoryUnitOfWork](uow),
			warren.Bind[app.UnitOfWork](persistence.ForApp(uow)),
			warren.Bind[outbox.Store](store),
			warren.Bind[inbox.Store](inbox.NewMemoryStore()),
		)
	}

	a.app = warren.New(modules...)
	if err := a.app.Substitute(cfg.subs...); err != nil {
		t.Fatalf("warrentest: %v", err)
	}
	if err := a.app.Start(context.Background()); err != nil {
		t.Fatalf("warrentest: booting module %q:\n%v", m.Name(), err)
	}
	t.Cleanup(a.Close)
	return a
}

// protocolClaims exists only to be built at boot, so its constructor's
// Claim calls run.
type protocolClaims struct{}

// Close stops the app. It is idempotent and registered by NewModuleTest.
func (a *App) Close() {
	if a == nil || a.closed {
		return
	}
	a.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = a.app.Stop(ctx)
}

// Warren returns the booted app, for anything this harness does not wrap.
func (a *App) Warren() *warren.App { return a.app }

// Invoke resolves app.Handler[Req, Res] from the module under test and calls
// it, returning the handler's own result and error. Context is first, as
// everywhere else in Warren. A resolution failure — the module does not
// provide that handler — is returned as Warren's boot diagnostic.
//
// IT BOOTS THE GRAPH, NOT THE EDGE. Everything the transport does before the
// handler — decoding the body, binding `param:` and `query:` fields, and
// applying `validate:` tags — is compiled per route at boot step 5 and lives
// on the invoker, and Invoke calls Handle directly. So a request Invoke
// accepts may be one the same service would answer 400 to, and a test written
// to prove a validation rule works passes whether or not it does. See
// [WithValidator], which does NOT change this.
//
// Use it for the use case's own logic. Use transport/http's servertest for
// anything the edge decides.
func Invoke[Req, Res any](ctx context.Context, a *App, req Req) (Res, error) {
	return InvokeIn[Req, Res](ctx, a, a.module, req)
}

// InvokeIn is Invoke against a NAMED module, for a test that spans several.
//
// Invoke resolves from the one module NewModuleTest was pointed at, and
// InModule fixes that choice at boot for every call — so an end-to-end test
// driving three features has neither. This is that test's entry point, and
// it exists because writing it by hand over App.Warren().Invoke was the
// first thing a real service had to do.
func InvokeIn[Req, Res any](ctx context.Context, a *App, module string, req Req) (Res, error) {
	var res Res
	var handleErr error
	err := a.app.Invoke(module, func(h app.Handler[Req, Res]) {
		res, handleErr = h.Handle(ctx, req)
	})
	if err != nil {
		var zero Res
		return zero, err
	}
	return res, handleErr
}

// Resolve returns the T the module under test provides, as the boot built
// it — not a second construction.
//
// warrentest.Invoke reaches app.Handler types and nothing else, so a test
// that wants the repository, the publisher or a sweeper had to drop to
// App.Warren().Invoke(module, fn) and pass a CONTAINER SCOPE NAME, which is
// documented nowhere a test author would look. A field test did exactly
// that, and reported it.
//
//	pub := warrentest.Resolve[broker.Publisher](t, a)
//
// A type the module does not provide fails the test with Warren's own
// resolution diagnostic, rather than returning a zero value the test then
// asserts against.
func Resolve[T any](t testing.TB, a *App) T {
	t.Helper()
	return ResolveIn[T](t, a, a.module)
}

// ResolveIn is Resolve against a NAMED module, for a test spanning several —
// the same escape hatch InvokeIn gives handlers.
func ResolveIn[T any](t testing.TB, a *App, module string) T {
	t.Helper()
	var got T
	if err := a.app.Invoke(module, func(v T) { got = v }); err != nil {
		var zero T
		t.Fatalf("warrentest: resolving %T from module %q: %v", zero, module, err)
	}
	return got
}

// Recorder wraps a Publisher and Subscriber, remembering what was
// published. WithMemoryBroker installs one.
type Recorder struct {
	inner *memory.Broker
	mu    sync.Mutex
	sent  map[string][]broker.Message
}

// Publish records the messages and forwards them.
func (r *Recorder) Publish(ctx context.Context, topic string, msgs ...broker.Message) error {
	r.mu.Lock()
	if r.sent == nil {
		r.sent = map[string][]broker.Message{}
	}
	r.sent[topic] = append(r.sent[topic], msgs...)
	r.mu.Unlock()
	return r.inner.Publish(ctx, topic, msgs...)
}

// Redelivers forwards the wrapped broker's answer, so a module test sees the
// same dead-letter behaviour the application will. Swallowing it would make
// the harness disagree with production about whether an exhausted
// UNAVAILABLE is preserved.
func (r *Recorder) Redelivers() bool { return r.inner.Redelivers() }

// Subscribe forwards to the in-process broker.
func (r *Recorder) Subscribe(ctx context.Context, topic string, h broker.MessageHandler) error {
	return r.inner.Subscribe(ctx, topic, h)
}

// Published returns the messages recorded on a topic, in publish order.
//
// It takes t for one reason: without WithMemoryBroker() there is no recorder,
// and this used to answer that case with an empty slice — indistinguishable
// from "nothing was published". A test asserting `len(Published(...)) == 0`
// then passed while observing nothing at all. AssertPublished has always
// failed loudly here; this now does too, and the argument order matches it.
func Published(t testing.TB, a *App, topic string) []broker.Message {
	t.Helper()
	if a.recorder == nil {
		t.Fatal("warrentest: Published needs WithMemoryBroker() — without it nothing records what was published, and an empty result would mean two different things")
		return nil
	}
	a.recorder.mu.Lock()
	defer a.recorder.mu.Unlock()
	return append([]broker.Message(nil), a.recorder.sent[topic]...)
}

// AssertPublished fails unless a message of E's event name reached the
// broker, and returns it decoded. It waits briefly, because publishing is
// asynchronous through the relay. The failure names the event expected AND
// what actually reached the broker — the two facts needed to fix it.
//
// It takes testing.TB rather than *testing.T so a helper can wrap it and a
// test can verify the message.
func AssertPublished[E domain.Event](t testing.TB, a *App) E {
	t.Helper()
	var zero E
	if a.recorder == nil {
		t.Fatal("warrentest: AssertPublished needs WithMemoryBroker()")
	}
	name := zero.EventName()

	deadline := time.Now().Add(2 * time.Second)
	for {
		a.recorder.mu.Lock()
		for topic, msgs := range a.recorder.sent {
			for _, m := range msgs {
				if m.Type == name || topic == name {
					a.recorder.mu.Unlock()
					var e E
					if err := json.Unmarshal(m.Payload, &e); err != nil {
						t.Fatalf("warrentest: decoding %s: %v", name, err)
					}
					return e
				}
			}
		}
		published := a.recorder.topics()
		a.recorder.mu.Unlock()

		if time.Now().After(deadline) {
			t.Fatalf("warrentest: no %q was published.\nPublished topics: %s",
				name, describe(published))
		}
		runtime.Gosched()
	}
}

func (r *Recorder) topics() []string {
	var out []string
	for topic, msgs := range r.sent {
		out = append(out, fmt.Sprintf("%s (%d)", topic, len(msgs)))
	}
	sort.Strings(out)
	return out
}

func describe(topics []string) string {
	if len(topics) == 0 {
		return "none"
	}
	return strings.Join(topics, ", ")
}

// Golden compares got against testdata/<name>.golden, rewriting it when the
// test binary is run with -update. Warren's diagnostics are a product
// feature; this is how a project pins its own.
func Golden(t testing.TB, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if updateGolden() {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("warrentest: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("warrentest: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("warrentest: reading %s: %v\n\nRun the test with -update to create it.", path, err)
	}
	if got != string(want) {
		t.Errorf("golden %s does not match\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

func updateGolden() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// AsCaller returns a context carrying an identity with subject and scopes —
// the one line a test writes to drive a guarded handler.
//
// Every use case behind transport.Guard needs an identity to reach it, so
// without this every test in every service opens with the same
// context/app.Identity incantation. A field test wrote it by hand in a dozen
// places before pointing out that warren/testing contained no mention of
// app.Identity at all.
//
//	res, err := warrentest.Invoke[application.ShareDoc, application.DocView](
//	    warrentest.AsCaller(ctx, "u-1", "docs:write"), app, cmd)
//
// For a caller with claims — a tenant, say — build the Identity yourself and
// use app.WithIdentity; this covers the common case, not every case.
func AsCaller(ctx context.Context, subject string, scopes ...string) context.Context {
	return app.WithIdentity(ctx, app.Identity{Subject: subject, Scopes: scopes})
}
