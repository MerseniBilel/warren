// Package generate writes the code a feature needs and wires it into the
// module that owns it.
//
// Every generator obeys three rules. It is ATOMIC — every target is checked
// before anything is written, so a refused run leaves no partial state, not
// even a module.go edit. It NEVER overwrites: a conflict is an error naming
// every colliding path, not a prompt (impossible in CI) and not a silent
// clobber. And re-running it is a refusal, never a duplicate provider.
//
// Each generator returns the plan it carried out — the same text --dry-run
// prints — so the caller can show what changed without diffing the tree.
package generate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/MerseniBilel/warren/cli/internal/astedit"
)

// Options configure a generator.
type Options struct {
	// Dir is the project root: the directory holding go.mod.
	Dir string
	// Module is the feature module to generate into. Module ignores it.
	Module string
	// Name is the thing being generated, in Go's exported form — Order,
	// SuspendUser.
	Name string
	// Topic is the message topic a consumer subscribes to. Empty derives it
	// from Name: OrderPlaced becomes order.placed.
	Topic string
	// DryRun returns the plan and writes nothing.
	DryRun bool
	// Force overwrites files that already exist.
	Force bool
}

// Module creates a feature module, its three layers, and wires it into main.
func Module(opts Options) (string, error) {
	if opts.Name == "" {
		return "", errMissingName("a module name")
	}
	modPath, err := goModulePath(opts.Dir)
	if err != nil {
		return "", err
	}
	name := strings.ToLower(opts.Name)
	base := "internal/modules/" + name
	data := map[string]string{
		"Module": modPath, "Name": name, "Title": title(name),
		"EnvPrefix": envPrefix(opts.Dir),
	}

	files := map[string]string{
		base + "/module.go":             "module.go.tmpl",
		base + "/module_test.go":        "module_test.go.tmpl",
		base + "/domain/doc.go":         "layer_domain.go.tmpl",
		base + "/application/doc.go":    "layer_app.go.tmpl",
		base + "/infrastructure/doc.go": "layer_infra.go.tmpl",
	}
	p := &plan{dir: opts.Dir, dryRun: opts.DryRun, force: opts.Force, files: map[string][]byte{}}
	for path, tmpl := range files {
		content, rerr := render(tmpl, data)
		if rerr != nil {
			return "", rerr
		}
		p.files[path] = content
	}

	mainPath, err := findMain(opts.Dir)
	if err != nil {
		return "", err
	}
	p.edits = []edit{{
		path: mainPath,
		what: "register " + name + ".Module()",
		fn: func(src []byte) ([]byte, error) {
			out, aerr := astedit.AddImport(src, modPath+"/internal/modules/"+name)
			if aerr != nil {
				return nil, aerr
			}
			return astedit.AddCallArgument(out, "warren.New", name+".Module()")
		},
	}}
	return p.apply()
}

// Entity creates a domain aggregate: the identity, the aggregate, its first
// event, and the repository port.
//
// It edits no module: an aggregate is not a provider, and a port with no
// implementation has nothing to register.
func Entity(opts Options) (string, error) {
	data, base, err := featureData(opts)
	if err != nil {
		return "", err
	}
	content, err := render("entity.go.tmpl", data)
	if err != nil {
		return "", err
	}
	p := &plan{
		dir: opts.Dir, dryRun: opts.DryRun, force: opts.Force,
		files: map[string][]byte{base + "/domain/" + data["Snake"] + ".go": content},
	}
	return p.apply()
}

// Command creates a use case with its test and provides it from the module.
func Command(opts Options) (string, error) {
	data, base, err := featureData(opts)
	if err != nil {
		return "", err
	}
	handler, err := render("command.go.tmpl", data)
	if err != nil {
		return "", err
	}
	test, err := render("command_test.go.tmpl", data)
	if err != nil {
		return "", err
	}
	p := &plan{
		dir: opts.Dir, dryRun: opts.DryRun, force: opts.Force,
		files: map[string][]byte{
			base + "/application/" + data["Snake"] + ".go":      handler,
			base + "/application/" + data["Snake"] + "_test.go": test,
		},
		edits: []edit{provide(data, base, "application", "New"+opts.Name+"Handler")},
	}
	return p.apply()
}

