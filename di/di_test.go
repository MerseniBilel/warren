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
		if !strings.Contains(err.Error(), `is exported by module "user"`) {
			t.Errorf("diagnostic does not point at the exported candidate:\n%s", err)
		}
		// Pointing at the candidate is half an answer; the reader still has to
		// know that Imports is the word. Field-test defect B3.
		if !strings.Contains(err.Error(), `warren.Imports(user.Module())`) {
			t.Errorf("diagnostic does not say what to type:\n%s", err)
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

type tDep struct{}

type rootThing struct{}

type twoScopeHandler struct{}

// TestScopeRelativeResolvabilityDoesNotLeakDig covers the review's F1: a
// type resolvable from a child scope but not from root must fail the
// pre-check with Warren's diagnostic — never reach dig and leak its wording.
func TestScopeRelativeResolvabilityDoesNotLeakDig(t *testing.T) {
	t.Parallel()

	root := di.New()
	userScope := root.Scope("user")
	if err := userScope.Provide(func() *tDep { return &tDep{} }); err != nil {
		t.Fatalf("providing dep: %v", err)
	}
	if err := root.Provide(func(*tDep) *rootThing { return &rootThing{} }); err != nil {
		t.Fatalf("providing root thing: %v", err)
	}
	if err := userScope.Provide(func(*tDep, *rootThing) *twoScopeHandler { return &twoScopeHandler{} }); err != nil {
		t.Fatalf("providing handler: %v", err)
	}

	// *tDep resolves from "user", but *rootThing's constructor lives in root,
	// where *tDep is invisible. The memo must not carry the child-scope
	// success into the root-scope check.
	_, err := di.Resolve[*twoScopeHandler](userScope)
	if err == nil {
		t.Fatal("Resolve succeeded though rootThing's constructor cannot see *tDep")
	}
	if !strings.Contains(err.Error(), "✗ cannot resolve dependency") {
		t.Errorf("not Warren's diagnostic:\n%s", err)
	}
	assertNoDigLeak(t, err)
}

// TestChainDoesNotRouteThroughInvisibleSibling covers the review's F2: the
// requirement chain must follow a consumer that can actually see the
// provider, not a type-name coincidence in an unrelated sibling scope.
func TestChainDoesNotRouteThroughInvisibleSibling(t *testing.T) {
	t.Parallel()

	root := di.New()
	userScope := root.Scope("user")
	billing := root.Scope("billing") // alphabetically before "user"

	if err := userScope.Provide(user.NewRegisterUserHandler, di.DeclaredAt("internal/modules/user/module.go", 12)); err != nil {
		t.Fatalf("providing handler: %v", err)
	}
	if err := userScope.Provide(user.NewUserController, di.DeclaredAt("internal/modules/user/module.go", 14)); err != nil {
		t.Fatalf("providing controller: %v", err)
	}
	// billing declares a consumer of the same handler type — but it cannot
	// see user's provider, so the chain must not route through it.
	if err := billing.Provide(func(*user.RegisterUserHandler) *tDep { return &tDep{} }, di.DeclaredAt("internal/modules/billing/module.go", 99)); err != nil {
		t.Fatalf("providing billing consumer: %v", err)
	}

	err := userScope.Validate()
	if err == nil {
		t.Fatal("Validate() = nil with the repository missing")
	}
	if strings.Contains(err.Error(), "billing/module.go") {
		t.Errorf("the chain routed through an invisible sibling's module:\n%s", err)
	}
	if !strings.Contains(err.Error(), "internal/modules/user/module.go:14") {
		t.Errorf("the chain does not end at the real consumer's declaration:\n%s", err)
	}
}

// TestInvokeVariadic covers the review's F4: dig invokes variadic functions
// without supplying the trailing parameter, so the pre-check must not demand
// a provider for it.
func TestInvokeVariadic(t *testing.T) {
	t.Parallel()

	called := false
	if err := di.New().Invoke(func(xs ...string) { called = true }); err != nil {
		t.Fatalf("Invoke of a variadic function: %v", err)
	}
	if !called {
		t.Fatal("the variadic function was not called")
	}
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

func TestVariadicConstructorIsProvidedWithNoArguments(t *testing.T) {
	t.Parallel()

	// Warren's own constructors take options — memory.New, health.New,
	// inbox.NewMemoryStore. Rejecting them as providers would make the
	// framework unusable with itself, so a variadic constructor is provided
	// with no variadic arguments and its trailing slice is not a dependency.
	root := di.New()
	if err := root.Provide(func(opts ...string) *user.UserService {
		if len(opts) != 0 {
			t.Errorf("variadic parameter received %d arguments, want none", len(opts))
		}
		return user.NewUserService()
	}); err != nil {
		t.Fatalf("Provide of a variadic constructor: %v", err)
	}
	got, err := di.Resolve[*user.UserService](root)
	if err != nil || got == nil {
		t.Fatalf("Resolve: %v", err)
	}
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

	t.Run("a diamond renders both occurrences as found", func(t *testing.T) {
		t.Parallel()
		root := di.New()
		if err := root.Provide(func() *tDep { return &tDep{} }); err != nil {
			t.Fatalf("providing: %v", err)
		}
		if err := root.Provide(func(*tDep) *rootThing { return &rootThing{} }); err != nil {
			t.Fatalf("providing: %v", err)
		}
		if err := root.Provide(func(*tDep, *rootThing) *twoScopeHandler { return &twoScopeHandler{} }); err != nil {
			t.Fatalf("providing: %v", err)
		}

		out := root.Explain((*twoScopeHandler)(nil)).String()
		if strings.Contains(out, "no provider visible") {
			t.Errorf("a healthy diamond rendered as broken:\n%s", out)
		}
	})

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

// TestForwardedBindingIsNeverACandidate covers field-test defect B3. A module
// that imports an exported type gets a FORWARDER registered in its own scope.
// A forwarder is not a place a user can add anything: telling a third module
// to add warren.Exports there names a type that module does not provide, and
// the suggested fix fails the boot. The origin provider is already in the
// candidate list, so the forwarder must be dropped entirely.
func TestForwardedBindingIsNeverACandidate(t *testing.T) {
	t.Parallel()

	root := di.New()
	platform := root.Scope("platform")
	billing := root.Scope("billing")
	userScope := root.Scope("user")

	if err := platform.Provide(postgres.NewUserRepository, di.Exported(), di.DeclaredAt("internal/platform/module.go", 20)); err != nil {
		t.Fatalf("providing repository: %v", err)
	}
	// billing imports platform: the bootstrapper's forwarder, verbatim.
	if err := billing.Provide(
		func() domain.UserRepository { return nil },
		di.Named(`domain.UserRepository (exported by module "platform")`),
		di.ForwardedFrom("platform"),
	); err != nil {
		t.Fatalf("providing forwarder: %v", err)
	}
	// user does not import platform, so the repository is missing there.
	if err := userScope.Provide(user.NewRegisterUserHandler, di.DeclaredAt("internal/modules/user/module.go", 12)); err != nil {
		t.Fatalf("providing handler: %v", err)
	}

	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil though user cannot see the repository")
	}
	got := err.Error()
	if strings.Contains(got, `scope "billing"`) {
		t.Errorf("the forwarder in billing was offered as a candidate:\n%s", got)
	}
	// The forwarder's synthesized name is parenthesised — "domain.UserRepository
	// (exported by module "platform")". The corrected diagnostic's own sentence
	// uses the same words unparenthesised, so match the shape, not the phrase.
	if strings.Contains(got, `(exported by module`) {
		t.Errorf("a forwarder's synthesized name reached the diagnostic:\n%s", got)
	}
	if !strings.Contains(got, `warren.Imports(platform.Module())`) {
		t.Errorf("diagnostic does not offer the real fix, warren.Imports:\n%s", got)
	}
	if !strings.Contains(got, "postgres.NewUserRepository") {
		t.Errorf("diagnostic does not name the origin provider:\n%s", got)
	}
	assertNoDigLeak(t, err)
}

// TestExportedCandidateGoldenDiagnostic locks the second shape of the
// missing-provider block: the type IS exported, by a module the asking one
// does not import. warren.md §2.2 shows the not-exported shape; this is its
// sibling, and the two fixes are different sentences.
func TestExportedCandidateGoldenDiagnostic(t *testing.T) {
	t.Parallel()

	root := di.New()
	platform := root.Scope("platform")
	userScope := root.Scope("user")

	if err := platform.Provide(postgres.NewUserRepository, di.Exported(), di.DeclaredAt("internal/platform/module.go", 20)); err != nil {
		t.Fatalf("providing repository: %v", err)
	}
	if err := userScope.Provide(user.NewRegisterUserHandler, di.DeclaredAt("internal/modules/user/module.go", 12)); err != nil {
		t.Fatalf("providing handler: %v", err)
	}
	if err := userScope.Provide(user.NewUserController, di.DeclaredAt("internal/modules/user/module.go", 14)); err != nil {
		t.Fatalf("providing controller: %v", err)
	}

	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil though user does not import platform")
	}
	assertGolden(t, "missing_provider_exported", err.Error())
	assertNoDigLeak(t, err)
}

// TestCandidateBulletsAreValidGo covers the third half of B3: every bullet
// that tells the user what to type must be something they can actually type.
func TestCandidateBulletsAreValidGo(t *testing.T) {
	t.Parallel()

	root, _, _ := section12Graph(t)
	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil with the repository missing")
	}
	for line := range strings.SplitSeq(err.Error(), "\n") {
		// Every "type this" line ends in a warren.X(...) call; the prose
		// bullets above them do not.
		_, expr, ok := strings.Cut(strings.TrimSpace(line), "warren.")
		if !ok {
			continue
		}
		expr = "warren." + expr
		if strings.Contains(expr, `"`) {
			t.Errorf("suggested fix is not valid Go, it carries a quoted scope name: %q", expr)
		}
		if strings.Contains(expr, " ") {
			t.Errorf("suggested fix is not valid Go, it carries a bare space: %q", expr)
		}
	}
}

