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

// TestANilProviderFailsTheBoot — warren.md §1.3's headline rule is "every
// error the framework can detect surfaces at boot, never on request 1". A
// provider returning a nil interface booted clean, logged "http server
// listening", and produced a 500 on the first request that used it:
//
//	{"error":{"code":"INTERNAL","message":"internal error",...}}
//
// It is detectable exactly where the value is constructed.
func TestANilProviderFailsTheBoot(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(func() pricing { return nil }),
		warren.Controllers(func(p pricing) *pricedController { return &pricedController{p: p} }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a provider returning nil booted — the first request that touches it is a 500")
	}
	for _, want := range []string{"provider returned nil", "pricing", "catalog"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, err)
		}
	}
}

// TestANilPointerFieldIsNotMistakenForANilProvider — the check must look at
// the RETURNED value, not inside it. A perfectly good value with a nil field
// is none of boot's business.
func TestANilPointerFieldIsNotMistakenForANilProvider(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(func() pricing { return fixedPricing{next: nil} }),
		warren.Controllers(func(p pricing) *pricedController { return &pricedController{p: p} }),
	)
	if err := warren.New(m).Start(context.Background()); err != nil {
		t.Fatalf("a value carrying a nil field was refused:\n%v", err)
	}
}

// TestOptionalPermitsADeliberateNil — a capability that is legitimately
// ABSENT is not the defect TestANilProviderFailsTheBoot describes.
// warren/observability returns a nil app.Telemetry when no collector is
// configured, and app.WithTelemetry drops a nil so the uninstrumented request
// path stays a pass-through; forcing a no-op value instead would put that
// value on every request context and cost real work per request.
//
// Optional is how a module DECLARES that, so the intent is written down
// rather than inferred from a nil nobody meant.
func TestOptionalPermitsADeliberateNil(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(func() pricing { return nil }),
		warren.Optional[pricing](),
		warren.Providers(func(p pricing) *priceReport { return &priceReport{p: p} }),
		warren.Eager[*priceReport](),
	)
	if err := warren.New(m).Start(context.Background()); err != nil {
		t.Fatalf("a declared-optional nil was refused:\n%v", err)
	}
}

// TestOptionalIsPerTypeNotPerModule — declaring one type optional must not
// disarm the check for every other type the module provides. The defect the
// guard exists for is a nil nobody meant; a blanket opt-out would reintroduce
// it wholesale in any module that had one honest absence.
func TestOptionalIsPerTypeNotPerModule(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(
			func() pricing { return nil },
			func() shipping { return nil },
		),
		warren.Optional[pricing](),
		warren.Providers(func(p pricing, s shipping) *priceReport { return &priceReport{p: p} }),
		warren.Eager[*priceReport](),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("shipping was not declared Optional, but its nil booted clean")
	}
	for _, want := range []string{"provider returned nil", "shipping", "catalog"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, err)
		}
	}
}

// TestOptionalWithoutAProviderFailsTheBoot — a stale or typo'd Optional is a
// dead declaration that silently disarms the nil check for that type for
// ever. warren.Exports for an unprovided type is already a boot error for
// exactly this reason; the waiver is more dangerous than the export, because
// nothing downstream ever notices it.
func TestOptionalWithoutAProviderFailsTheBoot(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(func() pricing { return fixedPricing{} }),
		warren.Optional[shipping](), // nothing here provides shipping
		warren.Providers(func(p pricing) *priceReport { return &priceReport{p: p} }),
		warren.Eager[*priceReport](),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("an Optional for a type the module does not provide booted clean")
	}
	for _, want := range []string{"optional", "shipping", "catalog"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, err)
		}
	}
}

