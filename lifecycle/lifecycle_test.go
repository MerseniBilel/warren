package lifecycle_test

import (
	"context"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/lifecycle"
)

var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("error text does not match golden file %s\ngot:  %q\nwant: %q", path, got, want)
	}
}

// journal records hook activity in order, safely across the runner's
// goroutines.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(entry string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, entry)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.entries)
}

func hook(name string, j *journal) lifecycle.Hook {
	return lifecycle.Hook{
		Name:    name,
		OnStart: func(context.Context) error { j.add("start " + name); return nil },
		OnStop:  func(context.Context) error { j.add("stop " + name); return nil },
	}
}

func TestStartOrderAndReverseStopOrder(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New()
	l.Append(hook("A", j))
	l.Append(hook("B", j))
	l.Append(hook("C", j))

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	want := []string{"start A", "start B", "start C", "stop C", "stop B", "stop A"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("hook order = %v, want %v — start in order, stop in exact reverse", got, want)
	}
}

func TestReadiness(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	readyDuringStop := true
	l.Append(lifecycle.Hook{
		Name:   "observer",
		OnStop: func(context.Context) error { readyDuringStop = l.Ready(); return nil },
	})

	if l.Ready() {
		t.Error("Ready() = true before Start")
	}
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !l.Ready() {
		t.Error("Ready() = false after a successful Start — readiness opens at step 7")
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if readyDuringStop {
		t.Error("Ready() = true while an OnStop ran — readiness must close before the first OnStop")
	}
	if l.Ready() {
		t.Error("Ready() = true after Stop")
	}
}

func TestFailingStartRollsBackInReverse(t *testing.T) {
	t.Parallel()

	j := &journal{}
	boom := stderrors.New("connection refused")
	l := lifecycle.New()
	l.Append(hook("A", j))
	l.Append(hook("B", j))
	l.Append(lifecycle.Hook{
		Name:    "cache",
		OnStart: func(context.Context) error { return boom },
	})

	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("Start with a failing hook returned nil")
	}
	if !stderrors.Is(err, boom) {
		t.Errorf("Start error does not wrap the hook's cause: %v", err)
	}
	if l.Ready() {
		t.Error("Ready() = true after a failed Start — readiness must not open")
	}
	want := []string{"start A", "start B", "stop B", "stop A"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("activity = %v, want %v — already-started hooks stop in reverse", got, want)
	}
}

func TestStopContinuesAndAggregates(t *testing.T) {
	t.Parallel()

	j := &journal{}
	flushErr := stderrors.New("flush failed")
	closeErr := stderrors.New("close failed")
	l := lifecycle.New()
	l.Append(lifecycle.Hook{Name: "pool", OnStop: func(context.Context) error { j.add("stop pool"); return closeErr }})
	l.Append(hook("consumer", j))
	l.Append(lifecycle.Hook{Name: "relay", OnStop: func(context.Context) error { j.add("stop relay"); return flushErr }})

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := l.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop with failing hooks returned nil")
	}
	if !stderrors.Is(err, flushErr) || !stderrors.Is(err, closeErr) {
		t.Errorf("Stop error does not carry every failure joined: %v", err)
	}
	want := []string{"start consumer", "stop relay", "stop consumer", "stop pool"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("activity = %v, want %v — a failing hook must not stop the sequence", got, want)
	}
}

func TestNilSidesAreSkipped(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New()
	l.Append(lifecycle.Hook{Name: "start-only", OnStart: func(context.Context) error { j.add("start start-only"); return nil }})
	l.Append(lifecycle.Hook{Name: "stop-only", OnStop: func(context.Context) error { j.add("stop stop-only"); return nil }})

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := []string{"start start-only", "stop stop-only"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("activity = %v, want %v — nil sides are skipped, not errors", got, want)
	}
}

func TestPerHookTimeout(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Append(lifecycle.Hook{
		Name:    "cache",
		Timeout: 50 * time.Millisecond,
		// Respects its context: blocks until the per-hook deadline fires.
		OnStart: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	})

	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("Start with a hook exceeding its timeout returned nil")
	}
	assertGolden(t, "hook_start_timeout", err.Error())
}

