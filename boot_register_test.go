package warren_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
)

// --- fixtures --------------------------------------------------------------

type greet struct {
	Name string `json:"name" validate:"required"`
}

type greeting struct {
	Text string `json:"text"`
}

// echoController is the ordinary case: a controller whose Register the boot
// must call exactly once, at step 5.
type echoController struct{ registered int }

func (c *echoController) hello(_ context.Context, g greet) (greeting, error) {
	return greeting{Text: "hello " + g.Name}, nil
}

func (c *echoController) Register(r transport.Registrar) {
	c.registered++
	transport.Post(r, "/greet", app.HandlerFunc[greet, greeting](c.hello))
}

// silentController is the failure §1.3 exists to prevent: a type listed in
// warren.Controllers whose Register has the wrong signature, so it registers
// nothing and every route it meant to declare 404s in production.
type silentController struct{}

func (c *silentController) Register() {} // wrong signature — takes no Registrar

// tableReader stands in for a transport adapter: it injects the Table the
// bootstrapper provides at step 2, claims a protocol, and reads the routes
// that step 5 filled in.
type tableReader struct {
	tbl   *transport.Table
	seen  int
	hooks *[]string
}

func newTableReader(tbl *transport.Table, lc lifecycle.Lifecycle, order *[]string) *tableReader {
	tbl.Claim(transport.ProtocolHTTP, "test/adapter")
	tr := &tableReader{tbl: tbl, seen: len(tbl.HTTP()), hooks: order}
	lc.Append(lifecycle.Hook{Name: "adapter", OnStart: func(context.Context) error {
		*order = append(*order, "adapter")
		return nil
	}})
	return tr
}

// --- tests -----------------------------------------------------------------

func TestBootCallsRegisterOnce(t *testing.T) {
	t.Parallel()

	c := &echoController{}
	m := warren.NewModule("greeter",
		warren.Controllers(func() *echoController { return c }),
		warren.Providers(func(tbl *transport.Table, lc lifecycle.Lifecycle) *tableReader {
			return newTableReader(tbl, lc, new([]string))
		}),
		warren.Eager[*tableReader](),
	)
	a := warren.New(m)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop(context.Background()) }()

	if c.registered != 1 {
		t.Fatalf("Register called %d times, want exactly 1 — step 5 runs once", c.registered)
	}
}

func TestAdapterSeesTheFilledTable(t *testing.T) {
	t.Parallel()

	var reader *tableReader
	m := warren.NewModule("greeter",
		warren.Controllers(func() *echoController { return &echoController{} }),
		warren.Providers(func(tbl *transport.Table, lc lifecycle.Lifecycle) *tableReader {
			reader = newTableReader(tbl, lc, new([]string))
			return reader
		}),
		warren.Eager[*tableReader](),
	)
	a := warren.New(m)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop(context.Background()) }()

	if reader == nil {
		t.Fatal("the adapter was never constructed")
	}
	if reader.seen != 1 {
		t.Errorf("adapter saw %d routes at construction, want 1 — eager singletons build AFTER step 5", reader.seen)
	}
	if got := reader.tbl.HTTP()[0].Pattern; got != "/greet" {
		t.Errorf("route pattern = %q", got)
	}
}

