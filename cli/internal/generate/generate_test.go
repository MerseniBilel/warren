package generate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/cli/internal/generate"
	"github.com/MerseniBilel/warren/cli/internal/scaffold"
)

// app scaffolds a project to generate into — generators edit real files, so
// they are tested against a real tree.
func app(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	return dir
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

func TestGenerateEntity(t *testing.T) {
	t.Parallel()

	dir := app(t)
	_, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Order"})
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	src := read(t, dir, "internal/modules/user/domain/order.go")
	for _, want := range []string{
		"type OrderID string",
		"type Order struct",
		// Versioned by default. A generated aggregate whose repository blind-
		// upserts is a lost update waiting for its second concurrent request,
		// and the generator is where that default is set.
		"VersionedRoot[OrderID]",
		"func NewOrder(",
		// The load path is a DIFFERENT constructor: NewOrder raises
		// OrderCreated, so reconstituting through it republishes a creation
		// fact and loses the version.
		"func ReconstituteOrder(",
		"type OrderCreated struct",
		"type OrderRepository interface",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated entity is missing %q:\n%s", want, src)
		}
	}
	// An aggregate is not a provider: no module.go edit.
	if strings.Contains(read(t, dir, "internal/modules/user/module.go"), "Order") {
		t.Error("generating an entity edited module.go — an aggregate is not a provider")
	}
}

func TestGenerateCommandWiresItIn(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "user", Name: "SuspendUser"}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	src := read(t, dir, "internal/modules/user/application/suspend_user.go")
	for _, want := range []string{"type SuspendUser struct", "func NewSuspendUserHandler(", "app.Handler[SuspendUser,"} {
		if !strings.Contains(src, want) {
			t.Errorf("generated command is missing %q:\n%s", want, src)
		}
	}
	// The generator's real work: wiring it into the module.
	mod := read(t, dir, "internal/modules/user/module.go")
	if !strings.Contains(mod, "application.NewSuspendUserHandler") {
		t.Errorf("the handler was not wired into module.go:\n%s", mod)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/modules/user/application/suspend_user_test.go")); err != nil {
		t.Error("no test was generated — a generator that skips the test teaches skipping the test")
	}
}

func TestGenerateIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := app(t)
	opts := generate.Options{Dir: dir, Module: "user", Name: "SuspendUser"}
	if _, err := generate.Command(opts); err != nil {
		t.Fatalf("first: %v", err)
	}
	before := read(t, dir, "internal/modules/user/module.go")

	_, err := generate.Command(opts)
	if err == nil {
		t.Fatal("re-generating over existing files succeeded — it would clobber edits")
	}
	if !strings.Contains(err.Error(), "suspend_user.go") {
		t.Errorf("the diagnostic does not name the conflict:\n%v", err)
	}
	// And nothing changed: a refused generator leaves no trace.
	if read(t, dir, "internal/modules/user/module.go") != before {
		t.Error("a refused generator still edited module.go")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	dir := app(t)
	before := read(t, dir, "internal/modules/user/module.go")
	plan, err := generate.Command(generate.Options{Dir: dir, Module: "user", Name: "SuspendUser", DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "internal/modules/user/application/suspend_user.go")); statErr == nil {
		t.Error("--dry-run wrote a file")
	}
	if read(t, dir, "internal/modules/user/module.go") != before {
		t.Error("--dry-run edited module.go")
	}
	if !strings.Contains(plan, "suspend_user.go") || !strings.Contains(plan, "module.go") {
		t.Errorf("the plan does not list what would happen:\n%s", plan)
	}
}

func TestGenerateRepository(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
		t.Fatalf("Repository: %v", err)
	}
	src := read(t, dir, "internal/modules/user/infrastructure/order_repository.go")
	if !strings.Contains(src, "func NewOrderRepository(") {
		t.Errorf("generated repository:\n%s", src)
	}
	if !strings.Contains(read(t, dir, "internal/modules/user/module.go"), "infrastructure.NewOrderRepository") {
		t.Error("the repository was not wired into module.go")
	}
}

// TestASecondPostgresRepositoryNumbersItsMigration — every aggregate's
// migration was written as db/migrations/00001_<plural>.sql, so a second one
// collided at version 00001. GETTING_STARTED tells the user files apply in
// NAME order and to zero-pad them; two files at 00001 then order
// alphabetically, and "invoices" applies before "reservations" — wrong for
// any foreign key between them.
func TestASecondPostgresRepositoryNumbersItsMigration(t *testing.T) {
	t.Parallel()

	dir := app(t)
	for _, name := range []string{"Reservation", "Invoice"} {
		if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: name}); err != nil {
			t.Fatalf("Entity %s: %v", name, err)
		}
		if _, err := generate.Repository(generate.Options{
			Dir: dir, Module: "user", Name: name, Driver: "postgres",
		}); err != nil {
			t.Fatalf("Repository %s: %v", name, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, "db/migrations"))
	if err != nil {
		t.Fatalf("reading migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		// schema.go lives here too — it is the //go:embed that compiles these
		// files into cmd/migrate, so the deploy binary runs from anywhere.
		// nextMigration already skips anything that is not .sql; so does this.
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) != 2 {
		t.Fatalf("migrations = %v, want one per aggregate", names)
	}
	if names[0] == names[1] {
		t.Fatalf("both aggregates wrote the same file: %v", names)
	}
	// Distinct, ordered, and in generation order — the order the FKs need.
	if !strings.HasPrefix(names[0], "00001_reservations") {
		t.Errorf("first migration = %q, want 00001_reservations", names[0])
	}
	if !strings.HasPrefix(names[1], "00002_invoices") {
		t.Errorf("second migration = %q, want 00002_invoices — a second 00001 makes apply order alphabetical", names[1])
	}
}

// TestASecondPostgresRepositoryDoesNotFightOverCmdMigrate — cmd/migrate/main.go
// is a PROJECT-level file the generator itself wrote. Listing it as a conflict
// made the second aggregate fail with "these files already exist … delete
// them, or pass --force", where deleting means deleting the shared migrate
// command and --force silently overwrites hand-edited migrations.
func TestASecondPostgresRepositoryDoesNotFightOverCmdMigrate(t *testing.T) {
	t.Parallel()

	dir := app(t)
	for _, name := range []string{"Reservation", "Invoice"} {
		if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: name}); err != nil {
			t.Fatalf("Entity %s: %v", name, err)
		}
		if _, err := generate.Repository(generate.Options{
			Dir: dir, Module: "user", Name: name, Driver: "postgres",
		}); err != nil {
			t.Fatalf("a second postgres repository was refused over a file the generator itself wrote: %v", err)
		}
	}
}

func TestGenerateModule(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "billing"}); err != nil {
		t.Fatalf("Module: %v", err)
	}
	if !strings.Contains(read(t, dir, "internal/modules/billing/module.go"), `warren.NewModule("billing"`) {
		t.Error("the module was not created")
	}
	// The layers exist in git so lint arch can see them.
	for _, layer := range []string{"domain", "application", "infrastructure"} {
		if _, err := os.Stat(filepath.Join(dir, "internal/modules/billing", layer, "doc.go")); err != nil {
			t.Errorf("layer %s was not created: %v", layer, err)
		}
	}
	// And it is wired into main.
	if !strings.Contains(read(t, dir, "cmd/myapp/main.go"), "billing.Module()") {
		t.Error("the module was not wired into main.go")
	}
}

