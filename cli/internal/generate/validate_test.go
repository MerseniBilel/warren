package generate_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/generate"
)

// TestRefusesToEscapeTheProject is the security test. The <module>
// positional becomes a directory path, so an unvalidated one writes files
// into — and rewrites the module.go of — a DIFFERENT project.
func TestRefusesToEscapeTheProject(t *testing.T) {
	t.Parallel()

	dir := app(t)
	victim := app(t)
	before := read(t, victim, "internal/modules/user/module.go")

	// The relative path from the target project to the victim's user module.
	escape, err := filepath.Rel(filepath.Join(dir, "internal/modules"),
		filepath.Join(victim, "internal/modules/user"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(escape, "..") {
		t.Fatalf("the test is not testing an escape: %q", escape)
	}

	for _, tc := range []struct {
		name string
		run  func() (string, error)
	}{
		{"repository", func() (string, error) {
			return generate.Repository(generate.Options{Dir: dir, Module: escape, Name: "Pwned"})
		}},
		{"entity", func() (string, error) {
			return generate.Entity(generate.Options{Dir: dir, Module: escape, Name: "Pwned"})
		}},
		{"command", func() (string, error) {
			return generate.Command(generate.Options{Dir: dir, Module: escape, Name: "Pwned"})
		}},
		{"consumer", func() (string, error) {
			return generate.Consumer(generate.Options{Dir: dir, Module: escape, Name: "Pwned"})
		}},
		{"module", func() (string, error) {
			return generate.Module(generate.Options{Dir: dir, Name: escape})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.run(); err == nil {
				t.Fatal("a path-traversing name was accepted")
			}
		})
	}

	if read(t, victim, "internal/modules/user/module.go") != before {
		t.Error("another project's module.go was rewritten")
	}
	if _, err := os.Stat(filepath.Join(victim, "internal/modules/user/infrastructure/pwned_repository.go")); err == nil {
		t.Error("a file was written into another project")
	}
}

// TestRejectsNamesThatAreNotValidGo covers every way a name reaches the
// generated source: as a package name, as an import identifier, and as a
// type name. Producing uncompilable Go is bad; producing Go that breaks
// code the generator did not write is worse.
func TestRejectsNamesThatAreNotValidGo(t *testing.T) {
	t.Parallel()

	dir := app(t)
	mainBefore := read(t, dir, "cmd/myapp/main.go")

	// A module name shadows a predeclared identifier for the whole of
	// main.go: `nil.Module()` and, worse, every `if err != nil` after it.
	for _, name := range []string{"nil", "true", "error", "len", "new", "int", "string", "type", "map", "", "1st", "a-b", "a b", "a.b"} {
		if _, err := generate.Module(generate.Options{Dir: dir, Name: name}); err == nil {
			t.Errorf("module name %q was accepted", name)
		}
	}
	if read(t, dir, "cmd/myapp/main.go") != mainBefore {
		t.Error("a rejected module name still edited main.go")
	}

	// A generated type must be exported, or infrastructure cannot see the
	// domain's own port.
	for _, name := range []string{"lowercase", "", "1st", "a-b", "type", "_"} {
		if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: name}); err == nil {
			t.Errorf("entity name %q was accepted", name)
		}
	}
}

// TestRefusesToRedeclareAnIdentifier catches the collision the path check
// cannot see: `g command Ship` and `g consumer Ship` write DIFFERENT files
// that both declare `type Ship`, so the package stops compiling.
func TestRefusesToRedeclareAnIdentifier(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "user", Name: "Ship"}); err != nil {
		t.Fatalf("command: %v", err)
	}
	mod := read(t, dir, "internal/modules/user/module.go")

	_, err := generate.Consumer(generate.Options{Dir: dir, Module: "user", Name: "Ship"})
	if err == nil {
		t.Fatal("a consumer redeclaring the command's type was accepted")
	}
	for _, want := range []string{"Ship", "ship.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not name %q:\n%v", want, err)
		}
	}
	if read(t, dir, "internal/modules/user/module.go") != mod {
		t.Error("a refused generator still edited module.go")
	}
}

// TestTopicIsQuotedNotConcatenated — the topic reaches three Go string
// literals. Spliced raw, a quote in it closes the literal and whatever
// follows becomes an expression; a backslash makes the template blame
// Warren for the user's input.
func TestTopicIsQuotedNotConcatenated(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Consumer(generate.Options{
		Dir: dir, Module: "user", Name: "Injected", Topic: `a" + "b`,
	}); err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	sub := read(t, dir, "internal/modules/user/on_injected_subscription.go")
	if !strings.Contains(sub, `const topic = "a\" + \"b"`) {
		t.Errorf("the topic was not quoted:\n%s", sub)
	}

	// And a backslash must not produce an unknown-escape-sequence error
	// that names one of Warren's own templates.
	if _, err := generate.Consumer(generate.Options{
		Dir: dir, Module: "user", Name: "Escaped", Topic: `a\qb`,
	}); err != nil {
		t.Fatalf("a topic containing a backslash was rejected: %v", err)
	}
}

