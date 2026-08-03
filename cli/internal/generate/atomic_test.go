package generate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/generate"
)

// TestAFailedEditLeavesNothingBehind is the atomicity claim, tested at the
// point it actually breaks: the files are written first and the edit fails
// afterwards. Without a rollback the user is left with a file on disk that
// nothing references, and the next run refuses because that file exists —
// while telling them "nothing was written".
func TestAFailedEditLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	dir := app(t)
	// The aggregate first: a repository for an entity that does not exist is
	// refused before any of this runs, and this test is about the rollback
	// underneath.
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
		t.Fatalf("g entity: %v", err)
	}
	modPath := filepath.Join(dir, "internal/modules/user/module.go")
	before, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(modPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(modPath, 0o644) })

	if _, err := generate.Repository(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err == nil {
		t.Fatal("generating over a read-only module.go succeeded")
	}

	created := filepath.Join(dir, "internal/modules/user/infrastructure/order_repository.go")
	if _, err := os.Stat(created); err == nil {
		t.Error("the repository file survived a failed run — the generator is not atomic")
	}
	after, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("module.go changed despite the failure")
	}

	// And having rolled back, the SAME command must now succeed.
	if err := os.Chmod(modPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := generate.Repository(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
		t.Fatalf("the retry after a rolled-back failure was refused: %v", err)
	}
}

// TestThePlanSaysOverwriteWhenItOverwrites — a plan that calls an
// overwrite "create" is how a user loses a hand-wired module.go without
// noticing.
func TestThePlanSaysOverwriteWhenItOverwrites(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "user", Name: "Ship"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	plan, err := generate.Command(generate.Options{
		Dir: dir, Module: "user", Name: "Ship", Force: true, DryRun: true,
	})
	if err != nil {
		t.Fatalf("forced dry run: %v", err)
	}
	if !strings.Contains(plan, "overwrite") {
		t.Errorf("the plan does not say it would overwrite:\n%s", plan)
	}
}

// TestModuleWillNotOverwriteAModuleDeclaration guards the worst --force
// case: regenerating a module replaces module.go with an empty declaration,
// silently discarding every provider, consumer and export it had.
func TestModuleWillNotOverwriteAModuleDeclaration(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "billing"}); err != nil {
		t.Fatalf("Module: %v", err)
	}
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "billing", Name: "Ship"}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	before := read(t, dir, "internal/modules/billing/module.go")

	_, err := generate.Module(generate.Options{Dir: dir, Name: "billing", Force: true})
	if err == nil {
		t.Fatal("--force regenerated a module over its own wiring")
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("the diagnostic does not name the module:\n%v", err)
	}
	if read(t, dir, "internal/modules/billing/module.go") != before {
		t.Error("the module declaration was destroyed")
	}
}
