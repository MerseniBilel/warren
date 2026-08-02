package postgres

import (
	"context"
	"time"

	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/lifecycle"
)

// InboxOption configures the inbox store.
type InboxOption struct{ apply func(*inboxConfig) }

type inboxConfig struct {
	table     string
	retention time.Duration
}

// InboxRetention is how long a seen key is remembered. The default is 24h: it
// must exceed the longest redelivery window the broker can produce, or a
// late duplicate is processed twice.
func InboxRetention(d time.Duration) InboxOption {
	return InboxOption{apply: func(c *inboxConfig) { c.retention = d }}
}

// WithInbox enables a durable inbox.Store over the warren_inbox table.
//
// The dedupe row is written in the handler's own UnitOfWork transaction,
// which is the whole reason a durable store is worth having: handler-success
// and mark-seen become one commit, closing the crash-after-success duplicate
// window that an in-memory store cannot. Re-marking refreshes the window.
//
// Its OnStart verifies the table exists and fails the boot naming the fix. It
// does not create it: see Migrate.
func WithInbox(opts ...InboxOption) Option {
	cfg := inboxConfig{table: "warren_inbox", retention: 24 * time.Hour}
	for _, o := range opts {
		o.apply(&cfg)
	}
	return Option{apply: func(c *config) { c.inbox = &cfg }}
}

func newInboxStore(p *pool, lc lifecycle.Lifecycle, cfg inboxConfig) inbox.Store {
	s := &inboxStore{pool: p, cfg: cfg}
	lc.Append(lifecycle.Hook{
		Name: ModuleName + "/inbox",
		OnStart: func(ctx context.Context) error {
			if err := s.verify(ctx); err != nil {
				return err
			}
			s.stopSweep = startSweeper(cfg.retention, s.sweep)
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

type inboxStore struct {
	pool      *pool
	cfg       inboxConfig
	stopSweep func()
}

var _ inbox.Store = (*inboxStore)(nil)

func (s *inboxStore) verify(ctx context.Context) error {
	var one int
	err := s.pool.boxed.QueryRow(ctx, `SELECT 1 FROM `+quoteIdent(s.cfg.table)+` LIMIT 1`).Scan(&one)
	if err != nil && !errorsAs2(err, ErrNoRows) {
		return errTableMissing(s.cfg.table)
	}
	return nil
}

// Seen reports whether id has been recorded and not yet expired.
//
// It reads through DB, so inside a UnitOfWork it sees that transaction's own
// uncommitted MarkSeen — which is what makes "check then mark" correct within
// one handler.
func (s *inboxStore) Seen(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.pool.db(ctx).QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM `+quoteIdent(s.cfg.table)+`
			WHERE id = $1 AND expires_at > now()
		)`, id).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// MarkSeen records id, expiring after ttl. Marking an existing id refreshes
// its window.
//
// ON CONFLICT rather than a read-then-write: two consumers racing the same
// message must not both conclude they are first, and only the database can
// settle that.
func (s *inboxStore) MarkSeen(ctx context.Context, id string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = s.cfg.retention
	}
	_, err := s.pool.db(ctx).Exec(ctx, `
		INSERT INTO `+quoteIdent(s.cfg.table)+` (id, expires_at)
		VALUES ($1, now() + $2::interval)
		ON CONFLICT (id) DO UPDATE
		SET seen_at = now(), expires_at = EXCLUDED.expires_at`,
		id, ttl.String())
	return err
}

// sweep deletes expired keys. Retention is bounded by a periodic DELETE in
// v0.1; partitioning by day is the answer at volume, and volume will say.
func (s *inboxStore) sweep(ctx context.Context) error {
	_, err := s.pool.db(ctx).Exec(ctx,
		`DELETE FROM `+quoteIdent(s.cfg.table)+` WHERE expires_at < now()`)
	return err
}