func TestHookFailureText(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Append(lifecycle.Hook{
		Name:    "cache",
		OnStart: func(context.Context) error { return stderrors.New("connection refused") },
	})
	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil")
	}
	assertGolden(t, "hook_start_failed", err.Error())

	l2 := lifecycle.New()
	l2.Append(lifecycle.Hook{
		Name:   "relay",
		OnStop: func(context.Context) error { return stderrors.New("flush failed") },
	})
	if err := l2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = l2.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop returned nil")
	}
	assertGolden(t, "hook_stop_failed", err.Error())
}

func TestForceExitDeadline(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New(lifecycle.ForceExitDeadline(50 * time.Millisecond))
	l.Append(hook("never-reached", j))
	l.Append(lifecycle.Hook{
		Name: "wedged",
		// Ignores every deadline: only returns when the test's own context
		// is cancelled at cleanup, so no goroutine outlives the test.
		OnStop: func(context.Context) error { <-t.Context().Done(); return nil },
	})

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := l.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop past the force-exit deadline returned nil")
	}
	assertGolden(t, "force_exit", err.Error())
	if slices.Contains(j.all(), "stop never-reached") {
		t.Error("a hook ran after the force-exit deadline expired")
	}
}

func TestStartTwiceIsAnError(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("second Start returned nil — every OnStart would run twice")
	}
	assertGolden(t, "start_again", err.Error())
}

// TestStopDuringStart covers the review's readiness inversion: SIGTERM
// arriving mid-boot must leave readiness closed forever — Start must not
// re-open it after Stop has begun.
func TestStopDuringStart(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New()
	hookRunning := make(chan struct{})
	release := make(chan struct{})
	l.Append(lifecycle.Hook{
		Name: "slow-boot",
		OnStart: func(context.Context) error {
			j.add("start slow-boot")
			close(hookRunning)
			<-release
			return nil
		},
		OnStop: func(context.Context) error { j.add("stop slow-boot"); return nil },
	})
	l.Append(hook("never-started", j))

	startErr := make(chan error, 1)
	go func() { startErr <- l.Start(context.Background()) }()
	<-hookRunning

	stopErr := make(chan error, 1)
	go func() { stopErr <- l.Stop(context.Background()) }()
	// Stop's first section (readiness + state, under mu) has provably run
	// once its goroutine is parked on runMu waiting for Start's loop — spin
	// on the goroutine dump until it is, then release the boot hook. No
	// sleeps: this waits on an observable state, not on wall time.
	buf := make([]byte, 1<<20)
	for {
		stacks := string(buf[:runtime.Stack(buf, true)])
		if strings.Contains(stacks, "lifecycle.(*lifecycle).Stop") && strings.Contains(stacks, "sync.(*Mutex).Lock") {
			break
		}
		runtime.Gosched()
	}
	close(release)

	if err := <-startErr; err == nil {
		t.Error("Start returned nil though Stop arrived mid-boot")
	} else {
		assertGolden(t, "stopped_during_start", err.Error())
	}
	if err := <-stopErr; err != nil {
		t.Errorf("Stop: %v", err)
	}
	if l.Ready() {
		t.Error("Ready() = true after Stop — a stopped process advertises ready")
	}
	want := []string{"start slow-boot", "stop slow-boot"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("activity = %v, want %v — Stop unwinds what started; never-started stays untouched", got, want)
	}
}

// TestAppendFromInsideAHook covers the review's deadlock: a hook whose
// OnStart appends another hook is the documented adapter pattern and must
// neither deadlock nor lose the appended hook.
func TestAppendFromInsideAHook(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New()
	l.Append(lifecycle.Hook{
		Name: "adapter",
		OnStart: func(context.Context) error {
			j.add("start adapter")
			l.Append(hook("late", j))
			return nil
		},
	})

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start deadlocked or failed: %v", err)
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := []string{"start adapter", "start late", "stop late"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("activity = %v, want %v — the appended hook starts in its turn", got, want)
	}
}

