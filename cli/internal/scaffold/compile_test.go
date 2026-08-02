package scaffold_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

// TestScaffoldCompilesAndPasses is the anti-rot mechanism, and the most
// valuable test in this module: it generates the app and then BUILDS, VETS
// and TESTS it against the checked-out framework.
//
// Templates rot silently otherwise — a signature changes in the framework
// and the scaffold keeps generating code that no longer compiles, which the
// first user discovers instead of CI. A go.work is written into the temp
// dir so the generated go.mod resolves against the local checkout; nothing
// is added to the repository (invariant 8: no committed replace).
func TestScaffoldCompilesAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the scaffold; skipped under -short")
	}
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}

	framework, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	work := "go 1.26.3\n\nuse (\n\t.\n\t" + framework + "\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "go.work"), []byte(work), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, step := range [][]string{
		{"go", "build", "./..."},
		{"go", "vet", "./..."},
		{"go", "test", "./..."},
	} {
		cmd := exec.Command(step[0], step[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the generated app failed `%s`:\n%s", strings.Join(step, " "), out)
		}
	}
}
