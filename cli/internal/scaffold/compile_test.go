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
	// Replace directives in the TEMP app's go.mod, not a go.work.
	//
	// A workspace cannot resolve a require on an untagged sibling module —
	// the generated app requires warren/transport/http v0.1.0, which does not
	// exist yet — so `use` is not enough and `go work sync` would write the
	// workspace's resolved versions back into the FRAMEWORK's go.mod, which
	// is how an indirect dependency once contaminated the core module.
	// Replaces here are scoped to this temp directory and nothing is added to
	// the repository (invariant 8: no COMMITTED replace).
	mod := filepath.Join(dir, "go.mod")
	src, rerr := os.ReadFile(mod)
	if rerr != nil {
		t.Fatal(rerr)
	}
	src = append(src, []byte(
		"\nreplace github.com/MerseniBilel/warren => "+framework+
			"\n\nreplace github.com/MerseniBilel/warren/transport/http => "+framework+"/transport/http\n")...)
	if err := os.WriteFile(mod, src, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, step := range [][]string{
		// tidy first: the replaces above point at untagged local modules, so
		// the generated go.sum has no entries for them or their own
		// dependencies until it is written.
		{"go", "mod", "tidy"},
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
