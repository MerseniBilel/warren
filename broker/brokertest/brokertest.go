// Package brokertest is the contract suite every broker driver must pass:
// the in-process one, Kafka, RabbitMQ, NATS. It is written before the
// drivers and updated before them, so "swapping Kafka for RabbitMQ changes
// one line in main.go" is a tested property rather than a claim.
//
// A driver runs it in one line:
//
//	func TestContract(t *testing.T) {
//	    brokertest.Run(t, func(t *testing.T) (broker.Publisher, broker.Subscriber) {
//	        b := memory.New()
//	        return b, b
//	    })
//	}
//
// The suite asserts only port-level behaviour — envelope fidelity,
// at-least-once delivery, ordering, and drain — never a driver's own
// topology. The consumer chain's dispositions are the port's own tests;
// what a driver must prove is that a handler's nil and non-nil returns
// reach it unchanged.
package brokertest

import (
	"context"
	stderrors "errors"
	"maps"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/broker"
)

// NewBroker builds a fresh, isolated publisher/subscriber pair for one
// subtest. Drivers clean up with t.Cleanup.
type NewBroker func(t *testing.T) (broker.Publisher, broker.Subscriber)

// Run executes the whole contract suite against the driver.
func Run(t *testing.T, newBroker NewBroker) {
	t.Helper()
	t.Run("envelope round-trip", func(t *testing.T) { testEnvelope(t, newBroker) })
	t.Run("at-least-once delivery", func(t *testing.T) { testAtLeastOnce(t, newBroker) })
	t.Run("handler errors reach the driver", func(t *testing.T) { testErrorReachesDriver(t, newBroker) })
	t.Run("batch publish delivers every message", func(t *testing.T) { testBatch(t, newBroker) })
	t.Run("topics are isolated", func(t *testing.T) { testTopicIsolation(t, newBroker) })
	t.Run("fan-out to several subscriptions", func(t *testing.T) { testFanOut(t, newBroker) })
	t.Run("per-key ordering", func(t *testing.T) { testOrdering(t, newBroker) })
	t.Run("drain on context cancellation", func(t *testing.T) { testDrain(t, newBroker) })
}

// collector receives messages and lets a test await a count without sleeps.
type collector struct {
	mu     sync.Mutex
	got    []broker.Message
	probed bool
	errs   map[string]error // per message ID, what the handler returns
}

func newCollector() *collector { return &collector{errs: map[string]error{}} }

func (c *collector) handle(_ context.Context, m broker.Message) error {
	c.mu.Lock()
	if strings.HasPrefix(m.ID, probeID) {
		c.probed = true
		c.mu.Unlock()
		return nil
	}
	c.got = append(c.got, m)
	err := c.errs[m.ID]
	c.mu.Unlock()
	return err
}

func (c *collector) sawProbe() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probed
}

// reset forgets everything delivered so far — called once a subscription is
// known live, so probes never appear in an assertion.
func (c *collector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = nil
}

func (c *collector) messages() []broker.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]broker.Message(nil), c.got...)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

// awaitCount spins until the collector has n messages, yielding the
// scheduler — an observable-state wait, never a sleep. It fails the test
// rather than hanging forever.
func awaitCount(t *testing.T, c *collector, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d messages; got %d", n, c.count())
		}
		runtime.Gosched()
	}
}

// subscribe starts a subscription in the background and returns its cancel
// and a channel carrying Subscribe's return.
//
// Subscribe blocks while it delivers, so it necessarily runs in a goroutine
// and no driver can promise the subscription is live the instant the call
// starts — an in-process broker registers a moment later, a Kafka consumer
// joins its group seconds later. Tests therefore establish liveness with
// ready() before asserting on published messages, rather than assuming it.
func subscribe(t *testing.T, sub broker.Subscriber, topic string, h broker.MessageHandler) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(ctx, topic, h) }()
	t.Cleanup(cancel)
	return cancel, done
}

// ready publishes probe messages until the collector sees one, then forgets
// them — the driver-neutral way to know a subscription is delivering. Probes
// are ordinary messages with a reserved ID prefix; at-least-once means a
// duplicate probe is harmless.
func ready(t *testing.T, pub broker.Publisher, c *collector, topic string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; ; i++ {
		if c.sawProbe() {
			c.reset()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscription on %q never became ready", topic)
		}
		if err := pub.Publish(context.Background(), topic, broker.Message{ID: probeID + itoa(i), Type: probeID}); err != nil {
			t.Fatalf("probe publish: %v", err)
		}
		runtime.Gosched()
	}
}

const probeID = "warren-brokertest-probe-"

