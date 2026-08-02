// Package postgres implements Warren's persistence, outbox and inbox ports
// against Postgres.
//
// It provides a UnitOfWork whose Do commits aggregate state and the outbox
// rows for the events those aggregates raised in ONE transaction, resolves
// that transaction onto repositories through the context, ships a durable
// outbox store with LISTEN/NOTIFY wake-up, an advisory-lock leader elector, a
// durable inbox dedupe store, and a ping health check.
//
// Repositories are plain SQL over a small driver-free query handle. There is
// no ORM, by design, and no pgx type in any signature here except Raw's.
//
// # It never runs a migration
//
// Schema is a deploy step: see Schema and Migrate. Migrating from a lifecycle
// hook races every replica of a rolling deploy, applies DDL the still-serving
// old replicas were not written against, and turns one bad file into a
// simultaneous crash-loop. There is no option to enable it.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/outbox"
	"github.com/MerseniBilel/warren/persistence"
)

// ModuleName is the name of the module Module returns — the scope name that
// appears in boot diagnostics.
const ModuleName = "warren/persistence/postgres"

// Option configures the Postgres module.
type Option struct{ apply func(*config) }

// CacheMode is how the driver caches prepared statements.
type CacheMode string

const (
	// StatementCachePrepare is the default: named prepared statements. It is
	// the fastest, and it is incompatible with a pgbouncer in transaction
	// pooling mode, which does not keep a session for the statement to live in.
	StatementCachePrepare CacheMode = "prepare"
	// StatementCacheDescribe uses unnamed statements: slower, and what a
	// transaction-pooling pgbouncer requires.
	StatementCacheDescribe CacheMode = "describe"
)

type config struct {
	dsn             string
	maxConns        int32
	minConns        int32
	maxConnLifetime time.Duration
	maxConnIdleTime time.Duration
	connectTimeout  time.Duration
	cacheMode       CacheMode
	healthTimeout   time.Duration
	raw             []func(context.Context, *pgxpool.Pool) error

	outbox *outboxConfig
	inbox  *inboxConfig
	lock   *lockConfig
}

func defaults() config {
	return config{
		maxConns:        10,
		minConns:        0,
		maxConnLifetime: time.Hour,
		maxConnIdleTime: 30 * time.Minute,
		connectTimeout:  5 * time.Second,
		cacheMode:       StatementCachePrepare,
		healthTimeout:   2 * time.Second,
	}
}

// Module returns the warren.Module providing the connection pool, the
// UnitOfWork, the query-handle resolver repositories inject, and a ping
// health check.
//
// The constructor PARSES: a malformed DSN or pool setting fails at wiring,
// before any hook runs, because pgxpool.ParseConfig does no I/O. The
// lifecycle hook CONNECTS: the pool is created and pinged in OnStart under
// ConnectTimeout, so an unreachable database rolls the boot back instead of
// leaving a live pool and open sockets behind a boot that failed later for an
// unrelated reason.
//
// Because everything else here depends on the pool, its hook is appended
// first and reverse-order teardown closes it last — after the outbox relay's
// final flush.
//
// It provides, into the graph:
//
//	*UnitOfWork             the concrete unit of work, for OnCommit
//	persistence.UnitOfWork  the port handlers take
//	DB                      the resolver a repository injects
func Module(opts ...Option) warren.Module {
	cfg := defaults()
	for _, o := range opts {
		o.apply(&cfg)
	}
	// The outbox, inbox and elector are OPTIONS rather than sibling modules.
	// They need the pool, and a sibling module cannot see another module's
	// providers — that is the encapsulation the framework exists to enforce,
	// and routing around it would mean exporting *pool and making every user
	// wire an import. One module, one scope, no puzzle.
	opts2 := []warren.ModuleOption{
		warren.Providers(
			func(lc lifecycle.Lifecycle, reg health.Registry) (*pool, error) {
				return newPool(cfg, lc, reg)
			},
			func(p *pool) DB { return p.db },
			func(p *pool) *UnitOfWork { return &UnitOfWork{pool: p} },
			func(u *UnitOfWork) persistence.UnitOfWork { return u },
		),
		warren.Exports[DB](),
		warren.Exports[*UnitOfWork](),
		warren.Exports[persistence.UnitOfWork](),
		warren.Eager[*pool](),
	}
	if cfg.outbox != nil {
		oc := *cfg.outbox
		opts2 = append(opts2,
			warren.Providers(func(p *pool, uow *UnitOfWork, lc lifecycle.Lifecycle) outbox.Store {
				return newOutboxStore(p, uow, lc, oc)
			}),
			warren.Exports[outbox.Store](),
			warren.Eager[outbox.Store](),
		)
	}
	if cfg.inbox != nil {
		ic := *cfg.inbox
		opts2 = append(opts2,
			warren.Providers(func(p *pool, lc lifecycle.Lifecycle) inbox.Store {
				return newInboxStore(p, lc, ic)
			}),
			warren.Exports[inbox.Store](),
			warren.Eager[inbox.Store](),
		)
	}
	if cfg.lock != nil {
		lk := *cfg.lock
		opts2 = append(opts2,
			warren.Providers(func(p *pool) outbox.Elector { return advisoryLock{pool: p, cfg: lk} }),
			warren.Exports[outbox.Elector](),
		)
	}
	return warren.NewModule(ModuleName, opts2...)
}

