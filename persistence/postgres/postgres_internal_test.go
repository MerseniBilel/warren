package postgres

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/persistence"

	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
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
		// A "/" or "?" INSIDE the password. `openssl rand -base64` emits "/"
		// routinely, so this is ordinary generated-credential territory, not
		// an exotic edge case — and cutting the authority at the first "/"
		// put the "@" outside the slice and leaked the whole password.
		"postgres://app:" + secret + "/x@localhost:5432/db",
		"postgres://app:" + secret + "?x@localhost:5432/db",
		"postgres://app:a/b?c" + secret + "@localhost:5432/db",
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

// TestApplicationNameIdentifiesTheService covers the column an operator reads
// on a shared database. A two-replica outbox has exactly one leader, and
// finding which process holds the advisory lock used to start with a blank
// pg_stat_activity.application_name.
func TestApplicationNameIdentifiesTheService(t *testing.T) {
	t.Parallel()

	const dsn = "postgres://app:pw@localhost:5432/db"

	t.Run("defaults to the binary name", func(t *testing.T) {
		t.Parallel()
		cfg := defaults()
		cfg.dsn = dsn
		p, err := newPool(cfg, lifecycle.New(), health.New(func() bool { return true }))
		if err != nil {
			t.Fatalf("newPool: %v", err)
		}
		got := p.pgxCfg.ConnConfig.RuntimeParams["application_name"]
		if got == "" {
			t.Fatal("application_name is empty; every connection is anonymous")
		}
		if got != binaryName() {
			t.Errorf("application_name = %q, want the binary name %q", got, binaryName())
		}
	})

	t.Run("the option overrides it", func(t *testing.T) {
		t.Parallel()
		cfg := defaults()
		cfg.dsn = dsn
		ApplicationName("billing").apply(&cfg)
		p, err := newPool(cfg, lifecycle.New(), health.New(func() bool { return true }))
		if err != nil {
			t.Fatalf("newPool: %v", err)
		}
		if got := p.pgxCfg.ConnConfig.RuntimeParams["application_name"]; got != "billing" {
			t.Errorf("application_name = %q, want %q", got, "billing")
		}
	})

	// An explicit choice in the connection string is not overridden by a
	// default derived from argv.
	t.Run("the DSN wins", func(t *testing.T) {
		t.Parallel()
		cfg := defaults()
		cfg.dsn = dsn + "?application_name=from-dsn"
		p, err := newPool(cfg, lifecycle.New(), health.New(func() bool { return true }))
		if err != nil {
			t.Fatalf("newPool: %v", err)
		}
		if got := p.pgxCfg.ConnConfig.RuntimeParams["application_name"]; got != "from-dsn" {
			t.Errorf("application_name = %q, want the DSN's %q", got, "from-dsn")
		}
	})
}

// --- leadership, and the silence a field test found in it -----------------

// TestASecondLeadOnOneElectorIsRefused — an elector is ONE lock key. A field
// test wired the outbox relay and an SLA sweeper to the same one, following
// the README's own advice that a scheduler is "an ordinary lifecycle.Hook"
// and "outbox.Elector already gives leader-only". Whichever goroutine woke
// first took the lock; the other returned to a retry loop and did nothing for
// the life of the process, emitting NOTHING, while /readyz reported 200 and
// named the outbox an up critical dependency.
//
// Four runs of one binary: twice the relay won and no ticket ever escalated,
// twice the sweeper won and no event was ever published. Both looked healthy.
func TestASecondLeadOnOneElectorIsRefused(t *testing.T) {
	t.Parallel()
	el := advisoryLock{
		cfg:   lockConfig{key: "warren/outbox", retry: time.Hour},
		busy:  &atomic.Bool{},
		state: &atomic.Int32{},
	}

	// A real first Lead cannot be held open here: with no pool, leadOnce
	// returns errNotStarted at once and the flag is cleared before a second
	// goroutine could observe it. So the state a live Lead leaves behind is
	// set directly — the subject is the guard, not the scheduling that
	// reaches it, and TestLeadIsReusableAfterItReturns covers the clearing.
	el.busy.Store(true)

	err := el.Lead(context.Background(), func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("a second Lead on one elector was accepted — one of the two would silently never run")
	}
	for _, want := range []string{"competing for one leadership", "warren/outbox", "outbox.Electors"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, err)
		}
	}
}

