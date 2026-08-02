package postgres

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/persistence"
)

var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("does not match golden file %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// --- the credential test --------------------------------------------------

// A connection failure is the single most likely error a service prints. If
// it prints the DSN with the password intact, that password is now in every
// log aggregator the service reaches. This test is the reason redact exists.
func TestPasswordIsNeverRendered(t *testing.T) {
	t.Parallel()

	const secret = "hunter2"
	for _, dsn := range []string{
		"postgres://app:" + secret + "@db.internal:5432/app?sslmode=require",
		"postgresql://app:" + secret + "@db.internal/app",
		"host=db.internal user=app password=" + secret + " dbname=app sslmode=require",
		"password=" + secret,
		// MALFORMED, and therefore unparseable by net/url — which is exactly
		// when errBadDSN fires. Redaction that depends on a successful parse
		// leaks the credential on the one path guaranteed to print it.
		"postgres://app:" + secret + "@:not-a-port/app",
		"postgres://app:" + secret + "@host:99999999/app",
		"postgres://app:" + secret + "@host/app?password=" + secret,
	} {
		t.Run(dsn[:min(24, len(dsn))], func(t *testing.T) {
			if got := redact(dsn); strings.Contains(got, secret) {
				t.Errorf("redact leaked the password: %s", got)
			}
			// And every diagnostic that takes a DSN must go through it.
			for _, err := range []error{
				errBadDSN(dsn, context.DeadlineExceeded),
				errCannotConnect(dsn, context.DeadlineExceeded),
			} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("diagnostic leaked the password:\n%s", err)
				}
			}
		})
	}
}

func TestRedactKeepsEverythingElse(t *testing.T) {
	t.Parallel()

	got := redact("postgres://app:hunter2@db.internal:5432/app?sslmode=require")
	for _, want := range []string{"app", "db.internal:5432", "sslmode=require"} {
		if !strings.Contains(got, want) {
			t.Errorf("redact(%q) dropped %q — the message must still be diagnosable", got, want)
		}
	}
}

func TestRedactPassesThroughADSNWithNoPassword(t *testing.T) {
	t.Parallel()

	const dsn = "postgres://app@db.internal:5432/app"
	if got := redact(dsn); got != dsn {
		t.Errorf("redact(%q) = %q", dsn, got)
	}
}

// --- diagnostics ----------------------------------------------------------

func TestDiagnosticsAreGolden(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	write := func(label string, err error) {
		b.WriteString("── " + label + "\n")
		b.WriteString(err.Error())
		b.WriteString("\n\n")
	}
	write("no DSN", errNoDSN())
	write("bad DSN", errBadDSN("postgres://app:hunter2@:not-a-port/app", context.DeadlineExceeded))
	write("cannot connect", errCannotConnect("postgres://app:hunter2@db:5432/app", context.DeadlineExceeded))
	write("not started", errNotStarted())
	write("write outside a transaction", errNoTransaction("save user"))
	write("table missing", errTableMissing("warren_outbox"))
	write("no migrations", errNoMigrations())
	write("empty migration", errEmptyMigration("00003_add_index.sql"))
	write("migration failed", errMigrationFailed("00003_add_index.sql", context.DeadlineExceeded))

	assertGolden(t, "diagnostics", strings.TrimRight(b.String(), "\n")+"\n")
}

// --- wiring, without a database -------------------------------------------

func TestMissingDSNFailsAtWiring(t *testing.T) {
	t.Parallel()

	_, err := newPool(defaults(), nil, nil)
	if err == nil {
		t.Fatal("a module with no DSN must fail")
	}
	if !strings.Contains(err.Error(), "postgres.DSN") {
		t.Errorf("diagnostic must name the fix:\n%s", err)
	}
}

// ParseConfig does no I/O, so a malformed DSN is a WIRING failure: it happens
// before any hook runs, and rolls nothing back.
func TestMalformedDSNFailsBeforeAnythingIsDialled(t *testing.T) {
	t.Parallel()

	cfg := defaults()
	cfg.dsn = "postgres://app:pw@host:notaport/db"
	_, err := newPool(cfg, nil, nil)
	if err == nil {
		t.Fatal("a malformed DSN must fail at wiring")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("diagnostic:\n%s", err)
	}
}

// --- DB resolution ---------------------------------------------------------

type fakeQueryer struct{ name string }

func (fakeQueryer) Query(context.Context, string, ...any) (Rows, error) { return nil, nil }
func (fakeQueryer) QueryRow(context.Context, string, ...any) Row        { return nil }
func (fakeQueryer) Exec(context.Context, string, ...any) (int64, error) { return 0, nil }

func TestDBResolvesTheTransactionThenThePool(t *testing.T) {
	t.Parallel()

	poolHandle := fakeQueryer{name: "pool"}
	txHandle := fakeQueryer{name: "tx"}
	p := &pool{boxed: poolHandle}

	if got := p.db(context.Background()).(fakeQueryer).name; got != "pool" {
		t.Errorf("outside a transaction, DB resolved %q — reads must reach the pool", got)
	}
	ctx := context.WithValue(context.Background(), txKey{}, Queryer(txHandle))
	if got := p.db(ctx).(fakeQueryer).name; got != "tx" {
		t.Errorf("inside a transaction, DB resolved %q", got)
	}
}

// --- RequireTx -------------------------------------------------------------

func TestRequireTxRefusesOutsideATransaction(t *testing.T) {
	t.Parallel()

	err := RequireTx(context.Background(), "save user")
	if err == nil {
		t.Fatal("a write outside a unit of work must be refused, not autocommitted")
	}
	for _, want := range []string{"outside a transaction", "uow.Do", "app.Transactional", "Reads need no transaction"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic must contain %q:\n%s", want, err)
		}
	}
}

