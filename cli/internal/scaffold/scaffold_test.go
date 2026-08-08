package scaffold_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

func TestNewWritesTheWholeTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	err := scaffold.New(scaffold.Options{
		Dir:        dir,
		Name:       "myapp",
		ModulePath: "example.com/myapp",
		Version:    "v0.1.0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Every file the layout promises, and nothing missing: a scaffold with a
	// hole in it fails at `go build`, which is a worse first impression than
	// any error message.
	want := []string{
		"go.mod",
		".gitignore",
		"Makefile",
		"README.md",
		"cmd/myapp/main.go",
		"internal/config/config.go",
		"internal/config/config_test.go",
		"internal/platform/module.go",
		"internal/modules/user/controller.go",
		"internal/modules/user/module.go",
		"internal/modules/user/module_test.go",
		"internal/modules/user/domain/user.go",
		"internal/modules/user/application/register_user.go",
		"internal/modules/user/application/register_user_test.go",
		"internal/modules/user/infrastructure/user_repository.go",
		"internal/modules/notification/module.go",
		"internal/modules/notification/application/on_user_registered.go",
	}
	var got []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(got)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %s", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("wrote %d files, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

func TestGeneratedGoIsFormatted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	// Unformatted generated code teaches the wrong thing on line one.
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		formatted, err := scaffold.Format(src)
		if err != nil {
			t.Errorf("%s does not parse: %v", path, err)
			return nil
		}
		if string(formatted) != string(src) {
			t.Errorf("%s is not gofmt-clean", path)
		}
		return nil
	})
}

func TestSubstitutionsReachEveryFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "orders", ModulePath: "github.com/acme/orders", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}

	mod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	if !strings.Contains(string(mod), "module github.com/acme/orders") {
		t.Errorf("go.mod: %s", mod)
	}
	if !strings.Contains(string(mod), "github.com/MerseniBilel/warren v0.1.0") {
		t.Errorf("go.mod does not pin the framework version:\n%s", mod)
	}
	main, _ := os.ReadFile(filepath.Join(dir, "cmd/orders/main.go"))
	if !strings.Contains(string(main), `"github.com/acme/orders/internal/platform"`) {
		t.Errorf("main.go imports were not rewritten:\n%s", main)
	}
	// The env prefix lives with the config wiring, which platform owns.
	plat, _ := os.ReadFile(filepath.Join(dir, "internal/platform/module.go"))
	if !strings.Contains(string(plat), `WithEnvPrefix("ORDERS")`) {
		t.Errorf("platform does not use the app's env prefix:\n%s", plat)
	}
}

func TestRefusesANonEmptyTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	})
	if err == nil {
		t.Fatal("scaffolding over an existing file succeeded — it would clobber someone's work")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("the diagnostic does not name the conflicting file:\n%v", err)
	}
	// Nothing else was written.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("a refused scaffold left %d entries behind — it must be atomic", len(entries))
	}
}

func TestUnreleasedAdapterIsRefusedWithTheAlternative(t *testing.T) {
	t.Parallel()

	err := scaffold.New(scaffold.Options{
		Dir: t.TempDir(), Name: "myapp", ModulePath: "example.com/myapp",
		Version: "v0.1.0", Transport: "grpc",
	})
	if err == nil {
		t.Fatal("--transport grpc scaffolded against an adapter that does not exist")
	}
	for _, want := range []string{"transport/grpc", "does not exist yet", "serves HTTP"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic is missing %q — it must say what is released and what to do:\n%v", want, err)
		}
	}
}

// http is the transport the scaffold wires, so asking for it is not a
// refusal — the flag names what you already get.
func TestReleasedTransportIsAccepted(t *testing.T) {
	t.Parallel()

	if err := scaffold.New(scaffold.Options{
		Dir: t.TempDir(), Name: "myapp", ModulePath: "example.com/myapp",
		Version: "v0.1.0", Transport: "http",
	}); err != nil {
		t.Errorf("--transport http was refused: %v", err)
	}
}

