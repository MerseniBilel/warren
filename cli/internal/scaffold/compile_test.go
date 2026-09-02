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

	// ALL FOUR COMBINATIONS, since 2026-09-02.
	//
	// This loop covered the two --db values and neither --broker one, and it
	// never BOOTED anything — so `warren new x --db postgres` shipped a
	// scaffold that compiled, vetted, passed its own tests, and then refused
	// to start:
	//
	//	✗ registered routes have no adapter serving them
	//	    1 event subscription(s) — add a broker module to warren.New
	//
	// Exactly one of the four combinations was broken, and it was the one the
	// README quickstart and GETTING_STARTED §8 both lead with. A compile-only
	// gate could never have caught it: the broken project builds perfectly.
	for _, combo := range []struct{ db, broker string }{
		{"memory", ""},
		{"memory", "kafka"},
		{"postgres", ""},
		{"postgres", "kafka"},
	} {
		db, brokerFlag := combo.db, combo.broker
		name := db + "+" + brokerFlag
		if brokerFlag == "" {
			name = db + "+memory"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := scaffold.New(scaffold.Options{
				Dir: dir, Name: "myapp", ModulePath: "example.com/myapp",
				Version: scaffold.DefaultVersion,
				DB:      db,
				Broker:  brokerFlag,
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
				t.Fatalf("New --db %s --broker %q: %v", db, brokerFlag, err)
			}

			// The boot assertion, injected into the tree `go test ./...`
			// below runs. It is here rather than in this package because it
			// has to boot the GENERATED graph — platform, both features and
			// the HTTP server — which only exists inside the scaffold.
			if err := os.WriteFile(
				filepath.Join(dir, "internal", "platform", "boot_test.go"),
				[]byte(scaffoldBootTest), 0o644); err != nil {
				t.Fatalf("writing the boot test: %v", err)
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
					t.Fatalf("the generated --db %s --broker %q app failed `%s`:\n%s",
						db, brokerFlag, strings.Join(step, " "), out)
				}
			}
		})
	}
}

// scaffoldBootTest is dropped into every scaffolded tree and run by its own
// `go test ./...`. It is the half that would have caught field test #16's
// blocker, and the half a compile gate cannot be.
//
// It BOOTS the generated module graph — platform, both feature modules, the
// HTTP server — with a DSN and a broker seed that are syntactically valid and
// point at a closed port. That combination is deliberate: an EMPTY DSN fails
// in the postgres constructor at boot step 4, before the route table exists,
// so the assertion below would pass vacuously. An unreachable one parses,
// the pool is built, registration runs, Table.Unserved() runs — and only then
// does OnStart fail to dial. Measured: the fixed scaffold reaches
//
//	lifecycle: hook "warren/persistence/postgres" failed during OnStart:
//	✗ cannot connect to postgres
//
// while the broken one never gets there and reports the unserved-route
// diagnostic instead.
//
// So the assertion is not "boot succeeds" — it cannot be, in a test with no
// database and no broker — it is "boot never fails for THIS reason". A
// scaffold whose own generated subscription has no adapter is a scaffold that
// does not work, and that is a property this test can see without Docker.
const scaffoldBootTest = `package platform_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren"
	whttp "github.com/MerseniBilel/warren/transport/http"

	"example.com/myapp/internal/modules/notification"
	"example.com/myapp/internal/modules/user"
	"example.com/myapp/internal/platform"
)

func TestEveryGeneratedRouteHasAnAdapterServingIt(t *testing.T) {
	// Valid and unreachable, for the reason in scaffoldBootTest's comment.
	t.Setenv("MYAPP_DATABASE_URL",
		"postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("MYAPP_KAFKA_BROKERS", "127.0.0.1:1")

	app := warren.New(
		platform.Module(),
		user.Module(),
		notification.Module(),
		whttp.Server(whttp.Port(0)),
	)
	err := app.Start(context.Background())
	if err == nil {
		// Everything this scaffold needs is in process. Stop it again.
		_ = app.Stop(context.Background())
		return
	}
	if strings.Contains(err.Error(), "no adapter serving them") {
		t.Fatalf("this scaffold registers a subscription nothing serves, so it "+
			"cannot boot as generated:\n%v", err)
	}
	// Any other failure is this test's environment — no database, no broker —
	// and is the expected outcome. The boot reached step 6, which is past the
	// check above.
	t.Logf("boot stopped after the route-table check, which is the pass: %v", err)
}
`