func TestRequireTxPassesInsideOne(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(context.Background(), txKey{}, Queryer(fakeQueryer{}))
	if err := RequireTx(ctx, "save user"); err != nil {
		t.Errorf("RequireTx inside a transaction: %v", err)
	}
}

// --- nested Do -------------------------------------------------------------

// The refusal precedes all I/O, so it needs no database: a nested Do cannot
// honour an Option, because Postgres sets isolation on a transaction's first
// statement.
func TestNestedDoRefusesOptions(t *testing.T) {
	t.Parallel()

	u := &UnitOfWork{}
	ctx := context.WithValue(context.Background(), txKey{}, Queryer(fakeQueryer{}))

	err := u.Do(ctx, func(context.Context) error { return nil }, persistence.ReadOnly())
	if err == nil {
		t.Fatal("a nested Do with an Option must be refused, not silently ignored")
	}
	if err.Error() != persistence.ErrNestedOptions().Error() {
		t.Errorf("drivers must return the SAME error:\ngot:  %v\nwant: %v", err, persistence.ErrNestedOptions())
	}
}

func TestNestedDoJoinsWithoutOpeningAnything(t *testing.T) {
	t.Parallel()

	// u has no pool: if the nested Do tried to begin a transaction this would
	// fail, which is exactly what proves it joined instead.
	u := &UnitOfWork{}
	ctx := context.WithValue(context.Background(), txKey{}, Queryer(fakeQueryer{}))

	ran := false
	if err := u.Do(ctx, func(context.Context) error { ran = true; return nil }); err != nil {
		t.Fatalf("nested Do: %v", err)
	}
	if !ran {
		t.Error("the nested function did not run")
	}
}

func TestDoWithoutAPoolIsUnavailableNotAPanic(t *testing.T) {
	t.Parallel()

	u := &UnitOfWork{}
	err := u.Do(context.Background(), func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("Do before the pool started must fail")
	}
	if !strings.Contains(err.Error(), "before it was started") {
		t.Errorf("diagnostic:\n%s", err)
	}
}

// --- isolation mapping -----------------------------------------------------

func TestIsolationLevelsMap(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   persistence.Level
		want string
	}{
		{persistence.ReadCommitted, "read committed"},
		{persistence.RepeatableRead, "repeatable read"},
		{persistence.Serializable, "serializable"},
		{"", ""},
	} {
		if got := string(isoLevel(tc.in)); got != tc.want {
			t.Errorf("isoLevel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- the advisory lock key -------------------------------------------------

// A collision means two different services silently share one leadership, and
// one of them never drains its outbox. Pin the derivation.
func TestLockKeyIsStableAndDistinct(t *testing.T) {
	t.Parallel()

	// Pinned, not merely self-consistent: the value must survive a refactor,
	// because a leader that changes keys on upgrade starts draining beside
	// the old one.
	const outboxKey = 2018530580611280526
	if got := lockKey("warren/outbox"); got != outboxKey {
		t.Errorf("lockKey(\"warren/outbox\") = %d, want %d — changing it splits leadership across a rolling upgrade", got, outboxKey)
	}
	seen := map[int64]string{}
	for _, name := range []string{
		"warren/outbox", "warren/inbox", "billing/outbox", "orders/outbox", "",
	} {
		k := lockKey(name)
		if prev, dup := seen[k]; dup {
			t.Errorf("lockKey(%q) collides with lockKey(%q)", name, prev)
		}
		seen[k] = name
	}
	if lockKey("warren/outbox") == migrateLockKey {
		t.Error("the outbox lock and the migration lock share a key — a deploy would block the relay")
	}
}

// --- migrations ------------------------------------------------------------

func TestSchemaShipsBothTables(t *testing.T) {
	t.Parallel()

	names, err := migrationNames(Schema)
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("Schema has %d files: %v", len(names), names)
	}
	// Numbered so that name order is apply order — "00010" after "00009".
	if names[0] > names[1] {
		t.Errorf("migrations are not sorted: %v", names)
	}
	for _, want := range []string{"warren_outbox", "warren_inbox"} {
		found := false
		for _, n := range names {
			if strings.Contains(n, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("Schema has no migration for %s", want)
		}
	}
}

func TestUpSectionStopsAtDown(t *testing.T) {
	t.Parallel()

	body := "-- +goose Up\nCREATE TABLE a();\n-- +goose Down\nDROP TABLE a;\n"
	got := upSection(body)
	if !strings.Contains(got, "CREATE TABLE") {
		t.Errorf("upSection dropped the Up statements: %q", got)
	}
	if strings.Contains(got, "DROP TABLE") {
		t.Errorf("upSection included the Down statements: %q", got)
	}
}

func TestUpSectionUsesTheWholeFileWithNoMarkers(t *testing.T) {
	t.Parallel()

	body := "CREATE TABLE a();\n"
	if got := upSection(body); got != body {
		t.Errorf("upSection(%q) = %q", body, got)
	}
}

func TestEveryShippedMigrationHasAnUpSection(t *testing.T) {
	t.Parallel()

	names, err := migrationNames(Schema)
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	for _, n := range names {
		body, err := readSchemaFile(n)
		if err != nil {
			t.Fatalf("reading %s: %v", n, err)
		}
		if strings.TrimSpace(upSection(body)) == "" {
			t.Errorf("%s has an empty Up section — it would be recorded as applied having done nothing", n)
		}
	}
}

func TestMigrateWithoutADSNIsRefused(t *testing.T) {
	t.Parallel()

	if err := Migrate(context.Background(), "  ", Schema); err == nil {
		t.Fatal("Migrate with no DSN must fail")
	}
}
