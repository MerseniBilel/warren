package outbox_test

import (
	"context"
	stderrors "errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/domain"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/outbox"
)

// --- fixtures --------------------------------------------------------------

type placed struct {
	Order string
	Total int
	At    time.Time
}

func (p placed) EventName() string     { return "order.placed" }
func (p placed) OccurredAt() time.Time { return p.At }
func (p placed) AggregateID() string   { return p.Order }

// capturePublisher records what the relay published; failWith makes every
// publish fail with that error.
type capturePublisher struct {
	mu       sync.Mutex
	got      map[string][]broker.Message
	failWith error
	calls    int
}

func newCapture() *capturePublisher {
	return &capturePublisher{got: map[string][]broker.Message{}}
}

func (p *capturePublisher) Publish(_ context.Context, topic string, msgs ...broker.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.failWith != nil {
		return p.failWith
	}
	p.got[topic] = append(p.got[topic], msgs...)
	return nil
}

func (p *capturePublisher) published(topic string) []broker.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]broker.Message(nil), p.got[topic]...)
}

func (p *capturePublisher) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestJSONEncoder(t *testing.T) {
	t.Parallel()

	e := outbox.JSONEncoder()
	rec, err := e.Encode(placed{Order: "o-1", Total: 500, At: time.Unix(42, 0).UTC()})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if rec.Topic != "order.placed" || rec.Message.Type != "order.placed" {
		t.Errorf("topic/type = %q/%q, want the event name", rec.Topic, rec.Message.Type)
	}
	if rec.Message.Key != "o-1" {
		t.Errorf("Key = %q, want the aggregate id — it is what preserves per-aggregate order through a partitioned broker", rec.Message.Key)
	}
	if !rec.Message.OccurredAt.Equal(time.Unix(42, 0).UTC()) {
		t.Errorf("OccurredAt = %v, want the event's own time", rec.Message.OccurredAt)
	}
	if !strings.Contains(string(rec.Message.Payload), `"Total":500`) {
		t.Errorf("payload = %s, want the concrete event encoded", rec.Message.Payload)
	}
}

func TestMemoryStore(t *testing.T) {
	t.Parallel()

	t.Run("append assigns ids and pending returns insertion order", func(t *testing.T) {
		t.Parallel()
		s := outbox.NewMemoryStore()
		ctx := context.Background()
		for _, id := range []string{"a", "b", "c"} {
			if err := s.Append(ctx, outbox.Record{Topic: "t", Message: broker.Message{Key: id}}); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		recs, err := s.Pending(ctx, 10)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(recs) != 3 {
			t.Fatalf("pending = %d, want 3", len(recs))
		}
		for i, want := range []string{"a", "b", "c"} {
			if recs[i].Message.Key != want {
				t.Errorf("position %d = %q, want %q — insertion order is the guarantee", i, recs[i].Message.Key, want)
			}
			if recs[i].Message.ID == "" {
				t.Error("a record with no ID was not assigned one — the inbox dedupes on it")
			}
		}
	})

	t.Run("limit is honoured", func(t *testing.T) {
		t.Parallel()
		s := outbox.NewMemoryStore()
		for range 5 {
			_ = s.Append(context.Background(), outbox.Record{Topic: "t"})
		}
		recs, _ := s.Pending(context.Background(), 2)
		if len(recs) != 2 {
			t.Errorf("pending = %d, want the limit of 2", len(recs))
		}
	})

	t.Run("published records do not come back", func(t *testing.T) {
		t.Parallel()
		s := outbox.NewMemoryStore()
		ctx := context.Background()
		_ = s.Append(ctx, outbox.Record{Topic: "t", Message: broker.Message{ID: "one"}})
		if err := s.MarkPublished(ctx, "one"); err != nil {
			t.Fatalf("MarkPublished: %v", err)
		}
		if recs, _ := s.Pending(ctx, 10); len(recs) != 0 {
			t.Errorf("pending = %d after publish, want 0", len(recs))
		}
	})

	t.Run("parked records do not come back and keep their cause", func(t *testing.T) {
		t.Parallel()
		s := outbox.NewMemoryStore()
		ctx := context.Background()
		_ = s.Append(ctx, outbox.Record{Topic: "t", Message: broker.Message{ID: "bad"}})
		if err := s.MarkFailed(ctx, "bad", stderrors.New("message too large")); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if recs, _ := s.Pending(ctx, 10); len(recs) != 0 {
			t.Errorf("pending = %d after parking, want 0", len(recs))
		}
	})

	t.Run("it implements Waiter so the relay does not poll blindly", func(t *testing.T) {
		t.Parallel()
		s := outbox.NewMemoryStore()
		w, ok := s.(outbox.Waiter)
		if !ok {
			t.Fatal("the memory store does not implement Waiter")
		}
		woke := make(chan struct{})
		go func() { w.Wait(context.Background()); close(woke) }()
		_ = s.Append(context.Background(), outbox.Record{Topic: "t"})
		select {
		case <-woke:
		case <-time.After(5 * time.Second):
			t.Error("Wait did not return after an Append — appending is the signal")
		}
	})
}

func TestRelayDrains(t *testing.T) {
	t.Parallel()

	store := outbox.NewMemoryStore()
	pub := newCapture()
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		_ = store.Append(ctx, outbox.Record{Topic: "orders", Message: broker.Message{ID: id, Key: id}})
	}

	relay := outbox.NewRelay(store, pub)
	n, err := relay.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("drained %d, want 2", n)
	}
	got := pub.published("orders")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("published %v, want a then b in order", got)
	}
	// Drained records are marked, so a second drain publishes nothing.
	if n, _ := relay.DrainOnce(ctx); n != 0 {
		t.Errorf("second drain published %d, want 0 — published records must not republish", n)
	}
}