// TestOptionalWithoutANilStillDelivers — Optional relaxes the check, it does
// not change what the graph receives. A configured collector must arrive
// exactly as it would without the declaration.
func TestOptionalWithoutANilStillDelivers(t *testing.T) {
	t.Parallel()

	var got pricing
	m := warren.NewModule("catalog",
		warren.Providers(func() pricing { return fixedPricing{} }),
		warren.Optional[pricing](),
		warren.Providers(func(p pricing) *priceReport { got = p; return &priceReport{p: p} }),
		warren.Eager[*priceReport](),
	)
	if err := warren.New(m).Start(context.Background()); err != nil {
		t.Fatalf("boot failed:\n%v", err)
	}
	if got == nil {
		t.Fatal("Optional swallowed a real value")
	}
}

type pricing interface{ Price() int }

type shipping interface{ Ship() int }

// priceReport is a plain eager value, not a controller — Optional is about
// what a provider returns, not about how the value is registered.
type priceReport struct{ p pricing }

type fixedPricing struct{ next *fixedPricing }

func (fixedPricing) Price() int { return 1 }

type pricedController struct{ p pricing }

func (*pricedController) Register(transport.Registrar) {}

// newCatalogPricing is a package-level constructor so the diagnostic has a
// real name to print. A literal closure would print as a file:line func, and
// the point of this test is that SOME real name survives the nil-check
// wrapper.
func newCatalogPricing() pricing { return fixedPricing{} }

// TestNilCheckedProviderKeepsItsName covers field-test defect B3's second
// half. Every nilable-output constructor is wrapped in a reflect.MakeFunc so
// a nil return fails the boot. A reflect.MakeFunc value's runtime name is the
// assembly stub reflect.makeFuncStub — so without an explicit name, every
// candidate line and every ambiguity in a real application named the stub
// instead of the constructor the user wrote.
func TestNilCheckedProviderKeepsItsName(t *testing.T) {
	t.Parallel()

	// platform provides the pricing but does not export it; catalog needs it.
	platform := warren.NewModule("platform", warren.Providers(newCatalogPricing))
	catalog := warren.NewModule("catalog",
		warren.Controllers(func(p pricing) *pricedController { return &pricedController{p: p} }),
	)
	err := warren.New(platform, catalog).Start(context.Background())
	if err == nil {
		t.Fatal("catalog booted though it cannot see platform's pricing")
	}
	got := err.Error()
	if strings.Contains(got, "makeFuncStub") {
		t.Errorf("the nil-check wrapper's assembly stub reached the diagnostic:\n%s", got)
	}
	if !strings.Contains(got, "newCatalogPricing") {
		t.Errorf("diagnostic does not name the constructor the user wrote:\n%s", got)
	}
}