func TestModulePathIsRequired(t *testing.T) {
	t.Parallel()

	err := scaffold.New(scaffold.Options{Dir: t.TempDir(), Name: "myapp", Version: "v0.1.0"})
	if err == nil || !strings.Contains(err.Error(), "--module") {
		t.Errorf("err = %v, want the missing flag named", err)
	}
}

// TestInvalidModulePathIsRefused — `warren new q2 --module 'not a module
// path!!'` exited 0 and wrote a tree whose every go command fails with
// "go.mod:1: usage: module module/path". --db and --transport are both
// validated; the one flag that is REQUIRED was not.
func TestInvalidModulePathIsRefused(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"not a module path!!",
		"has space/pkg",
		"trailing/",
		"/leading",
		"double//slash",
		"github.com/you/app\n",
		"tab\there",
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			err := scaffold.New(scaffold.Options{
				Dir: t.TempDir(), Name: "app", ModulePath: bad, Version: "v0.1.0",
				DB: "memory", Broker: "memory",
			})
			if err == nil {
				t.Fatalf("--module %q was accepted; every go command in that tree then fails", bad)
			}
			if !strings.Contains(err.Error(), "not a valid Go module path") {
				t.Errorf("diagnostic:\n%v", err)
			}
		})
	}
}

// TestOrdinaryModulePathsAreAccepted guards the check against being so
// strict it refuses what people actually type.
func TestOrdinaryModulePathsAreAccepted(t *testing.T) {
	t.Parallel()

	for _, good := range []string{
		"github.com/you/app",
		"example.com/inventory",
		"gitlab.com/org/sub-group/app.v2",
		"my-internal-thing",
		"github.com/you/app/v2",
		"git.company.internal/team/svc_name",
	} {
		t.Run(good, func(t *testing.T) {
			t.Parallel()
			if err := scaffold.New(scaffold.Options{
				Dir: t.TempDir(), Name: "app", ModulePath: good, Version: "v0.1.0",
				DB: "memory", Broker: "memory",
			}); err != nil {
				t.Errorf("--module %q was refused: %v", good, err)
			}
		})
	}
}

// TestFrameworkPathWritesReplaceDirectives — `warren new` produced a tree
// that did not build: "missing go.sum entry for module providing package
// github.com/MerseniBilel/warren/app", fifteen times over, because the
// framework is not published yet and go could not resolve the require.
// `warren new --help` claimed "It compiles and passes `go test` as
// generated". This is the flag that makes that true, and the CLI's own
// compile test now uses it rather than hand-patching go.mod — so the path a
// user takes is the path CI exercises.
func TestFrameworkPathWritesReplaceDirectives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0",
		FrameworkPath: "/somewhere/warren",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"replace github.com/MerseniBilel/warren => /somewhere/warren",
		"replace github.com/MerseniBilel/warren/transport/http => /somewhere/warren/transport/http",
	} {
		if !strings.Contains(string(mod), want) {
			t.Errorf("go.mod is missing %q:\n%s", want, mod)
		}
	}
}

// TestNoFrameworkPathWritesNoReplace — invariant 8 is about this repository,
// but a replace nobody asked for in a user's go.mod is its own bug: it
// would point at a path that does not exist on their machine.
func TestNoFrameworkPathWritesNoReplace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "replace") {
		t.Errorf("go.mod carries a replace nobody asked for:\n%s", mod)
	}
}

