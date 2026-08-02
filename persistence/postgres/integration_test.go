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
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MerseniBilel/warren"
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

// --- LISTEN/NOTIFY ---------------------------------------------------------

// Wait must return when a record is appended — and the notify is issued
// inside the business transaction, so it fires exactly when the rows become
// visible.
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
