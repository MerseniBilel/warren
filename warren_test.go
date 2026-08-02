package warren_test

import (
	"context"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/config"
	"github.com/MerseniBilel/warren/lifecycle"
)

var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("diagnostic does not match golden file %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// journal records boot activity in order across goroutines.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(e string) { j.mu.Lock(); defer j.mu.Unlock(); j.entries = append(j.entries, e) }
func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.entries)
}

// The §1.2 fixture types: platform provides the pool, user provides the
// repository (exported) and a private service, billing consumes the
// repository.
type pool struct{}

type userRepository interface{ Kind() string }

type pgUserRepository struct{ p *pool }

func (r *pgUserRepository) Kind() string { return "pg" }

type userService struct{}

type invoiceService struct{ repo userRepository }

func TestNewModuleIsInert(t *testing.T) {
	t.Parallel()

	constructed := false
	_ = warren.NewModule("user",
		warren.Providers(func() *userService { constructed = true; return &userService{} }),
		warren.Controllers(func(*userService) *pool { constructed = true; return &pool{} }),
		warren.Consumers(func() *pool { constructed = true; return &pool{} }),
		warren.Exports[*userService](),
		warren.OnStart(func(context.Context) error { constructed = true; return nil }),
		warren.OnStop(func(context.Context) error { constructed = true; return nil }),
	)
	_ = warren.New(warren.NewModule("x"))
	if constructed {
		t.Fatal("NewModule or New ran a constructor or hook — a module declaration must be an inert value")
	}
}

// section12App builds §1.2's graph: billing imports platform and user; user
// exports the repository and keeps its service private.
func section12App(j *journal) (*warren.App, *bool) {
	resolvedInBilling := new(bool)

	platform := warren.NewModule("platform",
		warren.Providers(func() *pool { j.add("build pool"); return &pool{} }),
		warren.Exports[*pool](),
	)
	user := warren.NewModule("user",
		warren.Imports(platform),
		warren.Providers(
			func(p *pool) userRepository { j.add("build repo"); return &pgUserRepository{p: p} },
			func() *userService { j.add("build service"); return &userService{} },
		),
		warren.Exports[userRepository](),
	)
	billing := warren.NewModule("billing",
		warren.Imports(platform, user),
		warren.Consumers(func(r userRepository) *invoiceService {
			j.add("build invoices with " + r.Kind())
			*resolvedInBilling = true
			return &invoiceService{repo: r}
		}),
		warren.OnStart(func(context.Context) error { j.add("start billing"); return nil }),
		warren.OnStop(func(context.Context) error { j.add("stop billing"); return nil }),
	)
	return warren.New(platform, user, billing), resolvedInBilling
}

func TestEncapsulationAcrossModules(t *testing.T) {
	t.Parallel()

	t.Run("an exported binding crosses the import edge", func(t *testing.T) {
		t.Parallel()
		j := &journal{}
		app, resolved := section12App(j)
		if err := app.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = app.Stop(context.Background()) }()
		if !*resolved {
			t.Fatal("billing's consumer was not built with user's exported repository")
		}
		if got := j.all(); !slices.Contains(got, "build invoices with pg") {
			t.Errorf("journal %v — the forwarded repository did not reach billing", got)
		}
	})

	t.Run("a private binding does not cross", func(t *testing.T) {
		t.Parallel()
		platform := warren.NewModule("platform2",
			warren.Providers(func() *pool { return &pool{} }), warren.Exports[*pool]())
		user := warren.NewModule("user2",
			warren.Imports(platform),
			warren.Providers(func() *userService { return &userService{} }),
		)
		billing := warren.NewModule("billing2",
			warren.Imports(platform, user),
			warren.Consumers(func(*userService) *invoiceService { return &invoiceService{} }),
		)
		err := warren.New(platform, user, billing).Start(context.Background())
		if err == nil {
			t.Fatal("a sibling's private binding resolved across the module boundary")
		}
		if !strings.Contains(err.Error(), "✗ cannot resolve dependency") ||
			!strings.Contains(err.Error(), `is registered in scope "user2" but not exported`) {
			t.Errorf("diagnostic does not explain the encapsulation failure:\n%s", err)
		}
	})
}

