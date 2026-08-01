package di_test

import (
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/di/internal/fixture/domain"
	"github.com/MerseniBilel/warren/di/internal/fixture/postgres"
	"github.com/MerseniBilel/warren/di/internal/fixture/user"
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

// assertNoDigLeak is the leak test: no dig phrasing in any diagnostic
// (invariant 2).
func assertNoDigLeak(t *testing.T, err error) {
	t.Helper()
	for _, phrase := range []string{"missing dependencies for function", "go.uber.org/dig", "dig."} {
		if strings.Contains(err.Error(), phrase) {
			t.Errorf("diagnostic leaks dig wording %q:\n%s", phrase, err)
		}
	}
}

// section12Graph builds §1.2's fixture: user needs domain.UserRepository,
// which is registered in billing and not exported.
func section12Graph(t *testing.T) (root, userScope, billing di.Container) {
	t.Helper()
	root = di.New()
	userScope = root.Scope("user")
	billing = root.Scope("billing")

	if err := userScope.Provide(user.NewRegisterUserHandler, di.DeclaredAt("internal/modules/user/module.go", 12)); err != nil {
		t.Fatalf("providing handler: %v", err)
	}
	if err := userScope.Provide(user.NewUserController, di.DeclaredAt("internal/modules/user/module.go", 14)); err != nil {
		t.Fatalf("providing controller: %v", err)
	}
	if err := billing.Provide(postgres.NewUserRepository, di.DeclaredAt("internal/modules/billing/module.go", 9)); err != nil {
		t.Fatalf("providing repository: %v", err)
	}
	return root, userScope, billing
}

func TestGoldenMissingProviderDiagnostic(t *testing.T) {
	t.Parallel()

	root, _, _ := section12Graph(t)
	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil on a graph with a missing provider")
	}
	assertGolden(t, "missing_provider", err.Error())
	assertNoDigLeak(t, err)
}

func TestValidateConstructsNothing(t *testing.T) {
	t.Parallel()

	root := di.New()
	constructed := false
	if err := root.Provide(func() *user.UserService { constructed = true; return user.NewUserService() }); err != nil {
		t.Fatalf("providing: %v", err)
	}
	if err := root.Provide(user.NewRegisterUserHandler); err != nil {
		t.Fatalf("providing: %v", err)
	}

	if err := root.Validate(); err == nil {
		t.Fatal("Validate() = nil on a graph with a missing provider")
	}
	if constructed {
		t.Error("Validate() ran a constructor — step 3 must complete before step 4 instantiates anything")
	}
}

func TestEncapsulation(t *testing.T) {
	t.Parallel()

	root := di.New()
	userScope := root.Scope("user")
	billing := root.Scope("billing")

	if err := userScope.Provide(user.NewUserService); err != nil {
		t.Fatalf("providing private service: %v", err)
	}
	if err := userScope.Provide(postgres.NewUserRepository, di.Exported()); err != nil {
		t.Fatalf("providing exported repository: %v", err)
	}

	// The three subtests share the containers and their order is the story:
	// invisible → visible-but-not-imported → forwarded. Not parallel.
	t.Run("a sibling's private binding is invisible", func(t *testing.T) {
		_, err := di.Resolve[*user.UserService](billing)
		if err == nil {
			t.Fatal("Resolve of a sibling's private binding succeeded — the module boundary is gone")
		}
		if !strings.Contains(err.Error(), `is registered in scope "user" but not exported`) {
			t.Errorf("diagnostic does not point at the unexported candidate:\n%s", err)
		}
		assertNoDigLeak(t, err)
	})

	t.Run("an exported binding still needs the import copied in", func(t *testing.T) {
		_, err := di.Resolve[domain.UserRepository](billing)
		if err == nil {
			t.Fatal("Resolve without the bootstrapper's copy-in succeeded")
		}
		if !strings.Contains(err.Error(), `is exported by scope "user"`) {
			t.Errorf("diagnostic does not point at the exported candidate:\n%s", err)
		}
	})

	t.Run("the bootstrapper's forwarder makes an exported binding resolvable", func(t *testing.T) {
		if err := billing.Provide(func() domain.UserRepository {
			return di.MustResolve[domain.UserRepository](userScope)
		}); err != nil {
			t.Fatalf("providing forwarder: %v", err)
		}
		repo, err := di.Resolve[domain.UserRepository](billing)
		if err != nil {
			t.Fatalf("Resolve through the forwarder: %v", err)
		}
		if repo == nil {
			t.Fatal("Resolve returned a nil repository")
		}
	})
}

func TestAmbiguousBinding(t *testing.T) {
	t.Parallel()

	t.Run("same scope, at Provide time", func(t *testing.T) {
		t.Parallel()
		root := di.New()
		if err := root.Provide(postgres.NewUserRepository); err != nil {
			t.Fatalf("first Provide: %v", err)
		}
		err := root.Provide(postgres.NewUserRepository)
		if err == nil {
			t.Fatal("second Provide of the same type in one scope succeeded")
		}
		if !strings.Contains(err.Error(), "✗ ambiguous binding") {
			t.Errorf("diagnostic is not the ambiguous-binding block:\n%s", err)
		}
		assertNoDigLeak(t, err)
	})

	t.Run("across scope and ancestor, at Validate time", func(t *testing.T) {
		t.Parallel()
		root := di.New()
		userScope := root.Scope("user")
		if err := root.Provide(postgres.NewUserRepository); err != nil {
			t.Fatalf("root Provide: %v", err)
		}
		if err := userScope.Provide(postgres.NewUserRepository); err != nil {
			t.Fatalf("scope Provide: %v", err)
		}
		if err := userScope.Provide(user.NewRegisterUserHandler); err != nil {
			t.Fatalf("handler Provide: %v", err)
		}
		err := root.Validate()
		if err == nil {
			t.Fatal("Validate() = nil with two visible providers of one type")
		}
		if !strings.Contains(err.Error(), `domain.UserRepository has 2 providers visible from scope "user"`) {
			t.Errorf("diagnostic does not name the type and count:\n%s", err)
		}
		assertNoDigLeak(t, err)
	})
}

