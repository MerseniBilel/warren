package broker_test

import (
	"context"
	"testing"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/log"
)

// recordingPublisher keeps what it was handed, so a test can assert on the
// headers that actually crossed the boundary.
type recordingPublisher struct {
	topic string
	msgs  []broker.Message
}

func (p *recordingPublisher) Publish(_ context.Context, topic string, msgs ...broker.Message) error {
	p.topic = topic
	p.msgs = append(p.msgs, msgs...)
	return nil
}

// TestCorrelatingStampsTheID — the defect this closes: a request logged under
// correlation ID X published an event, and every log line the consumer wrote
// while handling it belonged to no request at all. The trail ended at the
// broker.
func TestCorrelatingStampsTheID(t *testing.T) {
	t.Parallel()

	rec := &recordingPublisher{}
	pub := broker.Correlating(rec)
	ctx := log.WithCorrelationID(context.Background(), "corr-1")

	if err := pub.Publish(ctx, "orders", broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(rec.msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(rec.msgs))
	}
	if got := rec.msgs[0].Headers[broker.CorrelationHeader]; got != "corr-1" {
		t.Errorf("header %q = %q, want %q", broker.CorrelationHeader, got, "corr-1")
	}
	if rec.topic != "orders" {
		t.Errorf("topic = %q — the decorator must pass everything else through", rec.topic)
	}
}

// TestCorrelatingDoesNotOverwriteAnExistingHeader — the outbox stamps the ID
// at Append, inside the request. The relay publishes much later, from a
// background context that has no correlation ID at all; if the decorator
// overwrote, it would erase the only true value with an empty one.
func TestCorrelatingDoesNotOverwriteAnExistingHeader(t *testing.T) {
	t.Parallel()

	rec := &recordingPublisher{}
	pub := broker.Correlating(rec)
	// A relay's context: no correlation ID.
	err := pub.Publish(context.Background(), "orders", broker.Message{
		ID:      "m-1",
		Headers: map[string]string{broker.CorrelationHeader: "from-the-outbox"},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := rec.msgs[0].Headers[broker.CorrelationHeader]; got != "from-the-outbox" {
		t.Errorf("header = %q, want the outbox's own value preserved", got)
	}
}

// TestCorrelatingAddsNothingWhenThereIsNothingToAdd — an empty header is
// worse than no header: it looks like a correlation ID that is genuinely
// blank, and consumers would seed one.
func TestCorrelatingAddsNothingWhenThereIsNothingToAdd(t *testing.T) {
	t.Parallel()

	rec := &recordingPublisher{}
	pub := broker.Correlating(rec)
	if err := pub.Publish(context.Background(), "orders", broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, ok := rec.msgs[0].Headers[broker.CorrelationHeader]; ok {
		t.Error("an empty correlation ID was stamped as a header")
	}
}

// TestCorrelatingDoesNotMutateTheCallersMessage — the relay reuses the
// records it read from the store, and a decorator writing into a shared map
// would be a data race between two concurrent publishes of the same batch.
func TestCorrelatingDoesNotMutateTheCallersMessage(t *testing.T) {
	t.Parallel()

	rec := &recordingPublisher{}
	pub := broker.Correlating(rec)
	ctx := log.WithCorrelationID(context.Background(), "corr-1")

	headers := map[string]string{"trace": "t-1"}
	msg := broker.Message{ID: "m-1", Headers: headers}
	if err := pub.Publish(ctx, "orders", msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, ok := headers[broker.CorrelationHeader]; ok {
		t.Error("the decorator wrote into the caller's own headers map")
	}
	if got := rec.msgs[0].Headers["trace"]; got != "t-1" {
		t.Errorf("existing header lost: trace = %q", got)
	}
}

// TestCorrelatingIsNilSafe — Correlating wraps whatever boot resolved, and a
// module with no broker configured resolves nothing.
func TestCorrelatingIsNilSafe(t *testing.T) {
	t.Parallel()

	if got := broker.Correlating(nil); got != nil {
		t.Error("Correlating(nil) must stay nil rather than produce a publisher that panics on first use")
	}
}

// TestPipelineSeedsTheCorrelationIDOnTheConsumer — the receiving half. A
// consumer's own log lines, and everything it publishes onward, must belong
// to the request that started it.
func TestPipelineSeedsTheCorrelationIDOnTheConsumer(t *testing.T) {
	t.Parallel()

	var seen string
	h, _ := broker.Pipeline("subs", "orders", func(ctx context.Context, _ broker.Message) error {
		seen = log.CorrelationID(ctx)
		return nil
	}, inbox.NewMemoryStore(), &recordingPublisher{})

	err := h(context.Background(), broker.Message{
		ID:      "m-1",
		Headers: map[string]string{broker.CorrelationHeader: "corr-1"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if seen != "corr-1" {
		t.Errorf("log.CorrelationID(ctx) = %q inside the consumer, want %q — the trail still ends at the broker", seen, "corr-1")
	}
}

// TestPipelineWithoutTheHeaderSeedsNothing — a message published by something
// that is not a Warren service has no correlation header, and inventing one
// silently would tie unrelated work together.
func TestPipelineWithoutTheHeaderSeedsNothing(t *testing.T) {
	t.Parallel()

	var seen string
	h, _ := broker.Pipeline("subs", "orders", func(ctx context.Context, _ broker.Message) error {
		seen = log.CorrelationID(ctx)
		return nil
	}, inbox.NewMemoryStore(), &recordingPublisher{})

	if err := h(context.Background(), broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if seen != "" {
		t.Errorf("log.CorrelationID(ctx) = %q with no header on the message", seen)
	}
}