// TestPostgresScaffoldIsWired — field test #4, section C4. `warren new` only
// accepted --db memory, so reaching Postgres meant manual surgery on the
// platform module, and `warren g repository --driver postgres` printed
// instructions that, followed verbatim, produced a boot failure:
//
//	✗ ambiguous binding
//	    persistence.UnitOfWork has 2 providers visible from scope "shipment"
//
// because the memory unit of work the scaffold had already wired was still
// there. The instructions never said "and now delete newUnitOfWork,
// unitOfWork, appUnitOfWork, newOutboxStore and three Exports".
func TestPostgresScaffoldIsWired(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0", DB: "postgres",
	}); err != nil {
		t.Fatalf("New --db postgres: %v", err)
	}

	platform := read(t, dir, "internal/platform/module.go")
	for _, want := range []string{
		"postgres.Module(",
		"postgres.WithOutbox()",
		"postgres.WithInbox()",
	} {
		if !strings.Contains(platform, want) {
			t.Errorf("platform does not wire %q:\n%s", want, platform)
		}
	}
	// The memory drivers must be GONE, not merely joined — two providers of
	// persistence.UnitOfWork is the ambiguous-binding boot failure above.
	// Comments are stripped first: the generated file NAMES them, in a note
	// explaining why they are absent, and a substring check would read that
	// as wiring.
	code := stripComments(platform)
	for _, gone := range []string{
		"persistence.NewMemoryUnitOfWork",
		"outbox.NewMemoryStore",
		"inbox.NewMemoryStore",
	} {
		if strings.Contains(code, gone) {
			t.Errorf("platform still wires the memory driver %q alongside postgres:\n%s", gone, platform)
		}
	}

	// The deploy step, and the schema it applies.
	for _, f := range []string{"cmd/migrate/main.go", "db/migrations/00001_users.sql"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("--db postgres did not write %s: %v", f, err)
		}
	}

	mod := read(t, dir, "go.mod")
	if !strings.Contains(mod, "github.com/MerseniBilel/warren/persistence/postgres") {
		t.Errorf("go.mod does not require the postgres adapter:\n%s", mod)
	}

	// The repository must be real SQL, not the in-memory one.
	repo := read(t, dir, "internal/modules/user/infrastructure/user_repository.go")
	if !strings.Contains(repo, "postgres.DB") {
		t.Errorf("the user repository is not the postgres one:\n%s", repo)
	}
	if strings.Contains(repo, "MemoryRepository") {
		t.Errorf("the user repository is still in-memory under --db postgres:\n%s", repo)
	}

	// And the DSN has somewhere to come from.
	cfg := read(t, dir, "internal/config/config.go")
	if !strings.Contains(cfg, "DatabaseURL") {
		t.Errorf("config has no database URL:\n%s", cfg)
	}
}

// TestMemoryScaffoldIsUnchanged — the default must not grow a postgres
// dependency, a migrate binary, or a migrations directory.
func TestMemoryScaffoldIsUnchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.Contains(read(t, dir, "go.mod"), "persistence/postgres") {
		t.Error("the memory scaffold requires the postgres adapter")
	}
	for _, f := range []string{"cmd/migrate/main.go", "db/migrations/00001_users.sql"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("the memory scaffold wrote %s", f)
		}
	}
	if !strings.Contains(read(t, dir, "internal/platform/module.go"), "persistence.NewMemoryUnitOfWork") {
		t.Error("the memory scaffold lost its unit of work")
	}
}