func TestRoutesWithNoAdapterFailTheBoot(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("greeter",
		warren.Controllers(func() *echoController { return &echoController{} }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a registered route nobody serves must fail the boot")
	}
	if !strings.Contains(err.Error(), "http.Server") {
		t.Errorf("diagnostic must name the fix:\n%s", err)
	}
}

func TestControllerWithoutRegisterFailsTheBoot(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("greeter",
		warren.Controllers(func() *silentController { return &silentController{} }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a Controllers entry that is not a controller must fail the boot")
	}
	got := regexp.MustCompile(`\w+_test\.go:\d+`).ReplaceAllString(err.Error(), "module.go:14")
	assertGolden(t, "controller_registers_nothing", got)
}

func TestConsumerWithoutRegisterNamesConsumers(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("greeter",
		warren.Consumers(func() *silentController { return &silentController{} }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a Consumers entry that is not a consumer must fail the boot")
	}
	if !strings.Contains(err.Error(), "warren.Consumers") {
		t.Errorf("the diagnostic must name the option the user actually wrote:\n%s", err)
	}
}

// A type that registers nothing and only needs building at boot has an
// answer, and the diagnostic names it: Eager. This proves the escape works.
func TestEagerIsTheEscapeFromTheControllerAssertion(t *testing.T) {
	t.Parallel()

	var built bool
	m := warren.NewModule("greeter",
		warren.Providers(func() *silentController { built = true; return &silentController{} }),
		warren.Eager[*silentController](),
	)
	if err := warren.New(m).Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !built {
		t.Error("Eager did not build the type")
	}
}

func TestValidatorIsReachableFromTheApp(t *testing.T) {
	t.Parallel()

	// greet.Name is `validate:"required"`, so the default validator refuses an
	// empty name. validate.None() must reach the Builder boot step 5 drives.
	c := &echoController{}
	var reader *tableReader
	m := warren.NewModule("greeter",
		warren.Controllers(func() *echoController { return c }),
		warren.Providers(func(tbl *transport.Table, lc lifecycle.Lifecycle) *tableReader {
			reader = newTableReader(tbl, lc, new([]string))
			return reader
		}),
		warren.Eager[*tableReader](),
	)
	a := warren.New(m)
	if err := a.Validator(validate.None()); err != nil {
		t.Fatalf("Validator: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop(context.Background()) }()

	invoke := reader.tbl.HTTP()[0].Bind(transport.JSON())
	if _, err := invoke(context.Background(), []byte(`{}`)); err != nil {
		t.Errorf("validate.None() did not reach the route closure: %v", err)
	}
}

func TestValidatorAfterStartIsRefused(t *testing.T) {
	t.Parallel()

	a := warren.New(warren.NewModule("empty"))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop(context.Background()) }()
	if err := a.Validator(validate.None()); err == nil {
		t.Error("Validator after Start must be refused — it is boot-time")
	}
}

// The three-pass step 5 has a consequence worth pinning: an adapter is an
// eager singleton, so its hook appends after every module's own OnStart
// hooks. §1.3's "pool → repos → consumers → servers" then holds by
// construction rather than by the order the user listed modules in New.
func TestAdapterHooksStartAfterDomainHooks(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	order := []string{}
	appendOrder := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	// The adapter module is listed FIRST, the position that would break the
	// ordering if hooks were appended in module order.
	adapter := warren.NewModule("adapter",
		warren.Providers(func(tbl *transport.Table, lc lifecycle.Lifecycle) *tableReader {
			return newTableReader(tbl, lc, &order)
		}),
		warren.Eager[*tableReader](),
	)
	domain := warren.NewModule("greeter",
		warren.Controllers(func() *echoController { return &echoController{} }),
		warren.OnStart(func(context.Context) error { appendOrder("domain"); return nil }),
	)
	a := warren.New(adapter, domain)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop(context.Background()) }()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "domain" || order[1] != "adapter" {
		t.Errorf("start order = %v, want [domain adapter] — servers start last", order)
	}
}

// A raw route is served by the same adapter and counts the same way.
type rawUploadController struct{}

type upload struct{}

func (c *rawUploadController) Register(r transport.Registrar) {
	transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", &upload{})
}

func TestRawRouteRequiresAnAdapterToo(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("files",
		warren.Controllers(func() *rawUploadController { return &rawUploadController{} }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a raw HTTP route with no HTTP server must fail the boot")
	}
	if !strings.Contains(err.Error(), "http.Server") {
		t.Errorf("diagnostic must name the fix:\n%s", err)
	}
}

// A controller listed under Providers instead of Controllers used to boot
// clean and serve nothing: step 5 only walks Controllers and Consumers, and
// with no routes registered Unserved had nothing to report either. Both
// safety nets missed the most likely way to ship a dead service. Found by
// field-testing, 2026-08-02.
func TestControllerUnderProvidersFailsTheBoot(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("greeter",
		warren.Providers(func() *echoController { return &echoController{} }),
		warren.Eager[*echoController](),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a controller under Providers registered nothing, silently")
	}
	for _, want := range []string{"declared as a plain provider", "warren.Controllers"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic must contain %q:\n%s", want, err)
		}
	}
}

// telemetryStub registers a flush hook from its constructor, exactly as
// warren/observability does.
type telemetryStub struct{}

func (telemetryStub) Span(ctx context.Context, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}
func (telemetryStub) Record(string, time.Duration, error)             {}
func (telemetryStub) Inject(context.Context, func(key, value string)) {}
func (telemetryStub) Extract(ctx context.Context, _ func(string) string) context.Context {
	return ctx
}

// The telemetry flush must unwind LAST — after servers stop, after consumers
// drain, after pools close — because the spans emitted while everything else
// shuts down are the ones nobody can reproduce.
//
// This was documented as a property of listing observability.Module first in
// warren.New, and it was not true: the hook was appended when the provider
// was resolved, which happened after every module's declared hooks, so the
// flush ran BEFORE them and argument order changed nothing. Found by
// field-testing, 2026-08-02. It is now a property of the boot phase, so it
// cannot be got wrong — and this test is what says so.
func TestTelemetryFlushesLastWhereverItIsListed(t *testing.T) {
	for _, listedFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "listed first", false: "listed last"}[listedFirst], func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			mark := func(s string) func(context.Context) error {
				return func(context.Context) error {
					mu.Lock()
					order = append(order, s)
					mu.Unlock()
					return nil
				}
			}

			telemetry := warren.NewModule("telemetry",
				warren.Providers(func(lc lifecycle.Lifecycle) app.Telemetry {
					lc.Append(lifecycle.Hook{Name: "telemetry", OnStop: mark("telemetry-flush")})
					return telemetryStub{}
				}),
				warren.Exports[app.Telemetry](),
			)
			// A domain module whose OnStop stands in for closing a pool.
			domain := warren.NewModule("greeter",
				warren.Controllers(func() *echoController { return &echoController{} }),
				warren.Providers(func(tbl *transport.Table, lc lifecycle.Lifecycle) *tableReader {
					return newTableReader(tbl, lc, new([]string))
				}),
				warren.Eager[*tableReader](),
				warren.OnStop(mark("pool-close")),
			)

			mods := []warren.Module{telemetry, domain}
			if !listedFirst {
				mods = []warren.Module{domain, telemetry}
			}
			a := warren.New(mods...)
			if err := a.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if err := a.Stop(context.Background()); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(order) != 2 || order[0] != "pool-close" || order[1] != "telemetry-flush" {
				t.Errorf("stop order = %v, want [pool-close telemetry-flush] — the flush must be last", order)
			}
		})
	}
}

