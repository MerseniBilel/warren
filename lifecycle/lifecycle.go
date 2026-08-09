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

	"github.com/MerseniBilel/warren/internal/panics"
)

// Hook is one participant in the application lifecycle. OnStart runs in
// dependency order during boot step 6; OnStop runs in the exact reverse
// during shutdown step 10. A nil OnStart or OnStop makes a stop-only or
// start-only hook; the nil side is skipped.
//
// A hook that PANICS is contained and behaves exactly as one that returns an
// error. A panic in OnStart stops the already-started hooks in reverse, so
// the rollback guarantee holds, and readiness never opens; a panic in OnStop
// does not abandon the drain — the remaining hooks still stop and the panic
// joins the returned errors. Either way the process exits through the
// application's own error path with a "✗ lifecycle hook panicked" block, not
// through a Go stack dump.
//
// A panic in a goroutine the hook SPAWNED is not Warren's to catch — Go
// gives no one but that goroutine the chance — so a hook that starts a loop
// recovers inside it or the process dies. This is the same goroutine whose
// context OnStart's documentation below tells you how to derive.
type Hook struct {
	// Name identifies the hook in logs and in lifecycle errors.
	Name string

	// OnStart runs during startup, in registration order.
	//
	// Its context carries the application's boot-time context VALUES —
	// app.Telemetry among them — and it is CANCELLED as soon as boot
	// finishes. A hook that starts a long-running loop must therefore keep
	// the values and drop the cancellation:
	//
	//	ctx, cancel := context.WithCancel(context.WithoutCancel(bootCtx))
	//
	// Deriving that loop's context from context.Background() instead loses
	// the values SILENTLY: nothing fails, and a consumer built that way
	// begins a new root trace per message and strips traceparent from every
	// event it raises.
	OnStart func(context.Context) error

	// OnStop runs during shutdown, in reverse registration order.
	OnStop func(context.Context) error

	// Timeout bounds OnStart and OnStop individually. Zero means no per-hook
	// timeout; the force-exit deadline still bounds the whole of Stop.
	Timeout time.Duration
}