func TestBootOrder(t *testing.T) {
	t.Parallel()

	j := &journal{}
	app, _ := section12App(j)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := app.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := j.all()
	// Dependency order: the pool before the repository before billing's
	// consumer; hooks after instantiation; stop after start.
	idx := func(entry string) int {
		for i, e := range got {
			if e == entry {
				return i
			}
		}
		t.Fatalf("journal %v is missing %q", got, entry)
		return -1
	}
	if idx("build pool") >= idx("build repo") || idx("build repo") >= idx("build invoices with pg") {
		t.Errorf("instantiation out of dependency order: %v", got)
	}
	if idx("build invoices with pg") >= idx("start billing") || idx("start billing") >= idx("stop billing") {
		t.Errorf("hooks out of order: %v", got)
	}
	if slices.Contains(got, "build service") {
		t.Errorf("user's unconsumed private service was built: %v — only what entry points need materialises", got)
	}
}

func TestValidationFailureStopsBootBeforeHooks(t *testing.T) {
	t.Parallel()

	started := false
	m := warren.NewModule("broken",
		warren.Consumers(func(userRepository) *invoiceService { return &invoiceService{} }),
		warren.OnStart(func(context.Context) error { started = true; return nil }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("Start with an unresolvable consumer returned nil")
	}
	if !strings.Contains(err.Error(), "✗ cannot resolve dependency") {
		t.Errorf("not the resolution diagnostic:\n%s", err)
	}
	if started {
		t.Error("an OnStart hook ran after validation failed — step 3 must stop boot before step 6")
	}
}

func TestDuplicateModuleName(t *testing.T) {
	t.Parallel()

	a := warren.NewModule("user")
	b := warren.NewModule("user")
	err := warren.New(a, b).Start(context.Background())
	if err == nil {
		t.Fatal("two modules with one name booted")
	}
	if !strings.Contains(err.Error(), "✗ duplicate module name") {
		t.Errorf("not the duplicate-name diagnostic:\n%s", err)
	}
}

func TestExportWithoutProvider(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("user", warren.Exports[userRepository]())
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("an export with no provider booted")
	}
	assertGolden(t, "export_without_provider", err.Error())
}

func TestLifecycleIsResolvable(t *testing.T) {
	t.Parallel()

	var got lifecycle.Lifecycle
	m := warren.NewModule("adapter",
		warren.Consumers(func(lc lifecycle.Lifecycle) *pool { got = lc; return &pool{} }),
	)
	app := warren.New(m)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = app.Stop(context.Background()) }()
	if got == nil {
		t.Fatal("a constructor could not inject lifecycle.Lifecycle from the root container")
	}
	if !got.Ready() {
		t.Error("the injected lifecycle does not report ready after Start")
	}
}

func TestStartTwice(t *testing.T) {
	t.Parallel()

	app := warren.New(warren.NewModule("x"))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() { _ = app.Stop(context.Background()) }()
	if err := app.Start(context.Background()); err == nil {
		t.Fatal("second Start returned nil — an App boots once")
	}
}

func TestStopBeforeStartIsANoOp(t *testing.T) {
	t.Parallel()

	if err := warren.New().Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

// makeGreeter is the review's factory pattern: one call site, two distinct
// modules with different closure state.
func makeGreeter(greeting string) warren.Module {
	return warren.NewModule("greeter",
		warren.Providers(func() string { return greeting }),
	)
}

func TestFactoryCalledTwiceIsADuplicateNotASilentDedup(t *testing.T) {
	t.Parallel()

	err := warren.New(makeGreeter("hello"), makeGreeter("goodbye")).Start(context.Background())
	if err == nil {
		t.Fatal("two modules from one factory booted — one closure's state silently replaced the other's")
	}
	if !strings.Contains(err.Error(), "✗ duplicate module name") ||
		!strings.Contains(err.Error(), "a module factory called twice") {
		t.Errorf("diagnostic does not explain the factory mistake:\n%s", err)
	}
}

func TestSharedModuleValueThroughTwoPathsIsOneModule(t *testing.T) {
	t.Parallel()

	built := 0
	platform := warren.NewModule("platform",
		warren.Providers(func() *pool { built++; return &pool{} }),
		warren.Exports[*pool](),
	)
	// The same VALUE reaches the graph via the New list and via two imports —
	// §2.1's package-variable pattern. One module, one instantiation.
	a := warren.NewModule("a", warren.Imports(platform),
		warren.Consumers(func(*pool) *userService { return &userService{} }))
	b := warren.NewModule("b", warren.Imports(platform, platform),
		warren.Consumers(func(*pool) *invoiceService { return &invoiceService{} }))

	app := warren.New(platform, a, b)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = app.Stop(context.Background()) }()
	if built != 1 {
		t.Errorf("the shared platform pool was built %d times, want 1", built)
	}
}

