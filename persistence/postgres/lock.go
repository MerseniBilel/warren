package postgres

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync/atomic"
	"time"

	wlog "github.com/MerseniBilel/warren/log"
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
	// busy is held for the duration of one Lead call. One elector is ONE
	// lock key, so two components calling Lead on it are competing for a
	// single leadership: whichever wakes first wins and the other does
	// nothing, for the lifetime of the process, choosing by goroutine
	// scheduling. A field test hit exactly that — the relay and a scheduler
	// on one elector — and neither the log nor /readyz said anything.
	busy *atomic.Bool
	// state is what sayOnce reports transitions against, so a follower says
	// so once rather than every retry interval for ever.
	state *atomic.Int32
}

const (
	stateUnknown  int32 = iota // nothing said yet, so the first word is news
	stateLeading               // this process holds the lock
	stateStanding              // it does not
)

// sayOnce logs a leadership transition, and only a transition. The retry
// loop runs every few seconds for the life of the process; a line per pass
// is a log nobody reads, which is the same silence by a different route.
func (l advisoryLock) sayOnce(ctx context.Context, leading bool, msg string, args ...any) {
	want := stateStanding
	if leading {
		want = stateLeading
	}
	if l.state == nil || l.state.Swap(want) == want {
		return
	}
	wlog.FromContext(ctx).InfoContext(ctx, msg, append([]any{
		"module", ModuleName, "lock_key", l.cfg.key,
	}, args...)...)
}

// errLeadershipContended refuses the configuration the field test found:
// two components sharing one elector, hence one lock key, so one of them
// silently never runs and which one is decided by goroutine scheduling.
func errLeadershipContended(key string) error {
	return diagnostic(fmt.Sprintf(
		"✗ two components are competing for one leadership\n\n"+
			"    Both called Lead on the same elector, and an elector is ONE\n"+
			"    advisory lock — %q. Only one of them can ever hold it, so the\n"+
			"    other does nothing for the lifetime of the process, and which\n"+
			"    one loses is decided by whichever goroutine woke first.\n\n"+
			"  Leader-only work that must run ALONGSIDE the outbox relay needs\n"+
			"  its own lock, not a share of the relay's. Until Warren exports a\n"+
			"  second elector, run that work on the leader you already have —\n"+
			"  inside the function you pass to the relay's Lead — or give it a\n"+
			"  lock of its own with pg_try_advisory_lock on a key you choose.",
		key))
}

var _ outbox.Elector = advisoryLock{}

// Lead blocks until this replica holds the lock, then runs fn with a context
// cancelled the moment leadership is lost.
//
// It says so, at INFO, on every transition: taken, stood down, lost. A
// follower that reports nothing is indistinguishable from a follower that is
// wedged, and this is the one component whose whole job is not to run on
// most replicas.
func (l advisoryLock) Lead(ctx context.Context, fn func(context.Context) error) error {
	if l.busy != nil && !l.busy.CompareAndSwap(false, true) {
		return errLeadershipContended(l.cfg.key)
	}
	if l.busy != nil {
		defer l.busy.Store(false)
	}
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
		if ctx.Err() == nil {
			// Not shutdown, then: the pool could not give this a connection,
			// and leader-only work is about to not happen for `retry`.
			wlog.FromContext(ctx).WarnContext(ctx, "cannot stand for leadership: no connection",
				"module", ModuleName, "lock_key", l.cfg.key, "error", err.Error())
		}
		return nil
	}
	defer conn.Release()

	// A SESSION-level lock, not a transaction-level one: it must outlive any
	// transaction and be released by the connection dying, which is what makes
	// a crashed leader's lock free itself with no lease and no clock.
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey(l.cfg.key)).Scan(&got); err != nil {
		if ctx.Err() == nil {
			wlog.FromContext(ctx).WarnContext(ctx, "cannot stand for leadership: the lock query failed",
				"module", ModuleName, "lock_key", l.cfg.key, "error", err.Error())
		}
		return nil
	}
	if !got {
		// THE line this file did not have. Standing by is the correct and
		// common state — most replicas are followers — but a component that
		// silently does nothing for the life of a process is the one thing
		// an operator cannot tell from a component that is working.
		l.sayOnce(ctx, false, "standing by: another holder has this leadership",
			"retry_in", l.cfg.retry.String())
		return nil
	}
	l.sayOnce(ctx, true, "leadership taken")
	defer l.sayOnce(context.WithoutCancel(ctx), false, "leadership released")
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
// unit-tested, and the name is on every leadership line this file logs.
//
// An earlier version of this sentence promised "the diagnostic when a lock
// cannot be taken". There was no such diagnostic, and no logging in this
// file at all: a follower stood by in perfect silence for the life of the
// process. The promise is kept now rather than deleted.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64())
}
