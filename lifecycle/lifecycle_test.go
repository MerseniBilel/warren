package lifecycle_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/lifecycle"
)

// The components a real service registers, in dependency order. They are
// constants because most tests in this package name at least two of them.
const (
	poolHook      = "postgres.pool"
	consumerHook  = "kafka.consumer"
	serverHook    = "http.server"
	schedulerHook = "orders.scheduler"
)

// recorder records the order in which hooks ran. A phase runs on its own
// goroutine (SPEC.md §5.4), so the recorder locks rather than assuming the
// test's goroutine is the one appending.
type recorder struct {
	events []string
	mu     sync.Mutex
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

// seq is the recorded sequence as one string, which makes an ordering failure
// readable in the test output rather than a slice diff.
func (r *recorder) seq() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return strings.Join(r.events, " ")
}

// hook records "start:name" and "stop:name" and never fails.
func (r *recorder) hook(name string) lifecycle.Hook {
	return lifecycle.Hook{
		Name:    name,
		OnStart: func(context.Context) error { r.add("start:" + name); return nil },
		OnStop:  func(context.Context) error { r.add("stop:" + name); return nil },
	}
}

// failsToStart records that it was reached, then fails.
func (r *recorder) failsToStart(name string, err error) lifecycle.Hook {
	h := r.hook(name)
	h.OnStart = func(context.Context) error {
		r.add("start:" + name)

		return err
	}

	return h
}

// failsToStop records that it was reached, then fails.
func (r *recorder) failsToStop(name string, err error) lifecycle.Hook {
	h := r.hook(name)
	h.OnStop = func(context.Context) error {
		r.add("stop:" + name)

		return err
	}

	return h
}

// TestStartForwardsStopBackwards is the guarantee the package exists for:
// SPEC.md §5.1, asserted as a recorded sequence rather than as timings.
func TestStartForwardsStopBackwards(t *testing.T) {
	t.Parallel()

	var rec recorder

	h := lifecycle.New()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		h.Append(rec.hook(name))
	}

	ctx := context.Background()

	err := h.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = h.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	const want = "start:a start:b start:c start:d start:e " +
		"stop:e stop:d stop:c stop:b stop:a"

	if got := rec.seq(); got != want {
		t.Errorf("sequence:\n got %s\nwant %s", got, want)
	}
}

// TestStartUnwindsWhatItStarted covers SPEC.md §5.3: the fourth of five fails,
// the three before it stop in reverse, and the failed hook's own OnStop is
// never called — it was holding nothing to release.
func TestStartUnwindsWhatItStarted(t *testing.T) {
	t.Parallel()

	var rec recorder

	boom := errors.Internal("dial tcp 127.0.0.1:5432: connection refused")

	h := lifecycle.New()
	h.Append(rec.hook("a"))
	h.Append(rec.hook("b"))
	h.Append(rec.hook("c"))
	h.Append(rec.failsToStart("d", boom))
	h.Append(rec.hook("e"))

	err := h.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil, want the hook's failure")
	}

	const want = "start:a start:b start:c start:d stop:c stop:b stop:a"

	if got := rec.seq(); got != want {
		t.Errorf("sequence:\n got %s\nwant %s", got, want)
	}

	if !errors.Is(err, boom) {
		t.Errorf("errors.Is does not reach the hook's own error: %v", err)
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state after a failed start = %v, want Stopped", got)
	}
}

// TestStopContinuesThroughAFailingHook covers SPEC.md §5.4 and §6.4: one
// component refusing to close must not strand the rest open.
func TestStopContinuesThroughAFailingHook(t *testing.T) {
	t.Parallel()

	var rec recorder

	boom := errors.Internal("pool is already closed")

	h := lifecycle.New()
	h.Append(rec.hook("a"))
	h.Append(rec.failsToStop("b", boom))
	h.Append(rec.hook("c"))

	ctx := context.Background()

	err := h.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = h.Stop(ctx)
	if err == nil {
		t.Fatal("Stop returned nil, want the hook's failure")
	}

	const want = "start:a start:b start:c stop:c stop:b stop:a"

	if got := rec.seq(); got != want {
		t.Errorf("sequence:\n got %s\nwant %s", got, want)
	}

	if !errors.Is(err, boom) {
		t.Errorf("errors.Is does not reach the hook's own error: %v", err)
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state after Stop = %v, want Stopped", got)
	}
}

// TestStateTransitions covers SPEC.md §5.6: Ready is the only state that means
// ready, and the states either side of it do not.
func TestStateTransitions(t *testing.T) {
	t.Parallel()

	h := lifecycle.New()

	if got := h.State(); got != lifecycle.StateNew {
		t.Errorf("zero state = %v, want New", got)
	}

	starting := make(chan lifecycle.State, 1)
	stopping := make(chan lifecycle.State, 1)

	h.Append(lifecycle.Hook{
		Name:    "observer",
		OnStart: func(context.Context) error { starting <- h.State(); return nil },
		OnStop:  func(context.Context) error { stopping <- h.State(); return nil },
	})

	ctx := context.Background()

	err := h.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := <-starting; got != lifecycle.StateStarting {
		t.Errorf("state inside OnStart = %v, want Starting", got)
	}

	if got := h.State(); got != lifecycle.StateReady {
		t.Errorf("state after Start = %v, want Ready", got)
	}

	err = h.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := <-stopping; got != lifecycle.StateStopping {
		t.Errorf("state inside OnStop = %v, want Stopping", got)
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state after Stop = %v, want Stopped", got)
	}
}

