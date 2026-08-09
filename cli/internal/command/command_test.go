package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/command"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
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

// TestVersion pins the two halves of the line to different rules, because
// they answer different questions.
//
// The CLI's half is read from the build, and a `go test` binary carries no
// released version — so "(devel)" is the correct answer here, and printing a
// version number would be the lie this whole change removes. What it looks
// like for a real build is TestVersionMatchesTheBuild's business.
//
// The framework's half must be a release whatever the build is: it is the
// version a scaffold's go.mod pins, and a pseudo-version or "(devel)" in a
// require line is a project that does not resolve.
func TestVersion(t *testing.T) {
	t.Parallel()

	out, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(out, "warren (devel)") {
		t.Errorf("version prints %q — a test binary is not a release and must not claim one", out)
	}
	if !strings.Contains(out, "(framework "+scaffold.DefaultVersion+")") {
		t.Errorf("version prints %q — the framework half must name the release a scaffold pins (%s)",
			out, scaffold.DefaultVersion)
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

// TestNewWritesAResolvableGoModWithoutAReplace is the shape of the happy
// path after publication.
//
// Until all seven modules were tagged v0.2.0, `warren new` printed "Warren is
// not published yet, so `go mod tidy` cannot resolve it" and six replace
// directives to paste — and it was right: v0.1.0 tagged the core module
// alone, so nothing a scaffold required existed. A user's first command in a
// new project now has to be `go mod tidy` and nothing else, which means no
// replace, no notice telling them to add one, and requires pinned to a
// version the proxy serves.
func TestNewWritesAResolvableGoModWithoutAReplace(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "myapp")
	out, err := run(t, "new", "myapp", "--module", "example.com/myapp", "--dir", dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "replace ") {
		t.Errorf("the default scaffold pins the framework to a local path:\n%s", mod)
	}
	want := "github.com/MerseniBilel/warren " + scaffold.DefaultVersion
	if !strings.Contains(string(mod), want) {
		t.Errorf("go.mod does not require %s:\n%s", want, mod)
	}
	// And the printed steps must not send the user looking for a checkout.
	for _, gone := range []string{"not published", "--framework", "replace "} {
		if strings.Contains(out, gone) {
			t.Errorf("`warren new` still tells the user %q:\n%s", gone, out)
		}
	}
}

// TestNewFrameworkFlagStillWritesReplaces — the flag is the exception now,
// not the default, and an exception that quietly stopped working would strand
// the people developing Warren itself.
func TestNewFrameworkFlagStillWritesReplaces(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "myapp")
	out, err := run(t, "new", "myapp", "--module", "example.com/myapp", "--dir", dir,
		"--framework", "/path/to/warren")
	if err != nil {
		t.Fatalf("new --framework: %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"replace github.com/MerseniBilel/warren => /path/to/warren",
		"replace github.com/MerseniBilel/warren/transport/http => /path/to/warren/transport/http",
		"replace github.com/MerseniBilel/warren/validate/playground => /path/to/warren/validate/playground",
	} {
		if !strings.Contains(string(mod), want) {
			t.Errorf("go.mod is missing %q:\n%s", want, mod)
		}
	}
	// A machine-specific go.mod that nobody mentioned is the next person's
	// bug report, so it is said once, here.
	if !strings.Contains(out, "/path/to/warren") {
		t.Errorf("nothing told the user their go.mod now points at a local checkout:\n%s", out)
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