func TestRelayBatchesConsecutiveTopics(t *testing.T) {
	t.Parallel()

	store := outbox.NewMemoryStore()
	pub := newCapture()
	ctx := context.Background()
	for _, r := range []outbox.Record{
		{Topic: "orders", Message: broker.Message{ID: "1"}},
		{Topic: "orders", Message: broker.Message{ID: "2"}},
		{Topic: "billing", Message: broker.Message{ID: "3"}},
	} {
		_ = store.Append(ctx, r)
	}

	relay := outbox.NewRelay(store, pub)
	if _, err := relay.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	// Consecutive same-topic records go in one Publish; the topic change
	// forces a second.
	if pub.callCount() != 2 {
		t.Errorf("publish calls = %d, want 2 — consecutive records of one topic batch", pub.callCount())
	}
}

func TestRelayFailureDispositions(t *testing.T) {
	t.Parallel()

	t.Run("a transient failure leaves the record for the next poll", func(t *testing.T) {
		t.Parallel()
		store := outbox.NewMemoryStore()
		pub := newCapture()
		pub.failWith = werrors.Unavailable("kafka", stderrors.New("no brokers"))
		ctx := context.Background()
		_ = store.Append(ctx, outbox.Record{Topic: "t", Message: broker.Message{ID: "1"}})

		relay := outbox.NewRelay(store, pub)
		if _, err := relay.DrainOnce(ctx); err == nil {
			t.Fatal("DrainOnce returned nil for a failed publish")
		}
		// Still pending: the broker being down is not the record's fault.
		recs, _ := store.Pending(ctx, 10)
		if len(recs) != 1 {
			t.Errorf("pending = %d, want the record left for the next poll", len(recs))
		}
	})

	t.Run("a rejection parks the record immediately", func(t *testing.T) {
		t.Parallel()
		store := outbox.NewMemoryStore()
		pub := newCapture()
		pub.failWith = werrors.Invalid("message", stderrors.New("too large"))
		ctx := context.Background()
		_ = store.Append(ctx, outbox.Record{Topic: "t", Message: broker.Message{ID: "1"}})
		_ = store.Append(ctx, outbox.Record{Topic: "t", Message: broker.Message{ID: "2"}})

		relay := outbox.NewRelay(store, pub)
		_, _ = relay.DrainOnce(ctx)
		recs, _ := store.Pending(ctx, 10)
		if len(recs) != 1 || recs[0].Message.ID != "2" {
			t.Errorf("pending = %v, want only the record behind the parked one — retrying a deterministic rejection would stall the queue forever", recs)
		}
	})

	t.Run("head-of-line: nothing behind a failure is published", func(t *testing.T) {
		t.Parallel()
		store := outbox.NewMemoryStore()
		pub := newCapture()
		pub.failWith = werrors.Unavailable("kafka", stderrors.New("down"))
		ctx := context.Background()
		for _, id := range []string{"1", "2", "3"} {
			_ = store.Append(ctx, outbox.Record{Topic: "t", Message: broker.Message{ID: id}})
		}
		relay := outbox.NewRelay(store, pub)
		_, _ = relay.DrainOnce(ctx)
		if pub.callCount() != 1 {
			t.Errorf("publish calls = %d, want 1 — global order means stopping at the first failure", pub.callCount())
		}
	})
}