func TestUnknownModuleIsANamedError(t *testing.T) {
	t.Parallel()

	_, err := generate.Command(generate.Options{Dir: app(t), Module: "nope", Name: "X"})
	if err == nil {
		t.Fatal("generating into a module that does not exist succeeded")
	}
	for _, want := range []string{"nope", "warren g module"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic is missing %q:\n%v", want, err)
		}
	}
}

func TestGenerateConsumer(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "user", Name: "OrderPlaced"}); err != nil {
		t.Fatalf("Consumer: %v", err)
	}

	// The handler is a use case like any other: application layer, no
	// transport import, its own local view of the event.
	handler := read(t, dir, "internal/modules/user/application/on_order_placed.go")
	for _, want := range []string{"type OrderPlaced struct", "func NewOnOrderPlacedHandler("} {
		if !strings.Contains(handler, want) {
			t.Errorf("generated consumer handler is missing %q:\n%s", want, handler)
		}
	}
	if strings.Contains(handler, "warren/broker") {
		t.Error("the handler imports the broker — a consumer handler must not know its transport")
	}

	// The consumer is the plumbing, and it belongs in the module: it is the
	// only file allowed to see both the transport package and the handler.
	sub := read(t, dir, "internal/modules/user/on_order_placed_consumer.go")
	if !strings.Contains(sub, `r.OnEvent("order.placed", c.handle)`) {
		t.Errorf("the topic was not derived from the event name:\n%s", sub)
	}

	// Consumers, not Providers plus Eager. Amended 2026-09-02: this block
	// used to assert the opposite, because the generated subscription wired
	// its own broker.Pipeline and lifecycle hook and so was not a
	// transport.Controller. It is one now, and the two generators finally
	// emit the same idiom — field test #15 found one project containing both.
	mod := read(t, dir, "internal/modules/user/module.go")
	for _, unwanted := range []string{"warren.Eager", "Subscription"} {
		if strings.Contains(mod, unwanted) {
			t.Errorf("module.go still carries the superseded %q idiom:\n%s", unwanted, mod)
		}
	}
	for _, want := range []string{
		"application.NewOnOrderPlacedHandler",
		"warren.Consumers(",
		"newOrderPlacedConsumer,",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("module.go is missing %q:\n%s", want, mod)
		}
	}

	// The consumer must not hand-roll what the framework assembles. Every
	// one of these was in the 82-line template it replaces.
	for _, unwanted := range []string{"broker.Pipeline", "lifecycle.Hook", "json.Unmarshal", "sub.Subscribe"} {
		if strings.Contains(sub, unwanted) {
			t.Errorf("the generated consumer still hand-assembles %q:\n%s", unwanted, sub)
		}
	}
}

// TestConsumerDecodesWhatTheEntityGeneratorPublishes — the two generators
// disagreed about the wire format of the SAME event, and did so silently.
//
// `g entity billing Invoice` emits an event marshalling to
// {"invoice_id":...,"at":...}; `g consumer audit InvoiceCreated` produced a
// struct reading `json:"aggregate_id"`. That key is never present in anything
// Warren publishes, so the field decoded to "" on every delivery, nothing
// errored, and a field test observed a live consumer receiving {'aggregate_id': ”}.
// Two generators of one CLI, for one event, run in the documented order.
func TestConsumerDecodesWhatTheEntityGeneratorPublishes(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Invoice"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "user", Name: "InvoiceCreated"}); err != nil {
		t.Fatalf("Consumer: %v", err)
	}

	// The key the entity's event actually marshals to.
	entity := read(t, dir, "internal/modules/user/domain/invoice.go")
	if !strings.Contains(entity, "`json:\"invoice_id\"`") {
		t.Fatalf("the entity generator no longer emits invoice_id; this test's premise is stale:\n%s", entity)
	}

	consumer := read(t, dir, "internal/modules/user/application/on_invoice_created.go")
	if !strings.Contains(consumer, "`json:\"invoice_id\"`") {
		t.Errorf("the consumer does not decode the key the entity publishes:\n%s", consumer)
	}
	if strings.Contains(consumer, "aggregate_id") {
		t.Errorf("the consumer still reads aggregate_id, which nothing publishes:\n%s", consumer)
	}
}

// TestConsumerRefusesAPayloadItCouldNotRead — deriving the right key is a
// convention, and conventions break: a consumer wired to an event from
// another system will not match. What must NOT happen again is the silence.
// An empty identifier means the payload was not what this handler expects,
// and saying so routes the message to the DLQ with a readable cause instead
// of acking a no-op.
func TestConsumerRefusesAPayloadItCouldNotRead(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "user", Name: "InvoiceCreated"}); err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	src := read(t, dir, "internal/modules/user/application/on_invoice_created.go")
	if !strings.Contains(src, "errors.Invalid") {
		t.Errorf("a consumer that decoded nothing still acks silently:\n%s", src)
	}
}

// TestConsumerLogsWithTheContext — `.Info(` drops the correlation ID that
// `.InfoContext(ctx,` carries. log/log.go, GETTING_STARTED §7b and warren.md
// §7.1 each warn about exactly this, and the generator shipped the broken
// form, so every generated consumer had its correlation ID disabled.
func TestConsumerLogsWithTheContext(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "user", Name: "InvoiceCreated"}); err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	src := read(t, dir, "internal/modules/user/application/on_invoice_created.go")
	if !strings.Contains(src, "InfoContext(ctx") {
		t.Errorf("the generated consumer does not log through the context, so it has no correlation ID:\n%s", src)
	}
}

func TestConsumerTopicIsOverridable(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Consumer(generate.Options{
		Dir: dir, Module: "user", Name: "OrderPlaced", Topic: "billing.order.placed",
	}); err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	if !strings.Contains(read(t, dir, "internal/modules/user/on_order_placed_consumer.go"),
		`r.OnEvent("billing.order.placed", c.handle)`) {
		t.Error("--topic was ignored")
	}
}

// TestGeneratedModuleHasABootTest guards the only wiring mistake Warren
// cannot catch at compile time: a constructor asking the container for
// something no module exports. That is a boot error, so a generated module
// ships with a test that boots it — and the compile gate then runs it.
func TestGeneratedModuleHasABootTest(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "billing"}); err != nil {
		t.Fatalf("Module: %v", err)
	}
	src := read(t, dir, "internal/modules/billing/module_test.go")
	if !strings.Contains(src, "warrentest.NewModuleTest") {
		t.Errorf("the generated module has no boot test:\n%s", src)
	}
	// The env prefix is read out of the scaffolded platform module, not
	// guessed: a boot test that cannot load config fails for the wrong
	// reason and teaches people to delete it.
	if !strings.Contains(src, `"MYAPP_NAME"`) {
		t.Errorf("the boot test does not set the app's own env prefix:\n%s", src)
	}
}