func testEnvelope(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	c := newCollector()
	subscribe(t, sub, "orders", c.handle)
	ready(t, pub, c, "orders")

	sent := broker.Message{
		ID:      "evt-42",
		Type:    "order.placed",
		Key:     "order-7",
		Payload: []byte(`{"total":1250}`),
		Headers: map[string]string{
			"traceparent":    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"correlation-id": "req-9",
		},
		OccurredAt: time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}
	if err := pub.Publish(context.Background(), "orders", sent); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	awaitCount(t, c, 1)

	got := c.messages()[0]
	if got.ID != sent.ID {
		t.Errorf("ID = %q, want %q — inbox dedupe is keyed on it", got.ID, sent.ID)
	}
	if got.Type != sent.Type || got.Key != sent.Key {
		t.Errorf("Type/Key drifted: %q/%q", got.Type, got.Key)
	}
	if string(got.Payload) != string(sent.Payload) {
		t.Errorf("Payload = %q, want byte-for-byte %q", got.Payload, sent.Payload)
	}
	if !maps.Equal(got.Headers, sent.Headers) {
		t.Errorf("Headers = %v, want %v — losing them silently breaks tracing across the broker", got.Headers, sent.Headers)
	}
	if !got.OccurredAt.Equal(sent.OccurredAt) {
		t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, sent.OccurredAt)
	}
}

func testAtLeastOnce(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	c := newCollector()
	subscribe(t, sub, "orders", c.handle)
	ready(t, pub, c, "orders")

	const n = 50
	for i := range n {
		m := broker.Message{ID: id(i), Type: "order.placed"}
		if err := pub.Publish(context.Background(), "orders", m); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	awaitCount(t, c, n)

	seen := map[string]bool{}
	for _, m := range c.messages() {
		seen[m.ID] = true
	}
	for i := range n {
		if !seen[id(i)] {
			t.Errorf("message %s was never delivered — at-least-once means at least once", id(i))
		}
	}
}

func testErrorReachesDriver(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	c := newCollector()
	c.errs["bad"] = stderrors.New("handler said no")
	subscribe(t, sub, "orders", c.handle)
	ready(t, pub, c, "orders")

	// A handler error must not kill the subscription: the next message still
	// arrives. (What the driver does with the nack — redeliver or not — is
	// its own column; the suite only requires survival.)
	if err := pub.Publish(context.Background(), "orders",
		broker.Message{ID: "bad"}, broker.Message{ID: "good"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	awaitCount(t, c, 2)
}

func testBatch(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	c := newCollector()
	subscribe(t, sub, "orders", c.handle)
	ready(t, pub, c, "orders")

	batch := make([]broker.Message, 100)
	for i := range batch {
		batch[i] = broker.Message{ID: id(i), Type: "order.placed"}
	}
	// One call — the outbox relay publishes a batch this way.
	if err := pub.Publish(context.Background(), "orders", batch...); err != nil {
		t.Fatalf("Publish batch: %v", err)
	}
	awaitCount(t, c, 100)
}

func testTopicIsolation(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	orders, billing := newCollector(), newCollector()
	subscribe(t, sub, "orders", orders.handle)
	subscribe(t, sub, "billing", billing.handle)
	ready(t, pub, orders, "orders")
	ready(t, pub, billing, "billing")

	if err := pub.Publish(context.Background(), "orders", broker.Message{ID: "o1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	awaitCount(t, orders, 1)
	if got := billing.count(); got != 0 {
		t.Errorf("the billing subscription received %d messages from the orders topic", got)
	}
}

func testFanOut(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	a, b := newCollector(), newCollector()
	subscribe(t, sub, "orders", a.handle)
	subscribe(t, sub, "orders", b.handle)
	ready(t, pub, a, "orders")
	ready(t, pub, b, "orders")

	if err := pub.Publish(context.Background(), "orders", broker.Message{ID: "o1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	awaitCount(t, a, 1)
	awaitCount(t, b, 1)
}

func testOrdering(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	c := newCollector()
	subscribe(t, sub, "orders", c.handle)
	ready(t, pub, c, "orders")

	const n = 30
	for i := range n {
		m := broker.Message{ID: id(i), Key: "one-key", Type: "order.placed"}
		if err := pub.Publish(context.Background(), "orders", m); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	awaitCount(t, c, n)

	// Messages sharing a Key arrive in publish order. A monolith that relies
	// on this must keep working after extraction to a partitioned driver,
	// which is why every driver owes the same guarantee.
	var last = -1
	for _, m := range c.messages() {
		if m.Key != "one-key" {
			continue
		}
		i := index(m.ID)
		if i <= last {
			t.Fatalf("per-key ordering broken: %s arrived after %s", m.ID, id(last))
		}
		last = i
	}
}

func testDrain(t *testing.T, newBroker NewBroker) {
	pub, sub := newBroker(t)
	c := newCollector()
	cancel, done := subscribe(t, sub, "orders", c.handle)
	ready(t, pub, c, "orders")

	if err := pub.Publish(context.Background(), "orders", broker.Message{ID: "o1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	awaitCount(t, c, 1)

	// Shutdown step 3: cancelling the context stops fetching and Subscribe
	// returns — the subscription is a lifecycle component, not a goroutine
	// left running.
	cancel()
	select {
	case err := <-done:
		if err != nil && !stderrors.Is(err, context.Canceled) {
			t.Errorf("Subscribe returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return after its context was cancelled — the subscription cannot drain")
	}
}

func id(i int) string {
	return "evt-" + itoa(i)
}

func index(id string) int {
	n := 0
	for _, r := range id[len("evt-"):] {
		n = n*10 + int(r-'0')
	}
	return n
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