// stripComments drops // lines so an assertion about WIRING is not answered
// by prose that happens to name the thing.
func stripComments(src string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// TestKafkaScaffoldIsWired covers the shape --broker kafka has to produce,
// which differs from --db postgres in one structural way: kafka.Broker is a
// MODULE, so platform declares and IMPORTS it rather than providing it, and
// platform cannot export the ports on its behalf. A module may export only
// what its own providers return.
func TestKafkaScaffoldIsWired(t *testing.T) {
	t.Parallel()

	for _, db := range []string{"memory", "postgres"} {
		t.Run(db, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := scaffold.New(scaffold.Options{
				Dir: dir, Name: "app", ModulePath: "example.com/app",
				Version: "v0.1.0", DB: db, Broker: "kafka",
			}); err != nil {
				t.Fatalf("New --broker kafka --db %s: %v", db, err)
			}

			platform := read(t, dir, "internal/platform/module.go")
			for _, want := range []string{
				"kafka.Broker(",
				"kafka.ConsumerGroup(",
				"var Broker = sync.OnceValue(",
				"APP_KAFKA_BROKERS",
			} {
				if !strings.Contains(platform, want) {
					t.Errorf("platform does not wire %q:\n%s", want, platform)
				}
			}

			// The in-process broker must be GONE. Two publishers is an
			// ambiguous binding; worse, a scaffold that quietly kept memory
			// after --broker kafka would look like it worked and deliver
			// nothing to Kafka. Comments are stripped first — the file
			// names memory in prose explaining its absence.
			code := stripComments(platform)
			for _, gone := range []string{"memory.New()", "brokerPorts"} {
				if strings.Contains(code, gone) {
					t.Errorf("platform still wires the in-process broker %q:\n%s", gone, platform)
				}
			}
			// Exporting a port it does not provide is a boot failure with
			// its own diagnostic ("cannot re-export an imported type").
			for _, gone := range []string{
				"warren.Exports[broker.Publisher]()",
				"warren.Exports[broker.Subscriber]()",
			} {
				if strings.Contains(code, gone) {
					t.Errorf("platform re-exports %q, which fails the boot:\n%s", gone, platform)
				}
			}

			// A consumer injects the ports, so it imports the broker module
			// itself — the same reason it imports Postgres() for inbox.Store.
			notification := read(t, dir, "internal/modules/notification/module.go")
			if !strings.Contains(notification, "platform.Broker()") {
				t.Errorf("the consumer does not import platform.Broker():\n%s", notification)
			}

			if gomod := read(t, dir, "go.mod"); !strings.Contains(gomod, "warren/broker/kafka") {
				t.Errorf("go.mod does not require the kafka module:\n%s", gomod)
			}
		})
	}
}

// TestMemoryBrokerRemainsTheDefault guards the other direction: nothing about
// adding kafka may leak into a scaffold that did not ask for it.
func TestMemoryBrokerRemainsTheDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	platform := read(t, dir, "internal/platform/module.go")
	if !strings.Contains(platform, "memory.New()") {
		t.Errorf("the default scaffold lost its in-process broker:\n%s", platform)
	}
	if strings.Contains(platform, "kafka.") {
		t.Errorf("the default scaffold wires kafka:\n%s", platform)
	}
	if gomod := read(t, dir, "go.mod"); strings.Contains(gomod, "broker/kafka") {
		t.Errorf("the default go.mod requires the kafka module:\n%s", gomod)
	}
}

// TestEveryRequiredFrameworkModuleHasAReplace — Warren is unpublished, so a
// require with no replace is not a warning: `go mod tidy` fails on it and the
// project does not build at all.
//
// The list was written out by hand in TWO places — here and in the notice
// `warren new` prints when --framework is omitted — and they disagreed. The
// notice named two modules; a `--db postgres --broker kafka` project requires
// four. Following the printed advice verbatim left half the framework
// unresolvable, with a go.sum error naming neither cause nor fix, which is
// precisely what that notice exists to prevent.
func TestEveryRequiredFrameworkModuleHasAReplace(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ db, broker string }{
		{"memory", "memory"},
		{"postgres", "memory"},
		{"memory", "kafka"},
		{"postgres", "kafka"},
	} {
		t.Run(tc.db+"+"+tc.broker, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := scaffold.New(scaffold.Options{
				Dir: dir, Name: "app", ModulePath: "example.com/app", Version: "v0.1.0",
				DB: tc.db, Broker: tc.broker, FrameworkPath: "/path/to/warren",
			}); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
			if err != nil {
				t.Fatalf("go.mod: %v", err)
			}
			replaces := scaffold.Replaces("/path/to/warren", "")

			for line := range strings.SplitSeq(string(gomod), "\n") {
				f := strings.Fields(line)
				if len(f) != 2 || !strings.HasPrefix(f[0], "github.com/MerseniBilel/warren") {
					continue
				}
				if !strings.Contains(replaces, "replace "+f[0]+" =>") {
					t.Errorf("go.mod requires %s and nothing replaces it — the project will not build", f[0])
				}
				if !strings.Contains(string(gomod), "replace "+f[0]+" =>") {
					t.Errorf("--framework did not write a replace for %s", f[0])
				}
			}
		})
	}
}
