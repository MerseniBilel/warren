//go:build integration

// Package postgres_test's integration suite. Everything that needs a real
// database lives here, behind the build tag, because AGENT.md's unit-test
// rule is absolute: no Docker, no network, no sleeps.
//
//	docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=warren --name warren-pg postgres:17
//	WARREN_TEST_POSTGRES_DSN='postgres://postgres:warren@localhost:5432/postgres?sslmode=disable' \
//	    go test -tags integration ./...
package postgres_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/domain"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/outbox"
	"github.com/MerseniBilel/warren/persistence"
	"github.com/MerseniBilel/warren/persistence/postgres"
)

// --- harness ---------------------------------------------------------------

// dsn returns the connection string, or skips with the command that produces
// one. A test that silently passes because no database was present is worse
// than a test that says why it did not run.
func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("WARREN_TEST_POSTGRES_DSN")
	if v == "" {
		t.Skip("WARREN_TEST_POSTGRES_DSN is not set. To run these:\n" +
			"  docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=warren --name warren-pg postgres:17\n" +
			"  export WARREN_TEST_POSTGRES_DSN='postgres://postgres:warren@localhost:5432/postgres?sslmode=disable'")
	}
	return v
}

var schemaCounter atomic64

type atomic64 struct {
	mu sync.Mutex
	n  int64
}

func (a *atomic64) next() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return a.n
}

// isolated gives one test its own schema on the shared server, migrates it,
// and drops it afterwards. That is what makes t.Parallel() safe here without
// a container per test and without a sleep anywhere.
func isolated(t *testing.T) string {
	t.Helper()
	base := dsn(t)
	name := fmt.Sprintf("warren_test_%d_%d", time.Now().UnixNano()%1e9, schemaCounter.next())

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{name}.Sanitize()+` CASCADE`)
	})

	scoped := base
	sep := "?"
	if strings.Contains(scoped, "?") {
		sep = "&"
	}
	scoped += sep + "search_path=" + name

	if err := postgres.Migrate(ctx, scoped, postgres.Schema); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return scoped
}

// --- the fixture aggregate -------------------------------------------------

type userID string

func (id userID) String() string { return string(id) }

type registered struct {
	User userID
	At   time.Time
}

func (e registered) EventName() string     { return "user.registered" }
func (e registered) OccurredAt() time.Time { return e.At }
func (e registered) AggregateID() string   { return e.User.String() }

type user struct {
	domain.AggregateRoot[userID]
	Email string
}

func newUser(id userID, email string) *user {
	u := &user{Email: email}
	u.AggregateRoot = domain.NewAggregateRoot(id)
	u.Raise(registered{User: id, At: time.Unix(1, 0).UTC()})
	return u
}

// userRepo is the hand-written repository the suite drives — and the shape a
// generated one must have: RequireTx first on every write, DB(ctx) for the
// handle, persistence.Track to enlist.
type userRepo struct{ db postgres.DB }

func (r userRepo) FindByID(ctx context.Context, id userID) (*user, error) {
	var u user
	err := r.db(ctx).QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, string(id)).Scan(&u.Email)
	if err != nil {
		if werrors.Is(err, werrors.CodeNotFound) {
			return nil, err
		}
		if isNoRows(err) {
			return nil, werrors.NotFound("user", id)
		}
		return nil, err
	}
	u.AggregateRoot = domain.NewAggregateRoot(id)
	return &u, nil
}

func (r userRepo) Save(ctx context.Context, u *user) error {
	if err := postgres.RequireTx(ctx, "save user"); err != nil {
		return err
	}
	if _, err := r.db(ctx).Exec(ctx, `
		INSERT INTO users (id, email) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email`,
		u.ID().String(), u.Email); err != nil {
		return err
	}
	persistence.Track(ctx, u)
	return nil
}

func (r userRepo) Delete(ctx context.Context, id userID) error {
	if err := postgres.RequireTx(ctx, "delete user"); err != nil {
		return err
	}
	// Exec returns the rows affected, and a Delete that matched nothing is a
	// NOT_FOUND — the contract suite deletes twice and requires the second to
	// fail. Silent success hides bugs, and this is the line a generated
	// repository must also carry.
	n, err := r.db(ctx).Exec(ctx, `DELETE FROM users WHERE id = $1`, string(id))
	if err != nil {
		return err
	}
	if n == 0 {
		return werrors.NotFound("user", id)
	}
	return nil
}

func isNoRows(err error) bool { return err != nil && strings.Contains(err.Error(), "no rows") }