type cycleA struct{}

type cycleB struct{}

func TestDependencyCycle(t *testing.T) {
	t.Parallel()

	root := di.New()
	if err := root.Provide(func(*cycleB) *cycleA { return &cycleA{} }); err != nil {
		t.Fatalf("providing A: %v", err)
	}
	err := root.Provide(func(*cycleA) *cycleB { return &cycleB{} })
	if err == nil {
		t.Fatal("Provide closing a dependency cycle succeeded")
	}
	assertGolden(t, "cycle", err.Error())
	assertNoDigLeak(t, err)
}

func TestNonConstructor(t *testing.T) {
	t.Parallel()

	root := di.New()
	cases := []struct {
		name string
		got  any
	}{
		{"not a function", 42},
		{"nil", nil},
		{"no returns", func() {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := root.Provide(tc.got)
			if err == nil {
				t.Fatalf("Provide(%v) succeeded", tc.got)
			}
			if !strings.Contains(err.Error(), "✗ not a constructor") {
				t.Errorf("diagnostic is not the non-constructor block:\n%s", err)
			}
			assertNoDigLeak(t, err)
		})
	}
}

func TestVariadicConstructor(t *testing.T) {
	t.Parallel()

	root := di.New()
	err := root.Provide(func(...string) *user.UserService { return user.NewUserService() })
	if err == nil {
		t.Fatal("Provide of a variadic constructor succeeded")
	}
	if !strings.Contains(err.Error(), "✗ variadic constructor") {
		t.Errorf("diagnostic is not the variadic block:\n%s", err)
	}
	assertNoDigLeak(t, err)
}

func TestConstructorError(t *testing.T) {
	t.Parallel()

	sentinel := stderrors.New("connection refused")
	root := di.New()
	if err := root.Provide(func() (*user.UserService, error) { return nil, sentinel }); err != nil {
		t.Fatalf("providing: %v", err)
	}

	_, err := di.Resolve[*user.UserService](root)
	if err == nil {
		t.Fatal("Resolve through a failing constructor succeeded")
	}
	if !strings.Contains(err.Error(), "✗ constructor failed") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("diagnostic does not present the constructor's own error:\n%s", err)
	}
	if !stderrors.Is(err, sentinel) {
		t.Error("stdlib errors.Is does not reach the constructor's error through the diagnostic")
	}
	assertNoDigLeak(t, err)
}

func TestResolveAndMustResolve(t *testing.T) {
	t.Parallel()

	t.Run("resolve returns the constructed value", func(t *testing.T) {
		t.Parallel()
		root := di.New()
		if err := root.Provide(postgres.NewUserRepository); err != nil {
			t.Fatalf("providing: %v", err)
		}
		repo, err := di.Resolve[domain.UserRepository](root)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if repo == nil {
			t.Fatal("Resolve returned nil")
		}
	})

	t.Run("must-resolve panics with the diagnostic", func(t *testing.T) {
		t.Parallel()
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("MustResolve on an empty container did not panic")
			}
			err, ok := r.(error)
			if !ok || !strings.Contains(err.Error(), "✗ cannot resolve dependency") {
				t.Errorf("panic value is not the resolution diagnostic: %v", r)
			}
		}()
		di.MustResolve[domain.UserRepository](di.New())
	})
}

func TestScopeIsIdempotent(t *testing.T) {
	t.Parallel()

	root := di.New()
	if root.Scope("user") != root.Scope("user") {
		t.Error("repeat Scope(name) did not return the same child")
	}
	if root.Scope("user") == root.Scope("billing") {
		t.Error("different names returned the same child")
	}
}

func TestExplain(t *testing.T) {
	t.Parallel()

	t.Run("renders the resolution tree", func(t *testing.T) {
		t.Parallel()
		root := di.New()
		userScope := root.Scope("user")
		if err := root.Provide(postgres.NewUserRepository); err != nil {
			t.Fatalf("providing: %v", err)
		}
		if err := userScope.Provide(user.NewRegisterUserHandler); err != nil {
			t.Fatalf("providing: %v", err)
		}

		r := userScope.Explain((*user.RegisterUserHandler)(nil))
		if !r.Found || r.Provider != "user.NewRegisterUserHandler" || r.Scope != "user" {
			t.Fatalf("Explain resolution = %+v, want the handler's provider in scope user", r)
		}
		if len(r.Inputs) != 1 || !r.Inputs[0].Found || r.Inputs[0].Scope != "root" {
			t.Fatalf("Explain inputs = %+v, want the repository resolved from root", r.Inputs)
		}
		out := r.String()
		for _, want := range []string{"*user.RegisterUserHandler", "provided by user.NewRegisterUserHandler", "domain.UserRepository", `scope "root"`} {
			if !strings.Contains(out, want) {
				t.Errorf("Explain rendering is missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("reports an unresolvable target", func(t *testing.T) {
		t.Parallel()
		r := di.New().Explain((*domain.UserRepository)(nil))
		if r.Found {
			t.Fatal("Explain found a provider in an empty container")
		}
		if !strings.Contains(r.String(), "no provider visible") {
			t.Errorf("rendering does not say the target is unresolvable:\n%s", r)
		}
	})
}