// Lifecycle collects hooks at boot and runs them in order on the way up and
// in reverse on the way down. A lifecycle runs once: Start a second time —
// or after Stop — is an error.
type Lifecycle interface {
	// Append registers a hook. Registration order is start order. It is safe
	// to call from within a running hook — a hook whose OnStart appends
	// another hook adds it to the end of the sequence, and it is started in
	// its turn.
	Append(Hook)

	// Start runs every hook's OnStart in registration order. It is boot step
	// 6. If a hook fails, the already-started hooks are stopped in reverse
	// and the returned error carries the failure first, then any rollback
	// failures. If Stop is called while Start is running, Start abandons the
	// boot after the in-flight hook and Stop unwinds what had started.
	// Readiness opens only when Start returns nil.
	Start(context.Context) error

	// Stop runs every started hook's OnStop in reverse registration order,
	// bounded by the force-exit deadline. It is shutdown step 10. Its first
	// action is closing readiness — before the first OnStop runs, and even
	// when Start is still mid-boot. A failing hook does not stop the
	// sequence; every failure is returned joined. Stop is idempotent.
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

const (
	stateIdle = iota
	stateStarting
	stateStarted
	stateStopping
	stateStopped
)

type lifecycle struct {
	// mu guards hooks, started, and state — data only, never held while a
	// hook runs, so a hook's OnStart may Append without deadlocking.
	mu      sync.Mutex
	hooks   []Hook
	started int // hooks whose OnStart completed; the range Stop unwinds
	state   int

	// runMu serializes hook execution: Start's loop and Stop's unwind never
	// run hooks concurrently with each other.
	runMu sync.Mutex

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
	if l.state != stateIdle {
		l.mu.Unlock()
		return errStartAgain()
	}
	l.state = stateStarting
	l.mu.Unlock()

	l.runMu.Lock()
	for i := 0; ; i++ {
		l.mu.Lock()
		if l.state != stateStarting {
			// Stop arrived mid-boot: abandon; Stop owns the unwind.
			l.mu.Unlock()
			l.runMu.Unlock()
			return errStoppedDuringStart()
		}
		if i >= len(l.hooks) {
			l.state = stateStarted
			l.ready.Store(true)
			l.mu.Unlock()
			l.runMu.Unlock()
			return nil
		}
		h := l.hooks[i]
		l.mu.Unlock()

		if h.OnStart != nil {
			if err := runHook(ctx, h, h.OnStart, "OnStart"); err != nil {
				l.mu.Lock()
				l.state = stateStopped
				l.mu.Unlock()
				rollback := l.unwind(ctx)
				l.runMu.Unlock()
				return errors.Join(err, rollback)
			}
		}
		l.mu.Lock()
		l.started = i + 1
		l.mu.Unlock()
	}
}

func (l *lifecycle) Stop(ctx context.Context) error {
	l.mu.Lock()
	// Readiness closes first — before the first OnStop, and even mid-boot:
	// a Start still running will notice the state change, abandon, and never
	// re-open it.
	l.ready.Store(false)
	if l.state != stateStopped {
		l.state = stateStopping
	}
	l.mu.Unlock()

	l.runMu.Lock()
	defer l.runMu.Unlock()
	err := l.unwind(ctx)
	l.mu.Lock()
	l.state = stateStopped
	l.mu.Unlock()
	return err
}

// unwind stops the started hooks in reverse under the force-exit deadline.
// Callers hold runMu. It is idempotent: started drains to zero.
func (l *lifecycle) unwind(ctx context.Context) error {
	deadline, cancel := context.WithTimeout(ctx, l.forceExit)
	defer cancel()

	var errs []error
	for {
		l.mu.Lock()
		i := l.started - 1
		if i < 0 {
			l.mu.Unlock()
			break
		}
		h := l.hooks[i]
		l.started = i
		l.mu.Unlock()

		if deadline.Err() != nil {
			// The deadline expired before this hook ran: it and everything
			// below it are unfinished.
			errs = append(errs, errForceExit(l.forceExit, l.namesFrom(i)))
			break
		}
		if h.OnStop == nil {
			continue
		}
		if err := runHook(deadline, h, h.OnStop, "OnStop"); err != nil {
			var abandoned *hookAbandonedError
			if errors.As(err, &abandoned) && deadline.Err() != nil {
				// The force-exit deadline fired while this hook was still
				// running: it is genuinely unfinished, as is everything
				// below it.
				errs = append(errs, errForceExit(l.forceExit, l.namesFrom(i)))
				break
			}
			// The hook returned — its own failure or its own timeout. Keep
			// its real error; it is not "still stopping".
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// namesFrom names the hooks from index i down to 0 — the ones that had not
// finished when the force-exit deadline expired, current first.
func (l *lifecycle) namesFrom(i int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var names []string
	for ; i >= 0; i-- {
		names = append(names, l.hooks[i].Name)
	}
	return names
}

// runHook runs fn under the hook's own timeout, in a goroutine so that a
// hook that ignores its context cannot wedge the sequence past its bounds.
// A hook abandoned at a deadline leaks its goroutine: acceptable on the
// force-exit path (the process is about to exit) and a documented cost on a
// Start timeout, where the goroutine may complete its side effect
// unobserved — which is why OnStart should respect its context.
func runHook(ctx context.Context, h Hook, fn func(context.Context) error, phase string) error {
	hctx := ctx
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}

	done := make(chan error, 1)
	go func() {
		// The containment is INSIDE the goroutine because that is the only
		// place the panic can be caught: it is raised on a stack Warren
		// created, and the user's own main never sees it. Without this a
		// panicking OnStart skipped the rollback three documents guarantee and
		// a panicking OnStop abandoned the rest of the drain — the process
		// died holding whatever the hooks below it had acquired.
		var err error
		if caught := panics.Do(func() { err = fn(hctx) }); caught != nil {
			err = errHookPanicked(h.Name, phase, caught)
		}
		done <- err
	}()

	classify := func(err error) error {
		var panicked *hookPanickedError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &panicked):
			// Already the diagnostic, and already the right classification:
			// the goroutine RETURNED, so this is neither a timeout nor an
			// abandonment, and wrapping it would bury the block.
			return err
		case errors.Is(err, context.DeadlineExceeded) && h.Timeout > 0 && ctx.Err() == nil:
			// The hook's own deadline, not an expired parent propagating
			// through.
			return errHookTimeout(h.Name, phase, h.Timeout)
		default:
			return errHookFailed(h.Name, phase, err)
		}
	}

	select {
	case err := <-done:
		return classify(err)
	case <-hctx.Done():
		// The deadline fired, but the hook may be mid-return — waking on
		// ctx.Done and returning its own result is exactly the well-behaved
		// pattern. One short grace window tells that apart from a hook that
		// is genuinely wedged, so a hook that DID return is never reported
		// as "still stopping" and its real error is never swallowed.
		grace := time.NewTimer(returnGrace)
		defer grace.Stop()
		select {
		case err := <-done:
			return classify(err)
		case <-grace.C:
			// The hook goroutine is still running and is now abandoned.
			if h.Timeout > 0 && ctx.Err() == nil {
				return errHookTimeout(h.Name, phase, h.Timeout)
			}
			return errHookAbandoned(h.Name, phase, ctx.Err())
		}
	}
}

// returnGrace is how long a hook gets to deliver its result after its
// deadline fires before it is declared abandoned. It exists only to
// disambiguate "returned at the deadline" from "never returns"; it is not a
// second timeout.
const returnGrace = 100 * time.Millisecond

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

// hookPanickedError is a hook that panicked rather than returning. Its
// message is the whole rendered diagnostic — it is not wrapped in "hook %q
// failed", because the block already says which hook, which phase, and what
// the process did about it.
type hookPanickedError struct {
	name  string
	phase string
	text  string
}

func (e *hookPanickedError) Error() string { return e.text }

// errHookPanicked renders a hook's panic as the boot- or drain-time
// diagnostic. The two phases differ in one paragraph, and the difference is
// the whole point: OnStart rolled back, OnStop did not abandon the rest.
func errHookPanicked(name, phase string, caught *panics.Caught) error {
	var detail string
	if phase == "OnStop" {
		detail = fmt.Sprintf("Hook %q panicked during OnStop. The remaining hooks were\n", name) +
			"stopped anyway — shutdown does not abandon the drain because one hook\n" +
			"failed — and this is reported with whatever else failed on the way down.\n" +
			"\n" +
			"A panic here is one of two things: a refusal Warren raises on purpose — the\n" +
			"message above says so when it is — or a bug in the hook. The resources this\n" +
			"hook owns may not have been released."
	} else {
		detail = fmt.Sprintf("Hook %q panicked during %s, so Warren stopped the hooks that had\n", name, phase) +
			"already started, in reverse order, and the process never opened readiness.\n" +
			"A hook that RETURNS an error gets the same rollback; this one did not get\n" +
			"the chance to return.\n" +
			"\n" +
			"A panic here is one of two things: a refusal Warren raises on purpose — the\n" +
			"message above says so when it is — or a bug in the hook. Either way the\n" +
			"process never served a request."
	}
	// This package's own frames are the goroutine that ran the hook, which is
	// never the answer to "where did this come from".
	block := caught.Diagnostic("lifecycle hook panicked", detail, "github.com/MerseniBilel/warren/lifecycle.")
	return &hookPanickedError{name: name, phase: phase, text: block.Error()}
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

// hookAbandonedError marks a hook whose goroutine was still running when its
// context died — it never returned, unlike a failure or a per-hook timeout.
type hookAbandonedError struct {
	name  string
	phase string
	cause error
}

func (e *hookAbandonedError) Error() string {
	return fmt.Sprintf("lifecycle: hook %q was abandoned during %s: %v", e.name, e.phase, e.cause)
}

func (e *hookAbandonedError) Unwrap() error { return e.cause }

func errHookAbandoned(name, phase string, cause error) error {
	return &hookAbandonedError{name: name, phase: phase, cause: cause}
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

func errStartAgain() error {
	return errors.New("lifecycle: Start called again — a lifecycle starts once; construct a new one")
}

func errStoppedDuringStart() error {
	return errors.New("lifecycle: Stop arrived during Start — boot abandoned, readiness never opened")
}