// --- the VERSIONED fixture -------------------------------------------------
//
// invoice and invoiceRepo mirror, line for line, what `warren g entity` and
// `warren g repository --driver postgres` now emit. That correspondence is
// the point: the generated SQL is otherwise only compile-checked, and an
// optimistic lock that compiles but does not lock is worse than none — it
// reads as a guarantee in review.

type invoiceID string

func (id invoiceID) String() string { return string(id) }

type invoiced struct {
	Invoice invoiceID
	At      time.Time
}

func (e invoiced) EventName() string     { return "invoice.created" }
func (e invoiced) OccurredAt() time.Time { return e.At }
func (e invoiced) AggregateID() string   { return e.Invoice.String() }

type invoice struct {
	domain.VersionedRoot[invoiceID]
	Total int
}

func newInvoice(id invoiceID, total int) *invoice {
	i := &invoice{VersionedRoot: domain.NewVersionedRoot(id), Total: total}
	i.Raise(invoiced{Invoice: id, At: time.Unix(1, 0).UTC()})
	return i
}

type invoiceRepo struct{ db postgres.DB }

// FindByID reconstitutes — it does NOT call newInvoice, which would re-raise
// invoiced and republish a creation fact on the next Save, and would come
// back at version 0 so every update looked like an insert.
func (r invoiceRepo) FindByID(ctx context.Context, id invoiceID) (*invoice, error) {
	var (
		total   int
		version int64
	)
	err := r.db(ctx).QueryRow(ctx,
		`SELECT total, version FROM invoices WHERE id = $1`, string(id),
	).Scan(&total, &version)
	if err != nil {
		if werrors.Is(err, werrors.CodeNotFound) {
			return nil, err
		}
		if isNoRows(err) {
			return nil, werrors.NotFound("invoice", id)
		}
		return nil, err
	}
	return &invoice{
		VersionedRoot: domain.ReconstituteVersionedRoot(id, version),
		Total:         total,
	}, nil
}

func (r invoiceRepo) Save(ctx context.Context, inv *invoice) error {
	if err := postgres.RequireTx(ctx, "save invoice"); err != nil {
		return err
	}
	expected := inv.Version()
	var (
		n   int64
		err error
	)
	if expected == 0 {
		// DO NOTHING, not DO UPDATE: if the row is already there, two
		// requests minted the same identity, and overwriting one with the
		// other is the bug rather than the fix.
		n, err = r.db(ctx).Exec(ctx, `
			INSERT INTO invoices (id, total, version) VALUES ($1, $2, 1)
			ON CONFLICT (id) DO NOTHING`,
			string(inv.ID()), inv.Total)
	} else {
		n, err = r.db(ctx).Exec(ctx, `
			UPDATE invoices SET total = $2, version = version + 1
			WHERE id = $1 AND version = $3`,
			string(inv.ID()), inv.Total, expected)
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return werrors.Conflict("invoice %s was changed by another request since it was loaded (expected version %d)", inv.ID(), expected)
	}
	inv.SetVersion(expected + 1)
	persistence.Track(ctx, inv)
	return nil
}

func (r invoiceRepo) Delete(ctx context.Context, id invoiceID) error {
	if err := postgres.RequireTx(ctx, "delete invoice"); err != nil {
		return err
	}
	n, err := r.db(ctx).Exec(ctx, `DELETE FROM invoices WHERE id = $1`, string(id))
	if err != nil {
		return err
	}
	if n == 0 {
		return werrors.NotFound("invoice", id)
	}
	return nil
}

// bootApp brings up a real application on an isolated schema and returns the
// pieces the tests drive.
func bootApp(t *testing.T, opts ...postgres.Option) (*warren.App, postgres.DB, *postgres.UnitOfWork, outbox.Store) {
	t.Helper()
	url := isolated(t)

	var (
		db    postgres.DB
		uow   *postgres.UnitOfWork
		store outbox.Store
	)
	// Declared ONCE and shared: modules are deduplicated by identity, so
	// calling postgres.Module twice would be two modules sharing a name.
	pgModule := postgres.Module(append(
		[]postgres.Option{postgres.DSN(url), postgres.WithOutbox()}, opts...)...)

	// A module that wants postgres.DB must IMPORT the postgres module — a
	// provider is private to its module unless exported, and that is the
	// encapsulation the framework exists to enforce.
	probe := warren.NewModule("probe",
		warren.Imports(pgModule),
		warren.Providers(func(d postgres.DB, u *postgres.UnitOfWork, s outbox.Store) *captured {
			db, uow, store = d, u, s
			return &captured{}
		}),
		warren.Eager[*captured](),
	)
	a := warren.New(pgModule, probe)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	// The suite's own table, created after the framework's.
	if _, err := db(context.Background()).Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL)`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	// The versioned table, exactly as the generated migration writes it.
	if _, err := db(context.Background()).Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS invoices (
			id      TEXT   PRIMARY KEY,
			total   BIGINT NOT NULL,
			version BIGINT NOT NULL DEFAULT 0
		)`); err != nil {
		t.Fatalf("create invoices: %v", err)
	}
	return a, db, uow, store
}

