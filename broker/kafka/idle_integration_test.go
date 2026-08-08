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

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

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

	// PROVISIONED, not auto-created, and this is the third and largest half of
	// this test's flakiness. It relied on the driver's autoCreate, so on a
	// COLD broker the topic did not exist when both members joined, neither
	// was assigned anything, and the test reported idle=2 busy=0 — the shape
	// of "a rebalance that never settled" — every single run. On a broker
	// warmed by earlier runs the topic already existed and it passed. A test
	// whose verdict depends on how many times you have run it is not a test.
	//
	// ONE partition on purpose: an idle member only exists when the group's
	// members outnumber them, which is the whole subject here. Every other
	// suite in this package provisions SIX for the opposite reason.
	provisionOnePartition(t, addr, topic)

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
	// NOT "exactly once". A cooperative rebalance legitimately leaves BOTH
	// members holding nothing for a moment while the partition moves, and
	// with idleWarnAfter compressed to 3s for this test — against Kafka's own
	// 3s initial-rebalance delay — that window is wide enough for both to
	// warn. Counting log lines here failed roughly one full-suite run in
	// three.
	//
	// Nothing is lost by dropping it: the pairing check below asserts
	// "exactly one member is idle AND warned" against the members' own state,
	// which is strictly stronger than counting lines in a buffer, and it is
	// the assertion that actually guards the defect this test exists for.
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
	var idle, busy, contradictory int
	stable := time.Now().Add(30 * time.Second)
	for {
		idle, busy, contradictory = 0, 0, 0
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
				// TRANSIENT, not a failure — and it used to be t.Fatalf,
				// which is the second half of this test's flakiness. A member
				// that warned while empty and is then assigned a partition
				// keeps the flag until watchIdle's NEXT tick clears it
				// (consume.go: total > 0 → idleWarned = false), which is up
				// to idleWarnAfter/3 later. Killing the test inside the
				// polling loop samples exactly that window.
				//
				// If it is real rather than transient it persists, and the
				// deadline below reports it.
				contradictory++
			}
		}
		if idle == 1 && busy == 1 {
			break
		}
		if time.Now().After(stable) {
			if contradictory > 0 {
				t.Errorf("%d member(s) hold partitions and still say they hold none, and it did not "+
					"clear — watchIdle resets idleWarned when total > 0, so this persisting means it "+
					"is not resetting", contradictory)
				break
			}
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

// provisionOnePartition creates topic with exactly one partition and waits
// until the cluster reports it, so both members join a topic that already
// exists.
//
// The external test package has freshPartitions for the six-partition case,
// but this file is `package kafka` — it reaches the client's unexported held
// and idleWarned state — so it cannot call it.
func provisionOnePartition(t *testing.T, addr, topic string) {
	t.Helper()
	admin, err := kgo.NewClient(kgo.SeedBrokers(addr))
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	defer admin.Close()

	req := kmsg.NewPtrCreateTopicsRequest()
	rt := kmsg.NewCreateTopicsRequestTopic()
	rt.Topic = topic
	rt.NumPartitions = 1
	rt.ReplicationFactor = 1
	req.Topics = append(req.Topics, rt)
	req.TimeoutMillis = 10000
	resp, err := req.RequestWith(context.Background(), admin)
	if err != nil {
		t.Fatalf("CreateTopics %s: %v", topic, err)
	}
	for _, rtp := range resp.Topics {
		// 36 is TOPIC_ALREADY_EXISTS: a previous run left it, with one
		// partition, which is the shape wanted.
		if rtp.ErrorCode != 0 && rtp.ErrorCode != 36 {
			t.Fatalf("CreateTopics %s: error code %d", rtp.Topic, rtp.ErrorCode)
		}
	}

	// CreateTopics returning is not the cluster having told anyone. Without
	// this the members subscribe before the topic exists to them and wait out
	// a metadata refresh — which is what produced idle=2 busy=0.
	deadline := time.Now().Add(20 * time.Second)
	for {
		mreq := kmsg.NewPtrMetadataRequest()
		mrt := kmsg.NewMetadataRequestTopic()
		mrt.Topic = &topic
		mreq.Topics = append(mreq.Topics, mrt)
		mresp, err := mreq.RequestWith(context.Background(), admin)
		if err == nil {
			for _, mt := range mresp.Topics {
				if mt.ErrorCode == 0 && len(mt.Partitions) == 1 {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %s never became visible with 1 partition", topic)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