// TestNilProviderDiagnosticOffersOptional — a provider that returns nil ON
// PURPOSE is a supported shape, and warren.Optional[T]() is how it is
// declared. The boot error for an accidental nil is exactly where a reader
// with a deliberate one will be looking.
func TestNilProviderDiagnosticOffersOptional(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("catalog",
		warren.Providers(func() pricing { return nil }),
		warren.Controllers(func(p pricing) *pricedController { return &pricedController{p: p} }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("a provider returning nil booted")
	}
	if !strings.Contains(err.Error(), "warren.Optional[warren_test.pricing]()") {
		t.Errorf("the nil diagnostic never mentions the waiver for a deliberate nil:\n%s", err)
	}
}

// --- the jobs ruling, 2026-08-05 -------------------------------------------

// poolish stands in for anything a background loop depends on — a pool, a
// repository, a broker client. It registers a lifecycle hook from its
// constructor, exactly as postgres, kafka, http and the outbox relay all do.
type poolish struct{}

func newPoolish(lc lifecycle.Lifecycle, order *[]string, mu *sync.Mutex) *poolish {
	lc.Append(lifecycle.Hook{
		Name:    "pool",
		OnStart: func(context.Context) error { record(mu, order, "pool.start"); return nil },
		OnStop:  func(context.Context) error { record(mu, order, "pool.stop"); return nil },
	})
	return &poolish{}
}

// jobish is the background loop warren/jobs was going to own: it INJECTS the
// pool, so its own hook is necessarily appended second.
type jobish struct{}

func newJobish(_ *poolish, lc lifecycle.Lifecycle, order *[]string, mu *sync.Mutex) *jobish {
	lc.Append(lifecycle.Hook{
		Name:    "job",
		OnStart: func(context.Context) error { record(mu, order, "job.start"); return nil },
		OnStop:  func(context.Context) error { record(mu, order, "job.stop"); return nil },
	})
	return &jobish{}
}

func record(mu *sync.Mutex, order *[]string, s string) {
	mu.Lock()
	*order = append(*order, s)
	mu.Unlock()
}

// TestABackgroundLoopNeedsNoOrderingAmendment is the load-bearing test behind
// dropping warren/jobs.
//
// That spec's entire deferral rested on "cannot be built without amending two
// orderings AGENT.md forbids changing silently" — §1.3's boot step 6 and
// §2.3's shutdown. The claim was STALE. lifecycle's own package doc says hooks
// start in registration order, which IS dependency order because they are
// appended as their owning singletons are instantiated topologically, and stop
// in the exact reverse.
//
// So a scheduler that injects the pool it works against cannot start before
// that pool and cannot stop after it — the bug §7.4 existed to prevent is
// prevented by construction, with nobody deciding anything.
//
// This test turns that argument into a fact CI defends. It is what would fail
// if someone later "tidied" registration order, and the drop is only
// defensible while it passes.
func TestABackgroundLoopNeedsNoOrderingAmendment(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string

	infra := warren.NewModule("infra",
		warren.Providers(func(lc lifecycle.Lifecycle) *poolish {
			return newPoolish(lc, &order, &mu)
		}),
		warren.Exports[*poolish](),
	)
	jobs := warren.NewModule("jobs",
		warren.Imports(infra),
		warren.Providers(func(p *poolish, lc lifecycle.Lifecycle) *jobish {
			return newJobish(p, lc, &order, &mu)
		}),
		warren.Eager[*jobish](),
	)
	// The JOB module is listed FIRST in New — the position that would break
	// the ordering if hooks were appended in module order.
	a := warren.New(jobs, infra)

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	mu.Lock()
	got := strings.Join(order, " → ")
	mu.Unlock()

	const want = "pool.start → job.start → job.stop → pool.stop"
	if got != want {
		t.Errorf("lifecycle order = %s\nwant                 %s\n\n"+
			"A background loop must start AFTER what it depends on and be stopped BEFORE it. "+
			"If this changed, warren/jobs's ordering premise is live again and §7.4 needs revisiting.", got, want)
	}
}

// TestANilProviderIsADiagnosticNotAPanic — field test #7, defect B2, and a
// regression I introduced. sameFunc calls reflect.Value.Pointer on a nil
// `any`, so a stray nil in a Providers list produced:
//
//	panic: reflect: call of reflect.Value.Pointer on zero Value
//	    warren.sameFunc(...) app.go:696
//
// Nothing named the module, the provider list, or the word "provider". It was
// the ONE boot mistake in the whole matrix that produced a framework stack
// trace instead of a Warren diagnostic — in the package whose diagnostics are
// the product.
func TestANilProviderIsADiagnosticNotAPanic(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mod  warren.Module
	}{
		{"nil in Providers", warren.NewModule("catalog",
			warren.Providers(newCatalogPricing, nil))},
		{"nil in Controllers", warren.NewModule("catalog",
			warren.Controllers(nil))},
		{"a non-function", warren.NewModule("catalog",
			warren.Providers("not a constructor"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("boot panicked instead of failing with a diagnostic: %v", r)
					}
				}()
				err = warren.New(tc.mod).Start(context.Background())
			}()
			if err == nil {
				t.Fatal("a nil constructor booted")
			}
			// It must name the module and say what is wrong, like every other
			// boot failure.
			for _, want := range []string{"catalog", "constructor"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the diagnostic does not mention %q:\n%s", want, err)
				}
			}
		})
	}
}
