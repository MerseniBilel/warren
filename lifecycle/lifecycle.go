// Package lifecycle starts a service's components in order and stops them in
// reverse, with a deadline and a readiness state.
//
// Hooks are appended by constructors as they are built, so the order they
// arrive in is dependency order — see di/SPEC.md §7. This package records that
// order and never computes one: Start walks it forwards, Stop walks it
// backwards, and that reversal is what stops the connection pool closing
// underneath a consumer that is still committing.
package lifecycle

import (
	"context"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// callerSkip is the number of frames between callSiteOf and the user code a
// message should name: callSiteOf itself, and the exported function that called
// it.
const callerSkip = 2

// Hook is one component's participation in the lifecycle.
//
// Name identifies it in every message this package produces, so it is required
// and should read as the thing being started: "postgres.pool", "http.server".
// Either callback may be nil; both being nil is an error.
type Hook struct {
	OnStart func(context.Context) error
	OnStop  func(context.Context) error
	Name    string
}

// entry is a registered hook and the source position it was registered at.
type entry struct {
	hook Hook
	at   callSite
}

// callSite is a position in the source, used to name a file in an error
// message.
type callSite struct {
	file string
	line int
}

// String returns "file:line", or "" when the site is unknown.
func (s callSite) String() string {
	if s.file == "" {
		return ""
	}

	return s.file + ":" + strconv.Itoa(s.line)
}

// callSiteOf reports the position skip frames above callSiteOf itself.
func callSiteOf(skip int) callSite {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return callSite{}
	}

	return callSite{file: file, line: line}
}

// Hooks is the ordered set of start and stop callbacks a service has
// registered, and the state machine that runs them. Its zero value is not
// usable; construct one with New.
//
// Registration is not safe for concurrent use: hooks are appended from
// constructors, which run on one goroutine during di.Build. State is safe to
// read from any goroutine at any time, because a health endpoint does exactly
// that while Stop is running. Start and Stop are serialised against each other,
// so a SIGTERM that arrives mid-boot is defined behaviour rather than a race.
type Hooks struct {
	// Guarded by mu, which is never held while a hook runs.
	entries  []entry
	problems []error
	late     []entry

	// phase serialises Start against Stop. It is held for the whole of either,
	// so Stop queues behind a boot rather than interleaving with it.
	phase sync.Mutex
	mu    sync.Mutex

	// state is read by health from any goroutine, so it is atomic and never
	// guarded by mu: a readiness probe must not block a shutdown.
	state atomic.Uint32

	// unwindBudget bounds the rollback Start runs. Set once at construction and
	// read without mu, since nothing writes it after New returns.
	unwindBudget time.Duration

	// Guarded by mu. The counters every message in SPEC.md §6 reports, and what
	// the index of the hook in flight is derived from.
	started int
	stopped int
}

// Option configures a Hooks at construction. There is one today and it is
// deliberately not a general extension point, for the reason di.Option is not.
type Option func(*Hooks)

// UnwindBudget bounds the rollback Start runs when a hook fails or the start
// deadline expires.
//
// It exists because that rollback cannot use the context it failed with: a start
// that ran out of time would hand every OnStop a dead context and close nothing.
// The rollback therefore runs detached, and this is what stops a hook that hangs
// during it from hanging the boot. warren supplies the number, as it does for the
// start and stop deadlines; unset, the rollback is unbounded.
//
//	lifecycle.New(lifecycle.UnwindBudget(cfg.ShutdownGrace))
func UnwindBudget(d time.Duration) Option {
	return func(h *Hooks) { h.unwindBudget = d }
}

// New returns an empty Hooks.
func New(opts ...Option) *Hooks {
	h := &Hooks{}

	for _, opt := range opts {
		opt(h)
	}

	return h
}

// Append registers a hook. It is called from a constructor, which is what makes
// the order correct:
//
//	func NewPool(hooks *lifecycle.Hooks, cfg *Config) (*Pool, error) {
//		pool, err := open(cfg.DSN)
//		if err != nil {
//			return nil, err
//		}
//
//		hooks.Append(lifecycle.Hook{
//			Name:    "postgres.pool",
//			OnStart: pool.Ping,
//			OnStop:  func(context.Context) error { pool.Close(); return nil },
//		})
//
//		return pool, nil
//	}
//
// Append returns nothing, for the reason di.Provide does: a malformed hook is
// recorded against its call site and reported by Start, so a constructor's
// happy path stays readable.
func (h *Hooks) Append(hook Hook) {
	e := entry{hook: hook, at: callSiteOf(callerSkip)}

	h.mu.Lock()
	defer h.mu.Unlock()

	// A hook appended after Start cannot happen through the intended path,
	// since constructors run during di.Build. It is recorded anyway and
	// reported by Stop, which is the next place a caller would look.
	if State(h.state.Load()) != StateNew {
		h.late = append(h.late, e)

		return
	}

	switch {
	case hook.Name == "":
		h.problems = append(h.problems, errNoName(e))
	case hook.OnStart == nil && hook.OnStop == nil:
		h.problems = append(h.problems, errNothingToDo(e))
	}

	h.entries = append(h.entries, e)
}

