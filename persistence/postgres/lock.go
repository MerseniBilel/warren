package postgres

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MerseniBilel/warren/lifecycle"
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
			"  Leader-only work that runs ALONGSIDE the outbox relay needs its own\n"+
			"  leadership. Inject outbox.Electors and name one:\n\n"+
			"      el, err := electors.Elector(\"ticket/sla-sweeper\")\n\n"+
			"  Two leaderships lead at the same time, on the same replica.",
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
		err := errLeadershipContended(l.cfg.key)
		// LOGGED as well as returned. Lead BLOCKS for the duration of
		// leadership, so the obvious spelling is `go el.Lead(ctx, fn)` — and
		// `go f()` has nowhere to put a return value. A field test wrote
		// exactly that and got nine log lines, none of them about
		// leadership, while the component never ran and /readyz said 200.
		// The best diagnostic in this package was reachable only by code
		// that did not need it.
		wlog.FromContext(ctx).ErrorContext(ctx, "leadership contended",
			"module", ModuleName, "lock_key", l.cfg.key, "error", err.Error())
		return err
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

// --- named leaderships ----------------------------------------------------

// electors is the registry behind outbox.Electors: one advisory lock per
// NAME, all on the same connection pool.
//
// It claims names at construction rather than at Lead, so two components
// asking for the same leadership is a boot failure naming both, instead of
// one of them silently never running — which is the defect this whole type
// exists to answer.
type electors struct {
	pool  *pool
	cfg   lockConfig
	mu    sync.Mutex
	byKey map[int64]string // for the collision check, not for lookup
	names map[string]bool
}

func newElectors(p *pool, cfg lockConfig) *electors {
	e := &electors{
		pool:  p,
		cfg:   cfg,
		byKey: map[int64]string{},
		names: map[string]bool{},
	}
	// The relay's own leadership is claimed here, unconditionally, so that
	// Elector("warren/outbox") is a boot failure rather than a component
	// quietly fighting the relay for its lock.
	e.names[cfg.key] = true
	e.byKey[lockKey(cfg.key)] = cfg.key
	return e
}

// relay returns the elector the outbox relay receives — the one holding the
// key WithAdvisoryLock was configured with.
func (e *electors) relay() outbox.Elector { return e.elector(e.cfg.key) }

func (e *electors) Elector(name string) (outbox.Elector, error) {
	if name == "" {
		return nil, errUnnamedLeadership()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if name == e.cfg.key {
		return nil, errReservedLeadership(name)
	}
	if e.names[name] {
		return nil, errLeadershipClaimed(name)
	}
	// FNV-1a is 64 bits, so a collision is vanishingly unlikely and
	// catastrophic: two leaderships would silently share one lock, which is
	// the original defect with no name to blame. It costs one map lookup to
	// refuse instead.
	k := lockKey(name)
	if other, clash := e.byKey[k]; clash {
		return nil, errLeadershipKeyCollision(name, other, k)
	}
	e.names[name] = true
	e.byKey[k] = name
	return e.elector(name), nil
}

// elector builds one, with its own busy and state flags: leaderships are
// independent, so sharing either would make one report the other's
// transitions.
func (e *electors) elector(name string) outbox.Elector {
	cfg := e.cfg
	cfg.key = name
	return advisoryLock{
		pool: e.pool, cfg: cfg,
		busy:  &atomic.Bool{},
		state: &atomic.Int32{},
	}
}

// claimed reports how many leaderships this registry has handed out,
// including the relay's. Each one that LEADS holds a pooled connection for
// as long as it leads.
func (e *electors) claimed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.names)
}

func errUnnamedLeadership() error {
	return diagnostic(
		"✗ a leadership needs a name\n\n" +
			"    Elector(\"\") was called. An unnamed leadership is one that every\n" +
			"    unnamed claimant shares, so two components would contend for it\n" +
			"    and one of them would silently never run.\n\n" +
			"  Name it for the component that leads:\n\n" +
			"      electors.Elector(\"ticket/sla-sweeper\")")
}

func errLeadershipClaimed(name string) error {
	return diagnostic(fmt.Sprintf(
		"✗ the leadership %q is already claimed\n\n"+
			"    Two components in this process asked for the same leadership,\n"+
			"    and a leadership has one holder — so one of them would never\n"+
			"    run, and which one would be decided by whichever constructor\n"+
			"    the container reached first.\n\n"+
			"  Give them separate names. Leaderships are independent: two names\n"+
			"  lead at the same time, on the same replica.",
		name))
}

func errReservedLeadership(name string) error {
	return diagnostic(fmt.Sprintf(
		"✗ %q is the outbox relay's leadership\n\n"+
			"    Taking it would mean competing with the relay for one lock, and\n"+
			"    the loser publishes nothing for the life of the process.\n\n"+
			"  Name your own:\n\n"+
			"      electors.Elector(\"ticket/sla-sweeper\")\n\n"+
			"  Two leaderships run at the same time on the same replica — that\n"+
			"  is what makes them worth naming. If you meant to rename the\n"+
			"  RELAY's, use postgres.LockKey(...).",
		name))
}

func errLeadershipKeyCollision(name, other string, key int64) error {
	return diagnostic(fmt.Sprintf(
		"✗ two leaderships hash to one lock\n\n"+
			"    %q and %q both become advisory lock %d.\n\n"+
			"    Postgres locks an int64, so the name is hashed — and these two\n"+
			"    would share a single leadership without either knowing, which\n"+
			"    is the exact failure naming them was meant to prevent.\n\n"+
			"  Rename one of them.",
		name, other, key))
}

// warnOnPoolPressure says so at boot when the leaderships claimed could take
// a serious share of the pool.
//
// Each LEADING elector holds one pooled connection for as long as it leads —
// the session lock lives on that connection, which is what makes a crashed
// leader's lock free itself with no lease and no clock. On the leader
// replica that is one connection per leadership, permanently, and this file
// has already produced one connection-exhaustion incident: relay waiters
// held pooled connections until an idle service died MaxConns ticks after
// boot, serving nothing, with one INFO line in the log.
//
// It warns rather than refuses, because the right number depends on how many
// of these lead at once and only the operator knows that.
func (e *electors) warnOnPoolPressure(lc lifecycle.Lifecycle, maxConns int32) {
	lc.Append(lifecycle.Hook{
		Name: ModuleName + "/electors",
		OnStart: func(ctx context.Context) error {
			e.mu.Lock()
			names := make([]string, 0, len(e.names))
			for name := range e.names {
				names = append(names, name)
			}
			e.mu.Unlock()
			sort.Strings(names)
			n := len(names)

			// One line naming every leadership this process claimed. It is
			// what makes a MISSING one observable: a component that mints a
			// leadership and is never constructed — because nothing injects
			// it and nobody wrote warren.Eager — boots green, answers 200,
			// and does nothing. Step 3 does not catch that (see warren.md
			// §2.1), so the boot log is where its absence shows.
			wlog.FromContext(ctx).InfoContext(ctx, "leaderships claimed",
				"module", ModuleName, "count", n, "names", strings.Join(names, ", "))

			if maxConns <= 0 || int32(n)*2 <= maxConns {
				return nil
			}
			wlog.FromContext(ctx).WarnContext(ctx, "leaderships could exhaust the connection pool",
				"module", ModuleName,
				"leaderships", n,
				"max_conns", maxConns,
				"names", strings.Join(names, ", "),
				"risk", "each one holds a pooled connection for as long as it LEADS, so on the leader replica they are unavailable to every request",
				"fix", "raise postgres.MaxConns, or run fewer leader-only components")
			return nil
		},
	})
}
