package broker_test

import (
	"context"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/memory"
)

// warren.md §7.1: "One import instruments handlers, HTTP requests and BROKER
// PROPAGATION — three of the boundaries automatically." And InjectTrace's own
// doc: "A publisher adapter calls it as its first act."
//
// Neither was true. broker.InjectTrace had no caller anywhere in the
// repository — not an adapter, not the outbox — so nothing ever wrote a
// traceparent onto a message, and the chain's TraceExtract stage read a
// header nobody set. Every consumer span began a NEW ROOT TRACE.
//
// Nothing errors when that happens. The spans exist, the consumer looks
// instrumented, and the one question tracing is bought for — what did this
// request cause — has no answer.

// tracer is the smallest thing satisfying app.Telemetry: it stamps a header
// naming the span it was asked to inject from, which is all a propagation
// test needs to see.
type tracer struct {
	current string
	spans   []string
}

func (t *tracer) Span(ctx context.Context, name string) (context.Context, func(error)) {
	prev := t.current
	t.current = name
	t.spans = append(t.spans, name)
	return ctx, func(error) { t.current = prev }
}

func (t *tracer) Record(string, time.Duration, error) {}

func (t *tracer) Inject(_ context.Context, set func(key, value string)) {
	if t.current == "" {
		return // no active span, nothing to propagate — the relay's case
	}
	set("traceparent", "00-"+t.current)
}

func (t *tracer) Extract(ctx context.Context, get func(key string) string) context.Context {
	if get("traceparent") == "" {
		return ctx
	}
	return ctx
}

func TestPublishingCarriesTheTraceContext(t *testing.T) {
	t.Parallel()
	tel := &tracer{}
	b := memory.New()

	got := make(chan broker.Message, 1)
	if err := b.Subscribe(context.Background(), "orders", func(_ context.Context, m broker.Message) error {
		got <- m
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	ctx := app.WithTelemetry(context.Background(), tel)
	ctx, end := tel.Span(ctx, "place-order")
	defer end(nil)

	if err := b.Publish(ctx, "orders", broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case m := <-got:
		if m.Headers["traceparent"] != "00-place-order" {
			t.Errorf("traceparent = %q, want the publisher's span — headers %v",
				m.Headers["traceparent"], m.Headers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the message never arrived")
	}
}

func TestPublishingWithoutTelemetryAddsNoHeaders(t *testing.T) {
	t.Parallel()
	// An uninstrumented service must not start allocating a header map per
	// message — the nil check is the whole cost it agreed to pay.
	b := memory.New()

	got := make(chan broker.Message, 1)
	if err := b.Subscribe(context.Background(), "orders", func(_ context.Context, m broker.Message) error {
		got <- m
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := b.Publish(context.Background(), "orders", broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case m := <-got:
		if m.Headers != nil {
			t.Errorf("headers = %v, want none", m.Headers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the message never arrived")
	}
}

func TestAnAlreadyStampedTraceIsNotOverwritten(t *testing.T) {
	t.Parallel()
	// The outbox stamps at Append, inside the request. The relay publishes
	// minutes later from a background context whose span is long over, and a
	// publisher that overwrote would replace the one true value with nothing
	// at exactly the moment it mattered — the same rule Correlating follows.
	// The relay under its OWN span, which is the case that bites: an
	// injector that overwrote would reparent every event to the drain that
	// happened to carry it — a trace leading back to a timer.
	tel := &tracer{}
	b := memory.New()

	got := make(chan broker.Message, 1)
	if err := b.Subscribe(context.Background(), "orders", func(_ context.Context, m broker.Message) error {
		got <- m
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	ctx := app.WithTelemetry(context.Background(), tel)
	ctx, end := tel.Span(ctx, "outbox-drain")
	defer end(nil)

	err := b.Publish(ctx, "orders", broker.Message{
		ID:      "m-1",
		Headers: map[string]string{"traceparent": "00-the-request"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case m := <-got:
		if m.Headers["traceparent"] != "00-the-request" {
			t.Errorf("traceparent = %q, want the one stamped at Append", m.Headers["traceparent"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the message never arrived")
	}
}