// TestSuggestedFixesAreReferenceableGo — field test #4, defects B1 and B2,
// both of which are regressions from the B3 fix that added the Imports line.
//
// The diagnostic interpolated the SCOPE NAME into a Go expression. That is
// only valid when a module's name happens to equal its package name, which is
// true for application modules (platform, user) and false for every adapter
// Warren ships — they name themselves by path:
//
//	Add to billing's module: warren.Imports(warren/persistence/postgres.Module())
//
// which does not parse. And "Or provide it locally" printed a reflection-
// derived function name as if it were source:
//
//	warren.Providers(platform.newUnitOfWork)   ← unexported, unreferenceable
//	warren.Providers(postgres.Module.func2)    ← a closure, not an identifier
//
// A copy-pasteable fix is the entire reason dig is wrapped rather than
// re-exported, so a line that cannot be pasted is worse than no line.
func TestSuggestedFixesAreReferenceableGo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		scope      string // the providing module's name
		ctor       any    // its constructor
		wantNoPipe []string
	}{
		{
			name:  "adapter module named by path",
			scope: "warren/persistence/postgres",
			ctor:  postgres.NewUserRepository,
		},
		{
			name:  "module whose name is a plain identifier",
			scope: "platform",
			ctor:  postgres.NewUserRepository,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := di.New()
			owner := root.Scope(tc.scope)
			asking := root.Scope("billing")

			if err := owner.Provide(tc.ctor, di.Exported()); err != nil {
				t.Fatalf("providing: %v", err)
			}
			if err := asking.Provide(user.NewRegisterUserHandler); err != nil {
				t.Fatalf("providing handler: %v", err)
			}

			err := root.Validate()
			if err == nil {
				t.Fatal("Validate() = nil though billing cannot see the repository")
			}
			assertPasteable(t, err.Error())
		})
	}
}

// TestUnexportedProviderIsNotOfferedAsSource — the "provide it locally" line
// named an identifier the asking package cannot reference.
func TestUnexportedProviderIsNotOfferedAsSource(t *testing.T) {
	t.Parallel()

	root := di.New()
	platform := root.Scope("platform")
	billing := root.Scope("billing")

	// A lowercase constructor, exactly like the scaffold's platform module.
	if err := platform.Provide(newUnexportedRepository, di.Exported()); err != nil {
		t.Fatalf("providing: %v", err)
	}
	if err := billing.Provide(user.NewRegisterUserHandler); err != nil {
		t.Fatalf("providing handler: %v", err)
	}

	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil though billing cannot see the repository")
	}
	got := err.Error()
	if strings.Contains(got, "warren.Providers(di_test.newUnexportedRepository)") {
		t.Errorf("an unexported constructor was offered as something to type:\n%s", got)
	}
	assertPasteable(t, got)
}

func newUnexportedRepository() domain.UserRepository { return nil }

// assertPasteable checks every line that tells the reader what to type. Each
// such line ends in a warren.X(...) call, and every one of them has to be
// something that compiles where it is pasted.
func assertPasteable(t *testing.T, out string) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		_, expr, ok := strings.Cut(strings.TrimSpace(line), "warren.")
		if !ok {
			continue
		}
		expr = "warren." + expr
		// An expression containing "..." is a REFERENCE, not something to
		// type — "add it to warren.Imports(...)" names the option so the
		// reader can find it. A line you are meant to paste never elides its
		// own arguments, so the ellipsis tells the two apart.
		if strings.Contains(expr, "...") {
			continue
		}
		// A Go expression carries no quotes, no spaces, and no slashes — a
		// slash means a module PATH reached a position only an identifier
		// can occupy.
		for _, bad := range []string{`"`, " ", "/"} {
			if strings.Contains(expr, bad) {
				t.Errorf("suggested fix is not valid Go (contains %q): %s", bad, expr)
			}
		}
	}
}