type multiRepo struct{}

type multiCleaner struct{}

func TestMultiOutputConstructorWithTwoExports(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("m",
		warren.Providers(func() (*multiRepo, *multiCleaner) { return &multiRepo{}, &multiCleaner{} }),
		warren.Exports[*multiRepo](),
		warren.Exports[*multiCleaner](),
	)
	app := warren.New(m)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("a constructor providing both exported outputs failed boot: %v", err)
	}
	defer func() { _ = app.Stop(context.Background()) }()
}

func TestAmbiguousExportsNameTheModules(t *testing.T) {
	t.Parallel()

	p1 := warren.NewModule("platformA",
		warren.Providers(func() *pool { return &pool{} }), warren.Exports[*pool]())
	p2 := warren.NewModule("platformB",
		warren.Providers(func() *pool { return &pool{} }), warren.Exports[*pool]())
	consumer := warren.NewModule("cons",
		warren.Imports(p1, p2),
		warren.Consumers(func(*pool) *userService { return &userService{} }),
	)
	err := warren.New(p1, p2, consumer).Start(context.Background())
	if err == nil {
		t.Fatal("two imports exporting one type booted")
	}
	if strings.Contains(err.Error(), "makeFuncStub") {
		t.Errorf("diagnostic names a runtime stub instead of the modules:\n%s", err)
	}
	if !strings.Contains(err.Error(), `exported by module "platformA"`) ||
		!strings.Contains(err.Error(), `exported by module "platformB"`) {
		t.Errorf("diagnostic does not name both exporting modules:\n%s", err)
	}
}

func TestForwardedFailureRendersOneBlock(t *testing.T) {
	t.Parallel()

	boom := stderrors.New("connection refused")
	platform := warren.NewModule("platformF",
		warren.Providers(func() (*pool, error) { return nil, boom }),
		warren.Exports[*pool](),
	)
	consumer := warren.NewModule("consF",
		warren.Imports(platform),
		warren.Consumers(func(*pool) *userService { return &userService{} }),
	)
	err := warren.New(platform, consumer).Start(context.Background())
	if err == nil {
		t.Fatal("a failing exported constructor booted")
	}
	if got := strings.Count(err.Error(), "✗ constructor failed"); got != 1 {
		t.Errorf("one failure rendered %d blocks across the import hop, want 1:\n%s", got, err)
	}
	if !stderrors.Is(err, boom) {
		t.Errorf("the root cause is unreachable: %v", err)
	}
}

func TestTwoConfigStructs(t *testing.T) {
	t.Parallel()

	type appCfg struct {
		Name string `config:"name" default:"svc"`
	}
	type infraCfg struct {
		Port int `config:"port" default:"8080"`
	}
	gotName, gotPort := "", 0
	m := warren.NewModule("uses",
		warren.Imports(config.Module[appCfg](), config.Module[infraCfg]()),
		warren.Consumers(func(a appCfg, i infraCfg) *userService {
			gotName, gotPort = a.Name, i.Port
			return &userService{}
		}),
	)
	app := warren.New(m)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("two config modules with different structs failed boot: %v", err)
	}
	defer func() { _ = app.Stop(context.Background()) }()
	if gotName != "svc" || gotPort != 8080 {
		t.Errorf("resolved %q/%d, want both config structs' defaults", gotName, gotPort)
	}
}

func TestStopDuringBoot(t *testing.T) {
	t.Parallel()

	var app *warren.App
	m := warren.NewModule("selfstop",
		warren.Consumers(func() *pool {
			// A Stop landing mid-boot (steps 2–5): the boot must report
			// itself abandoned, and readiness must never open.
			_ = app.Stop(context.Background())
			return &pool{}
		}),
		warren.OnStart(func(context.Context) error {
			t.Error("an OnStart hook ran after Stop arrived mid-boot")
			return nil
		}),
	)
	app = warren.New(m)
	err := app.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil though Stop arrived during boot")
	}
	if !strings.Contains(err.Error(), "boot abandoned") {
		t.Errorf("Start error = %q, want the boot-abandoned diagnostic", err)
	}
}

func TestFailingConstructorSurfacesAtBoot(t *testing.T) {
	t.Parallel()

	boom := stderrors.New("connection refused")
	m := warren.NewModule("platform",
		warren.Consumers(func() (*pool, error) { return nil, boom }),
	)
	err := warren.New(m).Start(context.Background())
	if err == nil {
		t.Fatal("Start with a failing constructor returned nil")
	}
	if !strings.Contains(err.Error(), "✗ constructor failed") || !stderrors.Is(err, boom) {
		t.Errorf("the constructor's own error did not surface:\n%s", err)
	}
}

