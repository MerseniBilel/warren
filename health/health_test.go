package health_test

import (
	"context"
	stderrors "errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/health"
)

func up() func() bool { return func() bool { return true } }

func TestLivenessRunsNoChecks(t *testing.T) {
	t.Parallel()

	// Restarting a process does not fix a dead database: a liveness probe
	// wired to dependencies kills every replica when one blips. /healthz
	// answers only "this code ran".
	ran := false
	r := health.New(func() bool { return false })
	if err := r.Register(health.NewCheck("db", func(context.Context) error {
		ran = true
		return stderrors.New("down")
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	rep := r.Live(context.Background())
	if rep.Status != health.StatusUp {
		t.Errorf("Live() = %s, want up even with the gate closed and a failing check", rep.Status)
	}
	if ran {
		t.Error("liveness ran a dependency check — a database blip must not restart the process")
	}
}

func TestReadinessGate(t *testing.T) {
	t.Parallel()

	t.Run("before the gate opens it reports starting", func(t *testing.T) {
		t.Parallel()
		checked := false
		r := health.New(func() bool { return false })
		_ = r.Register(health.NewCheck("db", func(context.Context) error { checked = true; return nil }))

		rep := r.Ready(context.Background())
		if rep.Status != health.StatusDown || rep.Reason != "starting" {
			t.Errorf("Ready() = %+v, want down/starting", rep)
		}
		if checked {
			t.Error("checks ran while the gate was closed — the verdict was already decided")
		}
	})

	t.Run("after the gate closes it reports draining", func(t *testing.T) {
		t.Parallel()
		var open atomic.Bool
		open.Store(true)
		r := health.New(open.Load)
		if rep := r.Ready(context.Background()); rep.Status != health.StatusUp {
			t.Fatalf("Ready() = %+v, want up", rep)
		}
		open.Store(false)
		rep := r.Ready(context.Background())
		if rep.Status != health.StatusDown || rep.Reason != "draining" {
			t.Errorf("Ready() = %+v, want down/draining — a red probe must say which red it is", rep)
		}
	})
}

func TestReadinessConsumesChecks(t *testing.T) {
	t.Parallel()

	t.Run("a failing critical check makes readiness down and names it", func(t *testing.T) {
		t.Parallel()
		r := health.New(up())
		_ = r.Register(health.NewCheck("postgres", func(context.Context) error {
			return stderrors.New("connection refused")
		}))
		_ = r.Register(health.NewCheck("kafka", func(context.Context) error { return nil }))

		rep := r.Ready(context.Background())
		if rep.Status != health.StatusDown {
			t.Fatalf("Ready() = %+v, want down", rep)
		}
		byName := map[string]health.Result{}
		for _, c := range rep.Checks {
			byName[c.Name] = c
		}
		if byName["postgres"].Status != health.StatusDown || !strings.Contains(byName["postgres"].Error, "connection refused") {
			t.Errorf("postgres result = %+v, want down with its cause", byName["postgres"])
		}
		if byName["kafka"].Status != health.StatusUp {
			t.Errorf("kafka result = %+v, want up", byName["kafka"])
		}
	})

	t.Run("an informational check is reported but does not gate", func(t *testing.T) {
		t.Parallel()
		r := health.New(up())
		_ = r.Register(health.NewCheck("warm-cache", func(context.Context) error {
			return stderrors.New("cold")
		}), health.Informational())

		rep := r.Ready(context.Background())
		if rep.Status != health.StatusUp {
			t.Errorf("Ready() = %+v, want up — an informational failure must not drop the pod from the load balancer", rep)
		}
		if len(rep.Checks) != 1 || rep.Checks[0].Critical {
			t.Errorf("checks = %+v, want one non-critical result", rep.Checks)
		}
	})

	t.Run("results keep registration order", func(t *testing.T) {
		t.Parallel()
		r := health.New(up())
		for _, n := range []string{"a", "b", "c"} {
			_ = r.Register(health.NewCheck(n, func(context.Context) error { return nil }))
		}
		rep := r.Ready(context.Background())
		got := ""
		for _, c := range rep.Checks {
			got += c.Name
		}
		if got != "abc" {
			t.Errorf("order = %q, want registration order — golden files depend on it", got)
		}
	})
}

func TestCheckTimeout(t *testing.T) {
	t.Parallel()

	r := health.New(up(), health.DefaultTimeout(20*time.Millisecond))
	_ = r.Register(health.NewCheck("wedged", func(ctx context.Context) error {
		<-ctx.Done() // respects its context
		return ctx.Err()
	}))

	rep := r.Ready(context.Background())
	if rep.Status != health.StatusDown {
		t.Fatalf("Ready() = %+v, want down", rep)
	}
	if !strings.Contains(rep.Checks[0].Error, "exceeded its 20ms timeout") {
		t.Errorf("error = %q, want the timeout named", rep.Checks[0].Error)
	}
}

func TestChecksRunConcurrently(t *testing.T) {
	t.Parallel()

	// Probe latency is the slowest check, not the sum: three checks that
	// each wait for the others prove they overlap.
	const n = 3
	var arrived atomic.Int32
	gate := make(chan struct{})
	r := health.New(up(), health.DefaultTimeout(2*time.Second))
	for _, name := range []string{"a", "b", "c"} {
		_ = r.Register(health.NewCheck(name, func(ctx context.Context) error {
			if arrived.Add(1) == n {
				close(gate)
			}
			select {
			case <-gate:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}))
	}
	if rep := r.Ready(context.Background()); rep.Status != health.StatusUp {
		t.Errorf("Ready() = %+v — the checks did not run concurrently", rep)
	}
}

func TestDuplicateRegistration(t *testing.T) {
	t.Parallel()

	r := health.New(up())
	if err := r.Register(health.NewCheck("postgres", func(context.Context) error { return nil })); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(health.NewCheck("postgres", func(context.Context) error { return nil }))
	if err == nil {
		t.Fatal("a duplicate check name was accepted — one would silently shadow the other")
	}
	for _, want := range []string{"✗ duplicate health check", "postgres", "check after the instance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic is missing %q:\n%s", want, err)
		}
	}
}

func TestPanickingCheckIsReportedNotFatal(t *testing.T) {
	t.Parallel()

	r := health.New(up())
	_ = r.Register(health.NewCheck("buggy", func(context.Context) error { panic("bug in the check") }))
	_ = r.Register(health.NewCheck("fine", func(context.Context) error { return nil }))

	rep := r.Ready(context.Background())
	if rep.Status != health.StatusDown {
		t.Fatalf("Ready() = %+v, want down", rep)
	}
	if !strings.Contains(rep.Checks[0].Error, "panic") {
		t.Errorf("error = %q, want the panic reported", rep.Checks[0].Error)
	}
	if rep.Checks[1].Status != health.StatusUp {
		t.Error("a panicking check stopped the others from being reported")
	}
}

func TestNilCheckIsRejected(t *testing.T) {
	t.Parallel()

	if err := health.New(up()).Register(nil); err == nil {
		t.Error("a nil check was accepted")
	}
}
