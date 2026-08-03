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
	// grpc is still refused, and postgres is refused because the scaffold
	// cannot wire it yet even though the adapter exists — a flag that
	// silently produced an in-memory repository would be worse than a
	// refusal naming the manual steps.
	"transport": {"http"},
	"db":        {"memory"},
	"broker":    {"memory"},
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

	data := map[string]string{
		"Name":      opts.Name,
		"Module":    opts.ModulePath,
		"Version":   opts.Version,
		"EnvPrefix": strings.ToUpper(strings.ReplaceAll(opts.Name, "-", "_")),
		"GoVersion": goVersion,
	}

	// Render everything first, then check every target, then write: a
	// scaffold is all-or-nothing.
	files, err := render(data, opts.Name)
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

	if opts.FrameworkPath != "" {
		files["go.mod"] = append(files["go.mod"], []byte(fmt.Sprintf(
			"\nreplace github.com/MerseniBilel/warren => %s\n\nreplace github.com/MerseniBilel/warren/transport/http => %s/transport/http\n",
			opts.FrameworkPath, opts.FrameworkPath))...)
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
func render(data map[string]string, appName string) (map[string][]byte, error) {
	entries, err := templates.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
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

		path := targetPath(e.Name(), appName)
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