type captured struct{}

// --- the sequence ----------------------------------------------------------

// The claim the whole package exists for: aggregate state and the outbox rows
// for the events that aggregate raised land in ONE commit.
func TestStateAndEventsCommitTogether(t *testing.T) {
	_, db, uow, store := bootApp(t)
	repo := userRepo{db: db}
	ctx := context.Background()

	if err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newUser("u-1", "bob@example.com"))
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	got, err := repo.FindByID(ctx, "u-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Email != "bob@example.com" {
		t.Errorf("email = %q", got.Email)
	}

	pending, err := store.Pending(ctx, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("outbox has %d rows, want 1 — the event did not land with the state", len(pending))
	}
	if pending[0].Topic != "user.registered" {
		t.Errorf("topic = %q", pending[0].Topic)
	}
}

// The other half of the same claim: a rollback takes the outbox rows with it.
func TestRollbackTakesTheOutboxRowsWithIt(t *testing.T) {
	_, db, uow, store := bootApp(t)
	repo := userRepo{db: db}
	ctx := context.Background()

	boom := werrors.Conflict("nope")
	err := uow.Do(ctx, func(ctx context.Context) error {
		if err := repo.Save(ctx, newUser("u-2", "bob@example.com")); err != nil {
			return err
		}
		return boom
	})
	if !werrors.Is(err, werrors.CodeConflict) {
		t.Fatalf("Do returned %v, want the handler's error verbatim", err)
	}

	if _, err := repo.FindByID(ctx, "u-2"); !werrors.Is(err, werrors.CodeNotFound) {
		t.Errorf("the row survived a rollback: %v", err)
	}
	pending, err := store.Pending(ctx, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("outbox has %d rows after a rollback — events were published for state that never existed", len(pending))
	}
}

// A write outside a unit of work must be refused rather than autocommitted,
// because its events would be silently lost.
func TestWriteOutsideDoIsRefused(t *testing.T) {
	_, db, _, _ := bootApp(t)
	repo := userRepo{db: db}

	err := repo.Save(context.Background(), newUser("u-3", "bob@example.com"))
	if err == nil {
		t.Fatal("a write outside a unit of work was allowed to autocommit")
	}
	if !strings.Contains(err.Error(), "outside a transaction") {
		t.Errorf("diagnostic:\n%v", err)
	}
}

// Reads outside a unit of work must work — the shipped contract suite calls
// FindByID with a background context.
func TestReadOutsideDoUsesThePool(t *testing.T) {
	_, db, uow, _ := bootApp(t)
	repo := userRepo{db: db}
	ctx := context.Background()

	if err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newUser("u-4", "bob@example.com"))
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := repo.FindByID(ctx, "u-4"); err != nil {
		t.Errorf("a read outside a transaction must reach the pool: %v", err)
	}
}

// A nested Do joins: one transaction, one commit, and an inner failure rolls
// the outer back.
func TestNestedDoJoinsAndInnerFailureRollsBackTheOuter(t *testing.T) {
	_, db, uow, _ := bootApp(t)
	repo := userRepo{db: db}
	ctx := context.Background()

	boom := werrors.Conflict("inner")
	err := uow.Do(ctx, func(ctx context.Context) error {
		if err := repo.Save(ctx, newUser("u-5", "outer@example.com")); err != nil {
			return err
		}
		return uow.Do(ctx, func(ctx context.Context) error {
			if err := repo.Save(ctx, newUser("u-6", "inner@example.com")); err != nil {
				return err
			}
			return boom
		})
	})
	if !werrors.Is(err, werrors.CodeConflict) {
		t.Fatalf("Do = %v", err)
	}
	for _, id := range []userID{"u-5", "u-6"} {
		if _, err := repo.FindByID(ctx, id); !werrors.Is(err, werrors.CodeNotFound) {
			t.Errorf("%s survived: a nested failure must roll the OUTER transaction back too", id)
		}
	}
}

// --- the contract suite ----------------------------------------------------

