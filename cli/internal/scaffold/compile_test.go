package scaffold_test

import (
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
//
// BOTH --db values are compiled. The postgres templates are a second, larger
// half of the scaffold that render() only ever ran gofmt over — and gofmt
// catches a syntax error, not a signature that moved. The repository and its
// contract test are the files most exposed to a framework change, and under
// --db postgres neither of them was ever built by CI.
func TestScaffoldCompilesAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the scaffold; skipped under -short")
	}
	t.Parallel()

	framework, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}

	for _, db := range []string{"memory", "postgres"} {
		t.Run(db, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := scaffold.New(scaffold.Options{
				Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
				DB: db,
				// The SAME flag a user passes, not a hand-patched go.mod: the
				// scaffold produced a tree that did not build for anyone but
				// this test, precisely because this test patched around it.
				//
				// Replaces here are scoped to a temp directory and nothing is
				// added to the repository (invariant 8: no COMMITTED replace).
				// They go in the app's go.mod rather than a go.work because a
				// workspace cannot resolve a require on an untagged sibling,
				// and `go work sync` would write resolved versions back into
				// the FRAMEWORK's go.mod.
				FrameworkPath: framework,
			}); err != nil {
				t.Fatalf("New --db %s: %v", db, err)
			}

			for _, step := range [][]string{
				// tidy first: the replaces above point at untagged local
				// modules, so the generated go.sum has no entries for them or
				// their own dependencies until it is written.
				{"go", "mod", "tidy"},
				{"go", "build", "./..."},
				{"go", "vet", "./..."},
				// The postgres contract test SKIPS here — there is no database
				// — and that is the path being certified: a scaffold whose
				// contract test cannot even reach its own skip is broken for
				// every user who has no DSN, which is all of them on day one.
				{"go", "test", "./..."},
			} {
				cmd := exec.Command(step[0], step[1:]...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("the generated --db %s app failed `%s`:\n%s", db, strings.Join(step, " "), out)
				}
			}
		})
	}
}