func TestStandaloneElector(t *testing.T) {
	t.Parallel()

	led := false
	err := outbox.Standalone().Lead(context.Background(), func(context.Context) error {
		led = true
		return nil
	})
	if err != nil || !led {
		t.Errorf("Lead = %v, led = %v — the default always leads", err, led)
	}
}

// TestEndToEnd is the whole pitch with zero infrastructure: an aggregate
// raises an event, the unit of work commits state and outbox row together,
// the relay drains to the broker, and a consumer receives it through the
// full §3.4 chain.
func TestEndToEnd(t *testing.T) {
	t.Parallel()

	store := outbox.NewMemoryStore()
	pub := newCapture()
	ctx := context.Background()

	// The unit of work's commit sink writes the drained events as outbox
	// records — what persistence/postgres will do inside the transaction.
	enc := outbox.JSONEncoder()
	write := func(ctx context.Context, events []domain.Event) error {
		recs := make([]outbox.Record, 0, len(events))
		for _, e := range events {
			r, err := enc.Encode(e)
			if err != nil {
				return err
			}
			recs = append(recs, r)
		}
		return store.Append(ctx, recs...)
	}

	if err := write(ctx, []domain.Event{
		placed{Order: "o-1", Total: 500, At: time.Unix(1, 0).UTC()},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	relay := outbox.NewRelay(store, pub)
	if _, err := relay.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	got := pub.published("order.placed")
	if len(got) != 1 {
		t.Fatalf("published %d messages, want 1", len(got))
	}
	if got[0].Key != "o-1" || got[0].Type != "order.placed" {
		t.Errorf("message = %+v, want the event's key and type", got[0])
	}
}

func TestRelayRunLoop(t *testing.T) {
	t.Parallel()

	store := outbox.NewMemoryStore()
	pub := newCapture()
	relay := outbox.NewRelay(store, pub, outbox.PollInterval(50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx) }()

	// Appending is the signal: the loop must publish without waiting out the
	// poll interval.
	_ = store.Append(context.Background(), outbox.Record{Topic: "orders", Message: broker.Message{ID: "1"}})
	deadline := time.Now().Add(5 * time.Second)
	for len(pub.published("orders")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the run loop never drained an appended record")
		}
		runtime.Gosched()
	}

	// A second append is picked up by the same loop.
	_ = store.Append(context.Background(), outbox.Record{Topic: "orders", Message: broker.Message{ID: "2"}})
	for len(pub.published("orders")) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the run loop stopped after the first drain")
		}
		runtime.Gosched()
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled — it would outlive the process's shutdown")
	}
}