// DSN sets the Postgres connection string. Required: omitting it fails the
// boot. The password never appears in a log line, an error, or a diagnostic —
// see redact.
func DSN(dsn string) Option {
	return Option{apply: func(c *config) { c.dsn = dsn }}
}

// MaxConns caps the pool. The default is 10. It is the concurrency limit of
// every handler that touches the database, so size it against the request
// budget rather than against the server's max_connections.
func MaxConns(n int32) Option {
	return Option{apply: func(c *config) { c.maxConns = n }}
}

// MinConns keeps n connections warm, so the first request after an idle
// period does not pay a handshake. The default is 0.
func MinConns(n int32) Option {
	return Option{apply: func(c *config) { c.minConns = n }}
}

// MaxConnLifetime retires a connection after d regardless of health, which is
// how a pool behind a failing-over primary or a rotating credential
// eventually moves. The default is 1h.
func MaxConnLifetime(d time.Duration) Option {
	return Option{apply: func(c *config) { c.maxConnLifetime = d }}
}

// MaxConnIdleTime closes a connection idle for d. The default is 30m.
func MaxConnIdleTime(d time.Duration) Option {
	return Option{apply: func(c *config) { c.maxConnIdleTime = d }}
}

// ConnectTimeout bounds one connection attempt, including the ping in
// OnStart. The default is 5s: a boot that hangs on an unreachable database is
// worse than a boot that fails.
func ConnectTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.connectTimeout = d }}
}

// StatementCacheMode selects how prepared statements are cached. Use
// StatementCacheDescribe behind a pgbouncer in transaction pooling mode.
func StatementCacheMode(m CacheMode) Option {
	return Option{apply: func(c *config) { c.cacheMode = m }}
}

// HealthTimeout bounds one readiness ping. The default is 2s. The check gates
// readiness: a service whose database is unreachable should leave the load
// balancer's rotation.
func HealthTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.healthTimeout = d }}
}

// Raw runs fn against the raw pgx pool during OnStart, after the pool is live
// and the ping succeeded, and before any dependent hook starts. An error from
// fn fails the boot.
//
// It is the ONE place a pgx type appears in this package's exported surface
// (AGENT.md invariant 3), and it is an explicit opt-out: register a custom
// type, install a tracer, warm a cache. Repositories take DB, not the pool.
func Raw(fn func(context.Context, *pgxpool.Pool) error) Option {
	return Option{apply: func(c *config) { c.raw = append(c.raw, fn) }}
}

// --- the query handle ------------------------------------------------------

