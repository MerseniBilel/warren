package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/command"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := command.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestVersion(t *testing.T) {
	t.Parallel()

	out, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(out, "warren v") || !strings.Contains(out, "framework v") {
		t.Errorf("version prints %q — it must name both, since a scaffold pins the framework", out)
	}
}

func TestNewScaffolds(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "myapp")
	out, err := run(t, "new", "myapp", "--module", "example.com/myapp", "--dir", dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr != nil {
		t.Fatalf("no go.mod was written: %v", statErr)
	}
	// The next steps must be printed: a scaffold you have to guess how to
	// run is a scaffold nobody runs.
	for _, want := range []string{"go run ./cmd/myapp", "go test ./..."} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not tell the user how to run it (%q):\n%s", want, out)
		}
	}
	// And they must be the steps that WORK. The printed line used to set
	// MYAPP_NAME, a value the scaffolder itself chose; the generated config
	// defaults it now, so asking for it would be asking the user to supply
	// what the tool already knew.
	if strings.Contains(out, "MYAPP_NAME") {
		t.Errorf("the run step still demands a variable the config defaults:\n%s", out)
	}
}

// TestNewOnPostgresPrintsTheStepsThatActuallyRunIt covers the other half of
// the same defect: on --db postgres the printed steps stopped at `go run`,
// and a user who followed them verbatim hit a boot failure on the DSN and
// then, having supplied it, an empty schema. The DSN and the migration are
// not optional extras — they are what stands between a fresh scaffold and a
// service that answers.
func TestNewOnPostgresPrintsTheStepsThatActuallyRunIt(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "shop")
	out, err := run(t, "new", "shop", "--module", "example.com/shop", "--db", "postgres", "--dir", dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for _, want := range []string{"SHOP_DATABASE_URL", "go run ./cmd/migrate", "go run ./cmd/shop"} {
		if !strings.Contains(out, want) {
			t.Errorf("output omits a step the app cannot start without (%q):\n%s", want, out)
		}
	}
	// Order matters: migrating after the app has started is the deploy
	// mistake cmd/migrate's own comment exists to prevent.
	if strings.Index(out, "go run ./cmd/migrate") > strings.Index(out, "go run ./cmd/shop") {
		t.Errorf("the migration step is printed after the run step:\n%s", out)
	}
}

func TestNewRequiresModulePath(t *testing.T) {
	t.Parallel()

	_, err := run(t, "new", "myapp", "--dir", t.TempDir()+"/x")
	if err == nil {
		t.Fatal("new succeeded without --module")
	}
	if !strings.Contains(err.Error(), "--module") {
		t.Errorf("error does not name the missing flag:\n%v", err)
	}
}

func TestNewRefusesAnUnreleasedTransport(t *testing.T) {
	t.Parallel()

	// grpc, not http: transport/http shipped and is what a scaffold wires,
	// so asking for it names what you already get.
	_, err := run(t, "new", "myapp", "--module", "example.com/x", "--dir", t.TempDir()+"/y", "--transport", "grpc")
	if err == nil {
		t.Fatal("new accepted an adapter that does not exist")
	}
	if !strings.Contains(err.Error(), "does not exist yet") {
		t.Errorf("diagnostic:\n%v", err)
	}
}

func TestLintArchExitCodes(t *testing.T) {
	t.Parallel()

	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "go.mod"), []byte("module example.com/c\n\ngo 1.26.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "lint", "arch", clean)
	if err != nil {
		t.Fatalf("clean project: %v", err)
	}
	if command.ExitCode(err) != 0 || !strings.Contains(out, "No violations") {
		t.Errorf("clean run: exit=%d out=%q", command.ExitCode(err), out)
	}

	// Violations exit 1; a project that cannot be analysed exits 2. A CI
	// that cannot tell them apart treats "couldn't run" as "clean".
	_, err = run(t, "lint", "arch", t.TempDir())
	if command.ExitCode(err) != 2 {
		t.Errorf("un-analysable project: exit=%d, want 2", command.ExitCode(err))
	}
}
