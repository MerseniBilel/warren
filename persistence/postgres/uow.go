package postgres

import (
	"context"
	stderrors "errors"

	"github.com/jackc/pgx/v5"

	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/persistence"
)

// UnitOfWork runs a function inside one Postgres transaction and makes the
// state it wrote and the events its aggregates raised commit together.
//
// It is provided by Module; construct it only in a test.
type UnitOfWork struct {
	pool  *pool
	sinks []func(context.Context, []domain.Event) error
}

var _ persistence.UnitOfWork = (*UnitOfWork)(nil)

// OnCommit registers a sink for the events drained at commit — how the outbox
// writer attaches, and how a test asserts on what was published.
//
// Sinks run INSIDE the transaction, on its context, so a sink's own writes
// join it and a failing sink fails the commit. That atomicity is the entire
// pattern: rows and events land together or not at all.
//
//	uow.OnCommit(outbox.Sink(store, outbox.JSONEncoder()))
//
// Outbox wires this for you.
func (u *UnitOfWork) OnCommit(fn func(context.Context, []domain.Event) error) {
	if fn == nil {
		panic("postgres: OnCommit registered a nil sink")
	}
	u.sinks = append(u.sinks, fn)
}

// Do begins a transaction, puts it on the context, runs fn, drains the events
// of every aggregate that enlisted through persistence.Track, runs the commit
// sinks inside that transaction, and commits. fn returning an error, a sink
// failing, or a panic rolls everything back; a panic is re-raised.
//
// A nested Do JOINS the transaction in scope: it opens nothing, commits
// nothing, drains nothing, and returns fn's error — so an inner failure rolls
// back the outer transaction too. A caller who handles the inner error and
// returns nil from the outer fn therefore commits state the inner call may
// have left inconsistent; that is the cost of join semantics, and it is the
// same on every driver.
//
// Passing an Option to a nested Do is an error, not a silent no-op: Postgres
// sets isolation and read-only on a transaction's FIRST statement, so a
// nested Isolation(Serializable) is unimplementable — and ignoring it would
// mean a handler asking for Serializable and quietly getting Read Committed.
func (u *UnitOfWork) Do(ctx context.Context, fn func(context.Context) error, opts ...persistence.Option) error {
	if fn == nil {
		panic("postgres: UnitOfWork.Do called with a nil function")
	}
	// Step 0 — join, if a transaction is already in scope.
	if _, ok := ctx.Value(txKey{}).(Queryer); ok {
		if len(opts) > 0 {
			return persistence.ErrNestedOptions()
		}
		return fn(ctx)
	}
	if u.pool == nil || u.pool.p == nil {
		return errors.Unavailable("postgres", errNotStarted())
	}

	// Step 1 — the port's options become pgx's.
	cfg := persistence.Configure(opts...)
	txOpts := pgx.TxOptions{IsoLevel: isoLevel(cfg.Isolation)}
	if cfg.ReadOnly {
		txOpts.AccessMode = pgx.ReadOnly
	}

	// Step 2 — begin. Isolation is set here, as the transaction's first
	// statement, which is the only place Postgres accepts it.
	tx, err := u.pool.p.BeginTx(ctx, txOpts)
	if err != nil {
		return errors.Unavailable("postgres", err)
	}

	// Steps 3 and 4 — the transaction goes on the context so repositories
	// resolve through it, and the collector goes on so Track can enlist.
	txCtx := context.WithValue(ctx, txKey{}, Queryer(queryer{tx}))
	txCtx, drain := persistence.Collect(txCtx)

	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback on the ORIGINAL context: txCtx may be done, and a
		// rollback that cannot run leaks the transaction until the server
		// times it out. Panics unwind through here too, then re-raise.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	// Step 5 — the caller's work. Save calls RequireTx, then Track.
	if err := fn(txCtx); err != nil {
		return err
	}

	// Step 6 — drain. PullEvents is destructive and Collect guarantees once,
	// so this happens exactly here: after every Save has enlisted, and before
	// anything can drain again.
	events := drain()

	// Step 7 — the sinks, INSIDE the transaction. outbox.Sink lands here and
	// its INSERT joins this transaction, so a rollback takes the rows with it.
	for _, sink := range u.sinks {
		if err := sink(txCtx, events); err != nil {
			return errors.Unavailable("unit of work commit", err)
		}
	}

	// Step 8 — one commit: aggregate state and outbox rows together.
	if err := tx.Commit(ctx); err != nil {
		return errors.Unavailable("postgres", err)
	}
	committed = true
	return nil
}

// isoLevel maps the port's vocabulary onto pgx's. The port's empty level
// means "the server's default", which is what pgx's empty level means too.
func isoLevel(l persistence.Level) pgx.TxIsoLevel {
	switch l {
	case persistence.ReadCommitted:
		return pgx.ReadCommitted
	case persistence.RepeatableRead:
		return pgx.RepeatableRead
	case persistence.Serializable:
		return pgx.Serializable
	default:
		return ""
	}
}

// errorsAs and errorsAs2 are the standard library's As and Is, renamed so
// this package's own errors import does not shadow them.
func errorsAs(err error, target any) bool { return stderrors.As(err, target) }

func errorsAs2(err, target error) bool { return stderrors.Is(err, target) }
