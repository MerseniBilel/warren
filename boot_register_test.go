package warren_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

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