// TestForceOverwritesWithoutDuplicatingWiring covers the combination that
// actually bites: --force rewrites the files, but the module.go it edits is
// NOT a new file, so a naive implementation appends the same provider a
// second time and the module stops booting.
func TestForceOverwritesWithoutDuplicatingWiring(t *testing.T) {
	t.Parallel()

	dir := app(t)
	opts := generate.Options{Dir: dir, Module: "user", Name: "SuspendUser"}
	if _, err := generate.Command(opts); err != nil {
		t.Fatalf("first: %v", err)
	}

	// A hand edit, to prove --force really does overwrite the file.
	handler := filepath.Join(dir, "internal/modules/user/application/suspend_user.go")
	if err := os.WriteFile(handler, []byte("package application\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts.Force = true
	if _, err := generate.Command(opts); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if !strings.Contains(read(t, dir, "internal/modules/user/application/suspend_user.go"), "func NewSuspendUserHandler(") {
		t.Error("--force did not overwrite the file")
	}

	mod := read(t, dir, "internal/modules/user/module.go")
	if n := strings.Count(mod, "application.NewSuspendUserHandler"); n != 1 {
		t.Errorf("the handler is provided %d times, want 1 — a duplicate provider fails the boot:\n%s", n, mod)
	}
	if n := strings.Count(mod, `"example.com/myapp/internal/modules/user/application"`); n != 1 {
		t.Errorf("the application layer is imported %d times, want 1:\n%s", n, mod)
	}
}

// TestCommandWiresItsOwnRoute — `warren g command` used to end with a
// twelve-line patch: add a field, add a constructor parameter, add a route,
// all by hand, in internal/modules/<f>/controller.go — a file no generator
// created. Four commands meant four patches, and a handler nobody patched
// in was wired into the container and reachable by nothing. The field test
// called it the single biggest gap in the generator set.
func TestCommandWiresItsOwnRoute(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "catalog"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "catalog", Name: "CreateProduct"}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	got := read(t, dir, "internal/modules/catalog/controller.go")
	for _, want := range []string{
		"createProduct",
		"app.Handler[application.CreateProduct, application.CreateProductResult]",
		// Create<X> derives the resource path; see TestCommandRouteIsResourceShaped.
		`r.Post("/products", c.createProduct)`,
		"/internal/modules/catalog/application",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("controller.go is missing %q:\n%s", want, got)
		}
	}

	// A second command joins the first rather than replacing it.
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "catalog", Name: "Discontinue"}); err != nil {
		t.Fatalf("second g command: %v", err)
	}
	got = read(t, dir, "internal/modules/catalog/controller.go")
	for _, want := range []string{"createProduct", "discontinue", `c.discontinue)`} {
		if !strings.Contains(got, want) {
			t.Errorf("after the second command, controller.go is missing %q:\n%s", want, got)
		}
	}

	// And the plan no longer tells the user to do it themselves.
	plan, err := generate.Command(generate.Options{Dir: dir, Module: "catalog", Name: "Restock"})
	if err != nil {
		t.Fatalf("third g command: %v", err)
	}
	if strings.Contains(plan, "Still to do") {
		t.Errorf("the plan still asks the user to wire the controller by hand:\n%s", plan)
	}
	if !strings.Contains(plan, "controller.go") {
		t.Errorf("the plan does not mention the controller edit it made:\n%s", plan)
	}
}

// TestMigrateMainDoesNotMakeTheProjectAmbiguous — `warren g repository
// --driver postgres` writes cmd/migrate/main.go, and from that moment every
// `warren g module` in the project refused, because two directories under
// cmd/ held a main.go. The refusal then suggested
//
//	warren g module ... --main cmd/migrate/main.go
//
// which is the WRONG main: the migration runner registers no modules and has
// no warren.New to add one to. So the generator refused correct usage and
// then pointed at the one answer that cannot work.
//
// A module is registered in a warren.New(...) call. A main without one is
// not a candidate, whoever wrote it.
func TestMigrateMainDoesNotMakeTheProjectAmbiguous(t *testing.T) {
	t.Parallel()

	dir := app(t)
	// Exactly what `g repository --driver postgres` leaves behind.
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Invoice"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Invoice", Driver: "postgres",
	}); err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cmd/migrate/main.go")); err != nil {
		t.Fatalf("the postgres repository generator no longer writes cmd/migrate/main.go: %v", err)
	}

	if _, err := generate.Module(generate.Options{Dir: dir, Name: "billing"}); err != nil {
		t.Fatalf("g module refused a project that has a migration runner: %v", err)
	}
	main := read(t, dir, "cmd/myapp/main.go")
	if !strings.Contains(main, "billing.Module()") {
		t.Errorf("the module was not registered in the application's main:\n%s", main)
	}
	if strings.Contains(read(t, dir, "cmd/migrate/main.go"), "billing") {
		t.Error("the module was registered in the migration runner")
	}
}

// TestTwoRealMainsAreStillAmbiguous — the refusal is right when the choice is
// genuinely the author's, and then every option it lists must be one that
// works.
func TestTwoRealMainsAreStillAmbiguous(t *testing.T) {
	t.Parallel()

	dir := app(t)
	second := filepath.Join(dir, "cmd", "worker")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A second REAL application main: it registers modules.
	src := "package main\n\nimport \"github.com/MerseniBilel/warren\"\n\nfunc main() {\n\t_ = warren.New().Run()\n}\n"
	if err := os.WriteFile(filepath.Join(second, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("writing second main: %v", err)
	}

	_, err := generate.Module(generate.Options{Dir: dir, Name: "billing"})
	if err == nil {
		t.Fatal("two application mains were not reported as ambiguous")
	}
	got := err.Error()
	if !strings.Contains(got, "cmd/myapp/main.go") || !strings.Contains(got, "cmd/worker/main.go") {
		t.Errorf("the refusal does not list both candidates:\n%s", got)
	}
	if strings.Contains(got, "cmd/migrate") {
		t.Errorf("a non-application main was listed as a candidate:\n%s", got)
	}
}

// TestCommandRouteIsResourceShaped — field test #4, section D1. `warren g
// command shipment CreateShipment` registered POST /create_shipment: an
// RPC-shaped path in an otherwise REST-shaped scaffold. The tester rewrote
// all five by hand.
//
// Create<X> is the one derivation that is safe to make automatically: the
// method does not change, no path parameter appears, and the request is
// still decoded from the body — so the generated handler keeps working.
// Anything else keeps the literal, predictable form.
func TestCommandRouteIsResourceShaped(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "user", Name: "CreateShipment"}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	src := read(t, dir, "internal/modules/user/controller.go")
	if !strings.Contains(src, `r.Post("/shipments"`) {
		t.Errorf("CreateShipment did not produce a resource-shaped route:\n%s", src)
	}
	if strings.Contains(src, "/create_shipment") {
		t.Errorf("the RPC-shaped path survived:\n%s", src)
	}
}

// TestCommandRouteFlagWins — the explicit answer, for every shape the
// derivation deliberately does not guess at.
func TestCommandRouteFlagWins(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "user", Name: "SuspendUser", Route: "/users/{id}/suspend",
	}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	src := read(t, dir, "internal/modules/user/controller.go")
	if !strings.Contains(src, `r.Post("/users/{id}/suspend"`) {
		t.Errorf("--route was not used:\n%s", src)
	}
}

