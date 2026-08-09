// Package persistence is the port repositories and units of work are written
// against: load and store aggregates by identity, and make the state a
// handler wrote and the events its aggregates raised commit together or not
// at all.
//
// No driver appears here and no SQL is generated here — the deliberate
// omission of the whole framework is an ORM. warren/persistence/postgres and
// its siblings implement these interfaces; this package holds the contract,
// the enlistment seam that makes atomic event publication possible, and an
// in-memory driver so the contract is exercised by CI without a database.
package persistence

import (
	"context"
	"sync"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
)

// Repository loads, stores, and removes a single aggregate type by identity.
// It is the floor every driver implements; a project's own repository
// interface lives in its domain package and is usually wider.
type Repository[T domain.Root[K], K domain.ID] interface {
	// FindByID returns the aggregate, or an error carrying CodeNotFound.
	FindByID(ctx context.Context, id K) (T, error)

	// Save persists the whole aggregate, inserting or updating by identity,
	// and enlists it with the unit of work in scope by calling Track.
	//
	// The Track call is part of this contract, not an implementation detail:
	// a driver whose Save does not Track loses the aggregate's events, and
	// the contract suite asserts it.
	//
	// If root implements domain.Versioned, the write is CONDITIONAL on the
	// version the aggregate was loaded at. A write whose version the store has
	// moved past returns CodeConflict and changes nothing — it is the only
	// thing standing between two concurrent requests and a lost update, and
	// RunVersionedContract certifies it. An aggregate that does not implement
	// domain.Versioned is written unconditionally, as before.
	Save(ctx context.Context, root T) error

	// Delete removes the aggregate, or returns CodeNotFound. Deleting what
	// is not there is an error rather than a silent success: an idempotent
	// replay acks on NOT_FOUND anyway (§2.6), and succeeding silently hides
	// bugs.
	Delete(ctx context.Context, id K) error
}

// UnitOfWork runs a function inside one transaction and makes the state that
// function wrote and the events its aggregates raised commit together.
//
// A nested Do — a handler calling Do while app.Transactional already opened
// one — JOINS the transaction in scope: it opens nothing, commits nothing,
// and drains nothing. Both patterns appear in warren.md §10 and §3.2, so
// erroring would break documented code, and savepoints would make the port's
// semantics driver-dependent (Mongo and Redis have none) while publishing
// events for state that was rolled back.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(context.Context) error, opts ...Option) error
}

// Transaction is the resolved configuration of one Do — what a driver reads
// to begin the transaction it was asked for. It names no driver type.
type Transaction struct {
	ReadOnly  bool
	Isolation Level
}

// Level is a transaction isolation level. The empty level means the driver's
// default.
type Level string

const (
	// ReadCommitted is the common default.
	ReadCommitted Level = "read committed"
	// RepeatableRead prevents non-repeatable reads.
	RepeatableRead Level = "repeatable read"
	// Serializable is the level a cross-aggregate invariant needs. Its
	// serialization failures surface as CodeUnavailable, which is what makes
	// app.Retrying re-run the whole transaction.
	Serializable Level = "serializable"
)

// Option configures one Do.
type Option func(*Transaction)

// ReadOnly asks for a read-only transaction.
func ReadOnly() Option { return func(t *Transaction) { t.ReadOnly = true } }

// Isolation asks for an isolation level. A driver that cannot honour it
// fails the call with CodeInvalid rather than downgrading silently — a
// silently downgraded level is a corrupted invariant.
func Isolation(l Level) Option { return func(t *Transaction) { t.Isolation = l } }

// Configure applies opts — the call a driver makes at the top of Do.
func Configure(opts ...Option) Transaction {
	var tx Transaction
	for _, opt := range opts {
		opt(&tx)
	}
	return tx
}

// --- the enlistment seam ---------------------------------------------------

type collectorKey struct{}

// collector gathers the aggregates enlisted beneath one Do. It holds
// domain.Aggregate rather than a generic type because a unit of work holds
// aggregates of many types at once and cannot name their identifier types —
// which is exactly why domain.Aggregate exists.
type collector struct {
	mu    sync.Mutex
	roots []domain.Aggregate
}

// Track enlists aggregates with the unit of work in scope on ctx, so their
// events are drained into the outbox when it commits. Repositories call it
// from Save.
//
// Outside any Do it is a no-op — and a no-op loses nothing: PullEvents is
// never called, so the events stay pending on the aggregate and a later Do
// publishes them.
func Track(ctx context.Context, aggregates ...domain.Aggregate) {
	c, ok := ctx.Value(collectorKey{}).(*collector)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roots = append(c.roots, aggregates...)
}