// --- Fixes from the 2026-08-02 framework-user feedback round ---

func TestInvokeReachesWhatBootBuilt(t *testing.T) {
	t.Parallel()

	platform := warren.NewModule("platformI",
		warren.Providers(func() *pool { return &pool{} }),
		warren.Exports[*pool](),
	)
	user := warren.NewModule("userI",
		warren.Imports(platform),
		warren.Providers(func(p *pool) userRepository { return &pgUserRepository{p: p} }),
		warren.Consumers(func(userRepository) *invoiceService { return &invoiceService{} }),
	)
	app := warren.New(platform, user)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = app.Stop(context.Background()) }()

	t.Run("reaches a module's own binding", func(t *testing.T) {
		var got userRepository
		if err := app.Invoke("userI", func(r userRepository) { got = r }); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if got == nil || got.Kind() != "pg" {
			t.Error("Invoke did not deliver the repository the boot built")
		}
	})

	t.Run("module encapsulation still holds", func(t *testing.T) {
		// platformI cannot see user's private binding.
		err := app.Invoke("platformI", func(userRepository) {})
		if err == nil {
			t.Fatal("Invoke crossed the module boundary")
		}
		if !strings.Contains(err.Error(), "✗ cannot resolve dependency") {
			t.Errorf("not the resolution diagnostic:\n%s", err)
		}
	})

	t.Run("an unknown module names the ones that exist", func(t *testing.T) {
		err := app.Invoke("nope", func(*pool) {})
		if err == nil || !strings.Contains(err.Error(), "the graph has: platformI, userI") {
			t.Errorf("error = %v, want the known-module list", err)
		}
	})
}

func TestInvokeBeforeStart(t *testing.T) {
	t.Parallel()

	err := warren.New(warren.NewModule("x")).Invoke("x", func() {})
	if err == nil || !strings.Contains(err.Error(), "Invoke before Start") {
		t.Errorf("error = %v, want the boot-first diagnostic", err)
	}
}

type eagerThing struct{}

func TestEagerMaterialisesUnconsumedProviders(t *testing.T) {
	t.Parallel()

	t.Run("an eager provider runs even when nothing consumes it", func(t *testing.T) {
		t.Parallel()
		built := false
		m := warren.NewModule("eager",
			warren.Providers(func() *eagerThing { built = true; return &eagerThing{} }),
			warren.Eager[*eagerThing](),
		)
		app := warren.New(m)
		if err := app.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = app.Stop(context.Background()) }()
		if !built {
			t.Error("the eager provider was never built")
		}
	})

	t.Run("its failure fails the boot", func(t *testing.T) {
		t.Parallel()
		boom := stderrors.New("bad config")
		m := warren.NewModule("eagerFail",
			warren.Providers(func() (*eagerThing, error) { return nil, boom }),
			warren.Eager[*eagerThing](),
		)
		err := warren.New(m).Start(context.Background())
		if err == nil || !stderrors.Is(err, boom) {
			t.Errorf("Start = %v, want the eager provider's failure at boot", err)
		}
	})

	t.Run("without Eager an unconsumed provider is never built", func(t *testing.T) {
		t.Parallel()
		built := false
		m := warren.NewModule("lazy",
			warren.Providers(func() *eagerThing { built = true; return &eagerThing{} }),
		)
		app := warren.New(m)
		if err := app.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = app.Stop(context.Background()) }()
		if built {
			t.Error("an unconsumed, non-eager provider was built")
		}
	})
}

// TestModuleFactorySiteIsTheCaller pins the diagnostic fix: a module factory
// records ITS CALLER's line, never its own body inside the framework.
func TestModuleFactorySiteIsTheCaller(t *testing.T) {
	t.Parallel()

	a := config.Module[struct{}]()
	b := config.Module[struct{}]()
	err := warren.New(a, b).Start(context.Background())
	if err == nil {
		t.Fatal("two config modules for one type booted")
	}
	if strings.Contains(err.Error(), "config/config.go") {
		t.Errorf("the diagnostic points into the framework instead of the user's call site:\n%s", err)
	}
	if !strings.Contains(err.Error(), "warren_test.go") {
		t.Errorf("the diagnostic does not name the caller's file:\n%s", err)
	}
}