// TestAnUnrecognisedVerbKeepsTheLiteralPath — the derivation must not guess.
// SuspendUser has no REST shape anyone could infer, and inventing one would
// be worse than the predictable form.
func TestAnUnrecognisedVerbKeepsTheLiteralPath(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{Dir: dir, Module: "user", Name: "SuspendUser"}); err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !strings.Contains(read(t, dir, "internal/modules/user/controller.go"), `r.Post("/suspend_user"`) {
		t.Error("an unrecognised verb did not keep its literal path")
	}
}

// --- field test #5 ----------------------------------------------------------

// TestConsumerDerivesTheAggregateAcrossAParticle — field test #5, defect 4.
// `warren g consumer notices CopyCheckedOut` generated
//
//	CopyCheckedID string `json:"copy_checked_id"`
//
// against an event whose payload is {"copy_id":...}. Every real message
// dead-lettered: "the message carried no copy_checked_id".
//
// aggregateOf dropped ONE camel word, which is right for InvoiceCreated and
// wrong for every two-word past participle — CheckedOut, SignedUp, ShippedOut,
// RolledBack, PaidOff. The particle belongs to the verb, not to the aggregate.
func TestConsumerDerivesTheAggregateAcrossAParticle(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ event, wantField, wantJSON string }{
		{"CopyCheckedOut", "CopyID", "copy_id"},
		{"UserSignedUp", "UserID", "user_id"},
		{"PaymentRolledBack", "PaymentID", "payment_id"},
		{"InvoiceCreated", "InvoiceID", "invoice_id"},
		{"InvoicePDFGenerated", "InvoicePDFID", "invoice_pdf_id"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			t.Parallel()
			dir := app(t)
			if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "user", Name: tc.event}); err != nil {
				t.Fatalf("Consumer: %v", err)
			}
			src := read(t, dir, "internal/modules/user/application/on_"+snakeOf(tc.event)+".go")
			if !strings.Contains(src, tc.wantField+" string") {
				t.Errorf("%s: no field %q:\n%s", tc.event, tc.wantField, src)
			}
			if !strings.Contains(src, `json:"`+tc.wantJSON+`"`) {
				t.Errorf("%s: json key is not %q:\n%s", tc.event, tc.wantJSON, src)
			}
		})
	}
}

// TestPostgresRepositoryImportsTheAdapterModule — field test #5, defect 3.
// `warren g repository --driver postgres` writes a constructor taking
// postgres.DB and provides it, but only ever added platform.Module() to
// Imports — never platform.Postgres(), which is the module that exports
// postgres.DB. So every feature added to a --db postgres project built fine
// and then failed the boot:
//
//	✗ cannot resolve dependency
//	    postgres.DB └─ required by domain.BookRepository
//
// It happened on every module the tester created.
func TestPostgresRepositoryImportsTheAdapterModule(t *testing.T) {
	t.Parallel()

	dir := pgApp(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "catalog"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "catalog", Name: "Book"}); err != nil {
		t.Fatalf("g entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{
		Dir: dir, Module: "catalog", Name: "Book", Driver: "postgres",
	}); err != nil {
		t.Fatalf("g repository: %v", err)
	}
	src := read(t, dir, "internal/modules/catalog/module.go")
	if !strings.Contains(src, "platform.Postgres()") {
		t.Errorf("the module does not import the adapter that exports postgres.DB:\n%s", src)
	}
}

// TestConsumerImportsTheAdapterModule — same defect, inbox.Store instead of
// postgres.DB.
func TestConsumerImportsTheAdapterModule(t *testing.T) {
	t.Parallel()

	dir := pgApp(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "notices"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "notices", Name: "BookCreated"}); err != nil {
		t.Fatalf("g consumer: %v", err)
	}
	if src := read(t, dir, "internal/modules/notices/module.go"); !strings.Contains(src, "platform.Postgres()") {
		t.Errorf("the consumer's module does not import the adapter that exports inbox.Store:\n%s", src)
	}
}

// TestMemoryProjectDoesNotImportPostgres — the addition must be driven by
// what the PROJECT has, not by the generator guessing.
func TestMemoryProjectDoesNotImportPostgres(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "catalog"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "catalog", Name: "BookCreated"}); err != nil {
		t.Fatalf("g consumer: %v", err)
	}
	if src := read(t, dir, "internal/modules/catalog/module.go"); strings.Contains(src, "platform.Postgres") {
		t.Errorf("a memory project imported a postgres module that does not exist:\n%s", src)
	}
}

// TestForceDoesNotAddASecondRoute — field test #5, defect 6, a regression
// from --route. Re-running a --route'd command WITHOUT the flag re-derived
// the path from the name and appended it, leaving a second, un-asked-for
// public endpoint:
//
//	r.Post("/copies/{id}/checkout", c.checkoutCopy)
//	r.Post("/checkout_copy", c.checkoutCopy)      ← new
//
// A route is identified by the HANDLER it serves, not by its path.
func TestForceDoesNotAddASecondRoute(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "user", Name: "CheckoutCopy", Route: "/copies/{id}/checkout",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "user", Name: "CheckoutCopy", Force: true,
	}); err != nil {
		t.Fatalf("second --force: %v", err)
	}
	src := read(t, dir, "internal/modules/user/controller.go")
	if n := strings.Count(src, "c.checkoutCopy)"); n != 1 {
		t.Errorf("the handler is registered %d times, want 1:\n%s", n, src)
	}
	if strings.Contains(src, "/checkout_copy") {
		t.Errorf("--force added a route nobody asked for:\n%s", src)
	}
}

// TestReservedModuleNameIsRefused — field test #5, defect 5. `warren g module
// platform` wrote a module named "platform", a duplicate of the scaffold's
// own, added the import to main.go but NOT the registration, and reported no
// main.go edit at all. The project then did not compile, with no Warren
// diagnostic anywhere.
func TestReservedModuleNameIsRefused(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"platform", "config"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := app(t)
			_, err := generate.Module(generate.Options{Dir: dir, Name: name})
			if err == nil {
				t.Fatalf("g module %s was accepted; it collides with the scaffold's own package", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name the collision:\n%v", err)
			}
		})
	}
}

// pgApp scaffolds a --db postgres project to generate into.
func pgApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0", DB: "postgres",
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	return dir
}

// kafkaApp scaffolds a project whose broker is a MODULE rather than a set of
// providers platform re-exports — the shape that made the generator's
// adapter import wrong.
func kafkaApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := scaffold.New(scaffold.Options{
		Dir: dir, Name: "myapp", ModulePath: "example.com/myapp", Version: "v0.1.0",
		DB: "postgres", Broker: "kafka",
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	return dir
}

// snakeOf mirrors the generator's own file naming for the test's file lookup.
func snakeOf(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := rune(s[i-1])
			next := rune(0)
			if i+1 < len(s) {
				next = rune(s[i+1])
			}
			if prev < 'A' || prev > 'Z' || (next >= 'a' && next <= 'z') {
				b.WriteByte('_')
			}
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// TestARouteWildcardBecomesAParamField — the generator was handed the route
// and still wrote `json:"id"`, so the path segment bound nothing and the
// handler acted on the BODY. Field test #7, against real Postgres:
//
//	POST /tickets/A/assign  body {"id":B}  →  201 Created, and B was assigned.
//
// The URL is what a reverse proxy, an audit log and a rate limiter see; the
// body is what acted. Any authorization keyed on the path — including
// GETTING_STARTED's own sameTenant policy, which reads p.Path("tenant") —
// authorizes one resource and mutates another. Compiles, runs, 201, wrong.
func TestARouteWildcardBecomesAParamField(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "ticket"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "ticket", Name: "AssignTicket",
		Route: "/tickets/{id}/assign",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	got := read(t, dir, "internal/modules/ticket/application/assign_ticket.go")
	if !strings.Contains(got, "`param:\"id\"") {
		t.Errorf("the {id} wildcard did not become a param field:\n%s", got)
	}
	// Only the REQUEST type: the result's `json:"id"` is a response body and
	// belongs there.
	req := structBody(t, got, "type AssignTicket struct {")
	if strings.Contains(req, "json:\"") {
		t.Errorf("the command still reads a field from the BODY:\n%s", req)
	}
}

// structBody returns the text between a struct header and its closing brace.
func structBody(t *testing.T, src, header string) string {
	t.Helper()
	i := strings.Index(src, header)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", header, src)
	}
	rest := src[i+len(header):]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("unterminated struct after %q", header)
	}
	return rest[:end]
}

// TestEveryWildcardIsBound — a route may carry more than one, and an unbound
// one is exactly as silent as the first.
func TestEveryWildcardIsBound(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "billing"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "billing", Name: "VoidLine",
		Route: "/tenants/{tenantId}/invoices/{invoiceId}/lines/{lineId}",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	got := read(t, dir, "internal/modules/billing/application/void_line.go")
	for _, want := range []string{
		"TenantID string `param:\"tenantId\"",
		"InvoiceID string `param:\"invoiceId\"",
		"LineID string `param:\"lineId\"",
	} {
		if !strings.Contains(strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestARouteWithNoWildcardStillTakesABody — the common case must not regress
// into a command with no fields at all.
func TestARouteWithNoWildcardStillTakesABody(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "catalog"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "catalog", Name: "CreateProduct",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	got := read(t, dir, "internal/modules/catalog/application/create_product.go")
	if !strings.Contains(got, "`json:\"id\"") {
		t.Errorf("a body-only command lost its json field:\n%s", got)
	}
}

// TestCommandMethodSelectsTheVerb — every route the generator wrote was a
// POST returning 201, so `warren g command ticket GetTicket --route
// "/tickets/{id}"` produced a READ served as POST/201. Field test #7 hand-
// edited every route it generated.
func TestCommandMethodSelectsTheVerb(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "ticket"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "ticket", Name: "GetTicket",
		Route: "/tickets/{id}", Method: "get",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	got := read(t, dir, "internal/modules/ticket/controller.go")
	if !strings.Contains(got, `r.Get("/tickets/{id}", c.getTicket)`) {
		t.Errorf("the route was not registered as a GET:\n%s", got)
	}
}

// TestCommandMethodDefaultsToPost — a command is a write, and the flag is
// for the exception.
func TestCommandMethodDefaultsToPost(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "user", Name: "SuspendUser",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}
	if !strings.Contains(read(t, dir, "internal/modules/user/controller.go"), "r.Post(") {
		t.Error("the default verb is no longer POST")
	}
}

// TestAnUnknownMethodIsRefusedWithTheList — a typo must not silently become
// a POST, and it must not become transport.Delte(r, ...) either.
func TestAnUnknownMethodIsRefusedWithTheList(t *testing.T) {
	t.Parallel()

	dir := app(t)
	_, err := generate.Command(generate.Options{
		Dir: dir, Module: "user", Name: "SuspendUser", Method: "fetch",
	})
	if err == nil {
		t.Fatal("an unknown --method was accepted")
	}
	for _, want := range []string{"fetch", "get", "post", "put", "patch", "delete"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%v", want, err)
		}
	}
}

// TestConsumerImportsTheBrokerAdapterModule — field test #8, defect 1. The
// SAME defect as TestConsumerImportsTheAdapterModule above, on the broker
// side, and the fix for the postgres side was never carried across.
//
// With --broker kafka the driver is a MODULE (platform.Broker()) rather than
// providers platform re-exports, because a module may export only what its
// own providers return. The scaffold's own notification/module.go gets this
// right; the generator did not, so every generated consumer built cleanly
// and then failed its boot:
//
//	✗ cannot resolve dependency
//	    broker.Publisher └─ required by *orders.orderPlacedSubscription
//
// It happened on every consumer the tester generated.
func TestConsumerImportsTheBrokerAdapterModule(t *testing.T) {
	t.Parallel()

	dir := kafkaApp(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "orders"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "orders", Name: "OrderPlaced"}); err != nil {
		t.Fatalf("g consumer: %v", err)
	}
	src := read(t, dir, "internal/modules/orders/module.go")
	if !strings.Contains(src, "platform.Broker()") {
		t.Errorf("the consumer's module does not import the adapter that exports broker.Publisher:\n%s", src)
	}
	// And the scaffold's own consumer module is the reference for what this
	// should look like — the two must not disagree in one project.
	ref := read(t, dir, "internal/modules/notification/module.go")
	if !strings.Contains(ref, "platform.Broker()") {
		t.Fatalf("the scaffold's own consumer module changed shape; this test's premise is stale:\n%s", ref)
	}
}

// TestMemoryBrokerProjectImportsBrokerToo — the two drivers used to have
// different SHAPES: kafka was a module behind platform.Broker(), memory was
// three providers platform re-exported. A generated consumer therefore had to
// import platform.Broker() in one project and must not in the other, and the
// generator knew about only one of the two shapes — field test #8.
//
// Both are modules now, because serving r.OnEvent requires one: a module is
// what can claim transport.ProtocolEvent. So platform.Broker() exists in
// every project and a consumer imports it either way. The generator needs no
// branch, because it looks for the DECLARATION rather than guessing.
func TestMemoryBrokerProjectImportsBrokerToo(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "orders"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Consumer(generate.Options{Dir: dir, Module: "orders", Name: "OrderPlaced"}); err != nil {
		t.Fatalf("g consumer: %v", err)
	}
	if src := read(t, dir, "internal/modules/orders/module.go"); !strings.Contains(src, "platform.Broker()") {
		t.Errorf("a consumer was generated without importing the module that serves it:\n%s", src)
	}
}

// TestAReadIsNotWrappedInATransaction — field test #8, defect 7. Every
// generated command wrapped its handler in app.Transactional, including the
// ones generated as reads. Measured against real Postgres with
// log_statement=all, 20 GETs:
//
//	AS GENERATED (Transactional GET):   BEGIN statements: 22
//	WITHOUT Transactional:              BEGIN statements: 1
//
// A real BEGIN/COMMIT per read, with no flag to opt out — and the generated
// comment said "so the rows it writes ... commit together" on a handler that
// writes nothing.
func TestAReadIsNotWrappedInATransaction(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "ticket"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "ticket", Name: "GetTicket",
		Route: "/tickets/{id}", Method: "get",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	src := read(t, dir, "internal/modules/ticket/application/get_ticket.go")
	// The CALL, not the word: the generated comment explains why there is no
	// transaction, and naming it there is the point.
	if strings.Contains(src, "app.Transactional[") {
		t.Errorf("a read was wrapped in a transaction:\n%s", src)
	}
	if strings.Contains(src, "app.UnitOfWork") {
		t.Errorf("a read still injects a unit of work it cannot need:\n%s", src)
	}
	// And its test must not reference the double that no longer exists.
	tst := read(t, dir, "internal/modules/ticket/application/get_ticket_test.go")
	if strings.Contains(tst, "UoW") {
		t.Errorf("the generated test still builds a unit-of-work double:\n%s", tst)
	}
}

// TestAWriteIsStillTransactional — the control. Every other verb writes, and
// the transaction is what makes its rows and the outbox rows announcing them
// commit together.
func TestAWriteIsStillTransactional(t *testing.T) {
	t.Parallel()

	dir := app(t)
	for _, method := range []string{"", "post", "put", "patch", "delete"} {
		name := "Do" + strings.ToUpper(method) + "Thing"
		if method == "" {
			name = "DoDefaultThing"
		}
		if _, err := generate.Command(generate.Options{
			Dir: dir, Module: "user", Name: name, Method: method,
		}); err != nil {
			t.Fatalf("g command --method %q: %v", method, err)
		}
		src := read(t, dir, "internal/modules/user/application/"+snakeOf(name)+".go")
		if !strings.Contains(src, "app.Transactional") {
			t.Errorf("--method %q lost its transaction:\n%s", method, src)
		}
	}
}

// TestBothRepositoryDriversOfferTheSameSurface — field test #8, defect 11.
// The Postgres template implemented Delete and the in-memory one did not,
// while the port declares neither — so swapping drivers silently changed
// what the repository could do, and a project that had widened its port for
// Delete stopped compiling on the driver that lacked it.
func TestBothRepositoryDriversOfferTheSameSurface(t *testing.T) {
	t.Parallel()

	methods := func(driver string) []string {
		dir := app(t)
		if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
			t.Fatalf("g entity: %v", err)
		}
		if _, err := generate.Repository(generate.Options{
			Dir: dir, Module: "user", Name: "Order", Driver: driver,
		}); err != nil {
			t.Fatalf("g repository --driver %s: %v", driver, err)
		}
		src := read(t, dir, repoFile("order", driver, ".go"))
		var found []string
		for _, m := range []string{"FindByID", "Save", "Delete"} {
			if strings.Contains(src, ") "+m+"(ctx context.Context") {
				found = append(found, m)
			}
		}
		return found
	}

	mem, pg := methods("memory"), methods("postgres")
	if strings.Join(mem, ",") != strings.Join(pg, ",") {
		t.Errorf("the drivers differ: memory has %v, postgres has %v", mem, pg)
	}
	if len(mem) != 3 {
		t.Errorf("methods = %v, want FindByID, Save and Delete", mem)
	}
}

// TestGeneratedDeleteEnlistsItsAggregate — the generated Delete used to call
// postgres.RequireTx, which checks for a transaction and nothing else, so the
// aggregate's events were never enlisted. Measured against real Postgres:
// load, Raise(Closed{}), Delete inside uow.Do published ONE outbox row where
// persistence.Write around the same DELETE published two. The row went, the
// request answered 204, and the fact evaporated with no error, no outbox row
// and no log line.
//
// The assertions are on the CALL, never on a word in a comment: this project
// has twice paid for a test that passed because a template's prose mentioned
// the thing it was not doing.
func TestGeneratedDeleteEnlistsItsAggregate(t *testing.T) {
	t.Parallel()

	for _, driver := range []string{"memory", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			t.Parallel()
			dir := app(t)
			if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
				t.Fatalf("Entity: %v", err)
			}
			if _, err := generate.Repository(generate.Options{
				Dir: dir, Module: "user", Name: "Order", Driver: driver,
			}); err != nil {
				t.Fatalf("Repository: %v", err)
			}
			src := read(t, dir, repoFile("order", driver, ".go"))

			// The root, not an id. Load-then-delete inside Delete cannot fix
			// this: the pending events live on the CALLER's instance, so a
			// re-loaded aggregate is a different object with zero events.
			repoType := "OrderRepository"
			if driver == "postgres" {
				repoType = "OrderPostgresRepository"
			}
			if !strings.Contains(src, "func (r *"+repoType+") Delete(ctx context.Context, order *domain.Order) error") {
				t.Errorf("Delete does not take the aggregate root:\n%s", src)
			}
			// The memory driver delegates to persistence.MemoryRepository,
			// whose Delete Tracks — the same shape its Save already has. The
			// Postgres one makes the write itself, so it is the one that has
			// to carry persistence.Write.
			if driver == "postgres" && !strings.Contains(src, `persistence.Write(ctx, "delete order", order,`) {
				t.Errorf("Delete does not go through persistence.Write:\n%s", src)
			}
			if driver == "memory" && !strings.Contains(src, "return r.inner.Delete(ctx, order)") {
				t.Errorf("Delete does not hand the aggregate to the enlisting driver:\n%s", src)
			}
			// RequireTx appears nowhere else in either template, so this is a
			// call-level assertion and not a comment-level one.
			if strings.Contains(src, "postgres.RequireTx") {
				t.Errorf("Delete still uses postgres.RequireTx, which makes the transaction check and drops the enlistment:\n%s", src)
			}
			// The comment that told the user to declare the port method has
			// to agree with the method that was generated.
			if strings.Contains(src, "Delete(ctx context.Context, id "+"OrderID) error") {
				t.Errorf("the port-declaration comment still names the id-shaped Delete:\n%s", src)
			}
		})
	}
}

// TestGeneratedRepositoryHasAContractTest — `go test ./...` was green over a
// repository with persistence.Write deleted, because the only generated test
// was the module boot test and it skips without a DSN. persistence.RunContract
// asserts that Save and Delete ENLIST; nothing generated called it.
func TestGeneratedRepositoryHasAContractTest(t *testing.T) {
	t.Parallel()

	for _, driver := range []string{"memory", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			t.Parallel()
			dir := app(t)
			if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
				t.Fatalf("Entity: %v", err)
			}
			if _, err := generate.Repository(generate.Options{
				Dir: dir, Module: "user", Name: "Order", Driver: driver,
			}); err != nil {
				t.Fatalf("Repository: %v", err)
			}
			src := read(t, dir, repoFile("order", driver, "_test.go"))
			if !strings.Contains(src, "persistence.RunContract(") {
				t.Errorf("the generated test does not run the contract suite:\n%s", src)
			}
			if !strings.Contains(src, "persistence.RunVersionedContract(") {
				t.Errorf("the generated entity embeds VersionedRoot, so the versioned suite must run too:\n%s", src)
			}
		})
	}
}

// TestPostgresContractTestSkipNamesTheGuarantee — "this test needs a database"
// teaches people to ignore red; it does not tell them what stopped being
// checked. The skip has to name the guarantee that went unverified.
func TestPostgresContractTestSkipNamesTheGuarantee(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Order"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Order", Driver: "postgres",
	}); err != nil {
		t.Fatalf("Repository: %v", err)
	}
	src := read(t, dir, repoFile("order", "postgres", "_test.go"))
	skip := skipMessage(src)
	if skip == "" {
		t.Fatalf("the generated postgres test has no t.Skip:\n%s", src)
	}
	for _, want := range []string{"enlist", "contract"} {
		if !strings.Contains(strings.ToLower(skip), want) {
			t.Errorf("the skip message does not name what went unverified (%q missing):\n%s", want, skip)
		}
	}
}

// skipMessage returns the text of the first t.Skip in src.
func skipMessage(src string) string {
	i := strings.Index(src, "t.Skip(")
	if i < 0 {
		return ""
	}
	rest := src[i:]
	end := strings.Index(rest, "\n\t\t}")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// TestABodylessVerbGetsNoRequiredBodyField is SBX-002, pinned.
//
// The generator used to emit `ID string `+"`"+`json:"id" validate:"required"`+"`"+`
// whenever a route had no wildcard, whatever the method — so
// `warren g command thing ListThings --method get --route /things` produced a
// GET that answered 400 to every request, because a GET carries no body to
// satisfy a required body field.
//
// It shipped because everything downstream was green: it compiled, it vetted,
// it passed `warren lint arch`, and it passed its OWN generated test, since
// that test calls Handle directly and never crosses the transport. Nothing in
// the build could see it. This test is the thing that can.
func TestABodylessVerbGetsNoRequiredBodyField(t *testing.T) {
	t.Parallel()

	for _, method := range []string{"get", "delete"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			dir := app(t)
			if _, err := generate.Module(generate.Options{Dir: dir, Name: "thing"}); err != nil {
				t.Fatalf("g module: %v", err)
			}
			if _, err := generate.Command(generate.Options{
				Dir: dir, Module: "thing", Name: "ListThings",
				Route: "/things", Method: method,
			}); err != nil {
				t.Fatalf("g command: %v", err)
			}

			file := read(t, dir, "internal/modules/thing/application/list_things.go")
			// Scope to the REQUEST struct: the Result struct carries json
			// tags legitimately, and it is the request that must not.
			decl := structBody(t, file, "type ListThings struct {")
			got := withoutComments(decl)
			if strings.Contains(got, "json:") {
				t.Errorf("a %s with no wildcard was given a JSON BODY field — every request to it 400s:\n%s", method, got)
			}
			if strings.Contains(got, `validate:"required"`) {
				t.Errorf("a %s with no wildcard was given a required field it can never receive:\n%s", method, got)
			}
			// The empty struct is not enough on its own: it must teach the
			// two real ways to take input, because SBX-006 found the binding
			// contract documented in one sentence nobody finds.
			// These live in the COMMENTS, so they are asserted against the
			// declaration rather than the stripped fields.
			for _, want := range []string{`param:"id"`, `query:"limit"`} {
				if !strings.Contains(decl, want) {
					t.Errorf("the generated request struct does not mention %s, so it teaches nothing:\n%s", want, decl)
				}
			}
		})
	}
}

// TestAWildcardRouteStillBindsTheParam is the other half: the fix must not
// have made every request struct empty.
func TestAWildcardRouteStillBindsTheParam(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "thing"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "thing", Name: "GetThing",
		Route: "/things/{id}", Method: "get",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	got := read(t, dir, "internal/modules/thing/application/get_thing.go")
	if !strings.Contains(got, `param:"id"`) {
		t.Errorf("a GET with a wildcard must still bind it:\n%s", got)
	}
}

// TestAWriteWithNoWildcardStillTakesABody guards the third case: POST/PUT/PATCH
// with no wildcard legitimately reads the body, and the SBX-002 fix must not
// have emptied those too.
func TestAWriteWithNoWildcardStillTakesABody(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Module(generate.Options{Dir: dir, Name: "thing"}); err != nil {
		t.Fatalf("g module: %v", err)
	}
	if _, err := generate.Command(generate.Options{
		Dir: dir, Module: "thing", Name: "CreateThing",
		Route: "/things", Method: "post",
	}); err != nil {
		t.Fatalf("g command: %v", err)
	}

	got := read(t, dir, "internal/modules/thing/application/create_thing.go")
	if !strings.Contains(got, `json:"id"`) {
		t.Errorf("a POST with no wildcard reads the body and must keep its json field:\n%s", got)
	}
}

// withoutComments strips // lines from a struct body. The generated
// request struct EXPLAINS why it has no json field, and that explanation
// necessarily contains the word `json:` — so an assertion that reads the
// comments would be defeated by the very text that documents the fix.
func withoutComments(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "//") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// TestGeneratedMigrateReadsTheProjectsOwnDSNVariable — field test #15,
// finding 6. `warren g repository --driver postgres` printed
//
//	LIBRARY_DATABASE_URL=... go run ./cmd/migrate
//
// and wrote a cmd/migrate reading DATABASE_URL, in the same command, under a
// comment claiming they were "the SAME variable internal/platform reads, so
// the application and its migrations cannot disagree about which database
// they mean". The contract test written by the same command used the prefixed
// form, and so does `warren new --db postgres`. Three artifacts, two names,
// and the printed instruction failed against the file it had just written.
func TestGeneratedMigrateReadsTheProjectsOwnDSNVariable(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	out, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Book", Driver: "postgres",
	})
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}

	migrate := read(t, dir, "cmd/migrate/main.go")
	if !strings.Contains(migrate, `os.Getenv("MYAPP_DATABASE_URL")`) {
		t.Errorf("cmd/migrate does not read the project's own variable:\n%s", migrate)
	}
	if strings.Contains(migrate, `"DATABASE_URL"`) {
		t.Errorf("cmd/migrate still reads an unprefixed DATABASE_URL:\n%s", migrate)
	}

	// Field test #16, finding 4. cmd/migrate is a DEPLOY STEP and runs from
	// wherever the deploy job puts it, so a path relative to the working
	// directory found nothing: Warren's own tables applied, the project's did
	// not, exit 1, database half-migrated. The migrations must be compiled in.
	//
	// The companion test inside the generated project asserts the embed
	// actually carries the .sql files; this asserts cmd/migrate USES it,
	// which a build cannot tell you because os.DirFS compiles perfectly.
	if strings.Contains(migrate, "os.DirFS") {
		t.Errorf("cmd/migrate reads its migrations from the working directory, "+
			"which is not where a deploy step runs:\n%s", migrate)
	}
	if !strings.Contains(migrate, "migrations.FS") {
		t.Errorf("cmd/migrate does not use the embedded migrations:\n%s", migrate)
	}
	embed := read(t, dir, "db/migrations/schema.go")
	if !strings.Contains(embed, "//go:embed *.sql") {
		t.Errorf("db/migrations/schema.go does not embed the .sql files:\n%s", embed)
	}

	// And the printed instruction must name the same one it just wrote, which
	// is the half that made this visible at all.
	if !strings.Contains(out, "MYAPP_DATABASE_URL") {
		t.Errorf("the printed instruction does not name the variable cmd/migrate reads:\n%s", out)
	}
	contract := read(t, dir, repoFile("book", "postgres", "_test.go"))
	if !strings.Contains(contract, "MYAPP_DATABASE_URL") {
		t.Errorf("the generated contract test uses a different variable again:\n%s", contract)
	}
}

