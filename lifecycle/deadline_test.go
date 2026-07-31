package lifecycle_test

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/lifecycle"
)

// blocking returns a hook whose OnStop signals that it is in flight and then
// blocks until the test releases it. It is how a slow shutdown is written
// without a sleep: the test controls both edges.
func blocking(name string, inFlight, release chan struct{}) lifecycle.Hook {
	return lifecycle.Hook{
		Name: name,
		OnStop: func(context.Context) error {
			close(inFlight)
			<-release

			return nil
		},
	}
}

// TestStopAbandonsAHookThatOverruns covers SPEC.md §5.4 and §6.3: the hook that
// overran and the ones never reached are both named, and the state is Stopped
// even though the shutdown did not complete.
func TestStopAbandonsAHookThatOverruns(t *testing.T) {
	t.Parallel()

	inFlight := make(chan struct{})
	release := make(chan struct{})

	// Released at the end so the abandoned goroutine finishes rather than
	// outliving the test binary.
	defer close(release)

	h := lifecycle.New()
	h.Append(lifecycle.Close(poolHook, func() {}))
	h.Append(lifecycle.Close(consumerHook, func() {}))
	h.Append(blocking(serverHook, inFlight, release))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := h.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The grace period runs out precisely while http.server is in flight.
	go func() {
		<-inFlight
		cancel()
	}()

	err = h.Stop(ctx)
	if err == nil {
		t.Fatal("Stop returned nil, want §6.3")
	}

	detail := errors.Detail(err)

	for _, want := range []string{
		"the grace period expired",
		"http.server, kafka.consumer, postgres.pool",
		"stopped     0 of 3 hooks",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("Detail is missing %q:\n%s", want, detail)
		}
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state after an abandoned Stop = %v, want Stopped", got)
	}
}

// TestStartAbandonsAHookThatOverruns covers SPEC.md §6.10, the boot counterpart
// of §6.3 and the hole the draft spec left open.
func TestStartAbandonsAHookThatOverruns(t *testing.T) {
	t.Parallel()

	inFlight := make(chan struct{})
	release := make(chan struct{})

	defer close(release)

	h := lifecycle.New()
	h.Append(lifecycle.Hook{
		Name:    poolHook,
		OnStart: func(context.Context) error { return nil },
		OnStop:  func(context.Context) error { return nil },
	})
	h.Append(lifecycle.Hook{
		Name: consumerHook,
		OnStart: func(context.Context) error {
			close(inFlight)
			<-release

			return nil
		},
	})
	h.Append(lifecycle.Close(serverHook, func() {}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-inFlight
		cancel()
	}()

	err := h.Start(ctx)
	if err == nil {
		t.Fatal("Start returned nil, want §6.10")
	}

	detail := errors.Detail(err)

	for _, want := range []string{
		"the start deadline expired",
		"kafka.consumer, http.server",
		"rolled back",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("Detail is missing %q:\n%s", want, detail)
		}
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state after an abandoned Start = %v, want Stopped", got)
	}
}

// TestUnwindRunsOnADetachedContext is the SPEC.md §11.11 claim, tested rather
// than asserted: a start that failed because its context ended must still close
// what it opened, so the unwind cannot inherit that context.
func TestUnwindRunsOnADetachedContext(t *testing.T) {
	t.Parallel()

	var rec recorder

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := lifecycle.New()
	h.Append(lifecycle.Hook{
		Name: poolHook,
		OnStart: func(context.Context) error {
			rec.add("start:" + poolHook)
			// The budget runs out the instant the pool is open.
			cancel()

			return nil
		},
		OnStop: func(context.Context) error { rec.add("stop:" + poolHook); return nil },
	})
	h.Append(rec.hook(consumerHook))

	err := h.Start(ctx)
	if err == nil {
		t.Fatal("Start returned nil, want the expired context")
	}

	const want = "start:postgres.pool stop:postgres.pool"

	if got := rec.seq(); got != want {
		t.Errorf("sequence:\n got %s\nwant %s"+
			"\nan unwind that inherits the dead context leaks the pool", got, want)
	}
}

// TestUnwindBudgetBoundsTheRollback covers the second half of SPEC.md §11.11.
// Detaching alone leaves the rollback unbounded, so a hook that hangs while
// unwinding hangs the boot; Kratos bounds it, and so do we.
func TestUnwindBudgetBoundsTheRollback(t *testing.T) {
	t.Parallel()

	boom := errors.Internal("bind: address already in use")

	// A budget already in the past, which is how AGENT.md says to write a
	// timeout: an expired deadline, never a sleep.
	h := lifecycle.New(lifecycle.UnwindBudget(time.Nanosecond))
	h.Append(lifecycle.Hook{
		Name:    poolHook,
		OnStart: func(context.Context) error { return nil },
		OnStop: func(context.Context) error {
			<-make(chan struct{}) // never returns

			return nil
		},
	})
	h.Append(lifecycle.Hook{
		Name:    serverHook,
		OnStart: func(context.Context) error { return boom },
	})

	// Without the budget the rollback blocks on postgres.pool forever, and this
	// test fails by timing out rather than by assertion.
	err := h.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil, want the hook's failure")
	}

	if !errors.Is(err, boom) {
		t.Errorf("the start failure was lost behind the rollback's: %v", err)
	}

	if !strings.Contains(errors.Detail(err), "the grace period expired") {
		t.Errorf("Detail does not report the exhausted budget:\n%s", errors.Detail(err))
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state = %v, want Stopped", got)
	}
}