// The exported port-conformance suite, unmodified. This is what the suite
// exists for, and passing it is what makes this driver interchangeable with
// the in-memory one.
func TestRunContract(t *testing.T) {
	_, db, uow, _ := bootApp(t)
	if _, err := db(context.Background()).Exec(context.Background(),
		`TRUNCATE users`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	persistence.RunContract(t,
		func(*testing.T) (persistence.UnitOfWork, persistence.Repository[*user, userID]) {
			return uow, userRepo{db: db}
		},
		func(id userID) *user { return newUser(id, string(id)+"@example.com") },
		"c-1", "c-2", "c-3",
	)
}

// TestRunVersionedContract certifies the optimistic lock against a REAL
// database. The memory driver detects a conflict with a mutex; Postgres does
// it with `WHERE version = $n` and a rows-affected count, and only a real
// server proves that the UPDATE matches nothing when the version has moved.
//
// The concurrency subtest is the field-test defect exactly: N transactions
// that all read the same version, committing at once, of which precisely one
// may win.
func TestRunVersionedContract(t *testing.T) {
	_, db, uow, _ := bootApp(t)
	if _, err := db(context.Background()).Exec(context.Background(),
		`TRUNCATE invoices`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	persistence.RunVersionedContract(t,
		func(*testing.T) (persistence.UnitOfWork, persistence.Repository[*invoice, invoiceID]) {
			return uow, invoiceRepo{db: db}
		},
		func(id invoiceID) *invoice { return newInvoice(id, 100) },
		"v-1", "v-2", "v-3", "v-4", "v-5", "v-6", "v-7",
	)
}

// --- LISTEN/NOTIFY ---------------------------------------------------------

// Wait must return when a record is appended — and the notify is issued
// inside the business transaction, so it fires exactly when the rows become
// visible.
// TestIdleRelayDoesNotExhaustThePool is the field test's most severe finding,
// pinned against a real server.
//
// Relay.Run handed each Wait the relay-lifetime context and moved on when the
// poll timer won, so a new Wait began every interval and none ever ended. The
// Postgres store's Wait ACQUIRES A POOLED CONNECTION and holds it for its
// whole duration, so an idle service consumed one connection per tick and
// died after MaxConns of them: /readyz went 503, every request blocked
// indefinitely, and the entire log was one INFO line saying the server was
// listening.
//
// It could not recover on its own, which is what made it terminal rather than
// transient — releasing a waiter needs a NOTIFY, a NOTIFY is only issued
// inside Append, and Append needs a connection.
//
// The assertion is about POOL OCCUPANCY, not goroutines: this is the only
// place the defect is visible, since the memory store's Wait blocks on a
// channel and leaks nothing a pool can run out of.
func TestIdleRelayDoesNotExhaustThePool(t *testing.T) {
	// A pool small enough that the old behaviour exhausts it well within the
	// idle window below.
	_, db, _, store := bootApp(t, postgres.MaxConns(4))

	relay := outbox.NewRelay(store, noopPublisher{}, outbox.PollInterval(20*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = relay.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Idle for many poll intervals. Nothing is appended, so nothing NOTIFYs
	// and the timer wins every time — the exact condition that killed it.
	time.Sleep(600 * time.Millisecond)

	// The pool must still hand out a connection. Under the old behaviour this
	// blocked until the deadline: the defect's real symptom is not an error
	// but a request that never returns.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()
	var n int
	if err := db(probeCtx).QueryRow(probeCtx, `SELECT 1`).Scan(&n); err != nil {
		t.Fatalf("the pool could not serve a trivial query after an idle relay: %v", err)
	}

	// And the relay must be holding at most ONE listener, not one per tick.
	var listeners int
	if err := db(probeCtx).QueryRow(probeCtx,
		`SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'LISTEN%' AND pid <> pg_backend_pid()`,
	).Scan(&listeners); err != nil {
		t.Fatalf("counting listeners: %v", err)
	}
	if listeners > 1 {
		t.Errorf("%d LISTEN connections held after an idle relay, want at most 1 — each one is a pooled connection the service never gets back", listeners)
	}
}

// noopPublisher accepts everything: this test is about the waiter, not about
// publication.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, string, ...broker.Message) error { return nil }

func TestWaitWakesOnAppend(t *testing.T) {
	_, db, uow, store := bootApp(t)
	repo := userRepo{db: db}

	waiter, ok := store.(outbox.Waiter)
	if !ok {
		t.Fatal("the postgres store must implement outbox.Waiter")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	woke := make(chan struct{})
	go func() {
		waiter.Wait(ctx)
		close(woke)
	}()

	// Give the LISTEN a chance to be registered by retrying the write until
	// the waiter reacts — no sleep, just a bounded loop.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-woke:
				return
			default:
			}
			_ = uow.Do(context.Background(), func(c context.Context) error {
				return repo.Save(c, newUser(userID(fmt.Sprintf("u-w-%d", time.Now().UnixNano())), "w@example.com"))
			})
		}
	}()

	select {
	case <-woke:
	case <-ctx.Done():
		t.Fatal("Wait did not return after an append — LISTEN/NOTIFY is not wired")
	}
}

// --- migrations ------------------------------------------------------------

func TestMigrateIsIdempotent(t *testing.T) {
	url := isolated(t) // already migrated once
	ctx := context.Background()
	if err := postgres.Migrate(ctx, url, postgres.Schema); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := postgres.Migrate(ctx, url, postgres.Schema); err != nil {
		t.Fatalf("third Migrate: %v", err)
	}
}

func TestOutboxWithoutItsTableFailsTheBoot(t *testing.T) {
	base := dsn(t)
	ctx := context.Background()

	name := fmt.Sprintf("warren_empty_%d", time.Now().UnixNano()%1e9)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{name}.Sanitize()+` CASCADE`)
	})

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	// Deliberately NOT migrated.
	a := warren.New(postgres.Module(
		postgres.DSN(base+sep+"search_path="+name), postgres.WithOutbox()))
	err = a.Start(ctx)
	if err == nil {
		_ = a.Stop(ctx)
		t.Fatal("booting against an unmigrated database succeeded")
	}
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "postgres.Migrate") {
		t.Errorf("diagnostic must name the fix:\n%v", err)
	}
}

