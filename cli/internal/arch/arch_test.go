package arch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/arch"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

// fixture writes a throwaway Go module and returns its directory.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/fix\n\ngo 1.26.3\n"
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCleanProjectPasses(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/domain/user.go": "package domain\n\ntype User struct{}\n",
		"internal/modules/user/application/register.go": `package application

import "example.com/fix/internal/modules/user/domain"

func New() *domain.User { return nil }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("clean project reported %d violations:\n%s", len(report.Violations), report)
	}
	if report.Packages == 0 {
		t.Error("no packages were analysed — the check would pass vacuously")
	}
}

func TestDomainImportingInfrastructureIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/domain/user.go": `package domain

import "example.com/fix/internal/modules/user/infrastructure"

type User struct{ R infrastructure.Repo }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	v := report.Violations[0]
	if v.Layer != "domain" || v.ImportedLayer != "infrastructure" {
		t.Errorf("violation = %+v", v)
	}
	// The diagnostic must name the file, both layers, and the fix.
	out := report.String()
	for _, want := range []string{"user.go", "domain", "infrastructure", "Declare a port"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic is missing %q:\n%s", want, out)
		}
	}
}

func TestApplicationImportingInfrastructureIsAViolation(t *testing.T) {
	t.Parallel()

	dir := fixture(t, map[string]string{
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/application/svc.go": `package application

import "example.com/fix/internal/modules/user/infrastructure"

type S struct{ R infrastructure.Repo }
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if len(report.Violations) != 1 {
		t.Errorf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
}

func TestModuleGoSeesAllFourLayers(t *testing.T) {
	t.Parallel()

	// module.go is the one file permitted to wire the layers together.
	dir := fixture(t, map[string]string{
		"internal/modules/user/domain/user.go":         "package domain\n\ntype User struct{}\n",
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/module.go": `package user

import (
	"example.com/fix/internal/modules/user/domain"
	"example.com/fix/internal/modules/user/infrastructure"
)

var _ = domain.User{}
var _ = infrastructure.Repo{}
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if len(report.Violations) != 0 {
		t.Errorf("module.go was flagged:\n%s", report)
	}
}

func TestCrossModuleImportIsAViolation(t *testing.T) {
	t.Parallel()

	// Claim 2 stated mechanically: one feature reaching into another's
	// internals is what makes extraction a rewrite instead of a rewiring.
	dir := fixture(t, map[string]string{
		"internal/modules/billing/domain/invoice.go": "package domain\n\ntype Invoice struct{}\n",
		"internal/modules/user/application/svc.go": `package application

import "example.com/fix/internal/modules/billing/domain"

type S struct{ I domain.Invoice }
`,
	})
	report, _ := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if len(report.Violations) != 1 {
		t.Fatalf("violations = %d, want 1:\n%s", len(report.Violations), report)
	}
	if !strings.Contains(report.String(), "another feature module") {
		t.Errorf("diagnostic does not explain the cross-module rule:\n%s", report)
	}
}

func TestUnlayeredProjectIsExemptNotRefused(t *testing.T) {
	t.Parallel()

	// A plain Go project must run clean rather than being refused: a linter
	// that only works on projects it generated is a linter nobody adopts.
	dir := fixture(t, map[string]string{
		"pkg/a/a.go": "package a\n\ntype A struct{}\n",
		"pkg/b/b.go": "package b\n\nimport \"example.com/fix/pkg/a\"\n\ntype B struct{ A a.A }\n",
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check on a non-Warren project: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("a project with no layers reported violations:\n%s", report)
	}
}

func TestUncompilableProjectStillChecks(t *testing.T) {
	t.Parallel()

	// The fix for a layer violation usually breaks the build first, which is
	// exactly when the check is most needed. Imports are syntactic.
	dir := fixture(t, map[string]string{
		"internal/modules/user/infrastructure/repo.go": "package infrastructure\n\ntype Repo struct{}\n",
		"internal/modules/user/domain/user.go": `package domain

import "example.com/fix/internal/modules/user/infrastructure"

type User struct{ R infrastructure.Repo }

func broken() { this is not go }
`,
	})
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) == 0 {
		t.Error("a project that does not compile reported no violations — this is when the check matters most")
	}
}

// TestWarrenItselfIsClean is the dogfood test: the same binary, the same
// analyzer, run over the framework's own repository. "The same warren lint
// arch that ships to users" is a claim about the code path, and this is
// what keeps it honest.
func TestWarrenItselfIsClean(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "warren.md")); statErr != nil {
		t.Skip("not running inside the Warren repository")
	}
	report, err := arch.Check(root, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("Warren violates its own rules:\n%s", report)
	}
	if report.Packages < 10 {
		t.Errorf("only %d packages analysed — the dogfood test would pass vacuously", report.Packages)
	}
}

// TestGeneratedAppIsClean closes the loop: what the CLI scaffolds must pass
// the linter the same CLI ships.
func TestGeneratedAppIsClean(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Errorf("the scaffold the CLI generates breaks the rules the CLI enforces:\n%s", report)
	}
}

// TestRelativeRootIsNotSkipped pins a vacuous pass this linter shipped with
// for one commit: a root named ".." or "." starts with a dot, and the
// hidden-directory skip swallowed the whole tree — reporting "no violations
// in 0 packages", which reads like success.
func TestRelativeRootIsNotSkipped(t *testing.T) {
	dir := fixture(t, map[string]string{
		"internal/modules/user/domain/user.go": "package domain\n\ntype User struct{}\n",
	})
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	report, err := arch.Check("..", arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Packages == 0 {
		t.Error(`Check("..") analysed 0 packages — a vacuous pass that reads like success`)
	}
}