// TestControllerUnderPlainProvidersFailsTheBoot — GETTING_STARTED.md §5
// promised "A controller under `Providers` would register no routes — so the
// boot refuses it by name rather than letting you ship a dead service." It
// did not. The guard existed but only walked warren.Eager, and a plain
// provider nothing depends on is never constructed, so nothing looked at it:
// the app booted, logged "http server listening", and every route 404'd.
//
// That is the exact failure the whole boot-validation pitch exists to
// prevent, and it is the form a user actually mistypes.
func TestControllerUnderPlainProvidersFailsTheBoot(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(func() *deadController { return &deadController{} }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a controller declared under Providers booted — it registers no routes and every one of them 404s")
	}
	for _, want := range []string{"controller declared as a plain provider", "catalog", "warren.Controllers"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, err)
		}
	}
}

// TestAControllerSomethingDependsOnIsNotRefused — the check must not break
// composition. A sub-controller listed under Providers and injected into the
// controller that delegates to it IS constructed and IS registered, through
// its parent. Refusing it would make the guard worse than the bug.
func TestAControllerSomethingDependsOnIsNotRefused(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(func() *deadController { return &deadController{} }),
		warren.Controllers(func(sub *deadController) *parentController {
			return &parentController{sub: sub}
		}),
	)
	if err := warren.New(m).Start(context.Background()); err != nil {
		t.Fatalf("a sub-controller its parent depends on was refused:\n%v", err)
	}
}

type deadController struct{}

func (*deadController) Register(transport.Registrar) {}

type parentController struct{ sub *deadController }

func (p *parentController) Register(r transport.Registrar) { p.sub.Register(r) }
