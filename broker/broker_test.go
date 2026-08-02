package broker_test

import (
	"context"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/broker"
)

// The port is types only (invariant 5: zero implementations), so the suite
// pins the envelope's shape, the interfaces' shapes, and the ring position —
// the full behavioural contract suite lands with broker/memory, before any
// real driver.

// memoryPublisher is a minimal in-test double proving the interfaces are
// implementable and the envelope survives a publish→subscribe round trip
// with every field intact — the property that keeps distributed tracing
// alive across the broker.
type memoryPublisher struct {
	byTopic map[string][]broker.Message
}

func (m *memoryPublisher) Publish(_ context.Context, topic string, msgs ...broker.Message) error {
	if m.byTopic == nil {
		m.byTopic = map[string][]broker.Message{}
	}
	m.byTopic[topic] = append(m.byTopic[topic], msgs...)
	return nil
}

func (m *memoryPublisher) Subscribe(_ context.Context, topic string, h broker.MessageHandler) error {
	for _, msg := range m.byTopic[topic] {
		if err := h(context.Background(), msg); err != nil {
			return err
		}
	}
	return nil
}

var (
	_ broker.Publisher  = (*memoryPublisher)(nil)
	_ broker.Subscriber = (*memoryPublisher)(nil)
)

func TestEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	sent := broker.Message{
		ID:      "evt-42",
		Type:    "user.registered",
		Key:     "user-7",
		Payload: []byte(`{"email":"bob@example.com"}`),
		Headers: map[string]string{
			"traceparent":    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"correlation-id": "req-9",
		},
		OccurredAt: time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC),
	}

	m := &memoryPublisher{}
	if err := m.Publish(context.Background(), "user.events", sent); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var got broker.Message
	err := m.Subscribe(context.Background(), "user.events", func(_ context.Context, msg broker.Message) error {
		got = msg
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if got.ID != sent.ID || got.Type != sent.Type || got.Key != sent.Key {
		t.Errorf("identity fields drifted: got %+v", got)
	}
	if string(got.Payload) != string(sent.Payload) {
		t.Errorf("Payload drifted: %q", got.Payload)
	}
	if !maps.Equal(got.Headers, sent.Headers) {
		t.Errorf("Headers drifted: %v — losing them silently breaks tracing across the broker", got.Headers)
	}
	if !got.OccurredAt.Equal(sent.OccurredAt) {
		t.Errorf("OccurredAt drifted: %v", got.OccurredAt)
	}
}

func TestPublishIsVariadicForRelayBatches(t *testing.T) {
	t.Parallel()

	// The outbox relay publishes a batch in one call (§5.5's BatchSize).
	m := &memoryPublisher{}
	batch := make([]broker.Message, 100)
	for i := range batch {
		batch[i] = broker.Message{ID: "evt", Type: "t"}
	}
	if err := m.Publish(context.Background(), "topic", batch...); err != nil {
		t.Fatalf("Publish batch: %v", err)
	}
	if len(m.byTopic["topic"]) != 100 {
		t.Errorf("delivered %d, want the whole batch", len(m.byTopic["topic"]))
	}
}

func TestHandlerNilAcknowledges(t *testing.T) {
	t.Parallel()

	// The §2.6 consumer contract: nil acks; a non-nil error is classified by
	// its warren/errors code by the chain (which lands with broker/memory).
	// Here the port-level fact under test is only that MessageHandler is a
	// plain func type any closure satisfies.
	var h broker.MessageHandler = func(context.Context, broker.Message) error { return nil }
	if err := h(context.Background(), broker.Message{}); err != nil {
		t.Fatalf("handler: %v", err)
	}
}

// TestRingPosition holds the package to CONTRACTS rules: standard library
// only — no driver, no OTel; trace context travels as strings in Headers.
func TestRingPosition(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"context": true, "time": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !allowed[path] {
				t.Errorf("%s imports %q — the broker port is stdlib-only: no kgo, no amqp, no OTel", e.Name(), path)
			}
		}
	}
}
