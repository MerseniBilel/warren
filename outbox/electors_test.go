package outbox_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren/outbox"
)

// Electors exists because ONE Elector is ONE lock. A field test injected the
// relay's elector into an SLA sweeper, following README's own advice, and
// whichever goroutine woke first took it while the other did nothing for the
// life of the process — four runs, two each way, both reporting /readyz 200.
//
// A NAME is a leadership. Different names lead at the same time; the same
// name contends, which is the only thing a name is for.

func TestStandaloneElectorsLead(t *testing.T) {
	t.Parallel()

	els := outbox.StandaloneElectors()
	el, err := els.Elector("ticket/sla-sweeper")
	if err != nil {
		t.Fatalf("Elector: %v", err)
	}
	ran := false
	if err := el.Lead(context.Background(), func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("Lead: %v", err)
	}
	if !ran {
		t.Error("StandaloneElectors did not run the function — it always leads")
	}
}

func TestDifferentNamesLeadAtTheSameTime(t *testing.T) {
	t.Parallel()
	// The whole point. Two components, two leaderships, both running.
	els := outbox.StandaloneElectors()
	a, err := els.Elector("sweeper")
	if err != nil {
		t.Fatalf("Elector: %v", err)
	}
	b, err := els.Elector("reconciler")
	if err != nil {
		t.Fatalf("Elector: %v", err)
	}

	var wg sync.WaitGroup
	both := make(chan string, 2)
	for name, el := range map[string]outbox.Elector{"sweeper": a, "reconciler": b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = el.Lead(context.Background(), func(context.Context) error {
				both <- name
				return nil
			})
		}()
	}
	wg.Wait()
	close(both)
	if len(both) != 2 {
		t.Errorf("%d of 2 leaderships ran — different names must not contend", len(both))
	}
}

func TestClaimingOneNameTwiceIsRefused(t *testing.T) {
	t.Parallel()
	// Two components sharing a name is the ORIGINAL defect with a name on
	// it: one of them would silently never run. It has to be a boot error,
	// and it has to be one HERE too — a test double that permits what
	// production refuses is a test that passes for a service that cannot
	// start.
	els := outbox.StandaloneElectors()
	if _, err := els.Elector("sweeper"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := els.Elector("sweeper")
	if err == nil {
		t.Fatal("the same leadership was claimed twice — one of the two would silently never run")
	}
	for _, want := range []string{"sweeper", "already"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%v", want, err)
		}
	}
}

func TestAnUnnamedLeadershipIsRefused(t *testing.T) {
	t.Parallel()
	// "" would be a leadership every unnamed claimant shares, which is the
	// defect again, spelled with a default.
	els := outbox.StandaloneElectors()
	_, err := els.Elector("")
	if err == nil {
		t.Fatal("an empty leadership name was accepted")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("the diagnostic does not explain:\n%v", err)
	}
}

func TestStandaloneElectorSaysItLeadsUnconditionally(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Standalone() has the relay to warn on its behalf, gated on
	// outbox.Durable. A standalone ELECTOR has nobody: nothing else knows it
	// exists. This project lost 50% of a field test's events to exactly the
	// silent-standalone case, so the new API says so itself.
	els := outbox.StandaloneElectors()
	el, err := els.Elector("ticket/sla-sweeper")
	if err != nil {
		t.Fatalf("Elector: %v", err)
	}
	for range 3 {
		if err := el.Lead(context.Background(), func(context.Context) error { return nil }); err != nil {
			t.Fatalf("Lead: %v", err)
		}
	}

	if n := strings.Count(buf.String(), "leading unconditionally"); n != 1 {
		t.Errorf("warned %d times, want exactly 1 — a loop must not narrate itself", n)
	}
	if !strings.Contains(buf.String(), "ticket/sla-sweeper") {
		t.Errorf("the warning does not name the leadership: %s", buf.String())
	}
}

func TestStandaloneElectorsIsSafeForConcurrentClaims(t *testing.T) {
	t.Parallel()
	// Constructors run under the container, which is free to build in
	// parallel; a registry with a torn map is a boot that fails at random.
	els := outbox.StandaloneElectors()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := els.Elector(string(rune('a' + i)))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a distinct name was refused: %v", err)
		}
	}
}