// TestIdempotency covers the four cases in SPEC.md §9.2: Start twice is §6.7,
// Start after Stop is §6.11, and both Stop paths are no-ops so that
// `defer hooks.Stop(ctx)` is always safe.
func TestIdempotency(t *testing.T) {
	t.Parallel()

	t.Run("stop without start is a no-op", func(t *testing.T) {
		t.Parallel()

		h := lifecycle.New()
		h.Append(lifecycle.Close("never-started", func() {
			t.Error("OnStop ran for a lifecycle that never started")
		}))

		err := h.Stop(context.Background())
		if err != nil {
			t.Errorf("Stop without Start = %v, want nil", err)
		}

		if got := h.State(); got != lifecycle.StateNew {
			t.Errorf("state = %v, want New — a no-op Stop must not advance it", got)
		}
	})

	t.Run("stop twice is a no-op", func(t *testing.T) {
		t.Parallel()

		var rec recorder

		h := lifecycle.New()
		h.Append(rec.hook("a"))

		ctx := context.Background()

		err := h.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}

		err = h.Stop(ctx)
		if err != nil {
			t.Fatalf("first Stop: %v", err)
		}

		err = h.Stop(ctx)
		if err != nil {
			t.Errorf("second Stop = %v, want nil", err)
		}

		const want = "start:a stop:a"

		if got := rec.seq(); got != want {
			t.Errorf("sequence:\n got %s\nwant %s — the second Stop ran hooks", got, want)
		}
	})

	t.Run("start twice is an error", func(t *testing.T) {
		t.Parallel()

		var rec recorder

		h := lifecycle.New()
		h.Append(rec.hook("a"))

		ctx := context.Background()

		err := h.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}

		err = h.Start(ctx)
		if err == nil {
			t.Fatal("second Start returned nil, want §6.7")
		}

		if got := errors.CodeOf(err); got != errors.CodeInvalid {
			t.Errorf("code = %v, want Invalid", got)
		}

		const want = "start:a"

		if got := rec.seq(); got != want {
			t.Errorf("sequence:\n got %s\nwant %s — the second Start ran hooks", got, want)
		}
	})

	t.Run("start after stop is an error", func(t *testing.T) {
		t.Parallel()

		h := lifecycle.New()
		h.Append(lifecycle.Close("a", func() {}))

		ctx := context.Background()

		err := h.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}

		err = h.Stop(ctx)
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}

		err = h.Start(ctx)
		if err == nil {
			t.Fatal("Start after Stop returned nil, want §6.11 — restart is not supported")
		}

		if got := errors.CodeOf(err); got != errors.CodeInvalid {
			t.Errorf("code = %v, want Invalid", got)
		}
	})
}

// TestNamesAreInStartOrder covers SPEC.md §4: Names is what warren doctor reads
// and what a test asserts ordering on.
func TestNamesAreInStartOrder(t *testing.T) {
	t.Parallel()

	h := lifecycle.New()
	for _, name := range []string{poolHook, consumerHook, serverHook} {
		h.Append(lifecycle.Close(name, func() {}))
	}

	want := []string{poolHook, consumerHook, serverHook}

	got := h.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestNilCallbacksAreSkipped covers SPEC.md §4: either callback may be nil, and
// a hook with only one of them is legal.
func TestNilCallbacksAreSkipped(t *testing.T) {
	t.Parallel()

	var rec recorder

	h := lifecycle.New()
	h.Append(lifecycle.Hook{
		Name:    "start-only",
		OnStart: func(context.Context) error { rec.add("start:start-only"); return nil },
	})
	h.Append(lifecycle.Hook{
		Name:   "stop-only",
		OnStop: func(context.Context) error { rec.add("stop:stop-only"); return nil },
	})

	ctx := context.Background()

	err := h.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = h.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	const want = "start:start-only stop:stop-only"

	if got := rec.seq(); got != want {
		t.Errorf("sequence:\n got %s\nwant %s", got, want)
	}
}

// TestCloseHelpers covers SPEC.md §11.3: Close for a closer that cannot fail,
// Closer for the io.Closer shape the standard library actually returns.
func TestCloseHelpers(t *testing.T) {
	t.Parallel()

	t.Run("Close", func(t *testing.T) {
		t.Parallel()

		var closed bool

		hook := lifecycle.Close(poolHook, func() { closed = true })

		if hook.Name != poolHook {
			t.Errorf("Name = %q, want %q", hook.Name, poolHook)
		}

		if hook.OnStart != nil {
			t.Error("Close set an OnStart; it is a stop-only helper")
		}

		err := hook.OnStop(context.Background())
		if err != nil {
			t.Errorf("OnStop = %v, want nil", err)
		}

		if !closed {
			t.Error("OnStop did not call the closer")
		}
	})

	t.Run("Closer", func(t *testing.T) {
		t.Parallel()

		boom := errors.Internal("already closed")

		hook := lifecycle.Closer(poolHook, func() error { return boom })

		if hook.Name != poolHook {
			t.Errorf("Name = %q, want %q", hook.Name, poolHook)
		}

		err := hook.OnStop(context.Background())
		if !errors.Is(err, boom) {
			t.Errorf("OnStop = %v, want the closer's own error", err)
		}
	})
}