// DB resolves the handle a repository uses for one call: the transaction ctx
// carries if UnitOfWork.Do put one there, and the pool otherwise.
//
// It is the only piece of framework machinery in a repository, and it does
// one thing. Hold it in a field; never store the context:
//
//	type UserRepository struct{ db postgres.DB }
//
//	row := r.db(ctx).QueryRow(ctx, `SELECT … WHERE id = $1`, id)
//
// Both handles are boxed once — once per transaction, and once per process
// for the pool — so this allocates nothing per call.
type DB func(ctx context.Context) Queryer

// Queryer is the driver-free query handle. Its method set is deliberately
// three methods wide: it is the surface every non-pgx implementation of the
// same idea would have to provide. CopyFrom and SendBatch are reachable
// through Raw.
type Queryer interface {
	// Query runs a query returning many rows. Close the Rows.
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	// QueryRow runs a query returning at most one row. Its error surfaces
	// from Scan, and a missing row is ErrNoRows.
	QueryRow(ctx context.Context, sql string, args ...any) Row
	// Exec runs a statement and returns the number of rows it affected.
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

// Row is one row, or the error that prevented it. It is byte-identical to
// pgx.Row, so a pgx row satisfies it with no wrapping and no allocation.
type Row interface {
	Scan(dest ...any) error
}

// Rows is a result set. It is the subset of pgx.Rows a repository needs, so a
// pgx result satisfies it unwrapped.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// ErrNoRows is returned by Row.Scan when the query matched nothing. A
// repository maps it to a semantic error rather than leaking it upward:
//
//	if errors.Is(err, postgres.ErrNoRows) {
//	    return nil, errors.NotFound("user", id)
//	}
var ErrNoRows = pgx.ErrNoRows

// txKey carries the ambient transaction. It is this package's own key:
// persistence.Collect's collector answers "is there a unit of work", this
// answers "and what do I run SQL on".
type txKey struct{}

// RequireTx returns a diagnostic unless a Postgres transaction is in scope on
// ctx. Every repository Save and Delete calls it first, and so does
// Store.Append.
//
// Reads outside UnitOfWork.Do are fine and go to the pool — the persistence
// contract suite depends on it. Writes are not: the row would autocommit
// while persistence.Track stayed a no-op, so the aggregate's events would sit
// pending on an object about to go out of scope, and vanish. Silently, with
// no error anywhere. That is the exact loss the outbox exists to prevent, so
// it is refused rather than permitted quietly.
func RequireTx(ctx context.Context, op string) error {
	if _, ok := ctx.Value(txKey{}).(Queryer); ok {
		return nil
	}
	return errNoTransaction(op)
}

// --- the pool --------------------------------------------------------------

// pool owns the pgx pool and its lifecycle. It is unexported: what the graph
// sees is DB, the UnitOfWork, and nothing else.
type pool struct {
	cfg    config
	pgxCfg *pgxpool.Config
	p      *pgxpool.Pool
	boxed  Queryer // the pool as a Queryer, boxed once per process
}

func newPool(cfg config, lc lifecycle.Lifecycle, reg health.Registry) (*pool, error) {
	if strings.TrimSpace(cfg.dsn) == "" {
		return nil, errNoDSN()
	}
	// ParseConfig does no I/O, so every configuration mistake is a WIRING
	// failure: it happens before any hook runs and rolls nothing back.
	pgxCfg, err := pgxpool.ParseConfig(cfg.dsn)
	if err != nil {
		return nil, errBadDSN(cfg.dsn, err)
	}
	pgxCfg.MaxConns = cfg.maxConns
	pgxCfg.MinConns = cfg.minConns
	pgxCfg.MaxConnLifetime = cfg.maxConnLifetime
	pgxCfg.MaxConnIdleTime = cfg.maxConnIdleTime
	pgxCfg.ConnConfig.ConnectTimeout = cfg.connectTimeout
	if cfg.cacheMode == StatementCacheDescribe {
		pgxCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	}

	p := &pool{cfg: cfg, pgxCfg: pgxCfg}

	if err := reg.Register(
		health.NewCheck("postgres", p.ping),
		health.Timeout(cfg.healthTimeout),
	); err != nil {
		return nil, err
	}

	lc.Append(lifecycle.Hook{
		Name:    ModuleName,
		OnStart: p.open,
		OnStop:  p.close,
	})
	return p, nil
}

// open is acquisition: dialing, and the ping that proves the credentials work.
// A failure here rolls the boot back through the lifecycle, which is why it
// is not in the constructor.
func (p *pool) open(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.connectTimeout)
	defer cancel()

	pgxPool, err := pgxpool.NewWithConfig(ctx, p.pgxCfg)
	if err != nil {
		return errCannotConnect(p.cfg.dsn, err)
	}
	if err := pgxPool.Ping(ctx); err != nil {
		pgxPool.Close()
		return errCannotConnect(p.cfg.dsn, err)
	}
	p.p = pgxPool
	p.boxed = queryer{pgxPool}

	for _, fn := range p.cfg.raw {
		if err := fn(ctx, pgxPool); err != nil {
			pgxPool.Close()
			p.p, p.boxed = nil, nil
			return fmt.Errorf("postgres.Raw: %w", err)
		}
	}
	return nil
}

