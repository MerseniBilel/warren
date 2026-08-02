package postgres

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/outbox"
)

// notifyChannel is the LISTEN/NOTIFY channel the store signals on. It is
// fixed rather than configurable: two services sharing a database and a
// channel only wake each other up spuriously, which costs one empty drain.
const notifyChannel = "warren_outbox"

// OutboxOption configures the outbox store.
type OutboxOption struct{ apply func(*outboxConfig) }

type outboxConfig struct {
	table     string
	encoder   outbox.Encoder
	retention time.Duration
}

// Encoder sets how a domain event becomes an outbox record. The default is
// outbox.JSONEncoder().
func Encoder(e outbox.Encoder) OutboxOption {
	return OutboxOption{apply: func(c *outboxConfig) { c.encoder = e }}
}

// Table renames the outbox table. The default is "warren_outbox". Changing it
// after rows exist is a migration you write.
func Table(name string) OutboxOption {
	return OutboxOption{apply: func(c *outboxConfig) { c.table = name }}
}

// Retention deletes published rows older than d on a sweep, so the table does
// not grow without bound. The default is 24h; zero keeps rows forever, which
// is right only when something else archives them.
func Retention(d time.Duration) OutboxOption {
	return OutboxOption{apply: func(c *outboxConfig) { c.retention = d }}
}

// WithOutbox enables a durable outbox.Store over the warren_outbox table, and
// registers outbox.Sink on the UnitOfWork so events drained at commit become
// rows in the same transaction.
//
// The store implements outbox.Waiter with LISTEN/NOTIFY. The notify is issued
// INSIDE the business transaction, so Postgres delivers it exactly when the
// rows become visible — a missed signal costs the relay's poll interval and
// never loses a record.
//
// Its OnStart VERIFIES the table exists and fails the boot naming the fix if
// it does not. It does not create it: see Migrate.
//
// It is an option rather than a sibling module because it needs the pool, and
// a sibling module cannot see another module's providers.
func WithOutbox(opts ...OutboxOption) Option {
	cfg := outboxConfig{table: "warren_outbox", encoder: outbox.JSONEncoder(), retention: 24 * time.Hour}
	for _, o := range opts {
		o.apply(&cfg)
	}
	return Option{apply: func(c *config) { c.outbox = &cfg }}
}

func newOutboxStore(p *pool, uow *UnitOfWork, lc lifecycle.Lifecycle, cfg outboxConfig) outbox.Store {
	s := &store{pool: p, cfg: cfg}
	uow.OnCommit(outbox.Sink(s, cfg.encoder))
	lc.Append(lifecycle.Hook{
		Name: ModuleName + "/outbox",
		OnStart: func(ctx context.Context) error {
			if err := s.verify(ctx); err != nil {
				return err
			}
			s.stopSweep = startSweeper(cfg.retention, s.sweep)
			s.warnIfUndrained(ctx)
			return nil
		},
		OnStop: func(context.Context) error {
			if s.stopSweep != nil {
				s.stopSweep()
			}
			return nil
		},
	})
	return s
}

// store is the durable outbox. It writes in the caller's ambient transaction
// and opens no connection of its own — which is what makes the rows and the
// aggregate state one commit.
type store struct {
	pool      *pool
	cfg       outboxConfig
	stopSweep func()
}

var (
	_ outbox.Store  = (*store)(nil)
	_ outbox.Waiter = (*store)(nil)
)

func (s *store) verify(ctx context.Context) error {
	var one int
	err := s.pool.boxed.QueryRow(ctx, `SELECT 1 FROM `+quoteIdent(s.cfg.table)+` LIMIT 1`).Scan(&one)
	if err != nil && !errorsAs2(err, ErrNoRows) {
		return errTableMissing(s.cfg.table)
	}
	return nil
}

// Append writes records in the caller's ambient transaction, and issues the
// pg_notify in that same transaction so the relay is woken exactly when the
// rows become visible.
//
// It refuses to run outside a transaction: an outbox row that autocommits
// separately from the state it describes is the failure the outbox exists to
// prevent.
func (s *store) Append(ctx context.Context, recs ...outbox.Record) error {
	if len(recs) == 0 {
		return nil
	}
	if err := RequireTx(ctx, "outbox append"); err != nil {
		return err
	}
	db := s.pool.db(ctx)
	for _, r := range recs {
		headers, err := json.Marshal(r.Message.Headers)
		if err != nil {
			return errors.Internal(err)
		}
		occurred := r.Message.OccurredAt
		if occurred.IsZero() {
			occurred = time.Now().UTC()
		}
		// message_id may be empty: the column default derives it from the row
		// id, which is stable across republishes and is what the inbox
		// dedupes on.
		if _, err := db.Exec(ctx, `
			INSERT INTO `+quoteIdent(s.cfg.table)+`
				(message_id, topic, type, key, payload, headers, occurred_at)
			VALUES (NULLIF($1, ''), $2, $3, $4, $5, $6, $7)`,
			r.Message.ID, r.Topic, r.Message.Type, r.Message.Key,
			r.Message.Payload, headers, occurred,
		); err != nil {
			return err
		}
	}
	// Inside the transaction: Postgres holds notifications until commit and
	// delivers them atomically with it, so there is no post-commit code path
	// that could fail after the data is already durable.
	if _, err := db.Exec(ctx, `SELECT pg_notify($1, '')`, notifyChannel); err != nil {
		return err
	}
	return nil
}