// Repository creates a repository implementation and provides it. It needs
// the aggregate and its port to exist — run `warren g entity` first.
func Repository(opts Options) (string, error) {
	data, base, err := featureData(opts)
	if err != nil {
		return "", err
	}
	content, err := render("repository.go.tmpl", data)
	if err != nil {
		return "", err
	}
	p := &plan{
		dir: opts.Dir, dryRun: opts.DryRun, force: opts.Force,
		files: map[string][]byte{base + "/infrastructure/" + data["Snake"] + "_repository.go": content},
		edits: []edit{provide(data, base, "infrastructure", "New"+opts.Name+"Repository")},
	}
	return p.apply()
}

// Consumer creates an event handler and the subscription that feeds it.
//
// The two are separate files on purpose. The handler is an ordinary use
// case in the application layer and imports no transport package
// (invariant 5); the subscription lives beside module.go, which is the only
// place in a feature allowed to see both the broker and the handler.
func Consumer(opts Options) (string, error) {
	data, base, err := featureData(opts)
	if err != nil {
		return "", err
	}
	data["Topic"] = opts.Topic
	if data["Topic"] == "" {
		data["Topic"] = strings.ReplaceAll(data["Snake"], "_", ".")
	}

	handler, err := render("consumer.go.tmpl", data)
	if err != nil {
		return "", err
	}
	subscription, err := render("subscription.go.tmpl", data)
	if err != nil {
		return "", err
	}
	p := &plan{
		dir: opts.Dir, dryRun: opts.DryRun, force: opts.Force,
		files: map[string][]byte{
			base + "/application/on_" + data["Snake"] + ".go":  handler,
			base + "/on_" + data["Snake"] + "_subscription.go": subscription,
		},
		edits: []edit{
			provide(data, base, "application", "NewOn"+opts.Name+"Handler"),
			{
				path: base + "/module.go",
				what: "consume " + data["Topic"],
				fn: func(src []byte) ([]byte, error) {
					return astedit.AddArgument(src, "warren.Consumers", "new"+opts.Name+"Subscription")
				},
			},
		},
	}
	return p.apply()
}

// provide is the edit that adds a constructor to a module's provider list,
// importing the layer it lives in. Both halves are one edit because either
// alone leaves module.go uncompilable.
func provide(data map[string]string, base, layer, ctor string) edit {
	qualified := layer + "." + ctor
	imported := data["Module"] + "/internal/modules/" + data["Feature"] + "/" + layer
	return edit{
		path: base + "/module.go",
		what: "provide " + qualified,
		fn: func(src []byte) ([]byte, error) {
			out, err := astedit.AddImport(src, imported)
			if err != nil {
				return nil, err
			}
			return astedit.AddArgument(out, "warren.Providers", qualified)
		},
	}
}

// plan is the set of changes a generator will make: whole files to create,
// and edits to apply to files that already exist.
type plan struct {
	dir    string
	files  map[string][]byte
	edits  []edit
	dryRun bool
	force  bool
}

type edit struct {
	path string
	what string
	fn   func([]byte) ([]byte, error)
}