func (p *pool) close(context.Context) error {
	if p.p != nil {
		p.p.Close()
	}
	return nil
}

func (p *pool) ping(ctx context.Context) error {
	if p.p == nil {
		return errors.Unavailable("postgres", errNotStarted())
	}
	if err := p.p.Ping(ctx); err != nil {
		return errors.Unavailable("postgres", err)
	}
	return nil
}

// db is the DB resolver handed to repositories.
func (p *pool) db(ctx context.Context) Queryer {
	if q, ok := ctx.Value(txKey{}).(Queryer); ok {
		return q
	}
	return p.boxed
}

// --- pgx adaptation --------------------------------------------------------

// pgxQuerier is the subset both *pgxpool.Pool and pgx.Tx satisfy.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the only wrapper this package needs, and it exists for one
// reason: Exec must return int64 rather than pgconn.CommandTag, which is a
// driver type. It is boxed once per transaction and once per process, never
// per call — so DB(ctx) allocates nothing.
type queryer struct{ q pgxQuerier }

func (w queryer) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := w.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err)
	}
	return rows, nil
}

func (w queryer) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return w.q.QueryRow(ctx, sql, args...)
}

func (w queryer) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := w.q.Exec(ctx, sql, args...)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// mapError turns the Postgres codes that mean something semantic into Warren
// errors. It runs in the RUNTIME, not only in generated repositories, because
// a serialization failure has to become CodeUnavailable for app.Retrying to
// re-run the transaction — and without that, Isolation(Serializable) is
// unusable.
func mapError(err error) error {
	var pgErr *pgconn.PgError
	if !errorsAs(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "40001", "40P01": // serialization_failure, deadlock_detected
		return errors.Unavailable("postgres", err)
	case "23505": // unique_violation
		return errors.Conflict("%s", pgErr.ConstraintName+" violated")
	default:
		return err
	}
}

// startSweeper runs fn once now and then on an interval derived from the
// retention window, returning the function that stops it.
//
// It exists because a Retention option that documents a window and never
// enforces it is worse than no option: the table grows unbounded while the
// configuration says otherwise. The interval is a twelfth of the window,
// clamped to [1m, 1h] — frequent enough that the table tracks the setting,
// rare enough that a DELETE is never the reason the database is busy.
func startSweeper(retention time.Duration, fn func(context.Context) error) func() {
	if retention <= 0 {
		return func() {}
	}
	interval := max(min(retention/12, time.Hour), time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			// Failures are logged, not propagated: a sweep that cannot run is
			// a table that grows, not a service that should stop serving.
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("retention sweep failed", "module", ModuleName, "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