// TestPanicIsRecoveredAndNamed covers SPEC.md §5.5 and §6.9 in both phases: a
// panic in OnStart is a start failure so the unwind runs, and a panic in OnStop
// is a stop failure so the remaining hooks still run.
func TestPanicIsRecoveredAndNamed(t *testing.T) {
	t.Parallel()

	const wantSeq = "start:postgres.pool stop:postgres.pool"

	t.Run("during start", func(t *testing.T) {
		t.Parallel()

		var rec recorder

		h := lifecycle.New()
		h.Append(rec.hook(poolHook))
		h.Append(lifecycle.Hook{
			Name:    consumerHook,
			OnStart: func(context.Context) error { panic("no brokers configured") },
		})

		err := h.Start(context.Background())
		if err == nil {
			t.Fatal("Start returned nil, want §6.9")
		}

		detail := errors.Detail(err)
		for _, want := range []string{
			"the hook kafka.consumer panicked: no brokers configured",
			"this is a bug in the hook, not in the lifecycle",
		} {
			if !strings.Contains(detail, want) {
				t.Errorf("Detail is missing %q:\n%s", want, detail)
			}
		}

		if got := rec.seq(); got != wantSeq {
			t.Errorf("the unwind did not run after a panic: %s", got)
		}
	})

	t.Run("during stop", func(t *testing.T) {
		t.Parallel()

		var rec recorder

		h := lifecycle.New()
		h.Append(rec.hook(poolHook))
		h.Append(lifecycle.Hook{
			Name:   consumerHook,
			OnStop: func(context.Context) error { panic("rebalance in progress") },
		})

		ctx := context.Background()

		err := h.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}

		err = h.Stop(ctx)
		if err == nil {
			t.Fatal("Stop returned nil, want §6.9")
		}

		if !strings.Contains(errors.Detail(err), "the hook kafka.consumer panicked") {
			t.Errorf("Detail does not name the panicking hook:\n%s", errors.Detail(err))
		}

		if got := rec.seq(); got != wantSeq {
			t.Errorf("Stop did not continue past the panic: %s", got)
		}
	})
}

// TestGoexitFailsThePhaseInsteadOfHangingIt covers SPEC.md §6.12: a hook that
// calls runtime.Goexit — which is what t.Fatal inside one does — never returns
// and never sends, so without the sentinel the phase would block until its
// deadline and then blame the wrong hook.
func TestGoexitFailsThePhaseInsteadOfHangingIt(t *testing.T) {
	t.Parallel()

	h := lifecycle.New()
	h.Append(lifecycle.Hook{
		Name:    schedulerHook,
		OnStart: func(context.Context) error { runtime.Goexit(); return nil },
	})

	// context.Background has no deadline: if the sentinel is missing this call
	// never returns and the test times out rather than failing.
	err := h.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil, want §6.12")
	}

	if !strings.Contains(errors.Detail(err), "orders.scheduler exited without returning") {
		t.Errorf("Detail does not name the hook that exited:\n%s", errors.Detail(err))
	}
}

// TestStopDuringStart covers SPEC.md §5.6: a SIGTERM that arrives mid-boot is
// ordinary, and the two phases are serialised rather than interleaved.
func TestStopDuringStart(t *testing.T) {
	t.Parallel()

	var rec recorder

	inFlight := make(chan struct{})
	release := make(chan struct{})

	h := lifecycle.New()
	h.Append(rec.hook(poolHook))
	h.Append(lifecycle.Hook{
		Name: serverHook,
		OnStart: func(context.Context) error {
			rec.add("start:" + serverHook)
			close(inFlight)
			<-release

			return nil
		},
		OnStop: func(context.Context) error { rec.add("stop:" + serverHook); return nil },
	})

	ctx := context.Background()

	var wg sync.WaitGroup

	wg.Go(func() {
		err := h.Start(ctx)
		if err != nil {
			t.Errorf("Start: %v", err)
		}
	})

	wg.Go(func() {
		<-inFlight
		// Queues behind Start rather than interleaving with it.
		close(release)

		err := h.Stop(ctx)
		if err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	wg.Wait()

	const want = "start:postgres.pool start:http.server stop:http.server stop:postgres.pool"

	if got := rec.seq(); got != want {
		t.Errorf("sequence:\n got %s\nwant %s", got, want)
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state = %v, want Stopped", got)
	}
}

// TestStateIsReadableWhileStopping covers SPEC.md §5.6: a health endpoint reads
// State from a request goroutine while Stop runs on another, and must never
// block one on the other. The race detector is the assertion.
func TestStateIsReadableWhileStopping(t *testing.T) {
	t.Parallel()

	inFlight := make(chan struct{})
	release := make(chan struct{})

	h := lifecycle.New()
	h.Append(blocking(serverHook, inFlight, release))

	ctx := context.Background()

	err := h.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			<-inFlight

			if got := h.State(); got != lifecycle.StateStopping {
				t.Errorf("State during Stop = %v, want Stopping", got)
			}
		})
	}

	go func() {
		<-inFlight
		wg.Wait()
		close(release)
	}()

	err = h.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := h.State(); got != lifecycle.StateStopped {
		t.Errorf("state = %v, want Stopped", got)
	}
}