// --- regressions found by field-testing, 2026-08-02 ------------------------

// QueryRow returned the pgx row bare, so Scan's error was never classified:
// a duplicate key from "INSERT … RETURNING id" was a 500 instead of a 409,
// and a serialization failure never became CodeUnavailable — which made
// Isolation(Serializable) unusable, because app.Retrying never saw a
// retryable code.
func TestQueryRowClassifiesItsErrors(t *testing.T) {
	_, db, uow, _ := bootApp(t)
	ctx := context.Background()

	if err := uow.Do(ctx, func(ctx context.Context) error {
		_, err := db(ctx).Exec(ctx, `INSERT INTO users (id, email) VALUES ('dup', 'a@example.com')`)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The same violation through both handles must classify the same way.
	execErr := uow.Do(ctx, func(ctx context.Context) error {
		_, err := db(ctx).Exec(ctx, `INSERT INTO users (id, email) VALUES ('dup', 'b@example.com')`)
		return err
	})
	rowErr := uow.Do(ctx, func(ctx context.Context) error {
		var id string
		return db(ctx).QueryRow(ctx,
			`INSERT INTO users (id, email) VALUES ('dup', 'b@example.com') RETURNING id`).Scan(&id)
	})

	if !werrors.Is(execErr, werrors.CodeConflict) {
		t.Errorf("Exec duplicate = %v, want CONFLICT", execErr)
	}
	if !werrors.Is(rowErr, werrors.CodeConflict) {
		t.Errorf("QueryRow duplicate = %v, want CONFLICT — a 500 instead of a 409", rowErr)
	}
}

// ErrNoRows must survive mapError: only the repository knows which resource
// was missing, so classifying it here would take that away.
func TestQueryRowStillReportsNoRows(t *testing.T) {
	_, db, _, _ := bootApp(t)
	ctx := context.Background()

	var email string
	err := db(ctx).QueryRow(ctx, `SELECT email FROM users WHERE id = 'nobody'`).Scan(&email)
	if !stderrors.Is(err, postgres.ErrNoRows) {
		t.Errorf("Scan on no rows = %v, want ErrNoRows", err)
	}
}

// A read during an outage was a 500 while the write beside it was correctly a
// 503, for the same outage: mapError only inspected *pgconn.PgError, and a
// dial failure is not one.
func TestConnectionFailureIsUnavailableOnReadsToo(t *testing.T) {
	_, _, _, _ = bootApp(t) // ensures a database is present, so the skip is honest

	// A pool pointed at a port nothing listens on: the failure is a dial, not
	// a statement the server rejected.
	dead := postgres.Module(postgres.DSN("postgres://postgres:warren@127.0.0.1:1/x?sslmode=disable"),
		postgres.ConnectTimeout(2*time.Second))
	a := warren.New(dead)
	err := a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("booting against a dead address succeeded")
	}
	if !strings.Contains(err.Error(), "cannot connect to postgres") {
		t.Errorf("diagnostic:\n%v", err)
	}
}
