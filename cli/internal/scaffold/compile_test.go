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
// first user discovers instead of CI.
//
// It compiles against the CHECKOUT, deliberately, which is why it passes
// --framework: a change to the framework has to be exercised by a real
// service before it is tagged, and a test pinned to the last release would
// still be green the day after that release broke the templates. The other
// half — that a DEFAULT scaffold's requires resolve from the proxy with no
// replace at all — is a network check and lives in CI's install job, since
// AGENT.md's unit-test rule is no Docker and no network.
//
// Nothing is added to the repository either way (invariant 8: no COMMITTED
// replace).
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
				Dir: dir, Name: "myapp", ModulePath: "example.com/myapp",
				Version: scaffold.DefaultVersion,
				DB:      db,
				// The SAME flag a Warren contributor passes, not a
				// hand-patched go.mod: the scaffold produced a tree that did
				// not build for anyone but this test, precisely because this
				// test patched around it.
				//
				// The replaces go in the app's go.mod rather than a go.work
				// because `go work sync` would write the workspace's resolved
				// versions back into the FRAMEWORK's go.mod, which is how an
				// indirect dependency once contaminated the core module.
				FrameworkPath: framework,
			}); err != nil {
				t.Fatalf("New --db %s: %v", db, err)
			}

			for _, step := range [][]string{
				// tidy first: the replaces above redirect the requires at a
				// local checkout, so the generated go.sum has no entries for
				// those modules' own dependencies until it is written.
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