// implementer is a concrete type satisfying domain.UserRepository — what a
// user ends up with when they forget to declare the constructor's return
// type as the PORT.
type implementer struct{}

func (implementer) FindByID(string) (string, error) { return "", nil }

// TestAConcreteProviderOfTheRequiredInterfaceIsSuggested — field test #8's
// last diagnostic gap. Declaring a constructor's return type as the concrete
// type rather than the port is the commonest wiring mistake there is, and
// warren.Exports's own doc already names it — but the resolve diagnostic said
// nothing at all:
//
//	✗ cannot resolve dependency
//	    domain.UserRepository
//	  No provider found in scope "billing" or its imports.
//
// while a provider satisfying that interface sat in the SAME scope. The
// near-miss hinting that fires for the cross-module case did not fire here,
// which is where it is needed most: the fix is one word on one line the user
// is already looking at.
func TestAConcreteProviderOfTheRequiredInterfaceIsSuggested(t *testing.T) {
	t.Parallel()

	root := di.New()
	billing := root.Scope("billing")
	if err := billing.Provide(func() *implementer { return &implementer{} }); err != nil {
		t.Fatalf("providing: %v", err)
	}
	if err := billing.Provide(user.NewRegisterUserHandler); err != nil {
		t.Fatalf("providing handler: %v", err)
	}

	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil though nothing provides the port")
	}
	got := err.Error()
	if !strings.Contains(got, "*di_test.implementer") {
		t.Errorf("the diagnostic does not name the type that already satisfies the port:\n%s", got)
	}
	if !strings.Contains(got, "domain.UserRepository") {
		t.Errorf("the diagnostic does not name the port:\n%s", got)
	}
	assertNoDigLeak(t, err)
}

// TestAnUnrelatedConcreteTypeIsNotSuggested — the hint must only fire for a
// type that actually implements the port. A near-miss list that includes
// everything is noise.
func TestAnUnrelatedConcreteTypeIsNotSuggested(t *testing.T) {
	t.Parallel()

	root := di.New()
	billing := root.Scope("billing")
	if err := billing.Provide(func() *tDep { return &tDep{} }); err != nil {
		t.Fatalf("providing: %v", err)
	}
	if err := billing.Provide(user.NewRegisterUserHandler); err != nil {
		t.Fatalf("providing handler: %v", err)
	}

	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil")
	}
	if strings.Contains(err.Error(), "tDep") {
		t.Errorf("an unrelated type was offered as satisfying the port:\n%s", err)
	}
}

// TestAnUnprovidedTypeStillGetsAClosingSuggestion — field test #10, defect 5.
// When nothing in the graph provides the type AND nothing satisfies it, the
// diagnostic simply stopped:
//
//	✗ cannot resolve dependency
//	    domain.StockRepository
//	      └─ required by app.Handler[…]
//	  No provider found in scope "ordering" or its imports.
//
// The chain is excellent and the reader is then on their own. The two hint
// blocks above this one only fire when there is a candidate to name, which
// the docs never said — so the showcased "Did you mean" appeared to have
// vanished. There are only two shapes the fix can take, and saying both is
// better than saying nothing.
func TestAnUnprovidedTypeStillGetsAClosingSuggestion(t *testing.T) {
	t.Parallel()

	root := di.New()
	ordering := root.Scope("ordering")
	// A handler that needs a port nothing provides and nothing satisfies.
	if err := ordering.Provide(user.NewRegisterUserHandler); err != nil {
		t.Fatalf("providing handler: %v", err)
	}

	err := root.Validate()
	if err == nil {
		t.Fatal("Validate() = nil though nothing provides the port")
	}
	got := err.Error()
	for _, want := range []string{
		"warren.Providers", // the local fix
		"warren.Imports",   // the other-module fix
		"warren.Exports",   // and what the other module must have done
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the diagnostic never names %s:\n%s", want, got)
		}
	}
	assertNoDigLeak(t, err)
}

// TestAFailedConstructorIsNamed — field test #11 called this Warren's worst
// diagnostic, and it was the only one that named neither a symbol nor a
// file:line:
//
//	✗ constructor failed
//	    EOF
//	  A constructor returned this error while the graph for scope "ordering"
//	  was being built. Fix the constructor, or the configuration it read.
//
// With a realistic opaque cause — EOF, "permission denied", a context
// deadline — that leaves you grepping a Providers list by hand. Every other
// Warren diagnostic names the type, the scope, and the declaration site.
func TestAFailedConstructorIsNamed(t *testing.T) {
	t.Parallel()

	root := di.New()
	if err := root.Provide(newFailingService); err != nil {
		t.Fatalf("providing: %v", err)
	}

	_, err := di.Resolve[*user.UserService](root)
	if err == nil {
		t.Fatal("Resolve through a failing constructor succeeded")
	}
	got := err.Error()
	if !strings.Contains(got, "newFailingService") {
		t.Errorf("the diagnostic does not name the constructor that failed:\n%s", got)
	}
	if !strings.Contains(got, "di_test.go:") {
		t.Errorf("the diagnostic does not give the constructor's file:line:\n%s", got)
	}
	// The cause must survive intact, both as text and to errors.Is — the
	// attribution wraps it, it does not replace it.
	if !strings.Contains(got, "EOF") {
		t.Errorf("the constructor's own error was lost:\n%s", got)
	}
	if !stderrors.Is(err, errFailingService) {
		t.Error("errors.Is no longer reaches the constructor's error")
	}
	assertNoDigLeak(t, err)
}

// newFailingService is a package-level function so it has a name and a
// file:line to report — a closure would print as "func1", which is the
// unhelpful shape this test exists to rule out.
func newFailingService() (*user.UserService, error) {
	return nil, errFailingService
}

var errFailingService = stderrors.New("EOF")
