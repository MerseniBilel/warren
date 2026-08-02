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
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
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
		// tidy first: the replaces above point at untagged local modules, so
		// the generated go.sum has no entries for them or their own
		// dependencies until it is written.
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
