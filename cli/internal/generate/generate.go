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
	"strconv"
	"strings"
	"text/template"
	"unicode"

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
	// Main is the main.go a new module is registered in, relative to Dir.
	// Empty finds it, and errors when a project has more than one.
	Main string
	// Driver selects a repository implementation: "memory" (the default) or
	// "postgres".
	Driver string
}

// Module creates a feature module, its three layers, and wires it into main.
func Module(opts Options) (string, error) {
	if err := checkModuleName(opts.Name); err != nil {
		return "", err
	}
	modPath, err := goModulePath(opts.Dir)
	if err != nil {
		return "", err
	}
	name := strings.ToLower(opts.Name)
	base := "internal/modules/" + name

	// --force is deliberately not honoured for a module declaration. Every
	// other generator's --force costs you one file you can rewrite; this one
	// would replace module.go with an empty declaration and silently discard
	// every provider, consumer and export the module had accumulated.
	if _, err := os.Stat(filepath.Join(opts.Dir, base, "module.go")); err == nil {
		return "", errModuleExists(name, base)
	}

	data := map[string]string{
		"Module": modPath, "Name": name, "Title": upperFirst(name),
		"EnvPrefix": strconv.Quote(envPrefix(opts.Dir) + "_NAME"),
	}

	files := map[string]string{
		base + "/module.go":             "module.go.tmpl",
		base + "/controller.go":         "controller.go.tmpl",
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

	mainPath, err := findMain(opts.Dir, opts.Main)
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
		declares: []decl{{base + "/domain", []string{
			opts.Name, opts.Name + "ID", opts.Name + "Created", opts.Name + "Repository", "New" + opts.Name,
		}}},
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
		edits: []edit{
			provide(data, base, "application", "New"+opts.Name+"Handler"),
			expose(data, base),
		},
		declares: []decl{{base + "/application", []string{
			opts.Name, opts.Name + "Result", data["Lower"], "New" + opts.Name + "Handler",
		}}},
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
	driver := opts.Driver
	if driver == "" {
		driver = "memory"
	}
	tmpl, ok := repositoryTemplates[driver]
	if !ok {
		return "", errUnknownDriver(driver)
	}
	// The repository implements the port the ENTITY declares and stores the
	// aggregate the entity defines. Without it the generated file cannot
	// compile — and until this check existed the generator wrote it anyway,
	// spliced a provider and an import into module.go, and reported success.
	if err := checkEntityExists(opts.Dir, base, opts.Module, opts.Name); err != nil {
		return "", err
	}
	content, err := render(tmpl, data)
	if err != nil {
		return "", err
	}
	p := &plan{
		dir: opts.Dir, dryRun: opts.DryRun, force: opts.Force,
		files: map[string][]byte{base + "/infrastructure/" + data["Snake"] + "_repository.go": content},
		edits: []edit{provide(data, base, "infrastructure", "New"+opts.Name+"Repository")},
		declares: []decl{{base + "/infrastructure", []string{
			opts.Name + "Repository", "New" + opts.Name + "Repository",
		}}},
	}
	if driver == "postgres" {
		// The table is the user's to own, so it goes in the project's own
		// migration directory rather than being invented at boot — Warren
		// never migrates from a lifecycle hook.
		migration, merr := render("migration.sql.tmpl", data)
		if merr != nil {
			return "", merr
		}
		// Numbered after whatever is already there. Every aggregate used to
		// be written as 00001_<plural>.sql, so a second one collided: two
		// files at one version apply in ALPHABETICAL order, which is wrong
		// for any foreign key between them.
		p.files[fmt.Sprintf("db/migrations/%05d_%s.sql", nextMigration(opts.Dir), data["Plural"])] = migration

		// The migrate binary lives in the PROJECT, not in the warren CLI.
		// The CLI takes no dependency on the framework — it is build-time
		// tooling that never enters a service's go.mod — and a `warren
		// migrate` would have to import persistence/postgres and pgx to
		// exist. Generating it here also puts it where a deploy job can run
		// it: `go run ./cmd/migrate`.
		migrateMain, mmerr := render("migrate_main.go.tmpl", data)
		if mmerr != nil {
			return "", mmerr
		}
		p.files["cmd/migrate/main.go"] = migrateMain
		// One migrate command per PROJECT, not per aggregate.
		p.keepExisting = map[string]bool{"cmd/migrate/main.go": true}
		p.next = fmt.Sprintf(`  Still to do — the repository needs a pool and a table.

  1. Declare the Postgres module ONCE, in internal/platform, and export what
     the features need:

         // internal/platform/postgres.go
         var Postgres = sync.OnceValue(func() warren.Module {
             return postgres.Module(
                 postgres.DSN(os.Getenv("DATABASE_URL")),
                 postgres.WithOutbox(),
             )
         })

     Then import it from the feature that uses it — %s/module.go:

         warren.Imports(platform.Postgres())

     A module that wants postgres.DB must IMPORT the module providing it: a
     provider is private to its module unless exported. Passing a module as
     an ARGUMENT to a feature's factory does not work — modules are
     deduplicated by identity, so a factory called twice is two modules
     sharing a name, which is a boot error.

  2. Apply the schema as a DEPLOY STEP — Warren never migrates at boot.
     cmd/migrate was generated for you:

         DATABASE_URL=... go run ./cmd/migrate

     Name your migration files for your own aggregates: yours and Warren's
     record into one warren_schema_migrations table, keyed by bare filename,
     so 00001_warren_outbox.sql would be marked applied without running.

  3. Check db/migrations/00001_%s.sql — it has the aggregate's id and
     created_at, and nothing else. Add your columns and the Save/FindByID
     statements that read them.
`, data["Feature"], data["Plural"])
	}
	return p.apply()
}

// repositoryTemplates maps a driver to the file it generates. Adding a
// driver is adding a row and a template — the port is the same.
var repositoryTemplates = map[string]string{
	"memory":   "repository.go.tmpl",
	"postgres": "repository_postgres.go.tmpl",
}

// plural is the table name for an aggregate. Two rules cover almost every
// name; the rest is a line the user edits, which beats a dependency.
// nextMigration returns the version a new migration should carry: one past
// the highest already in db/migrations, or 1 when there are none.
//
// It reads the directory rather than counting this run's files because the
// user's own hand-written migrations live there too, and a generated file
// that reused their number would apply in alphabetical order against them.
func nextMigration(dir string) int {
	entries, err := os.ReadDir(filepath.Join(dir, "db", "migrations"))
	if err != nil {
		return 1
	}
	highest := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		digits := e.Name()
		if i := strings.IndexByte(digits, '_'); i >= 0 {
			digits = digits[:i]
		}
		n, convErr := strconv.Atoi(digits)
		if convErr == nil && n > highest {
			highest = n
		}
	}
	return highest + 1
}

func plural(s string) string {
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	default:
		return s + "s"
	}
}

func errUnknownDriver(driver string) error {
	return diagnostic(fmt.Sprintf(
		"✗ unknown repository driver %q\n\n    --driver %s\n\n"+
			"  Available: memory (the default), postgres.\n\n"+
			"  A driver is one template and one row in repositoryTemplates —\n"+
			"  the port every driver implements is the same.", driver, driver))
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
	topic := opts.Topic
	if topic == "" {
		topic = strings.ReplaceAll(data["Snake"], "_", ".")
	}
	data["Topic"] = topic
	// The aggregate whose fact this is. Warren's events are named
	// <Aggregate><PastParticiple> — "named in the past tense" is the entity
	// generator's own rule — so dropping the final word yields the aggregate,
	// and `g entity` marshals its identifier as "<aggregate>_id". Deriving it
	// here is what makes the two generators agree about the wire format of
	// one event; they used to disagree in silence.
	agg := aggregateOf(opts.Name)
	data["Aggregate"] = agg
	data["AggregateSnake"] = snake(agg)
	data["AggregateIDKey"] = strconv.Quote(snake(agg) + "_id")
	// The topic reaches three Go string literals. Quoted, a `"` in it closes
	// the literal and the rest becomes an expression; a `\` makes the
	// template fail and blame Warren for the user's input.
	data["TopicLiteral"] = strconv.Quote(topic)
	data["HandlingLiteral"] = strconv.Quote("handling " + topic)
	data["HookNameLiteral"] = strconv.Quote(opts.Module + " " + topic + " consumer")

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
					// Providers plus Eager, not Consumers: the generated
					// subscription wires its own pipeline and lifecycle hook
					// rather than registering through transport.OnEvent, so
					// it is not a transport.Controller. Eager is what builds
					// a type at boot that nothing else depends on.
					src, err := astedit.AddArgument(src, "warren.Providers", "new"+opts.Name+"Subscription")
					if err != nil {
						return nil, err
					}
					return astedit.AddArgument(src, "warren.NewModule", "warren.Eager[*"+data["Lower"]+"Subscription]()")
				},
			},
		},
		declares: []decl{
			{base + "/application", []string{
				opts.Name, opts.Name + "Handled", "on" + opts.Name, "NewOn" + opts.Name + "Handler",
			}},
			{base, []string{data["Lower"] + "Subscription", "new" + opts.Name + "Subscription"}},
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

// expose wires a generated handler into the feature's controller: the field
// that holds it, the constructor parameter that injects it, and the route
// that serves it.
//
// All three are one edit because any one alone leaves controller.go
// uncompilable — an unused parameter, a field assigned from nothing, a
// route referring to a field that does not exist.
//
// Until this existed the generator printed a twelve-line patch and asked
// the user to make all three by hand, every time, in a file no generator
// creates. Four commands meant four patches, and a handler nobody patched
// in was wired into the container and reachable by nothing.
func expose(data map[string]string, base string) edit {
	field := data["Lower"]
	handler := fmt.Sprintf("app.Handler[application.%s, application.%sResult]", data["Name"], data["Name"])
	route := fmt.Sprintf("transport.Post(r, %q, c.%s)", "/"+data["Snake"], field)
	appPkg := data["Module"] + "/internal/modules/" + data["Feature"] + "/application"

	return edit{
		path: base + "/controller.go",
		what: fmt.Sprintf("serve POST /%s from Controller.Register", data["Snake"]),
		fn: func(src []byte) ([]byte, error) {
			for _, path := range []string{
				"github.com/MerseniBilel/warren/app",
				"github.com/MerseniBilel/warren/transport",
				appPkg,
			} {
				out, err := astedit.AddImport(src, path)
				if err != nil {
					return nil, err
				}
				src = out
			}
			out, err := astedit.AddStructField(src, "Controller", field, handler)
			if err != nil {
				return nil, err
			}
			if out, err = astedit.AddConstructorParam(out, "NewController", "Controller", field, handler); err != nil {
				return nil, err
			}
			return astedit.AddStatement(out, "Register", route)
		},
	}
}

// plan is the set of changes a generator will make: whole files to create,
// edits to apply to files that already exist, and the identifiers the new
// code will declare.
type plan struct {
	dir      string
	files    map[string][]byte
	edits    []edit
	declares []decl
	// keepExisting names files that are the PROJECT's, not this feature's:
	// written when absent, left alone when present. cmd/migrate/main.go is
	// shared by every aggregate, so treating it as a conflict made the second
	// `g repository --driver postgres` fail over a file the generator itself
	// had written — where "delete it" meant deleting the shared migrate
	// command and --force meant overwriting hand-edited migrations.
	keepExisting map[string]bool

	// next is what the user must still do by hand, printed after the plan.
	next   string
	dryRun bool
	force  bool
}

type edit struct {
	path string
	what string
	fn   func([]byte) ([]byte, error)
}

// decl is a package directory and the identifiers the generated code will
// declare in it.
type decl struct {
	pkgDir string
	names  []string
}

// apply checks every target, computes every edit, writes — and undoes the
// lot if any write fails.
//
// The check-first pass catches the ordinary refusals. The rollback catches
// what it cannot: a read-only file, a full disk, a permission change
// between the check and the write. Without it a failed run leaves files
// nothing references, and the next run refuses because those files exist
// while telling the user nothing was written.
func (p *plan) apply() (string, error) {
	existing := make(map[string]bool, len(p.files))
	var conflicts []string
	for path := range p.files {
		if _, err := os.Stat(filepath.Join(p.dir, path)); err == nil {
			if p.keepExisting[path] {
				// The project already has it. Not a conflict and not a
				// rewrite — simply not this generator's file to touch.
				delete(p.files, path)
				continue
			}
			existing[path] = true
			conflicts = append(conflicts, path)
		}
	}
	if len(conflicts) > 0 && !p.force {
		slices.Sort(conflicts)
		return "", errConflict(conflicts)
	}

	// A collision with an identifier some OTHER file already declares. The
	// generator's own previous output is excluded: that is a path conflict,
	// which --force is for, not a redeclaration.
	for _, d := range p.declares {
		if err := checkNotDeclared(p.dir, d.pkgDir, p.files, d.names...); err != nil {
			return "", err
		}
	}

	// Every edit is computed up front, so an edit that cannot be computed
	// never reaches the disk.
	//
	// Two edits to the same file — a consumer both provides its handler and
	// registers its subscription — must COMPOSE: each one reads what the
	// previous produced, not what is still on disk, or the last writer wins
	// and the earlier wiring is silently dropped.
	edited := make(map[string][]byte, len(p.edits))
	original := make(map[string][]byte, len(p.edits))
	for _, e := range p.edits {
		src, seen := edited[e.path]
		if !seen {
			var err error
			if src, err = os.ReadFile(filepath.Join(p.dir, e.path)); err != nil {
				return "", errCannotRead(e.path, err)
			}
			original[e.path] = src
		}
		out, err := e.fn(src)
		if err != nil {
			return "", err
		}
		edited[e.path] = out
	}

	if p.dryRun {
		return p.describe(existing), nil
	}

	// From here on anything written is undone on failure.
	undo := &rollback{dir: p.dir, original: original}
	for _, path := range sortedKeys(p.files) {
		full := filepath.Join(p.dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", undo.after(errCannotWrite(path, err))
		}
		if existing[path] {
			if prev, rerr := os.ReadFile(full); rerr == nil {
				undo.original[path] = prev
			}
		} else {
			undo.created = append(undo.created, path)
		}
		if err := os.WriteFile(full, p.files[path], 0o644); err != nil {
			return "", undo.after(errCannotWrite(path, err))
		}
	}
	for _, path := range sortedKeys(edited) {
		if err := os.WriteFile(filepath.Join(p.dir, path), edited[path], 0o644); err != nil {
			return "", undo.after(errCannotWrite(path, err))
		}
	}
	return p.describe(existing), nil
}

// rollback restores what a half-finished run changed.
type rollback struct {
	dir      string
	created  []string
	original map[string][]byte
}

// after undoes the run and returns the error that caused it. A failure to
// undo is appended rather than replacing the cause: the user needs to know
// both what went wrong and that the tree was left dirty.
func (r *rollback) after(cause error) error {
	var stuck []string
	for _, path := range r.created {
		if err := os.Remove(filepath.Join(r.dir, path)); err != nil && !os.IsNotExist(err) {
			stuck = append(stuck, path)
		}
	}
	for path, src := range r.original {
		if err := os.WriteFile(filepath.Join(r.dir, path), src, 0o644); err != nil {
			stuck = append(stuck, path)
		}
	}
	if len(stuck) == 0 {
		return cause
	}
	slices.Sort(stuck)
	return diagnostic(fmt.Sprintf(
		"%v\n\n  AND the tree could not be restored. Check these by hand:\n\n      %s",
		cause, strings.Join(stuck, "\n      ")))
}

// describe renders the plan — what --dry-run prints, and what a real run
// reports afterwards. It distinguishes create from overwrite, because a
// plan that calls an overwrite "create" is how a hand-wired file gets lost
// without anyone noticing.
func (p *plan) describe(existing map[string]bool) string {
	var b strings.Builder
	for _, path := range sortedKeys(p.files) {
		verb := "create   "
		if existing[path] {
			verb = "overwrite"
		}
		fmt.Fprintf(&b, "  %s  %s\n", verb, path)
	}
	for _, e := range p.edits {
		fmt.Fprintf(&b, "  edit       %s  (%s)\n", e.path, e.what)
	}
	// What the generator could NOT do for you. A generator that reports only
	// what it wrote leaves the user believing the wiring is finished, and the
	// missing half surfaces as a 404 nobody can explain.
	if p.next != "" {
		b.WriteString("\n")
		b.WriteString(p.next)
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
	if err := checkModuleName(opts.Module); err != nil {
		return nil, "", err
	}
	if err := checkTypeName(opts.Name); err != nil {
		return nil, "", err
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
		// Plural is the table name. English pluralisation is a rabbit hole,
		// so this handles the two cases that cover almost every aggregate
		// name and leaves the rest to the user editing one generated line —
		// which is preferable to a dependency, or to "orderss".
		"Plural": plural(snake(opts.Name)),
	}, base, nil
}

// goModulePath reads the project's module path out of go.mod, which is what
// every generated import is built from.
func goModulePath(dir string) (string, error) {
	if info, serr := os.Stat(dir); serr == nil && !info.IsDir() {
		return "", diagnostic(fmt.Sprintf(
			"✗ %s is not a directory\n\n    --dir is the root of a Go module — the directory holding go.mod.", dir))
	}
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
// registered in.
//
// A project with more than one APPLICATION binary is an ERROR rather than a
// guess. Wiring the first one found leaves the others missing a module with
// nothing to say so, and which binary a feature belongs in is a decision
// only the author can make.
//
// Not every main under cmd/ is an application main, though. `warren g
// repository --driver postgres` writes cmd/migrate/main.go, and counting it
// made every subsequent `warren g module` in the project refuse — then
// suggest `--main cmd/migrate/main.go`, the one answer that cannot work,
// because the migration runner has no warren.New to register a module in.
// A main without that call is not a candidate, whoever wrote it.
func findMain(dir, named string) (string, error) {
	if named != "" {
		rel := filepath.ToSlash(named)
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			return "", diagnostic(fmt.Sprintf(
				"✗ no such file: %s\n\n    --main is a path relative to the project root.", rel))
		}
		return rel, nil
	}

	entries, err := os.ReadDir(filepath.Join(dir, "cmd"))
	if err != nil {
		return "", diagnostic("✗ no cmd directory\n\n    Cannot find the application's main package.\n\n" +
			"  Run `warren g module` from the root of a project scaffolded by `warren new`,\n" +
			"  or name the file with --main.")
	}
	var found, others []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("cmd", e.Name(), "main.go"))
		src, rerr := os.ReadFile(filepath.Join(dir, rel))
		if rerr != nil {
			continue
		}
		// The same matcher the edit itself runs, so a file this accepts is a
		// file AddCallArgument can write to.
		if astedit.HasCall(src, "warren.New") {
			found = append(found, rel)
		} else {
			others = append(others, rel)
		}
	}
	switch len(found) {
	case 0:
		if len(others) > 0 {
			slices.Sort(others)
			return "", diagnostic(fmt.Sprintf(
				"✗ no application main package\n\n      %s\n\n"+
					"    A module is registered as an argument to warren.New(...), and none\n"+
					"    of these files has that call — a migration runner or a one-shot\n"+
					"    tool is not somewhere a module can go.\n\n"+
					"  Name the application's main with --main, or scaffold one with\n"+
					"  `warren new`.",
				strings.Join(others, "\n      ")))
		}
		return "", diagnostic("✗ no main.go under cmd/\n\n    Cannot find the application's main package.\n\n" +
			"  Name it with --main if it lives elsewhere.")
	case 1:
		return found[0], nil
	}
	slices.Sort(found)
	return "", diagnostic(fmt.Sprintf(
		"✗ this project has %d main packages\n\n      %s\n\n"+
			"    A module is registered in one of them, and which one is a decision\n"+
			"    only you can make — wiring the first would leave the others short a\n"+
			"    module with nothing to say so.\n\n  Name it:\n\n      warren g module ... --main %s",
		len(found), strings.Join(found, "\n      "), found[0]))
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
// stem of the event name and the consumer topic.
//
// It keeps acronyms whole: HTTPServer is http_server, not h_t_t_p_server,
// and ID is id. That matters beyond aesthetics, because the result becomes
// a WIRE FORMAT — the event name and the topic other services subscribe to.
// aggregateOf returns the aggregate an event name belongs to, by dropping the
// event's final CamelCase word — its past participle. "InvoiceCreated" gives
// "Invoice", "PaymentReceived" gives "Payment".
//
// A single-word name has no verb to drop and is returned unchanged: the
// generated file is a starting point, and a wrong guess there is visible in
// the code the user is already reading. The failure that mattered was the
// invisible one — a key nothing publishes, decoding to "" for ever.
func aggregateOf(event string) string {
	words := camelWords(event)
	if len(words) < 2 {
		return event
	}
	return strings.Join(words[:len(words)-1], "")
}

// camelWords splits a CamelCase identifier into its words, keeping runs of
// upper-case letters together the way snake does ("InvoicePDFGenerated" is
// Invoice, PDF, Generated).
func camelWords(s string) []string {
	runes := []rune(s)
	var words []string
	start := 0
	for i, r := range runes {
		if i == 0 || !unicode.IsUpper(r) {
			continue
		}
		startsWord := !unicode.IsUpper(runes[i-1]) ||
			(i+1 < len(runes) && unicode.IsLower(runes[i+1]))
		if startsWord {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	return append(words, string(runes[start:]))
}

func snake(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if !unicode.IsUpper(r) {
			b.WriteRune(r)
			continue
		}
		// A separator goes before an upper-case rune only where a word
		// actually starts: after a non-upper rune, or at the end of a run of
		// upper-case runes that is followed by a lower-case one.
		startsWord := i > 0 && (!unicode.IsUpper(runes[i-1]) ||
			(i+1 < len(runes) && unicode.IsLower(runes[i+1])))
		if startsWord {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
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

// checkEntityExists refuses a repository for an aggregate that has not been
// generated. `warren g repository --help` already told the user to run
// `warren g entity` first; this is that sentence enforced.
//
// It looks for the DECLARATION rather than the file, so an entity a user
// hand-wrote into a differently-named file still counts — the check is about
// whether the code will compile, not about who wrote it.
func checkEntityExists(dir, base, module, name string) error {
	if err := checkNotDeclared(dir, base+"/domain", nil, name); err != nil {
		return nil // it IS declared: checkNotDeclared reports the collision
	}
	return diagnostic(fmt.Sprintf(
		"✗ no such entity: %q\n\n    Module %q has no %s aggregate in its domain layer.\n\n"+
			"    The repository stores that aggregate and implements the port the\n"+
			"    entity declares, so it could not compile. Nothing was written.\n\n"+
			"  Create it first:\n\n      warren g entity %s %s",
		name, module, name, module, name))
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

// errCannotRead and errCannotWrite dress a raw OS error as a diagnostic.
// Every other failure path prints a ✗ block; a bare "permission denied"
// with no path context is the one that makes people think the tool broke.
func errCannotRead(path string, cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ cannot read %s\n\n    %v\n\n  Nothing was written.", path, cause))
}

func errCannotWrite(path string, cause error) error {
	return diagnostic(fmt.Sprintf(
		"✗ cannot write %s\n\n    %v\n\n  Everything this run had written was undone.", path, cause))
}

func errModuleExists(name, base string) error {
	return diagnostic(fmt.Sprintf(
		"\u2717 the module %q already exists\n\n    %s/module.go is there, and it is the\n"+
			"    file every other generator wires into. Regenerating it would replace\n"+
			"    it with an empty declaration and discard every provider, consumer and\n"+
			"    export the module has — so --force does not apply here.\n\n"+
			"  To add to it:\n\n      warren g entity %s <Name>\n      warren g command %s <Name>\n\n"+
			"  To start it over, delete %s yourself first.",
		name, base, name, name, base))
}

func errMissingName(what string) error {
	return diagnostic(fmt.Sprintf("✗ missing %s\n\n    `warren g` needs a name to generate.", what))
}