// Pending returns up to limit undispatched records in insertion order.
func (s *store) Pending(ctx context.Context, limit int) ([]outbox.Record, error) {
	rows, err := s.pool.db(ctx).Query(ctx, `
		SELECT COALESCE(message_id, id::text), topic, type, key, payload, headers, occurred_at
		FROM `+quoteIdent(s.cfg.table)+`
		WHERE published_at IS NULL AND failed_at IS NULL
		ORDER BY id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []outbox.Record
	for rows.Next() {
		var (
			rec     outbox.Record
			headers []byte
		)
		if err := rows.Scan(&rec.Message.ID, &rec.Topic, &rec.Message.Type,
			&rec.Message.Key, &rec.Message.Payload, &headers, &rec.Message.OccurredAt); err != nil {
			return nil, err
		}
		if len(headers) > 0 {
			if err := json.Unmarshal(headers, &rec.Message.Headers); err != nil {
				return nil, errors.Internal(err)
			}
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// MarkPublished marks records dispatched.
func (s *store) MarkPublished(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.db(ctx).Exec(ctx, `
		UPDATE `+quoteIdent(s.cfg.table)+`
		SET published_at = now()
		WHERE COALESCE(message_id, id::text) = ANY($1)`, ids)
	return err
}

// MarkFailed parks a record: kept for inspection, never returned by Pending
// again. The cause is stored because a parked row nobody can explain is a row
// nobody will ever action.
func (s *store) MarkFailed(ctx context.Context, id string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := s.pool.db(ctx).Exec(ctx, `
		UPDATE `+quoteIdent(s.cfg.table)+`
		SET failed_at = now(), error = $2
		WHERE COALESCE(message_id, id::text) = $1`, id, msg)
	return err
}

// Wait blocks until a record is appended or ctx is done — the low-latency
// seam outbox.Relay asserts for at start.
//
// It takes a connection OUT of the pool for as long as it waits, which is why
// the relay holds exactly one waiter. A failure here is not propagated: the
// relay's poll interval is the safety net, so a dropped LISTEN delays a
// publication and never loses one.
func (s *store) Wait(ctx context.Context) {
	if s.pool == nil || s.pool.p == nil {
		<-ctx.Done()
		return
	}
	conn, err := s.pool.p.Acquire(ctx)
	if err != nil {
		<-ctx.Done()
		return
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `LISTEN `+quoteIdent(notifyChannel)); err != nil {
		<-ctx.Done()
		return
	}
	_, _ = conn.Conn().WaitForNotification(ctx)
}

// warnIfUndrained says so when rows are already waiting and nothing appears
// to be draining them.
//
// WithOutbox turns on half a pattern: it writes rows in the business
// transaction, and publishing them is outbox.NewRelay, which the user wires
// when they have a broker. Without one the table grows forever — and
// Retention cannot help, because it only sweeps rows that were PUBLISHED. A
// silent, unbounded, unpublishable backlog under an option whose
// documentation promises bounded growth is the one failure the outbox pattern
// exists to prevent, so it is said out loud.
//
// It warns rather than fails: a service may legitimately start before its
// relay, and refusing to boot would be worse than a log line.
func (s *store) warnIfUndrained(ctx context.Context) {
	pending, err := s.Pending(ctx, 1)
	if err != nil || len(pending) == 0 {
		return
	}
	slog.Warn("outbox rows are waiting and no relay has drained them",
		"module", ModuleName+"/outbox",
		"table", s.cfg.table,
		"fix", "wire outbox.NewRelay(store, publisher, ...) — WithOutbox() writes rows, it does not publish them",
		"note", "Retention only sweeps PUBLISHED rows, so an undrained table grows without bound")
}

// sweep deletes published rows older than the retention window. It is driven
// by the sweeper the OnStart hook starts — an option that documents a
// retention window and then never enforces it is worse than no option.
func (s *store) sweep(ctx context.Context) error {
	if s.cfg.retention <= 0 {
		return nil
	}
	_, err := s.pool.db(ctx).Exec(ctx, `
		DELETE FROM `+quoteIdent(s.cfg.table)+`
		WHERE published_at IS NOT NULL AND published_at < now() - $1::interval`,
		s.cfg.retention.String())
	return err
}

// quoteIdent quotes a SQL identifier. Table names arrive from configuration,
// not from a request, but they are still interpolated into SQL — so they are
// quoted rather than trusted.
func quoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }
