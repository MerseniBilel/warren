package scaffold_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

func TestNewWritesTheWholeTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	err := scaffold.New(scaffold.Options{
		Dir:        dir,
		Name:       "myapp",
		ModulePath: "example.com/myapp",
		Version:    "v0.1.0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Every file the layout promises, and nothing missing: a scaffold with a
	// hole in it fails at `go build`, which is a worse first impression than
	// any error message.
	want := []string{
		"go.mod",
		".gitignore",
		"Makefile",
		"README.md",
		"cmd/myapp/main.go",
		"internal/config/config.go",
		"internal/config/config_test.go",
		"internal/platform/module.go",
		"internal/modules/user/controller.go",
		"internal/modules/user/module.go",
		"internal/modules/user/module_test.go",
		"internal/modules/user/domain/user.go",
		"internal/modules/user/application/register_user.go",
		"internal/modules/user/application/register_user_test.go",
		"internal/modules/user/infrastructure/user_repository.go",
		"internal/modules/notification/module.go",
		"internal/modules/notification/application/on_user_registered.go",
	}
	var got []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(got)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %s", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("wrote %d files, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

func TestGeneratedGoIsFormatted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	// Unformatted generated code teaches the wrong thing on line one.
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		formatted, err := scaffold.Format(src)
		if err != nil {
			t.Errorf("%s does not parse: %v", path, err)
			return nil
		}
		if string(formatted) != string(src) {
			t.Errorf("%s is not gofmt-clean", path)
		}
		return nil
	})
}

func TestSubstitutionsReachEveryFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "orders", ModulePath: "github.com/acme/orders", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}

	mod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	if !strings.Contains(string(mod), "module github.com/acme/orders") {
		t.Errorf("go.mod: %s", mod)
	}
	if !strings.Contains(string(mod), "github.com/MerseniBilel/warren v0.1.0") {
		t.Errorf("go.mod does not pin the framework version:\n%s", mod)
	}
	main, _ := os.ReadFile(filepath.Join(dir, "cmd/orders/main.go"))
	if !strings.Contains(string(main), `"github.com/acme/orders/internal/platform"`) {
		t.Errorf("main.go imports were not rewritten:\n%s", main)
	}
	// The env prefix lives with the config wiring, which platform owns.
	plat, _ := os.ReadFile(filepath.Join(dir, "internal/platform/module.go"))
	if !strings.Contains(string(plat), `WithEnvPrefix("ORDERS")`) {
		t.Errorf("platform does not use the app's env prefix:\n%s", plat)
	}
}

func TestRefusesANonEmptyTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	})
	if err == nil {
		t.Fatal("scaffolding over an existing file succeeded — it would clobber someone's work")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("the diagnostic does not name the conflicting file:\n%v", err)
	}
	// Nothing else was written.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("a refused scaffold left %d entries behind — it must be atomic", len(entries))
	}
}

func TestUnreleasedAdapterIsRefusedWithTheAlternative(t *testing.T) {
	t.Parallel()

	err := scaffold.New(scaffold.Options{
		Dir: t.TempDir(), Name: "myapp", ModulePath: "example.com/myapp",
		Version: "v0.1.0", Transport: "grpc",
	})
	if err == nil {
		t.Fatal("--transport grpc scaffolded against an adapter that does not exist")
	}
	for _, want := range []string{"transport/grpc", "does not exist yet", "serves HTTP"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic is missing %q — it must say what is released and what to do:\n%v", want, err)
		}
	}
}

// http is the transport the scaffold wires, so asking for it is not a
// refusal — the flag names what you already get.
func TestReleasedTransportIsAccepted(t *testing.T) {
	t.Parallel()

	if err := scaffold.New(scaffold.Options{
		Dir: t.TempDir(), Name: "myapp", ModulePath: "example.com/myapp",
		Version: "v0.1.0", Transport: "http",
	}); err != nil {
		t.Errorf("--transport http was refused: %v", err)
	}
}

func TestModulePathIsRequired(t *testing.T) {
	t.Parallel()

	err := scaffold.New(scaffold.Options{Dir: t.TempDir(), Name: "myapp", Version: "v0.1.0"})
	if err == nil || !strings.Contains(err.Error(), "--module") {
		t.Errorf("err = %v, want the missing flag named", err)
	}
}

// TestInvalidModulePathIsRefused — `warren new q2 --module 'not a module
// path!!'` exited 0 and wrote a tree whose every go command fails with
// "go.mod:1: usage: module module/path". --db and --transport are both
// validated; the one flag that is REQUIRED was not.
func TestInvalidModulePathIsRefused(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"not a module path!!",
		"has space/pkg",
		"trailing/",
		"/leading",
		"double//slash",
		"github.com/you/app\n",
		"tab\there",
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			err := scaffold.New(scaffold.Options{
				Dir: t.TempDir(), Name: "app", ModulePath: bad, Version: "v0.1.0",
				DB: "memory", Broker: "memory",
			})
			if err == nil {
				t.Fatalf("--module %q was accepted; every go command in that tree then fails", bad)
			}
			if !strings.Contains(err.Error(), "not a valid Go module path") {
				t.Errorf("diagnostic:\n%v", err)
			}
		})
	}
}

// TestOrdinaryModulePathsAreAccepted guards the check against being so
// strict it refuses what people actually type.
func TestOrdinaryModulePathsAreAccepted(t *testing.T) {
	t.Parallel()

	for _, good := range []string{
		"github.com/you/app",
		"example.com/inventory",
		"gitlab.com/org/sub-group/app.v2",
		"my-internal-thing",
		"github.com/you/app/v2",
		"git.company.internal/team/svc_name",
	} {
		t.Run(good, func(t *testing.T) {
			t.Parallel()
			if err := scaffold.New(scaffold.Options{
				Dir: t.TempDir(), Name: "app", ModulePath: good, Version: "v0.1.0",
				DB: "memory", Broker: "memory",
			}); err != nil {
				t.Errorf("--module %q was refused: %v", good, err)
			}
		})
	}
}

// TestFrameworkPathWritesReplaceDirectives — `warren new` produced a tree
// that did not build: "missing go.sum entry for module providing package
// github.com/MerseniBilel/warren/app", fifteen times over, because the
// framework is not published yet and go could not resolve the require.
// `warren new --help` claimed "It compiles and passes `go test` as
// generated". This is the flag that makes that true, and the CLI's own
// compile test now uses it rather than hand-patching go.mod — so the path a
// user takes is the path CI exercises.
func TestFrameworkPathWritesReplaceDirectives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0",
		FrameworkPath: "/somewhere/warren",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"replace github.com/MerseniBilel/warren => /somewhere/warren",
		"replace github.com/MerseniBilel/warren/transport/http => /somewhere/warren/transport/http",
	} {
		if !strings.Contains(string(mod), want) {
			t.Errorf("go.mod is missing %q:\n%s", want, mod)
		}
	}
}

// TestNoFrameworkPathWritesNoReplace — invariant 8 is about this repository,
// but a replace nobody asked for in a user's go.mod is its own bug: it
// would point at a path that does not exist on their machine.
func TestNoFrameworkPathWritesNoReplace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "replace") {
		t.Errorf("go.mod carries a replace nobody asked for:\n%s", mod)
	}
}