// Write performs one aggregate write inside the unit of work in scope, and
// enlists root when — and only when — the write succeeded.
//
// It is the repository's write contract in a single call, and there is no way
// to perform half of it. Outside a unit of work it refuses, because the row
// would autocommit while the aggregate's events stayed pending on an object
// about to go out of scope. On success it Tracks, so those events reach the
// outbox in the same commit. A write that returns an error enlists nothing.
//
// This exists because the enlistment used to be a separate statement AFTER
// the write, and a separate statement is a statement that can be deleted. A
// field test deleted it: the row committed, the request returned 201, and the
// event evaporated with no error, no outbox row and no log line — the exact
// loss the outbox subsystem exists to prevent. There is now no such line.
//
//	func (r *UserRepository) Save(ctx context.Context, u *domain.User) error {
//	    return persistence.Write(ctx, "user.Save", u, func(ctx context.Context) error {
//	        _, err := r.db(ctx).Exec(ctx, `INSERT INTO users …`, u.ID(), u.Email)
//	        return err
//	    })
//	}
//
// One aggregate, because one repository owns one. A write that legitimately
// touches more calls Track for the others inside the write function.
//
// The check it makes is "is there a unit of work", which is precisely the
// precondition for Track to do anything. A DRIVER may hold a stricter one —
// postgres.RequireTx also insists on an ambient Postgres transaction to run
// SQL on — and keeps it; the two answer different questions and both are
// worth asking.
func Write(ctx context.Context, op string, root domain.Aggregate, write func(context.Context) error) error {
	if !InTransaction(ctx) {
		return ErrNoTransaction(op)
	}
	// Pointer compares, and they buy a named failure instead of a nil
	// dereference inside the unit of work's drain — a stack frame away from
	// the mistake, and long after the write committed.
	if root == nil {
		return stderrString("persistence.Write(" + quoted(op) + ") was given no aggregate to enlist — Write exists to enlist the root it is handed, so a nil one would drain nothing and lose the events silently. Pass the aggregate you just wrote.")
	}
	if write == nil {
		return stderrString("persistence.Write(" + quoted(op) + ") was given no write function — there is nothing for it to do.")
	}
	if err := write(ctx); err != nil {
		return err
	}
	Track(ctx, root)
	return nil
}

func quoted(s string) string { return `"` + s + `"` }

// ErrNoTransaction is the diagnostic a write outside a unit of work produces.
// It is exported for the same reason ErrNestedOptions is: every driver must
// produce the SAME message, and a second copy in another module drifts from
// this one within a month.
func ErrNoTransaction(op string) error {
	return stderrString("✗ " + op + ` outside a transaction

    This write was attempted with no persistence.UnitOfWork in scope.

  It would autocommit — and the events the aggregate raised would stay
  pending on an object about to go out of scope, and be lost. Silently:
  no error, no outbox row, no way to find out afterwards.

  Wrap the use case:

      func (h *handler) Handle(ctx context.Context, cmd Cmd) (Res, error) {
          return res, h.uow.Do(ctx, func(ctx context.Context) error {
              return h.repo.Save(ctx, aggregate)
          })
      }

  or declare the handler with app.Transactional, which does it for you.

  A repository's write does not make this check itself. It calls
  persistence.Write, which makes it — and enlists the aggregate when the
  write succeeds, so its events cannot be left behind.

  Reads need no transaction; only writes are refused.`)
}

// Collect returns a context that gathers the aggregates enlisted beneath it
// and the drain that pulls their events, in enlistment order, exactly once.
// A UnitOfWork calls it when it opens a transaction and calls the drain
// between the function returning nil and the commit.
//
// It is nesting-aware: called with a collector already on ctx it returns
// that same ctx and a drain returning nil, so only the outermost Do drains.
func Collect(ctx context.Context) (context.Context, func() []domain.Event) {
	if _, ok := ctx.Value(collectorKey{}).(*collector); ok {
		return ctx, func() []domain.Event { return nil }
	}
	c := &collector{}
	return context.WithValue(ctx, collectorKey{}, c), func() []domain.Event {
		c.mu.Lock()
		roots := c.roots
		c.roots = nil
		c.mu.Unlock()

		var events []domain.Event
		for _, r := range roots {
			events = append(events, r.PullEvents()...)
		}
		return events
	}
}

// ForApp adapts a UnitOfWork to app.UnitOfWork, the one-method seam
// app.Transactional takes. The two differ only in Do's variadic options,
// which app deliberately does not know about — this adapter is the seam, so
// no application has to write it.
//
//	warren.Providers(persistence.ForApp)   // *MemoryUnitOfWork → app.UnitOfWork
func ForApp(uow UnitOfWork, opts ...Option) app.UnitOfWork {
	return appUnitOfWork{uow: uow, opts: opts}
}

type appUnitOfWork struct {
	uow  UnitOfWork
	opts []Option
}

func (a appUnitOfWork) Do(ctx context.Context, fn func(context.Context) error) error {
	return a.uow.Do(ctx, fn, a.opts...)
}

// InTransaction reports whether a unit of work is in scope on ctx.
func InTransaction(ctx context.Context) bool {
	_, ok := ctx.Value(collectorKey{}).(*collector)
	return ok
}

// ErrNestedOptions is the error a driver returns when a nested Do is passed
// an Option. It is exported because every driver must return the SAME error
// for it — a driver in another module that re-implements the message drifts
// from this one, and the two then disagree in a way only a user notices.
//
// The refusal is not fastidiousness. On a SQL database isolation and
// read-only are properties of a transaction's first statement, so a nested
// Do cannot honour an Option even in principle; silently ignoring one would
// mean a handler asking for Serializable and quietly getting Read Committed.
func ErrNestedOptions() error {
	return errors.Invalid("transaction options",
		stderrString("a nested Do cannot configure the transaction it joins — move ReadOnly/Isolation to the outermost Do"))
}

type stderrString string

func (s stderrString) Error() string { return string(s) }
