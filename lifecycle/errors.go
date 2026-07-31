package lifecycle

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/MerseniBilel/warren/errors"
)

// The two ops every message in SPEC.md §6 carries. They are the public entry
// points, not the internal walk, because that is what a reader can act on.
const (
	opStart = "lifecycle.Start"
	opStop  = "lifecycle.Stop"
)

// errGoexit reports a hook that ended its goroutine without returning, which is
// what runtime.Goexit — and therefore t.Fatal inside a hook — does. It never
// reaches a caller: Start and Stop replace it with a message naming the hook.
var errGoexit = errors.Internal("a hook exited without returning")

// panicError carries a recovered panic so that a phase can render it as the
// message in SPEC.md §6.9.
type panicError struct{ value any }

func (p *panicError) Error() string { return fmt.Sprint(p.value) }

// isDeadline reports whether err is a context ending, which is how an abandoned
// phase arrives from run and from a walk's own ctx.Err check.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// isPanic reports whether err carries a recovered panic.
func isPanic(err error) bool {
	var p *panicError

	return errors.As(err, &p)
}

// progress is a phase's counters, read together so that a message renders one
// consistent view of them.
type progress struct {
	started int
	stopped int
	total   int
}

func (h *Hooks) progress() progress {
	h.mu.Lock()
	defer h.mu.Unlock()

	return progress{
		started: h.started,
		stopped: h.stopped,
		total:   len(h.entries),
	}
}

// inFlight is the index of the hook a phase was on when it was abandoned.
//
// It is derived from the counters rather than recorded in a field of its own,
// which fx needs (RunningHookCaller, lifecycle.go:347) and we do not: a counter
// is already correct when the phase goroutine never ran at all, and a field is
// still -1. That case is reached by every already-expired context, so it is the
// common one rather than the exotic one.
func (p progress) inFlight(op string) int {
	// started counts only hooks that succeeded, so it is the index of the one
	// that did not. The reverse walk begins at started-1 and has completed
	// stopped of them.
	if op == opStop {
		return p.started - 1 - p.stopped
	}

	return p.started
}

// countOf renders "2 of 5 hooks".
func countOf(done, total int) string {
	return strconv.Itoa(done) + " of " + strconv.Itoa(total) + " hooks"
}

// namesFrom renders the hooks from i to the end, in start order — the ones a
// start deadline never reached.
func (h *Hooks) namesFrom(i int) string {
	entries := h.snapshot()
	if i < 0 || i >= len(entries) {
		return ""
	}

	names := make([]string, 0, len(entries)-i)
	for _, e := range entries[i:] {
		names = append(names, e.hook.Name)
	}

	return strings.Join(names, ", ")
}

// namesDownFrom renders the hooks from i back to the first, in stop order — the
// one a stop deadline abandoned, and the ones it never reached.
func (h *Hooks) namesDownFrom(i int) string {
	entries := h.snapshot()
	if i < 0 || i >= len(entries) {
		return ""
	}

	names := make([]string, 0, i+1)
	for j := i; j >= 0; j-- {
		names = append(names, entries[j].hook.Name)
	}

	return strings.Join(names, ", ")
}

// registrationProblems returns the malformed registrations recorded by Append,
// which Start reports rather than starting anything (SPEC.md §5.2).
func (h *Hooks) registrationProblems() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return join(h.problems...)
}

// errNoName is SPEC.md §6.5.
func errNoName(e entry) error {
	return errors.Invalid("a hook was registered without a name").
		Op(opStart).
		Field("registered", e.at.String()).
		Fix("name the hook for the thing it starts, as in Name: %q — "+
			"every message in this package identifies a hook by its name",
			"orders.scheduler")
}

// errNothingToDo is SPEC.md §6.6.
func errNothingToDo(e entry) error {
	return errors.Invalid("the hook %s has neither OnStart nor OnStop", e.hook.Name).
		Op(opStart).
		Field("registered", e.at.String()).
		Fix("give it a callback, or do not register it")
}

// errAlreadyStarted is SPEC.md §6.7.
func errAlreadyStarted(s State) error {
	return errors.Invalid("already started").
		Op(opStart).
		Field("state", s).
		Fix("warren.Run starts the lifecycle once; a service should not call Start itself")
}

