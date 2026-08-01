// Package lifecycle owns ordered startup and shutdown, readiness gating, and
// drain.
//
// Hooks start in registration order — which is dependency order, because
// hooks are appended as their owning singletons are instantiated in
// topological order — and stop in the exact reverse. Readiness closes before
// the first OnStop runs, so the load balancer drains before anything stops:
// the ordering most hand-rolled Go services get backwards.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hook is one participant in the application lifecycle. OnStart runs in
// dependency order during boot step 6; OnStop runs in the exact reverse
// during shutdown step 10. A nil OnStart or OnStop makes a stop-only or
// start-only hook; the nil side is skipped.
type Hook struct {
	// Name identifies the hook in logs and in lifecycle errors.
	Name string

	// OnStart runs during startup, in registration order.
	OnStart func(context.Context) error

	// OnStop runs during shutdown, in reverse registration order.
	OnStop func(context.Context) error

	// Timeout bounds OnStart and OnStop individually. Zero means no per-hook
	// timeout; the force-exit deadline still bounds the whole of Stop.
	Timeout time.Duration
}

// Lifecycle collects hooks at boot and runs them in order on the way up and
// in reverse on the way down.
type Lifecycle interface {
	// Append registers a hook. Registration order is start order.
	Append(Hook)

	// Start runs every hook's OnStart in registration order. It is boot step
	// 6. If a hook fails, the already-started hooks are stopped in reverse
	// and the returned error carries the failure first, then any rollback
	// failures. Readiness opens only when Start returns nil.
	Start(context.Context) error

	// Stop runs every started hook's OnStop in reverse registration order,
	// bounded by the force-exit deadline. It is shutdown step 10. Its first
	// action is closing readiness — before the first OnStop runs. A failing
	// hook does not stop the sequence; every failure is returned joined.
	Stop(context.Context) error

	// Ready reports the readiness state warren/health serves: false until
	// Start returns nil, true until Stop begins.
	Ready() bool
}

// Option configures a Lifecycle.
type Option func(*lifecycle)

// ForceExitDeadline bounds the whole of Stop — every OnStop together, not
// each alone. When it expires, Stop returns immediately, naming the hooks
// that had not finished. The default is 30 seconds.
func ForceExitDeadline(d time.Duration) Option {
	return func(l *lifecycle) { l.forceExit = d }
}

// New returns an empty Lifecycle with the default 30s force-exit deadline.
func New(opts ...Option) Lifecycle {
	l := &lifecycle{forceExit: 30 * time.Second}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

type lifecycle struct {
	mu        sync.Mutex
	hooks     []Hook
	started   int // hooks whose OnStart completed; the range Stop unwinds
	forceExit time.Duration
	ready     atomic.Bool
}

func (l *lifecycle) Append(h Hook) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hooks = append(l.hooks, h)
}

func (l *lifecycle) Ready() bool { return l.ready.Load() }

func (l *lifecycle) Start(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, h := range l.hooks {
		if h.OnStart != nil {
			if err := runHook(ctx, h, h.OnStart, "OnStart"); err != nil {
				rollback := l.stopLocked(ctx)
				return errors.Join(err, rollback)
			}
		}
		l.started = i + 1
	}
	l.ready.Store(true)
	return nil
}

func (l *lifecycle) Stop(ctx context.Context) error {
	// Readiness closes before the first OnStop runs — shutdown step 9. The
	// load balancer must drain before anything stops.
	l.ready.Store(false)

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopLocked(ctx)
}

// stopLocked unwinds the started hooks in reverse under the force-exit
// deadline. It is shared by Stop and by Start's failure rollback.
func (l *lifecycle) stopLocked(ctx context.Context) error {
	deadline, cancel := context.WithTimeout(ctx, l.forceExit)
	defer cancel()

	var errs []error
	for i := l.started - 1; i >= 0; i-- {
		if deadline.Err() != nil {
			errs = append(errs, errForceExit(l.forceExit, l.unfinishedLocked(i)))
			break
		}
		h := l.hooks[i]
		l.started = i
		if h.OnStop == nil {
			continue
		}
		if err := runHook(deadline, h, h.OnStop, "OnStop"); err != nil {
			if deadline.Err() != nil && !isTimeout(err) {
				errs = append(errs, errForceExit(l.forceExit, l.unfinishedLocked(i)))
				break
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// unfinishedLocked names the hooks from index i down — the ones that had not
// finished when the force-exit deadline expired.
func (l *lifecycle) unfinishedLocked(i int) []string {
	var names []string
	for ; i >= 0; i-- {
		names = append(names, l.hooks[i].Name)
	}
	return names
}

// runHook runs fn under the hook's own timeout, in a goroutine so that a
// hook that ignores its context cannot wedge the sequence past its bounds.
// A hook abandoned at its deadline leaks its goroutine; the force-exit
// deadline exists because the process is about to exit anyway.
func runHook(ctx context.Context, h Hook, fn func(context.Context) error, phase string) error {
	hctx := ctx
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}

	done := make(chan error, 1)
	go func() { done <- fn(hctx) }()

	select {
	case err := <-done:
		switch {
		case err == nil:
			return nil
		case errors.Is(err, context.DeadlineExceeded) && h.Timeout > 0 && ctx.Err() == nil:
			// The hook's own deadline, not an expired parent propagating
			// through.
			return errHookTimeout(h.Name, phase, h.Timeout)
		default:
			return errHookFailed(h.Name, phase, err)
		}
	case <-hctx.Done():
		if h.Timeout > 0 && ctx.Err() == nil {
			return errHookTimeout(h.Name, phase, h.Timeout)
		}
		return errHookFailed(h.Name, phase, ctx.Err())
	}
}

func isTimeout(err error) bool {
	var t *hookTimeoutError
	return errors.As(err, &t)
}

type hookFailedError struct {
	name  string
	phase string
	cause error
}

func (e *hookFailedError) Error() string {
	return fmt.Sprintf("lifecycle: hook %q failed during %s: %v", e.name, e.phase, e.cause)
}

func (e *hookFailedError) Unwrap() error { return e.cause }

func errHookFailed(name, phase string, cause error) error {
	return &hookFailedError{name: name, phase: phase, cause: cause}
}

type hookTimeoutError struct {
	name    string
	phase   string
	timeout time.Duration
}

func (e *hookTimeoutError) Error() string {
	return fmt.Sprintf("lifecycle: hook %q exceeded its %v timeout during %s — raise Hook.Timeout or make the hook respect its context", e.name, e.timeout, e.phase)
}

func errHookTimeout(name, phase string, timeout time.Duration) error {
	return &hookTimeoutError{name: name, phase: phase, timeout: timeout}
}

type forceExitError struct {
	deadline   time.Duration
	unfinished []string
}

func (e *forceExitError) Error() string {
	names := make([]string, len(e.unfinished))
	for i, n := range e.unfinished {
		names[i] = strconv.Quote(n)
	}
	return fmt.Sprintf("lifecycle: force-exit deadline (%v) expired with hooks still stopping: %s — these hooks must respect their context's cancellation", e.deadline, strings.Join(names, ", "))
}

func errForceExit(deadline time.Duration, unfinished []string) error {
	return &forceExitError{deadline: deadline, unfinished: unfinished}
}