// TestLeadIsReusableAfterItReturns — the guard is against CONCURRENT callers,
// not against a component that leads, stops, and stands again. A flag that
// was never cleared would break every restart.
func TestLeadIsReusableAfterItReturns(t *testing.T) {
	t.Parallel()
	el := advisoryLock{
		cfg:   lockConfig{key: "warren/outbox", retry: time.Hour},
		busy:  &atomic.Bool{},
		state: &atomic.Int32{},
	}
	for i := range 2 {
		if err := el.Lead(context.Background(), func(context.Context) error { return nil }); err == nil {
			t.Fatalf("call %d: want errNotStarted from the nil pool", i)
		} else if strings.Contains(err.Error(), "competing") {
			t.Fatalf("call %d was refused as contended: %v", i, err)
		}
	}
}

// TestLeadershipTransitionsAreLoggedOnce — standing by is the CORRECT state
// on most replicas, and it was reported nowhere at all. It is now, once per
// transition rather than once per retry: a line every few seconds for the
// life of the process is the same silence by a different route.
func TestLeadershipTransitionsAreLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	el := advisoryLock{
		cfg:   lockConfig{key: "sla/sweeper", retry: time.Hour},
		busy:  &atomic.Bool{},
		state: &atomic.Int32{},
	}
	ctx := context.Background()

	el.sayOnce(ctx, false, "standing by: another holder has this leadership")
	el.sayOnce(ctx, false, "standing by: another holder has this leadership")
	el.sayOnce(ctx, false, "standing by: another holder has this leadership")
	if n := strings.Count(buf.String(), "standing by"); n != 1 {
		t.Errorf("standing by was logged %d times, want 1 — a retry loop must not narrate itself", n)
	}
	if !strings.Contains(buf.String(), `"lock_key":"sla/sweeper"`) {
		t.Errorf("the line does not name the lock: %s", buf.String())
	}

	el.sayOnce(ctx, true, "leadership taken")
	if !strings.Contains(buf.String(), "leadership taken") {
		t.Errorf("taking leadership was not logged: %s", buf.String())
	}
	// And back again: a demotion is news even though standing by was said
	// before, because what changed is that this process STOPPED working.
	el.sayOnce(ctx, false, "leadership released")
	if !strings.Contains(buf.String(), "leadership released") {
		t.Errorf("losing leadership was not logged: %s", buf.String())
	}
}

// --- named leaderships ----------------------------------------------------

func newTestElectors() *electors {
	return newElectors(nil, lockConfig{key: "warren/outbox", retry: time.Hour})
}

// TestTheRelaysLeadershipIsReserved — asking for it by name would mean
// competing with the relay for one lock, and the loser publishes nothing for
// the life of the process. That is the defect Electors exists to answer, so
// reaching it through the new API has to be a boot failure.
func TestTheRelaysLeadershipIsReserved(t *testing.T) {
	t.Parallel()

	_, err := newTestElectors().Elector("warren/outbox")
	if err == nil {
		t.Fatal("the relay's own leadership was handed out")
	}
	for _, want := range []string{"warren/outbox", "relay", "LockKey"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%v", want, err)
		}
	}
}

// TestRenamingTheRelaysLockMovesTheReservation — LockKey names the RELAY's
// leadership. A registry that reserved the literal default instead would let
// a renamed relay be shadowed.
func TestRenamingTheRelaysLockMovesTheReservation(t *testing.T) {
	t.Parallel()

	e := newElectors(nil, lockConfig{key: "svc/relay", retry: time.Hour})
	if _, err := e.Elector("svc/relay"); err == nil {
		t.Error("the renamed relay leadership was handed out")
	}
	if _, err := e.Elector("warren/outbox"); err != nil {
		t.Errorf("the DEFAULT name is not reserved once the relay was renamed: %v", err)
	}
}

