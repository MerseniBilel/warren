package postgres

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/MerseniBilel/warren/outbox"
)

// LockOption configures the advisory-lock elector.
type LockOption struct{ apply func(*lockConfig) }

type lockConfig struct {
	key   string
	retry time.Duration
}

// LockKey names the lock. The default is "warren/outbox". Two services
// sharing one database AND one key will contend for a single leadership, and
// one of them will never drain — give each its own.
func LockKey(name string) LockOption {
	return LockOption{apply: func(c *lockConfig) { c.key = name }}
}

// RetryInterval is how often a follower retries acquisition. The default is
// 5s: the window between a leader dying and the outbox draining again.
func RetryInterval(d time.Duration) LockOption {
	return LockOption{apply: func(c *lockConfig) { c.retry = d }}
}

// WithAdvisoryLock enables an outbox.Elector backed by a Postgres
// session-level advisory lock, so exactly one replica drains the outbox.
//
// It holds one connection outside the pool's rotation for as long as it
// leads, and cancels the context it handed the relay the moment that
// connection dies. A lock lost is leadership lost, immediately — the
// alternative is two replicas publishing the same rows.
//
//	postgres.Module(postgres.DSN(url), postgres.WithOutbox(), postgres.WithAdvisoryLock())
func WithAdvisoryLock(opts ...LockOption) Option {
	cfg := lockConfig{key: "warren/outbox", retry: 5 * time.Second}
	for _, o := range opts {
		o.apply(&cfg)
	}
	return Option{apply: func(c *config) { c.lock = &cfg }}
}

type advisoryLock struct {
	pool *pool
	cfg  lockConfig
}

var _ outbox.Elector = advisoryLock{}

// Lead blocks until this replica holds the lock, then runs fn with a context
// cancelled the moment leadership is lost.
func (l advisoryLock) Lead(ctx context.Context, fn func(context.Context) error) error {
	for {
		if err := l.leadOnce(ctx, fn); err != nil {
			return err
		}
		// Leadership ended without the process shutting down: the connection
		// died, or fn returned. Back off and try to take it again.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(l.cfg.retry):
		}
	}
}

func (l advisoryLock) leadOnce(ctx context.Context, fn func(context.Context) error) error {
	if l.pool == nil || l.pool.p == nil {
		return errNotStarted()
	}
	conn, err := l.pool.p.Acquire(ctx)
	if err != nil {
		return nil // ctx done, or the pool is closing: not this replica's turn
	}
	defer conn.Release()

	// A SESSION-level lock, not a transaction-level one: it must outlive any
	// transaction and be released by the connection dying, which is what makes
	// a crashed leader's lock free itself with no lease and no clock.
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey(l.cfg.key)).Scan(&got); err != nil {
		return nil
	}
	if !got {
		return nil
	}
	defer func() {
		// Best effort: if the connection is already gone the lock went with it.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey(l.cfg.key))
	}()

	leadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Watch the connection: a lost connection is a lost lock, and a leader
	// that keeps draining after losing it is the double-publish this exists
	// to prevent.
	done := make(chan error, 1)
	go func() { done <- fn(leadCtx) }()
	for {
		select {
		case err := <-done:
			// fn returning an error ends leadership for good: the relay does
			// not fail transiently, so retrying would loop on the same fault.
			return err
		case <-ctx.Done():
			cancel()
			<-done
			return nil
		case <-time.After(l.cfg.retry):
			if err := conn.Ping(ctx); err != nil {
				// The connection died, so the lock is already gone. Stop
				// draining NOW: another replica may hold it by the time the
				// next row would be published.
				cancel()
				<-done
				return nil
			}
		}
	}
}

// lockKey derives the int64 Postgres wants from a human-readable name.
//
// FNV-1a, 64-bit, reinterpreted as a signed integer. A collision would mean
// two different services silently sharing one leadership — so this is
// unit-tested, and the name is in the diagnostic when a lock cannot be taken.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64())
}