// TestForceBacksUpWhatItDestroys — field test #15, finding 11, reclassified
// from PAPERCUT to a data-loss bug.
//
// `warren g repository catalog Book --driver postgres --force` replaced 100
// lines of hand-written repository, including the List method the domain port
// required, with a template that knows id, created_at and version. The
// compiler caught that one. It also clobbered two hand-written migration
// files, and nothing caught those — there was no backup and no mention.
func TestForceBacksUpWhatItDestroys(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("Repository: %v", err)
	}

	// Stand in for the hand-written work: the file exists, and its contents
	// are the thing that must survive somewhere.
	const handWritten = "// hand-written, and the only copy\npackage infrastructure\n"
	repo := filepath.Join(dir, "internal/modules/user/infrastructure/book_repository.go")
	if err := os.WriteFile(repo, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}

	// The SAME driver, so the generator really is asked to replace the file.
	// A postgres regeneration writes book_repository_postgres.go and would not
	// touch this one at all — which is finding 7's fix, asserted below.
	out, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Book", Force: true,
	})
	if err != nil {
		t.Fatalf("Repository --force: %v", err)
	}

	backup, err := os.ReadFile(repo + ".orig")
	if err != nil {
		t.Fatalf("--force destroyed a hand-written file with no backup: %v", err)
	}
	if string(backup) != handWritten {
		t.Errorf("the backup is not what was destroyed:\n%s", backup)
	}
	if !strings.Contains(out, ".orig") {
		t.Errorf("--force did not say where the previous content went:\n%s", out)
	}

	// A second --force must not back up the FIRST run's generated output over
	// the hand-written original. The oldest backup is the valuable one.
	if _, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Book", Force: true,
	}); err != nil {
		t.Fatalf("second Repository --force: %v", err)
	}
	again, err := os.ReadFile(repo + ".orig")
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != handWritten {
		t.Errorf("a second --force overwrote the backup of the hand-written file:\n%s", again)
	}
}

// TestDryRunWarnsBeforeForceDestroysAnything — the other half: --dry-run is
// the one moment the content still exists, so it has to say what will go.
func TestDryRunWarnsBeforeForceDestroysAnything(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("Repository: %v", err)
	}

	out, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Book", Force: true, DryRun: true,
	})
	if err != nil {
		t.Fatalf("Repository --force --dry-run: %v", err)
	}
	for _, want := range []string{"WILL REPLACE", "overwrite", ".orig"} {
		if !strings.Contains(out, want) {
			t.Errorf("--dry-run does not warn that --force will destroy files (%q):\n%s", want, out)
		}
	}
}

// repoFile is where a driver's repository lands. postgres gets its own file
// so it can sit beside the memory implementation GETTING_STARTED §8 tells you
// to keep — before 2026-09-02 both drivers wrote <agg>_repository.go and
// --force destroyed the one the documentation asked for.
func repoFile(snake, driver, suffix string) string {
	if driver == "postgres" {
		return "internal/modules/user/infrastructure/" + snake + "_repository_postgres" + suffix
	}
	return "internal/modules/user/infrastructure/" + snake + "_repository" + suffix
}

// TestBothDriversCoexistForOneAggregate — field test #16, finding 7.
//
// GETTING_STARTED §8 tells you to KEEP a memory implementation so module
// tests run without Docker, and then `g repository --driver postgres` wrote to
// the same filename and declared the same type. The refusal was exemplary —
// nothing half-written — but the only ways past it were renaming the file, the
// type and the constructor by hand (the field test paid eight sed expressions)
// or `--force`, which silently destroyed the file the documentation had just
// told you to keep, including a hand-written List the port required.
func TestBothDriversCoexistForOneAggregate(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("memory Repository: %v", err)
	}
	memory := read(t, dir, repoFile("book", "memory", ".go"))

	// No --force, no rename, no refusal.
	if _, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Book", Driver: "postgres",
	}); err != nil {
		t.Fatalf("postgres Repository beside the memory one: %v", err)
	}

	if got := read(t, dir, repoFile("book", "memory", ".go")); got != memory {
		t.Error("generating the postgres driver modified the memory implementation")
	}
	pg := read(t, dir, repoFile("book", "postgres", ".go"))

	// Distinct types, so one package holds both and the compiler is happy.
	if !strings.Contains(pg, "type BookPostgresRepository struct") {
		t.Errorf("the postgres repository does not have its own type:\n%s", pg)
	}
	if !strings.Contains(memory, "type BookRepository struct") {
		t.Errorf("the memory repository's type changed:\n%s", memory)
	}
	// And both return the same PORT, which is what makes the swap a
	// provider-list edit rather than a rewrite.
	for _, src := range []string{memory, pg} {
		if !strings.Contains(src, "domain.BookRepository {") {
			t.Errorf("a driver stopped returning the domain port:\n%s", src)
		}
	}
	// The module wires the one just generated.
	if mod := read(t, dir, "internal/modules/user/module.go"); !strings.Contains(mod, "NewBookPostgresRepository") {
		t.Errorf("module.go does not provide the postgres constructor:\n%s", mod)
	}
}

// TestTheContractTestSurvivesAChangedConstructor — field test #16, finding 6.
//
// `warren g entity` tells you to "add the fields the invariants need", and the
// moment you do, New<Agg>'s signature changes. The generated contract test
// called it with two guessed arguments, so following the entity generator's
// own advice made the package stop compiling:
//
//	vet: stock_item_repository_test.go:139:54: not enough arguments in call to
//	     domain.NewStockItem
//
// That matters more than an ordinary generator mismatch, because this file's
// own header says deleting it returns you to a repository that silently drops
// events — so the fastest way out of a broken build was to remove the thing
// protecting you. It now fails at RUN time with an instruction instead.
func TestTheContractTestSurvivesAChangedConstructor(t *testing.T) {
	t.Parallel()

	dir := app(t)
	if _, err := generate.Entity(generate.Options{Dir: dir, Module: "user", Name: "Book"}); err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := generate.Repository(generate.Options{
		Dir: dir, Module: "user", Name: "Book", Driver: "postgres",
	}); err != nil {
		t.Fatalf("Repository: %v", err)
	}
	src := read(t, dir, repoFile("book", "postgres", "_test.go"))

	// It must not CALL the constructor, whose signature it cannot know. The
	// doc comment SHOWS the call, which is the point of it, so only
	// non-comment lines count.
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "domain.NewBook(") {
			t.Errorf("line %d calls the aggregate's constructor, so adding a field to "+
				"the aggregate stops this package compiling:\n\t%s", i+1, trimmed)
		}
	}
	// It must fail loudly, and the message must say what to write and where.
	for _, want := range []string{
		"panic(",
		"fill in newBookForContract",
		"carrying a pending",
		"book_repository_postgres_test.go",
		// And a FRESH CLONE must be green: the suites are gated on the one
		// function the user has to write, so `go test ./...` on an untouched
		// repository skips with an explanation instead of panicking for
		// anyone who happens to have a database.
		"const contractFactoryFilled = false",
		"did NOT run",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the placeholder does not carry %q:\n%s", want, src)
		}
	}
}