// errAlreadyStopped is SPEC.md §6.11. It is reachable only because StateNew and
// StateStopped are different values.
func errAlreadyStopped(s State) error {
	return errors.Invalid("this lifecycle has already stopped").
		Op(opStart).
		Field("state", s).
		Fix("restart is not supported — a process that wants to come back up is a " +
			"new process, which is what an orchestrator is for")
}

// lateProblems is SPEC.md §6.8: hooks appended after Start never ran.
func (h *Hooks) lateProblems() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.late) == 0 {
		return nil
	}

	err := errors.Invalid("%d hook was appended after the lifecycle started, and never ran",
		len(h.late)).
		Op(opStop)

	for _, e := range h.late {
		err = err.Field("hook", e.hook.Name).Field("registered", e.at.String())
	}

	return err.Fix("append hooks from a constructor, which runs during di.Build — before Start")
}

// errStartFailed is SPEC.md §6.1, or §6.9 when the hook panicked.
func (h *Hooks) errStartFailed(e entry, cause error) error {
	p := h.progress()

	msg, fix := "starting "+e.hook.Name+" failed",
		"read the cause above: the hook reported it, not the lifecycle"
	if isPanic(cause) {
		msg, fix = "the hook "+e.hook.Name+" panicked",
			"this is a bug in the hook, not in the lifecycle"
	}

	return errors.Internal("%s", msg).
		Wrapping(cause).
		Op(opStart).
		Field("hook", e.hook.Name).
		Field("registered", e.at.String()).
		Field("started", countOf(p.started, p.total)+", rolled back").
		Fix("%s", fix)
}

// errStopFailed is SPEC.md §6.4, or §6.9 when the hook panicked. Unlike a start
// failure it does not end the phase: Stop runs every remaining hook.
func (h *Hooks) errStopFailed(e entry, cause error) error {
	p := h.progress()

	msg, fix := "stopping "+e.hook.Name+" failed",
		"read the cause above. Stop continues through a failure by design: one "+
			"component refusing to close must not strand the rest open"
	if isPanic(cause) {
		msg, fix = "the hook "+e.hook.Name+" panicked",
			"this is a bug in the hook, not in the lifecycle"
	}

	return errors.Internal("%s", msg).
		Wrapping(cause).
		Op(opStop).
		Field("hook", e.hook.Name).
		Field("registered", e.at.String()).
		Field("stopped", countOf(p.stopped, p.total)+", continuing").
		Fix("%s", fix)
}

// errStartDeadline is SPEC.md §6.10.
func (h *Hooks) errStartDeadline() error {
	p := h.progress()
	unstarted := h.namesFrom(p.inFlight(opStart))

	return errors.Internal("the start deadline expired with %d hooks unstarted",
		p.total-p.started).
		Op(opStart).
		Field("unstarted", unstarted).
		Field("started", countOf(p.started, p.total)+", rolled back").
		Fix("raise the start deadline, or find what is slow to open — a pool that " +
			"dials for two seconds at boot is the pool's problem to fix")
}

// errStopDeadline is SPEC.md §6.3. It names the hook that overran and the ones
// never reached, because both are things the operator has to explain.
func (h *Hooks) errStopDeadline() error {
	p := h.progress()
	unfinished := h.namesDownFrom(p.inFlight(opStop))

	// Counted against what was started rather than what was registered: after a
	// clean start the two are equal, and during a rollback only the hooks that
	// opened something ever needed stopping.
	return errors.Internal("the grace period expired with %d hooks unfinished",
		p.started-p.stopped).
		Op(opStop).
		Field("unfinished", unfinished).
		Field("stopped", countOf(p.stopped, p.started)).
		Fix("raise the shutdown grace period, or find what is not draining — a hook " +
			"that never returns is usually waiting on a connection nobody closed")
}

// errExited names the hook that ended its goroutine without returning, which
// would otherwise block its phase until the deadline and then blame the wrong
// component. SPEC.md §6.12.
func (h *Hooks) errExited(op string) error {
	p := h.progress()

	name := "a hook"
	site := ""
	i := p.inFlight(op)

	if entries := h.snapshot(); i >= 0 && i < len(entries) {
		name = entries[i].hook.Name
		site = entries[i].at.String()
	}

	return errors.Internal("the hook %s exited without returning", name).
		Op(op).
		Field("hook", name).
		Field("registered", site).
		Fix("a hook must return; calling runtime.Goexit inside one — which is what " +
			"t.Fatal does — ends its goroutine and strands the phase")
}
