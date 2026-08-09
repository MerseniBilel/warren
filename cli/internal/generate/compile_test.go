package generate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/arch"
	"github.com/MerseniBilel/warren/cli/internal/generate"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

// TestGeneratedCodeCompilesAndPasses is the anti-rot gate for the
// generators, and the counterpart to the scaffold's. Every generator runs
// against a real scaffold, in the order a user would run them, and then the
// whole app is BUILT, VETTED and TESTED against the checked-out framework.
//
// A unit test that asserts on substrings cannot tell you the wiring
// type-checks. This can: if `warren g repository` provides a constructor
// whose port nothing exports, or an entity template drifts from
// domain.AggregateRoot, the build fails here rather than in a user's
// terminal.
func TestGeneratedCodeCompilesAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated app; skipped under -short")
	}
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp",
		Version: scaffold.DefaultVersion,
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	// The documented order: a module, then the aggregate, then the
	// repository that stores it, then the use case that drives it.
	steps := []struct {
		what string
		run  func() (string, error)
	}{
		{"g module billing", func() (string, error) {
			return generate.Module(generate.Options{Dir: dir, Name: "billing"})
		}},
		{"g entity billing Invoice", func() (string, error) {
			return generate.Entity(generate.Options{Dir: dir, Module: "billing", Name: "Invoice"})
		}},
		{"g repository billing Invoice", func() (string, error) {
			return generate.Repository(generate.Options{Dir: dir, Module: "billing", Name: "Invoice"})
		}},
		{"g command billing VoidInvoice", func() (string, error) {
			return generate.Command(generate.Options{Dir: dir, Module: "billing", Name: "VoidInvoice"})
		}},
		{"g consumer billing PaymentReceived", func() (string, error) {
			return generate.Consumer(generate.Options{Dir: dir, Module: "billing", Name: "PaymentReceived"})
		}},
		// And into a module the scaffold wrote, not one we just generated:
		// the splicer has to cope with a provider list that already exists,
		// and with a module that already declares consumers.
		{"g command user SuspendUser", func() (string, error) {
			return generate.Command(generate.Options{Dir: dir, Module: "user", Name: "SuspendUser"})
		}},
		{"g consumer notification InvoiceVoided", func() (string, error) {
			return generate.Consumer(generate.Options{Dir: dir, Module: "notification", Name: "InvoiceVoided"})
		}},
		// A route whose wildcards are NOT named "id". Those become fields
		// called TenantID and LineID, and the generated TEST used to say
		// `application.VoidLine{ID: …}` regardless — so this exact command
		// produced a package that did not compile:
		//
		//   vet: void_line_test.go:22:66: unknown field ID in struct
		//        literal of type application.VoidLine
		//
		// Every other case here uses the derived route, which has no
		// wildcard at all, so nothing noticed.
		{"g command user VoidLine --route /tenants/{tenantId}/lines/{lineId}", func() (string, error) {
			return generate.Command(generate.Options{
				Dir: dir, Module: "user", Name: "VoidLine",
				Route: "/tenants/{tenantId}/lines/{lineId}",
			})
		}},
		// And a --method, since the verb reaches controller.go.
		{"g command user ArchiveUser --route /users/{id}/archive --method put", func() (string, error) {
			return generate.Command(generate.Options{
				Dir: dir, Module: "user", Name: "ArchiveUser",
				Route: "/users/{id}/archive", Method: "put",
			})
		}},
		// The Postgres repository, which is the whole reason this test
		// exists: its three rules — RequireTx, db(ctx), persistence.Track —
		// are enforced by no compiler, so the only thing standing between a
		// template drift and a user losing events silently is that this
		// compiles here.
		// Into its own feature, whose generated module_test is then removed:
		// a Postgres repository needs a live pool, so booting it is an
		// integration concern. What this test owns is that the TEMPLATE
		// compiles against the current framework.
		{"g module ledger", func() (string, error) {
			return generate.Module(generate.Options{Dir: dir, Name: "ledger"})
		}},
		{"g entity ledger Entry", func() (string, error) {
			return generate.Entity(generate.Options{Dir: dir, Module: "ledger", Name: "Entry"})
		}},
		{"g repository ledger Entry --driver postgres", func() (string, error) {
			return generate.Repository(generate.Options{
				Dir: dir, Module: "ledger", Name: "Entry", Driver: "postgres",
			})
		}},
	}
	for _, step := range steps {
		if _, err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
	}

	framework, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// Replace directives in the TEMP app's go.mod, not a go.work.
	//
	// The generated app's requires resolve on their own now — every framework
	// module is tagged — so this is not about making the tree build. It is
	// about building it against the CHECKOUT: a generator that drifts from a
	// signature changed in this very commit would still compile happily
	// against the last release, and the point of this test is that it does
	// not. `go work sync` would write the workspace's resolved versions back
	// into the FRAMEWORK's go.mod, which is how an indirect dependency once
	// contaminated the core module, so replaces it is.
	//
	// Replaces here are scoped to this temp directory and nothing is added to
	// the repository (invariant 8: no COMMITTED replace).
	mod := filepath.Join(dir, "go.mod")
	src, rerr := os.ReadFile(mod)
	if rerr != nil {
		t.Fatal(rerr)
	}
	src = append(src, []byte(
		"\nrequire github.com/MerseniBilel/warren/persistence/postgres "+scaffold.DefaultVersion+"\n"+
			"\nreplace github.com/MerseniBilel/warren => "+framework+
			"\n\nreplace github.com/MerseniBilel/warren/transport/http => "+framework+"/transport/http"+
			"\n\nreplace github.com/MerseniBilel/warren/persistence/postgres => "+framework+"/persistence/postgres\n")...)
	if err := os.WriteFile(mod, src, 0o644); err != nil {
		t.Fatal(err)
	}

	// The ledger module's test boots the graph, and a Postgres repository
	// cannot resolve postgres.DB without a pool. Removing it keeps this test
	// about what it is about — that the generated SQL and the three rules
	// still compile — and leaves booting to persistence/postgres's own
	// integration suite.
	if err := os.Remove(filepath.Join(dir, "internal/modules/ledger/module_test.go")); err != nil {
		t.Fatalf("removing the ledger module test: %v", err)
	}

	// The generators must not be able to write code that breaks the rules
	// the framework is built on — a generated layer violation would be the
	// worst kind, because the user did not write it.
	report, err := arch.Check(dir, arch.Options{Rules: arch.Layers})
	if err != nil {
		t.Fatalf("lint arch: %v", err)
	}
	if len(report.Violations) > 0 {
		t.Errorf("the generated app breaks the layer rules:\n%s", report.String())
	}

	for _, cmdline := range [][]string{
		// tidy first: the replaces above redirect the requires at a local
		// checkout, so the generated go.sum has no entries for those modules'
		// own dependencies until it is written.
		{"go", "mod", "tidy"},
		{"go", "build", "./..."},
		{"go", "vet", "./..."},
		{"go", "test", "./..."},
	} {
		cmd := exec.Command(cmdline[0], cmdline[1:]...)
		cmd.Dir = dir
		out, cerr := cmd.CombinedOutput()
		if cerr != nil {
			t.Fatalf("the generated app failed `%s`:\n%s", strings.Join(cmdline, " "), out)
		}
	}
}
