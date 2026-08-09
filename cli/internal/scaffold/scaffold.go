// Package scaffold writes the tree `warren new` produces: a working Warren
// service that compiles, boots, and passes its own tests against the
// released framework.
//
// Two rules shape everything here. It is ATOMIC — every target is checked
// before anything is written, so a refused scaffold leaves no partial tree.
// And it teaches only shapes that stay correct: the scaffold has no
// transport adapter yet, so it demonstrates the paths that exist rather
// than inventing a temporary one users would have to unlearn.
package scaffold

import (
	"embed"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

//go:embed all:templates
var templates embed.FS

// Options configure a scaffold.
type Options struct {
	// Dir is where the tree is written.
	Dir string
	// Name is the application's name — the binary, and the env prefix.
	Name string
	// ModulePath is the generated go.mod's module path.
	ModulePath string
	// Version is the framework version the scaffold pins.
	Version string
	// FrameworkPath, when set, adds replace directives pointing the
	// framework modules at a local checkout.
	//
	// It exists because Warren is not published: the generated go.mod
	// requires github.com/MerseniBilel/warren v0.1.0, nothing serves that
	// version yet, and `go build` in a fresh scaffold fails fifteen times
	// over with "missing go.sum entry". A user who has the repository can
	// point at it; when v0.1.0 is tagged, this flag stops being necessary
	// and the templates stop mentioning it.
	//
	// AGENT.md invariant 8 forbids a replace COMMITTED TO THIS REPOSITORY.
	// One written into a user's own new project, on request, is not that.
	FrameworkPath string
	// Transport, DB and Broker select adapters. Only released values are
	// accepted; anything else is refused with what is available.
	Transport string
	DB        string
	Broker    string
}

// released names the adapter values that exist today. A scaffold that
// silently ignored an unreleased choice would produce code that does not
// compile, which is a worse first impression than a refusal.
var released = map[string][]string{
	// http is what a scaffold wires by default, so accepting the flag is
	// honest rather than a no-op: --transport http asks for what you get.
	// grpc is still refused. postgres wires the real adapter: the platform
	// module imports postgres.Module, the memory unit of work and the memory
	// outbox and inbox stores are NOT emitted beside it, the user repository
	// is plain SQL, and cmd/migrate plus db/migrations come with it.
	"transport": {"http"},
	"db":        {"memory", "postgres"},
	// kafka wires the real driver: platform declares kafka.Broker() as its
	// own sync.OnceValue module and imports it, the in-process broker is NOT
	// emitted beside it, and any feature module that consumes events imports
	// that module directly — a module cannot re-export what it imports.
	"broker": {"memory", "kafka"},
}

// New writes the scaffold.
func New(opts Options) error {
	if opts.ModulePath == "" {
		return diagnostic("✗ missing module path\n\n    warren new needs --module, the import path of the app you are creating.\n\n  warren new " + opts.Name + " --module github.com/you/" + opts.Name)
	}
	if err := checkModulePath(opts.ModulePath, opts.Name); err != nil {
		return err
	}
	for flag, value := range map[string]string{
		"transport": opts.Transport, "db": opts.DB, "broker": opts.Broker,
	} {
		if value == "" || slices.Contains(released[flag], value) {
			continue
		}
		return errUnreleased(flag, value)
	}

	db := opts.DB
	if db == "" {
		db = "memory"
	}
	brk := opts.Broker
	if brk == "" {
		brk = "memory"
	}
	data := map[string]string{
		"Name":      opts.Name,
		"Module":    opts.ModulePath,
		"Version":   opts.Version,
		"EnvPrefix": strings.ToUpper(strings.ReplaceAll(opts.Name, "-", "_")),
		"GoVersion": goVersion,
		"DB":        db,
		"Broker":    brk,
	}

	// Render everything first, then check every target, then write: a
	// scaffold is all-or-nothing.
	files, err := render(data, opts.Name, db)
	if err != nil {
		return err
	}
	var conflicts []string
	for path := range files {
		full := filepath.Join(opts.Dir, path)
		if _, err := os.Stat(full); err == nil {
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		return errConflict(conflicts)
	}

	// frameworkModules is EVERY Warren module, in go.mod order. It is one
	// list because it has two readers: the go.mod this writes, and the
	// notice `warren new` prints when --framework was NOT given. Those two
	// disagreed — the notice named two modules while a
	// `--db postgres --broker kafka` project requires four, so following it
	// verbatim left the project unable to resolve half the framework.
	if opts.FrameworkPath != "" {
		// A missing replace is not a warning: the module is untagged, so
		// `go mod tidy` fails on it and the scaffold does not build at all —
		// which is the whole reason the flag exists.
		files["go.mod"] = append(files["go.mod"], []byte(Replaces(opts.FrameworkPath, "\n"))...)
	}

	for path, content := range files {
		full := filepath.Join(opts.Dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("warren new: %w", err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return fmt.Errorf("warren new: %w", err)
		}
	}
	return nil
}

// goVersion is the toolchain the scaffold declares. It tracks the framework
// (AGENT.md invariant 9: Go 1.27 on its release, 1.26.x until then).
const goVersion = "1.26.3"

// render executes every template, formatting the Go ones. A template that
// produces unparseable Go is a bug in the CLI, not in the user's project, so
// it fails here with the file named.
// driverTemplate is a template that belongs to ONE --db value: written for
// that driver, skipped for every other. So a memory scaffold gains no migrate
// binary, and a postgres one gains no in-memory repository beside the real
// one — which is the ambiguous-binding boot failure a user hit when they
// added postgres by hand.
type driverTemplate struct {
	db string // the --db value this template is for
	as string // the template name whose PATH it takes, when they differ
}

var driverOnly = map[string]driverTemplate{
	"cmd__migrate__main.go.tmpl":                                            {db: "postgres"},
	"db__migrations__00001_users.sql.tmpl":                                  {db: "postgres"},
	"internal__platform__module.go.tmpl":                                    {db: "memory"},
	"internal__modules__user__infrastructure__user_repository.go.tmpl":      {db: "memory"},
	"internal__modules__user__infrastructure__user_repository_test.go.tmpl": {db: "memory"},

	// The postgres variants land at the SAME paths as the memory ones they
	// replace, so nothing downstream — g repository, lint arch, the reader —
	// has to know which driver a project was scaffolded with.
	"internal__platform__module_postgres.go.tmpl": {
		db: "postgres", as: "internal__platform__module.go.tmpl",
	},
	"internal__modules__user__infrastructure__user_repository_postgres.go.tmpl": {
		db: "postgres", as: "internal__modules__user__infrastructure__user_repository.go.tmpl",
	},
	// The contract test travels with the repository it certifies, at the same
	// path under either driver: a scaffold whose Save loses events is a green
	// build without it, which is the gap this pair closes.
	"internal__modules__user__infrastructure__user_repository_postgres_test.go.tmpl": {
		db: "postgres", as: "internal__modules__user__infrastructure__user_repository_test.go.tmpl",
	},
}

func render(data map[string]string, appName, db string) (map[string][]byte, error) {
	entries, err := templates.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if dt, ok := driverOnly[name]; ok {
			if dt.db != db {
				continue
			}
			if dt.as != "" {
				name = dt.as
			}
		}
		raw, err := templates.ReadFile("templates/" + e.Name())
		if err != nil {
			return nil, err
		}
		tmpl, err := template.New(e.Name()).Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("warren new: template %s: %w", e.Name(), err)
		}
		var buf strings.Builder
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("warren new: template %s: %w", e.Name(), err)
		}

		path := targetPath(name, appName)
		content := []byte(buf.String())
		if strings.HasSuffix(path, ".go") {
			formatted, err := Format(content)
			if err != nil {
				return nil, fmt.Errorf("warren new: template %s produced invalid Go: %w", e.Name(), err)
			}
			content = formatted
		}
		out[path] = content
	}
	return out, nil
}