// TestEnvPrefixIsQuoted — the prefix is read out of the project's own
// source, so it is only as trustworthy as that file.
func TestEnvPrefixIsQuoted(t *testing.T) {
	t.Parallel()

	dir := app(t)
	platform := filepath.Join(dir, "internal/platform/module.go")
	src, err := os.ReadFile(platform)
	if err != nil {
		t.Fatal(err)
	}
	hostile := strings.Replace(string(src), `WithEnvPrefix("MYAPP")`,
		`WithEnvPrefix("A\", os.Getenv(\"HOME\"), \"B")`, 1)
	if hostile == string(src) {
		t.Skip("the scaffold no longer spells WithEnvPrefix that way")
	}
	if err := os.WriteFile(platform, []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "billing"}); err != nil {
		t.Fatalf("Module: %v", err)
	}
	// Assert on the AST, not on the text: correctly quoted, the hostile
	// prefix legitimately contains "os.Getenv" INSIDE a string literal.
	// What must not happen is it becoming extra arguments.
	got := read(t, dir, "internal/modules/billing/module_test.go")
	f, perr := parser.ParseFile(token.NewFileSet(), "module_test.go", got, 0)
	if perr != nil {
		t.Fatalf("the generated test does not parse: %v\n%s", perr, got)
	}
	var checked bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Setenv" {
			return true
		}
		checked = true
		if len(call.Args) != 2 {
			t.Errorf("t.Setenv got %d arguments, want 2 — the prefix was spliced as an expression:\n%s",
				len(call.Args), got)
		}
		for _, arg := range call.Args {
			if _, isLit := arg.(*ast.BasicLit); !isLit {
				t.Errorf("t.Setenv got a non-literal argument:\n%s", got)
			}
		}
		return false
	})
	if !checked {
		t.Errorf("the generated test does not call t.Setenv:\n%s", got)
	}
}

// TestAmbiguousMainIsAnError — a project with an api and a worker binary
// has two main packages, and silently wiring the first one leaves the
// second missing a module with nothing to indicate it.
func TestAmbiguousMainIsAnError(t *testing.T) {
	t.Parallel()

	dir := app(t)
	worker := filepath.Join(dir, "cmd/worker")
	if err := os.MkdirAll(worker, 0o755); err != nil {
		t.Fatal(err)
	}
	src := read(t, dir, "cmd/myapp/main.go")
	if err := os.WriteFile(filepath.Join(worker, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := generate.Module(generate.Options{Dir: dir, Name: "billing"})
	if err == nil {
		t.Fatal("an ambiguous main package was silently resolved")
	}
	for _, want := range []string{"cmd/myapp/main.go", "cmd/worker/main.go", "--main"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/modules/billing")); err == nil {
		t.Error("a refused run still created the module")
	}

	// Naming one resolves it.
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "billing", Main: "cmd/worker/main.go"}); err != nil {
		t.Fatalf("--main was not honoured: %v", err)
	}
	if !strings.Contains(read(t, dir, "cmd/worker/main.go"), "billing.Module()") {
		t.Error("the named main was not wired")
	}
}

// TestDirThatIsNotADirectorySaysSo — "there is no go.mod here" is true and
// unhelpful when the path is a file.
func TestDirThatIsNotADirectorySaysSo(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := generate.Module(generate.Options{Dir: file, Name: "billing"})
	if err == nil {
		t.Fatal("a --dir pointing at a file was accepted")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("the diagnostic does not say the path is a file:\n%v", err)
	}
}

// TestRepositoryRequiresTheEntity — `warren g repository --help` says "Run
// `warren g entity` first: the port has to exist for this to compile." It
// did not check. The generator wrote a repository against domain.Widget,
// spliced a provider and an import into module.go, and reported success:
//
//	infrastructure/widget_repository.go:16:46: undefined: domain.Widget
//	... 8 errors
//
// And `warren generate --help` claims "there is no half-generated state to
// clean up" — there was: the file, the provider line, and the now-unused
// import all had to be removed by hand. The module-exists check three lines
// away proves the shape was already understood.
func TestRepositoryRequiresTheEntity(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "catalog"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	before := read(t, dir, "internal/modules/catalog/module.go")

	_, err := generate.Repository(generate.Options{Dir: dir, Module: "catalog", Name: "Widget"})
	if err == nil {
		t.Fatal("a repository for an entity that does not exist was generated — it cannot compile")
	}
	for _, want := range []string{"no such entity", "Widget", "warren g entity catalog Widget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic does not mention %q:\n%v", want, err)
		}
	}

	// Nothing was written, and module.go is byte-for-byte what it was.
	if _, serr := os.Stat(filepath.Join(dir, "internal/modules/catalog/infrastructure/widget_repository.go")); serr == nil {
		t.Error("the repository file was written despite the refusal")
	}
	if after := read(t, dir, "internal/modules/catalog/module.go"); after != before {
		t.Errorf("module.go was mutated by a refused generator:\n%s", after)
	}
}

// TestRepositoryAfterTheEntityStillWorks — the check must not refuse the
// documented order.
func TestRepositoryAfterTheEntityStillWorks(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "catalog"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "catalog", Name: "Widget"}); err != nil {
		t.Fatalf("g entity: %v", err)
	}

	if _, err := generate.Repository(generate.Options{Dir: dir, Module: "catalog", Name: "Widget"}); err != nil {
		t.Fatalf("a repository for an entity that DOES exist was refused: %v", err)
	}
}
