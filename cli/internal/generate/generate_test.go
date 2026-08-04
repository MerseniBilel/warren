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
		names = append(names, e.Name())
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

	// The subscription is the plumbing, and it belongs in the module: it is
	// the only file allowed to see both the broker and the handler.
	sub := read(t, dir, "internal/modules/user/on_order_placed_subscription.go")
	if !strings.Contains(sub, `const topic = "order.placed"`) {
		t.Errorf("the topic was not derived from the event name:\n%s", sub)
	}

	// Providers plus Eager, not Consumers: the subscription wires its own
	// pipeline and lifecycle hook rather than registering through
	// transport.OnEvent, so it is not a transport.Controller — and boot now
	// refuses a Consumers entry that registers nothing.
	mod := read(t, dir, "internal/modules/user/module.go")
	if strings.Contains(mod, "warren.Consumers") {
		t.Errorf("the subscription is not a transport.Controller and must not be listed in Consumers:\n%s", mod)
	}
	for _, want := range []string{
		"application.NewOnOrderPlacedHandler",
		"newOrderPlacedSubscription",
		"warren.Eager[*orderPlacedSubscription]()",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("module.go is missing %q:\n%s", want, mod)
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
	if !strings.Contains(read(t, dir, "internal/modules/user/on_order_placed_subscription.go"),
		`const topic = "billing.order.placed"`) {
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
		`transport.Post(r, "/create_product", c.createProduct)`,
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