// targetPath maps a flat template name to its place in the tree:
// "cmd__APPNAME__main.go.tmpl" → "cmd/<app>/main.go". Templates live in one
// flat directory so the embedded FS and the tree it produces stay easy to
// read side by side.
func targetPath(name, appName string) string {
	path := strings.TrimSuffix(name, ".tmpl")
	path = strings.ReplaceAll(path, "__", "/")
	return strings.ReplaceAll(path, "APPNAME", appName)
}

// Format runs gofmt over generated source.
func Format(src []byte) ([]byte, error) { return format.Source(src) }

type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errUnreleased(flag, value string) error {
	avail := released[flag]
	have := "nothing yet"
	if len(avail) > 0 {
		have = strings.Join(avail, ", ")
	}
	return diagnostic(fmt.Sprintf(
		"✗ %s adapter not released\n\n    --%s %s  →  that adapter does not exist yet.\n\n"+
			"  Released today: %s\n\n"+
			"  Scaffold without it: the app already serves HTTP, and you can add\n"+
			"  the adapter to cmd/<name>/main.go by hand.\n"+
			"  (transport/grpc and broker/kafka are the adapters still to come;\n"+
			"  transport/http is wired for you and persistence/postgres is ready\n"+
			"  to add manually.)",
		flag, flag, value, have))
}