func TestTwoComponentsCannotClaimOneLeadership(t *testing.T) {
	t.Parallel()

	e := newTestElectors()
	if _, err := e.Elector("sla-sweeper"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := e.Elector("sla-sweeper"); err == nil {
		t.Fatal("one leadership was claimed twice — one of the two would never run")
	}
}

func TestDistinctLeadershipsGetDistinctLocks(t *testing.T) {
	t.Parallel()

	e := newTestElectors()
	a, err := e.Elector("sweeper")
	if err != nil {
		t.Fatalf("Elector: %v", err)
	}
	b, err := e.Elector("reconciler")
	if err != nil {
		t.Fatalf("Elector: %v", err)
	}
	ka := a.(advisoryLock).cfg.key
	kb := b.(advisoryLock).cfg.key
	if ka == kb {
		t.Fatalf("both leaderships took the key %q", ka)
	}
	if lockKey(ka) == lockKey(kb) {
		t.Errorf("distinct names hashed to one lock: %d", lockKey(ka))
	}
	// Independent flags, or one elector reports the other's transitions and
	// the busy guard fires across unrelated components.
	if a.(advisoryLock).busy == b.(advisoryLock).busy {
		t.Error("two leaderships share one busy flag")
	}
	if a.(advisoryLock).state == b.(advisoryLock).state {
		t.Error("two leaderships share one transition state")
	}
}

// TestTwoNamesThatHashToOneLockAreRefused — Postgres locks an int64, so the
// name is hashed. Two names colliding would share a single leadership without
// either knowing, which is the exact failure naming them prevents. It is
// vanishingly unlikely over 64 bits and costs one map lookup to refuse.
func TestTwoNamesThatHashToOneLockAreRefused(t *testing.T) {
	t.Parallel()

	e := newTestElectors()
	if _, err := e.Elector("first"); err != nil {
		t.Fatalf("Elector: %v", err)
	}
	// Forge the collision rather than search for one: the check is what is
	// under test, not FNV's distribution.
	e.byKey[lockKey("second")] = "first"

	_, err := e.Elector("second")
	if err == nil {
		t.Fatal("two names hashing to one lock were both accepted")
	}
	if !strings.Contains(err.Error(), "hash to one lock") {
		t.Errorf("the diagnostic does not explain:\n%v", err)
	}
}

func TestAnUnnamedLeadershipIsRefusedByThePostgresRegistry(t *testing.T) {
	t.Parallel()

	if _, err := newTestElectors().Elector(""); err == nil {
		t.Fatal("an empty leadership name was accepted")
	}
}

// TestContentionIsLoggedNotOnlyReturned — field test #8, defect 3. Lead
// BLOCKS for the duration of leadership, so the obvious spelling is
// `go el.Lead(ctx, fn)`, and `go f()` has nowhere to put a return value. The
// tester wrote exactly that: nine log lines, none about leadership, the
// component never ran, /readyz 200. The best diagnostic in the package was
// reachable only from code that did not need it.
func TestContentionIsLoggedNotOnlyReturned(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	el := advisoryLock{
		cfg:   lockConfig{key: "warren/outbox", retry: time.Hour},
		busy:  &atomic.Bool{},
		state: &atomic.Int32{},
	}
	el.busy.Store(true) // as a live Lead would leave it

	// The return value is DISCARDED, exactly as `go el.Lead(...)` discards it.
	_ = el.Lead(context.Background(), func(context.Context) error { return nil })

	if !strings.Contains(buf.String(), "leadership contended") {
		t.Errorf("a discarded refusal left no trace in the log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"lock_key":"warren/outbox"`) {
		t.Errorf("the line does not name the lock: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "ERROR") {
		t.Errorf("a component that will never run was not logged at ERROR: %s", buf.String())
	}
}

// TestClaimedLeadershipsAreNamedAtBoot — step 3 does not check for an unused
// provider (warren.md §2.1), so a component that mints a leadership and is
// never constructed boots green and does nothing. The boot log is where its
// absence is visible, which only works if the present ones are named.
func TestClaimedLeadershipsAreNamedAtBoot(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newTestElectors()
	if _, err := e.Elector("user/sweeper"); err != nil {
		t.Fatalf("Elector: %v", err)
	}
	lc := lifecycle.New()
	e.warnOnPoolPressure(lc, 10)
	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = lc.Stop(context.Background()) })

	out := buf.String()
	if !strings.Contains(out, "leaderships claimed") {
		t.Fatalf("the boot said nothing about leaderships: %s", out)
	}
	for _, want := range []string{"user/sweeper", "warren/outbox"} {
		if !strings.Contains(out, want) {
			t.Errorf("the line does not name %q: %s", want, out)
		}
	}
	// Two leaderships against MaxConns 10 is not pressure.
	if strings.Contains(out, "exhaust the connection pool") {
		t.Errorf("warned about pool pressure at 2 of 10: %s", out)
	}
}

// TestPoolPressureIsWarnedAbout — each LEADING elector holds a pooled
// connection for as long as it leads, and this file has already produced one
// connection-exhaustion incident.
func TestPoolPressureIsWarnedAbout(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newTestElectors()
	for _, n := range []string{"a", "b", "c"} {
		if _, err := e.Elector(n); err != nil {
			t.Fatalf("Elector(%q): %v", n, err)
		}
	}
	lc := lifecycle.New()
	e.warnOnPoolPressure(lc, 4) // 4 leaderships (incl. the relay's) against 4
	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = lc.Stop(context.Background()) })

	if !strings.Contains(buf.String(), "exhaust the connection pool") {
		t.Errorf("4 leaderships against MaxConns 4 did not warn: %s", buf.String())
	}
}