// TestForceExitKeepsARealHookError covers the review's misreport: a hook
// that respects its context and returns its own failure at the deadline is
// not "still stopping" — its error must survive, and only genuinely
// unfinished hooks are named.
func TestForceExitKeepsARealHookError(t *testing.T) {
	t.Parallel()

	diskFull := stderrors.New("disk full: could not flush")
	l := lifecycle.New(lifecycle.ForceExitDeadline(50 * time.Millisecond))
	l.Append(lifecycle.Hook{Name: "pool", OnStop: func(context.Context) error { return nil }})
	l.Append(lifecycle.Hook{
		Name: "relay",
		// Respects its context: wakes on the force-exit deadline and reports
		// its own genuine failure.
		OnStop: func(ctx context.Context) error {
			<-ctx.Done()
			return diskFull
		},
	})

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := l.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop returned nil")
	}
	if !stderrors.Is(err, diskFull) {
		t.Errorf("the hook's real error was swallowed: %v", err)
	}
	if strings.Contains(err.Error(), `"relay"`) && strings.Contains(err.Error(), "still stopping") {
		// relay finished (it returned); only pool below it may be named.
		if strings.Contains(err.Error(), `still stopping: "relay"`) {
			t.Errorf("a finished hook is reported as still stopping: %v", err)
		}
	}
	if !strings.Contains(err.Error(), `still stopping: "pool"`) {
		t.Errorf("the genuinely unfinished hook is not named: %v", err)
	}
}

func BenchmarkReady(b *testing.B) {
	l := lifecycle.New()
	if err := l.Start(context.Background()); err != nil {
		b.Fatalf("Start: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = l.Ready()
	}
}

// --- panicking hooks -------------------------------------------------------
//
// A hook that RETURNS an error rolls back correctly; until 2026-08-09 a hook
// that PANICKED produced a raw Go dump, exit 2, and no rollback at all — the
// panic escaped on a goroutine the lifecycle spawned, where the user's own
// main could not recover it either. Three documents guarantee that rollback.
// These tests are what makes the guarantee true for both phases.

// panickingHook is a package-level helper so its frame has a name and a
// file:line to report.
func panickingHook(context.Context) error {
	panic("nil pointer in the pool")
}

// TestPanickingStartHookRunsPriorStopHooks is the F1 regression test: the
// rollback GETTING_STARTED and warren.md promise, for a hook that panics.
func TestPanickingStartHookRunsPriorStopHooks(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New()
	l.Append(hook("pool", j))
	l.Append(lifecycle.Hook{
		Name:    "user",
		OnStart: panickingHook,
		OnStop:  func(context.Context) error { j.add("stop user"); return nil },
	})

	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("Start with a panicking OnStart returned nil")
	}
	got := j.all()
	if !slices.Contains(got, "stop pool") {
		t.Errorf("the prior hook's OnStop did not run — the rollback guarantee is void: %v", got)
	}
	// The panicking hook never completed its start, so it is not unwound.
	if slices.Contains(got, "stop user") {
		t.Errorf("a hook whose OnStart panicked was stopped as if it had started: %v", got)
	}
	if !strings.Contains(err.Error(), "lifecycle hook panicked") {
		t.Errorf("the error is not the panic diagnostic:\n%v", err)
	}
	if !strings.Contains(err.Error(), "nil pointer in the pool") {
		t.Errorf("the panic value was lost:\n%v", err)
	}
}

func TestPanickingStartHookNeverOpensReadiness(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Append(lifecycle.Hook{Name: "user", OnStart: panickingHook})
	if err := l.Start(context.Background()); err == nil {
		t.Fatal("Start with a panicking OnStart returned nil")
	}
	if l.Ready() {
		t.Error("readiness opened after a hook panicked during OnStart")
	}
}