func errConflict(paths []string) error {
	return diagnostic(fmt.Sprintf(
		"✗ target is not empty\n\n    These files already exist:\n      %s\n\n"+
			"  Nothing was written. Scaffold into an empty directory, or move the\n"+
			"  existing files aside first.", strings.Join(paths, "\n      ")))
}

// checkModulePath refuses a --module value go.mod will not accept.
//
// It is deliberately narrower than golang.org/x/mod/module.CheckPath: this
// module budgets one dependency (cobra), and the whole point is to catch
// what a person actually mistypes — a space, a quote, a stray punctuation
// mark — before a tree is written whose every go command fails with
// "go.mod:1: usage: module module/path". A path this accepts and go rejects
// is a worse diagnostic, not a wrong one; the go command still has the last
// word.
func checkModulePath(path, name string) error {
	suggest := "github.com/you/" + name
	bad := func(why string) error {
		return diagnostic(fmt.Sprintf(
			"✗ %q is not a valid Go module path\n\n    %s\n\n"+
				"    It goes straight into go.mod, so every go command in the new tree\n"+
				"    would fail with \"go.mod:1: usage: module module/path\".\n\n  warren new %s --module %s",
			path, why, name, suggest))
	}

	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-._~/", r):
		case r <= ' ' || r == 0x7f:
			return bad("It contains a space or a control character.")
		default:
			return bad(fmt.Sprintf("The character %q is not allowed in an import path.", r))
		}
	}
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return bad("An import path has no leading or trailing slash.")
	}
	if strings.Contains(path, "//") {
		return bad("An import path has no empty element.")
	}
	for _, elem := range strings.Split(path, "/") {
		if strings.HasPrefix(elem, ".") || strings.HasSuffix(elem, ".") {
			return bad(fmt.Sprintf("The element %q begins or ends with a dot.", elem))
		}
	}
	return nil
}

// frameworkModules is every Warren module, as a path suffix. EVERY one, not
// only those a given scaffold requires today: a replace for an unrequired
// module is inert, but the moment a user runs `go get` for one that is
// missing it resolves from GITHUB instead of the local checkout, silently,
// and they are running two versions of the framework at once. A field test
// hit exactly that with validate/playground.
var frameworkModules = []string{
	"",
	"/transport/http",
	"/persistence/postgres",
	"/observability",
	"/broker/kafka",
	"/validate/playground",
}

// Replaces renders a replace directive per framework module, each prefixed
// by lead. It is what `--framework` writes into go.mod and what the notice
// prints when the flag was omitted, so the two cannot drift apart again.
//
// All of this goes away when v0.1.0 is tagged and the flag stops being
// necessary.
func Replaces(frameworkPath, lead string) string {
	var b strings.Builder
	for _, m := range frameworkModules {
		fmt.Fprintf(&b, "%sreplace github.com/MerseniBilel/warren%s => %s%s\n",
			lead, m, frameworkPath, m)
	}
	return b.String()
}