// Close is Append for a component with nothing to start and a closer that
// cannot fail — a pgx pool, an in-memory broker.
//
//	hooks.Append(lifecycle.Close("postgres.pool", pool.Close))
func Close(name string, fn func()) Hook {
	return Hook{
		Name: name,
		OnStop: func(context.Context) error {
			fn()

			return nil
		},
	}
}

// Closer is Close for the io.Closer shape, which is what most of the standard
// library returns — sql.DB, os.File, net.Listener.
//
//	hooks.Append(lifecycle.Closer("postgres.pool", db.Close))
func Closer(name string, fn func() error) Hook {
	return Hook{
		Name:   name,
		OnStop: func(context.Context) error { return fn() },
	}
}

// State reports where the lifecycle is. It is safe to call from any goroutine.
func (h *Hooks) State() State {
	return State(h.state.Load())
}

// Names returns the registered hook names, in start order. It is what
// warren doctor and warren graph read, and what a test asserts ordering on.
func (h *Hooks) Names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	names := make([]string, 0, len(h.entries))
	for _, e := range h.entries {
		names = append(names, e.hook.Name)
	}

	return names
}

// Start runs every OnStart in registration order, which is dependency order.
//
// On the first failure it stops what it already started, in reverse, and
// returns the failure — so a service either reaches Ready or reaches Stopped,
// never something in between.
//
// Any deadline on ctx is the budget for the whole sequence, exactly as it is
// for Stop: a hook still running when it expires is abandoned and named. ctx is
// otherwise the run context, not the boot context di.Build was given. A hook
// that wants to outlive Start — a listener's accept loop — starts a goroutine
// and registers its shutdown as OnStop; it must not retain ctx.
func (h *Hooks) Start(ctx context.Context) error {
	h.phase.Lock()
	defer h.phase.Unlock()

	switch s := h.State(); s {
	case StateStarting, StateReady:
		return errAlreadyStarted(s)
	case StateStopping, StateStopped:
		return errAlreadyStopped(s)
	case StateNew:
	}

	problems := h.registrationProblems()
	if problems != nil {
		return problems
	}

	h.state.Store(uint32(StateStarting))

	finished, err := run(ctx, h.startAll)
	if finished && err == nil {
		h.state.Store(uint32(StateReady))

		return nil
	}

	switch {
	case errors.Is(err, errGoexit):
		err = h.errExited(opStart)
	case !finished || isDeadline(err):
		err = h.errStartDeadline()
	}

	unwound := h.rollback(ctx)
	h.state.Store(uint32(StateStopped))

	return join(err, unwound)
}

// Stop flips readiness to failing, then runs every OnStop in reverse order.
//
// The deadline on ctx is the grace period for the whole sequence. Hooks that
// have not finished when it expires are abandoned rather than waited for, and
// named in the returned error: the process is exiting, and a goroutine that
// outlives it by microseconds costs nothing next to a SIGKILL mid-write.
//
// Stop on a lifecycle that never started, or that already stopped, is a no-op
// returning nil — so `defer hooks.Stop(ctx)` is always safe.
func (h *Hooks) Stop(ctx context.Context) error {
	h.phase.Lock()
	defer h.phase.Unlock()

	switch h.State() {
	case StateNew, StateStopping, StateStopped:
		return nil
	case StateStarting, StateReady:
	}

	// Readiness flips before any hook runs, so a load balancer stops routing
	// before the listener closes.
	h.state.Store(uint32(StateStopping))

	late := h.lateProblems()

	finished, err := run(ctx, h.unwind)
	h.state.Store(uint32(StateStopped))

	switch {
	case errors.Is(err, errGoexit):
		err = h.errExited(opStop)
	case !finished || isDeadline(err):
		err = h.errStopDeadline()
	}

	return join(late, err)
}