// TestPanickingStopHookDoesNotAbandonTheDrain is the second F1 regression
// test. One panicking stop hook used to strand every hook below it in the
// unwind — the pools and the outbox relay — which inverts §1.3's shutdown
// ordering, the single ordering lifecycle is a Build in order to own.
func TestPanickingStopHookDoesNotAbandonTheDrain(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New()
	l.Append(hook("pool", j))
	l.Append(lifecycle.Hook{
		Name:   "kafka",
		OnStop: func(context.Context) error { panic("send on closed channel") },
	})
	l.Append(lifecycle.Hook{
		Name:   "relay",
		OnStop: func(context.Context) error { j.add("stop relay"); return stderrors.New("flush failed") },
	})

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := l.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop with a panicking OnStop returned nil")
	}
	got := j.all()
	if !slices.Contains(got, "stop pool") {
		t.Errorf("the drain was abandoned below the panicking hook: %v", got)
	}
	if !slices.Contains(got, "stop relay") {
		t.Errorf("the hook above the panicking one did not stop: %v", got)
	}
	// Everything that failed on the way down is reported, joined.
	for _, want := range []string{"lifecycle hook panicked", "send on closed channel", "flush failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the joined error does not carry %q:\n%v", want, err)
		}
	}
}

// TestPanickingHookIsNotReportedAsAbandoned pins the classification: a hook
// that panicked RETURNED, so it is not "still stopping" and it must not take
// the force-exit short-circuit, which would strand the hooks below it exactly
// as the missing recover did.
func TestPanickingHookIsNotReportedAsAbandoned(t *testing.T) {
	t.Parallel()

	j := &journal{}
	l := lifecycle.New()
	l.Append(hook("pool", j))
	l.Append(lifecycle.Hook{Name: "kafka", OnStop: func(context.Context) error { panic("boom") }})

	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := l.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop with a panicking OnStop returned nil")
	}
	for _, forbidden := range []string{"was abandoned", "still stopping", "force-exit deadline"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("a panicking hook is reported as %q:\n%v", forbidden, err)
		}
	}
	if !slices.Contains(j.all(), "stop pool") {
		t.Error("the force-exit short-circuit fired and stranded the hooks below")
	}
}

// TestPanickingHookDiagnosticIsGolden pins both phases' wording.
func TestPanickingHookDiagnosticIsGolden(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Append(lifecycle.Hook{Name: "user", OnStart: panickingHook})
	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("Start with a panicking OnStart returned nil")
	}
	assertGolden(t, "hook_panicked_onstart", elideFrames(err.Error()))

	l2 := lifecycle.New()
	l2.Append(lifecycle.Hook{
		Name:   "warren/broker/kafka",
		OnStop: func(context.Context) error { panic("send on closed channel") },
	})
	if err := l2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = l2.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop with a panicking OnStop returned nil")
	}
	assertGolden(t, "hook_panicked_onstop", elideFrames(err.Error()))
}

// TestPanickingHookStackHasNoFrameworkFrames is C0 on this path: the frames a
// reader is shown start at the hook and carry none of the plumbing that ran
// it. A stack with lifecycle's goroutine in it is a stack that answers a
// question nobody asked.
func TestPanickingHookStackHasNoFrameworkFrames(t *testing.T) {
	t.Parallel()

	l := lifecycle.New()
	l.Append(lifecycle.Hook{Name: "user", OnStart: panickingHook})
	err := l.Start(context.Background())
	if err == nil {
		t.Fatal("Start with a panicking OnStart returned nil")
	}
	got := err.Error()
	if !strings.Contains(got, "lifecycle_test.panickingHook") {
		t.Errorf("the hook's own frame is missing:\n%s", got)
	}
	for _, noise := range []string{
		"go.uber.org/dig", // invariant 2
		"runtime.",
		"reflect.",
		"panic(",
		"created by ",
		"warren/lifecycle.runHook",
		"internal/panics.Do",
	} {
		if strings.Contains(got, noise) {
			t.Errorf("the diagnostic shows machinery %q:\n%s", noise, got)
		}
	}
}

// elideFrames replaces the "Where it came from" frames with a marker, so a
// golden file pins the wording without pinning this machine's file paths.
func elideFrames(block string) string {
	const marker = "\n\n  Where it came from:\n"
	i := strings.Index(block, marker)
	if i < 0 {
		return block
	}
	return block[:i+len(marker)] + "\n    <frames elided>"
}
