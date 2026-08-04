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
	subs     []warren.Substitution
	extra    []warren.Module
	inModule string
	broker   bool
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
func WithValidator(v validate.Validator) Option {
	return Option{apply: func(c *config) {
		c.subs = append(c.subs, warren.Bind[validate.Validator](v))
	}}
}

// WithMemoryBroker binds the in-process broker as Publisher and Subscriber
// in the root scope, wrapped in a recorder Published and AssertPublished
// read.
func WithMemoryBroker() Option {
	return Option{apply: func(c *config) { c.broker = true }}
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