// rollback stops what Start opened, detached from the context that failed and
// bounded by UnwindBudget.
//
// It cannot reuse ctx: a start that failed because its deadline expired would
// hand every OnStop a context that is already over, close nothing, and leak every
// connection the boot had already made. That is fx issue #1035, and Kratos's
// detach-then-bound (app.go:105) is the construction copied here.
func (h *Hooks) rollback(ctx context.Context) error {
	unwind := context.WithoutCancel(ctx)

	if h.unwindBudget > 0 {
		var cancel context.CancelFunc

		unwind, cancel = context.WithTimeout(unwind, h.unwindBudget)
		defer cancel()
	}

	finished, err := run(unwind, h.unwind)
	if !finished || isDeadline(err) {
		return h.errStopDeadline()
	}

	return err
}

// run executes fn on its own goroutine and waits for it, or for ctx to expire.
// The bool reports whether fn finished; false means the phase was abandoned.
//
// One goroutine per phase, not per hook: the walk is sequential either way, and
// the hook that overran is named from Hooks.running instead.
func run(ctx context.Context, fn func(context.Context) error) (bool, error) {
	done := make(chan error, 1)

	go func() {
		// A hook that calls runtime.Goexit — which is what t.Fatal inside one
		// does — never returns and never sends, so the phase would block until
		// the deadline and then blame the wrong hook. The deferred send reports
		// it immediately instead.
		finished := false

		defer func() {
			if !finished {
				done <- errGoexit
			}
		}()

		err := fn(ctx)
		finished = true
		done <- err
	}()

	select {
	case err := <-done:
		return true, err
	case <-ctx.Done():
		// The phase may have finished at the same instant. Prefer its result:
		// that is both more accurate than reporting a timeout for work that
		// completed, and deterministic, which a golden file needs.
		select {
		case err := <-done:
			return true, err
		default:
			return false, ctx.Err()
		}
	}
}

// startAll walks the hooks forwards. It runs on the phase goroutine.
func (h *Hooks) startAll(ctx context.Context) error {
	for _, e := range h.snapshot() {
		err := ctx.Err()
		if err != nil {
			return err
		}

		if e.hook.OnStart != nil {
			err = h.call(ctx, e, "starting", e.hook.OnStart)
			if err != nil {
				return h.errStartFailed(e, err)
			}
		}

		h.advance(&h.started)
	}

	return nil
}

// unwind walks backwards over the hooks whose OnStart succeeded, which is every
// hook after a clean start and a prefix of them after a failed one. It runs on
// the phase goroutine.
//
// It does not abort on a failing hook: one component refusing to close must not
// strand the rest open.
func (h *Hooks) unwind(ctx context.Context) error {
	entries := h.snapshot()

	h.mu.Lock()
	from := h.started - 1
	h.mu.Unlock()

	var failures []error

	for i := from; i >= 0; i-- {
		err := ctx.Err()
		if err != nil {
			return err
		}

		e := entries[i]

		if e.hook.OnStop != nil {
			err = h.call(ctx, e, "stopping", e.hook.OnStop)
			if err != nil {
				failures = append(failures, h.errStopFailed(e, err))
			}
		}

		h.advance(&h.stopped)
	}

	return join(failures...)
}

// call runs one hook, logging it either side and converting a panic into an
// ordinary error so that the phase can name the component that raised it.
func (h *Hooks) call(
	ctx context.Context,
	e entry,
	phase string,
	fn func(context.Context) error,
) error {
	logger := log.From(ctx)
	logger.DebugContext(ctx, "lifecycle: "+phase+" hook", slog.String("hook", e.hook.Name))

	began := time.Now()
	err := recovered(ctx, fn)

	logger.DebugContext(ctx, "lifecycle: "+phase+" hook finished",
		slog.String("hook", e.hook.Name),
		slog.Duration("took", time.Since(began)),
		log.Err(err),
	)

	return err
}

// recovered calls fn and converts a panic into an error. AGENT.md forbids panic
// in library code; it says nothing about recovering from a user's, and a raw
// panic with a stack full of runtime frames names no component.
func recovered(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p}
		}
	}()

	return fn(ctx)
}

// snapshot copies the registered hooks so that a phase walks a stable slice.
func (h *Hooks) snapshot() []entry {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]entry(nil), h.entries...)
}

// advance increments one of the phase counters that the messages report.
func (h *Hooks) advance(counter *int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	*counter++
}

// join returns one problem as itself and several as a joined error.
//
// The single case is not an optimisation: errors.Join holds several unrelated
// errors, so errors.Detail reports no fields and no fix for a join of one — and
// the fields are where the hook name, the call site, and the fix live. This is
// the rule di/validate.go:21 records, for the same reason.
func join(errs ...error) error {
	live := make([]error, 0, len(errs))

	for _, err := range errs {
		if err != nil {
			live = append(live, err)
		}
	}

	if len(live) == 1 {
		return live[0]
	}

	return errors.Join(live...)
}
