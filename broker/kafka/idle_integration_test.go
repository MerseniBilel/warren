//go:build integration

package kafka

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/broker"
)

// TestIdleMemberIsAnnounced covers the silent half of scaling out a consumer.
//
// A group divides a topic's partitions between its members, so a second
// replica on a one-partition topic — which is what an auto-created topic is —
// is assigned nothing and processes nothing, for ever, with no error. It
// serves HTTP and looks healthy. Measured before this existed: two replicas,
// 30 events, replica 1 consumed 30 and replica 2 consumed 0.
func TestIdleMemberIsAnnounced(t *testing.T) {
	addr := os.Getenv("WARREN_TEST_KAFKA_BROKERS")
	if addr == "" {
		t.Skip("WARREN_TEST_KAFKA_BROKERS is not set")
	}

	// Short enough to test, long enough that the rebalance settles first.
	restore := idleWarnAfter
	idleWarnAfter = 3 * time.Second
	t.Cleanup(func() { idleWarnAfter = restore })

	var buf syncBuffer
	restoreLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restoreLog) })

	topic := "warren-idle-" + t.Name()
	group := "warren-idle-group"

	// Two members of ONE group on a ONE-partition topic: the shape a second
	// replica creates.
	first := newIdleClient(t, addr, group, topic)
	second := newIdleClient(t, addr, group, topic)

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "holds no partitions") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	out := buf.String()
	if !strings.Contains(out, "holds no partitions") {
		t.Fatalf("the idle member never announced itself:\n%s", out)
	}
	if n := strings.Count(out, "holds no partitions"); n != 1 {
		t.Errorf("warning appeared %d times, want exactly 1:\n%s", n, out)
	}
	for _, want := range []string{"partitions can be added", "usually ONE partition", group} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("the warning omits %q:\n%s", want, out)
		}
	}

	// The log alone is not enough, and an earlier version of this test
	// proved it: the warning fired for a member whose partition was briefly
	// revoked mid-rebalance, the count was 1, the test was green — and the
	// real two-replica app stayed silent, because franz-go never calls
	// OnPartitionsAssigned for a member assigned NOTHING.
	//
	// So assert the pairing: exactly one member holds nothing, and the one
	// that warned is that one.
	//
	// POLLED, not sampled once. The warning fires as soon as one member has
	// been empty for idleWarnAfter, and the group can still be settling at
	// that instant — both members briefly hold nothing while the partition
	// moves. Sampling there made this test fail roughly one full-suite run
	// in three with "idle=1 busy=0", which is a flaky integration test, and
	// a flaky test is worse than none: it teaches you to re-run until green.
	// The property is that the pairing is reached, so wait for it.
	var idle, busy int
	stable := time.Now().Add(30 * time.Second)
	for {
		idle, busy = 0, 0
		for _, c := range []*client{first, second} {
			c.heldMu.Lock()
			total := 0
			for _, ps := range c.held {
				total += len(ps)
			}
			warned := c.idleWarned
			c.heldMu.Unlock()

			switch {
			case total == 0 && warned:
				idle++
			case total > 0 && !warned:
				busy++
			case total > 0 && warned:
				t.Fatalf("a member holds %d partition(s) and warned it holds none", total)
			}
		}
		if idle == 1 && busy == 1 {
			break
		}
		if time.Now().After(stable) {
			t.Errorf("want exactly one idle member and one busy one, got idle=%d busy=%d "+
				"(a member holding nothing and saying nothing is the defect this test exists for; "+
				"both holding nothing is a rebalance that never settled)", idle, busy)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func newIdleClient(t *testing.T, addr, group, topic string) *client {
	t.Helper()
	cfg := defaults()
	cfg.brokers = []string{addr}
	cfg.group = group
	cfg.autoCreate = true
	c, err := newClient(cfg, nopLifecycle{}, nopRegistry{})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if err := c.open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = c.stopConsuming(context.Background())
		_ = c.close(context.Background())
	})
	if err := c.Subscribe(context.Background(), topic, func(context.Context, broker.Message) error { return nil }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return c
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