// apply checks every target, computes every edit, and only then writes.
// Nothing reaches the disk unless everything can.
func (p *plan) apply() (string, error) {
	var conflicts []string
	for path := range p.files {
		if _, err := os.Stat(filepath.Join(p.dir, path)); err == nil {
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) > 0 && !p.force {
		slices.Sort(conflicts)
		return "", errConflict(conflicts)
	}

	// Every edit is computed up front, so an edit that fails cannot leave
	// newly written files behind.
	//
	// Two edits to the same file — a consumer both provides its handler and
	// registers its subscription — must COMPOSE: each one reads what the
	// previous produced, not what is still on disk, or the last writer wins
	// and the earlier wiring is silently dropped.
	edited := make(map[string][]byte, len(p.edits))
	for _, e := range p.edits {
		src, seen := edited[e.path]
		if !seen {
			var err error
			if src, err = os.ReadFile(filepath.Join(p.dir, e.path)); err != nil {
				return "", fmt.Errorf("warren g: reading %s: %w", e.path, err)
			}
		}
		out, err := e.fn(src)
		if err != nil {
			return "", err
		}
		edited[e.path] = out
	}

	if p.dryRun {
		return p.String(), nil
	}
	for _, path := range sortedKeys(p.files) {
		full := filepath.Join(p.dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, p.files[path], 0o644); err != nil {
			return "", err
		}
	}
	for _, path := range sortedKeys(edited) {
		if err := os.WriteFile(filepath.Join(p.dir, path), edited[path], 0o644); err != nil {
			return "", err
		}
	}
	return p.String(), nil
}

// String renders the plan — what --dry-run prints, and what a real run
// reports afterwards.
func (p *plan) String() string {
	var b strings.Builder
	for _, path := range sortedKeys(p.files) {
		fmt.Fprintf(&b, "  create  %s\n", path)
	}
	for _, e := range p.edits {
		fmt.Fprintf(&b, "  edit    %s  (%s)\n", e.path, e.what)
	}
	return b.String()
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// featureData validates the target module and builds the template data.
func featureData(opts Options) (map[string]string, string, error) {
	if opts.Name == "" {
		return nil, "", errMissingName("a name")
	}
	modPath, err := goModulePath(opts.Dir)
	if err != nil {
		return nil, "", err
	}
	base := "internal/modules/" + opts.Module
	if _, serr := os.Stat(filepath.Join(opts.Dir, base, "module.go")); serr != nil {
		return nil, "", errUnknownModule(opts.Module, opts.Dir)
	}
	return map[string]string{
		"Module":  modPath,
		"Feature": opts.Module,
		"Name":    opts.Name,
		"Lower":   lowerFirst(opts.Name),
		"Snake":   snake(opts.Name),
		"Article": article(opts.Name),
	}, base, nil
}

// goModulePath reads the project's module path out of go.mod, which is what
// every generated import is built from.
func goModulePath(dir string) (string, error) {
	src, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", diagnostic("✗ not a Go module\n\n    There is no go.mod here.\n\n" +
			"  Run `warren g` from the root of your project.")
	}
	for line := range strings.SplitSeq(string(src), "\n") {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(path), nil
		}
	}
	return "", diagnostic("✗ go.mod declares no module path")
}

// findMain locates the application's main.go — the file a new module is
// wired into.
func findMain(dir string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "cmd"))
	if err != nil {
		return "", diagnostic("✗ no cmd directory\n\n    Cannot find the application's main package.\n\n" +
			"  Run `warren g module` from the root of a project scaffolded by `warren new`.")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("cmd", e.Name(), "main.go"))
		if _, serr := os.Stat(filepath.Join(dir, rel)); serr == nil {
			return rel, nil
		}
	}
	return "", diagnostic("✗ no main.go under cmd/\n\n    Cannot find the application's main package.")
}

func render(name string, data map[string]string) ([]byte, error) {
	raw, err := templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, err
	}
	t, err := template.New(name).Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return nil, err
	}
	return gofmt([]byte(b.String()), name)
}

// snake turns SuspendUser into suspend_user — Go's file naming, and the
// stem of the event name.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// article picks "an" or "a" for a doc comment. It is wrong for the odd word
// ("an hour", "a union"), and worth it: the alternative is a comment that
// reads like a template.
func article(s string) string {
	if s == "" {
		return "a"
	}
	if strings.ContainsRune("AEIOUaeiou", rune(s[0])) {
		return "an"
	}
	return "a"
}

type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errConflict(paths []string) error {
	return diagnostic(fmt.Sprintf(
		"✗ these files already exist\n\n      %s\n\n"+
			"  Nothing was written and nothing was wired. Delete them, choose another\n"+
			"  name, or pass --force to overwrite.",
		strings.Join(paths, "\n      ")))
}

func errUnknownModule(name, dir string) error {
	var have []string
	entries, _ := os.ReadDir(filepath.Join(dir, "internal/modules"))
	for _, e := range entries {
		if e.IsDir() {
			have = append(have, e.Name())
		}
	}
	list := "none yet"
	if len(have) > 0 {
		list = strings.Join(have, ", ")
	}
	return diagnostic(fmt.Sprintf(
		"✗ no such module: %q\n\n    There is no internal/modules/%s/module.go.\n\n"+
			"  This project has: %s\n\n  Create it first:\n\n      warren g module %s",
		name, name, list, name))
}

func errMissingName(what string) error {
	return diagnostic(fmt.Sprintf("✗ missing %s\n\n    `warren g` needs a name to generate.", what))
}
