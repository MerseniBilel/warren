package lifecycle_test

import (
	"context"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
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