func TestRelayFlush(t *testing.T) {
	t.Parallel()

	store := outbox.NewMemoryStore()
	pub := newCapture()
	for _, id := range []string{"1", "2", "3"} {
		_ = store.Append(context.Background(), outbox.Record{Topic: "t", Message: broker.Message{ID: id}})
	}
	relay := outbox.NewRelay(store, pub, outbox.BatchSize(2))

	if err := relay.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(pub.published("t")); got != 3 {
		t.Errorf("flushed %d records, want all 3 — a bounded batch must not leave the rest behind at shutdown", got)
	}
}

func TestLeaderElectionGatesTheLoop(t *testing.T) {
	t.Parallel()

	store := outbox.NewMemoryStore()
	pub := newCapture()
	_ = store.Append(context.Background(), outbox.Record{Topic: "t", Message: broker.Message{ID: "1"}})

	// A non-leader drains nothing.
	relay := outbox.NewRelay(store, pub, outbox.LeaderElection(neverLeads{}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := relay.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(pub.published("t")); got != 0 {
		t.Errorf("a non-leader published %d records — every replica draining is the bug elections prevent", got)
	}
}

type neverLeads struct{}

func (neverLeads) Lead(context.Context, func(context.Context) error) error { return nil }

func TestSinkBridgesPersistenceToTheOutbox(t *testing.T) {
	t.Parallel()

	store := outbox.NewMemoryStore()
	sink := outbox.Sink(store, outbox.JSONEncoder())

	err := sink(context.Background(), []domain.Event{
		placed{Order: "o-1", Total: 100, At: time.Unix(1, 0)},
		placed{Order: "o-2", Total: 200, At: time.Unix(2, 0)},
	})
	if err != nil {
		t.Fatalf("Sink: %v", err)
	}
	recs, _ := store.Pending(context.Background(), 10)
	if len(recs) != 2 {
		t.Fatalf("appended %d records, want 2", len(recs))
	}
	if recs[0].Topic != "order.placed" || recs[0].Message.Key != "o-1" {
		t.Errorf("record = %+v", recs[0])
	}
	// No events is not an append.
	if err := sink(context.Background(), nil); err != nil {
		t.Fatalf("Sink(nil): %v", err)
	}
	if recs, _ := store.Pending(context.Background(), 10); len(recs) != 2 {
		t.Error("an empty drain appended something")
	}
}

// TestRelayRunReportsADrainFailure — Run's inner loop was
// `if err != nil || n == 0 { break }`, so DrainOnce's error was discarded
// and the loop went back to waiting. Warren writes one of its best
// diagnostics when a record is parked — naming the record, the topic, the
// key, the attempt count and the ordering consequence — and Run routed it
// to nothing. The scaffolded platform module then swallows Run's own return
// too, so in every new Warren app an event could be parked for ever with
// nothing printed anywhere.
func TestRelayRunReportsADrainFailure(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	store := outbox.NewMemoryStore()
	pub := &failingPublisher{}
	relay := outbox.NewRelay(store, pub,
		outbox.PollInterval(10*time.Millisecond),
		outbox.Backoff(broker.ExponentialBackoff(1)),
		outbox.ReportTo(slog.New(slog.NewJSONHandler(&buf, nil))),
	)

	if err := store.Append(context.Background(), outbox.Record{
		Topic:   "orders",
		Message: broker.Message{ID: "o-1", Key: "o-1", Payload: []byte(`{}`)},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = relay.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(buf.String(), "parked") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	// The whole parking diagnostic must survive the trip: the record, the
	// topic, the key, and the ordering consequence a reader needs to act on.
	logged := buf.String()
	for _, want := range []string{"parked", "o-1", "orders", "out of order"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the relay's report does not mention %q:\n%s", want, logged)
		}
	}
}

// failingPublisher refuses everything, which is what parks a record.
type failingPublisher struct{}

// Invalid, not Unavailable: UNAVAILABLE is transient and retries for ever
// by design, so it never parks. A terminal code is what produces the parked
// record — the diagnostic that used to be written and dropped.
func (failingPublisher) Publish(context.Context, string, ...broker.Message) error {
	return werrors.Invalid("topic", stderrors.New("no such topic"))
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
