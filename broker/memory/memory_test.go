package memory_test

import (
	"context"
	stderrors "errors"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/brokertest"
	"github.com/MerseniBilel/warren/broker/memory"
	werrors "github.com/MerseniBilel/warren/errors"
)

// TestContract runs the exported suite every driver must pass. Kafka,
// RabbitMQ, and NATS run this same function behind a build tag.
func TestContract(t *testing.T) {
	t.Parallel()
	brokertest.Run(t, func(*testing.T) (broker.Publisher, broker.Subscriber) {
		b := memory.New()
		return b, b
	})
}

func TestPublishToNobodyIsNotAnError(t *testing.T) {
	t.Parallel()

	// A topic with no subscription is normal at boot — publishing must not
	// fail, and must not block.
	if err := memory.New().Publish(context.Background(), "nobody", broker.Message{ID: "1"}); err != nil {
		t.Errorf("Publish to an unsubscribed topic: %v", err)
	}
}

func TestBackpressure(t *testing.T) {
	t.Parallel()

	// One subscriber, a one-deep queue, and a handler that never returns:
	// after two messages there is nowhere to put a third. Publishing must
	// block — backpressure — and surface its context's cancellation as
	// UNAVAILABLE, never drop the message.
	b := memory.New(memory.WithBuffer(1))
	release := make(chan struct{})
	defer close(release)
	handling := make(chan struct{})
	var once sync.Once

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = b.Subscribe(ctx, "t", func(context.Context, broker.Message) error {
			once.Do(func() { close(handling) })
			<-release
			return nil
		})
	}()

	// A publish to a topic with no live subscription is a no-op, so warm up
	// until the handler is actually entered — the readiness discipline the
	// contract suite uses, for the same reason. The warm-up runs in its own
	// goroutine because once the handler wedges, a warm-up publish can block
	// on the full queue; its context releases it.
	warmCtx, warmCancel := context.WithCancel(context.Background())
	warmed := make(chan struct{})
	go func() {
		defer close(warmed)
		for warmCtx.Err() == nil {
			if err := b.Publish(warmCtx, "t", broker.Message{ID: "warm"}); err != nil {
				return
			}
			runtime.Gosched()
		}
	}()
	select {
	case <-handling:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription never began handling")
	}
	warmCancel()
	<-warmed

	pubCtx, pubCancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		// Far more than capacity: whatever the subscription's exact state,
		// this run cannot complete while the handler is wedged.
		for i := range 100 {
			if err := b.Publish(pubCtx, "t", broker.Message{ID: strconv.Itoa(i)}); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()

	// It must still be blocked, not finished and not failed.
	for range 10_000 {
		select {
		case err := <-errCh:
			t.Fatalf("publishing past capacity did not block: %v", err)
		default:
			runtime.Gosched()
		}
	}

	pubCancel()
	select {
	case err := <-errCh:
		if !werrors.Is(err, werrors.CodeUnavailable) {
			t.Errorf("blocked publish = %v, want UNAVAILABLE — the caller retries, the message is never dropped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled publish never returned")
	}
}

func TestSubscribeReturnsOnCancel(t *testing.T) {
	t.Parallel()

	b := memory.New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- b.Subscribe(ctx, "t", func(context.Context, broker.Message) error { return nil })
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Subscribe = %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return")
	}

	// The drained subscription is deregistered: publishing must not block on
	// its abandoned queue.
	if err := b.Publish(context.Background(), "t", broker.Message{ID: "1"}); err != nil {
		t.Errorf("Publish after the subscriber left: %v", err)
	}
}

func TestNilHandlerPanicsAtSubscribe(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Subscribe with a nil handler did not panic")
		}
	}()
	_ = memory.New().Subscribe(context.Background(), "t", nil)
}

// BenchmarkPublishDeliver times one full publish → handler round trip.
//
// The channel is UNBUFFERED on purpose, so an iteration cannot finish until
// the handler has run. A buffered one lets the loop outrun the subscriber
// and measures enqueueing rather than delivery — and then deadlocks when
// the buffer fills, which a default -benchtime reaches easily.
func BenchmarkPublishDeliver(b *testing.B) {
	br := memory.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delivered := make(chan struct{})
	go func() {
		_ = br.Subscribe(ctx, "t", func(context.Context, broker.Message) error {
			delivered <- struct{}{}
			return nil
		})
	}()

	m := broker.Message{ID: "evt", Type: "t"}
	awaitSubscriber(b, br, ctx, delivered, m)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := br.Publish(ctx, "t", m); err != nil {
			b.Fatal(err)
		}
		<-delivered
	}
}

// awaitSubscriber publishes until the subscription is delivering, then
// drains what queued behind the probe so the timed loop starts balanced.
//
// It is needed because a topic with no live subscription accepts messages
// and discards them (see Publish): without it the first publish can vanish
// and the first receive blocks forever.
func awaitSubscriber(b *testing.B, br *memory.Broker, ctx context.Context, delivered <-chan struct{}, m broker.Message) {
	b.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := br.Publish(ctx, "t", m); err != nil {
			b.Fatal(err)
		}
		select {
		case <-delivered:
			// Drain the probes that queued while we were waiting; each
			// receive releases one blocked handler, and the silence means
			// the subscription is back to waiting on an empty queue.
			for {
				select {
				case <-delivered:
				case <-time.After(20 * time.Millisecond):
					return
				}
			}
		case <-time.After(time.Millisecond):
		}
		if time.Now().After(deadline) {
			b.Fatal("the subscription never started delivering")
		}
	}
}

// TestADroppedDispositionIsNotSilent — the in-process driver did
// `_ = h(ctx, msg)`. broker.DeadLetter nacks UNAVAILABLE precisely because
// "the broker redelivers", and this driver does not redeliver: after the
// retries were exhausted the message was neither handled, dead-lettered,
// redelivered nor logged. It was gone, and the app's logs were clean.
//
// Whether an in-process broker SHOULD redeliver is a design decision. That
// it must not lose a message in silence is not: warren.md §5 says duplicates
// over loss, never silently.
func TestADroppedDispositionIsNotSilent(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	b := memory.New(memory.WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := make(chan struct{}, 1)
	go func() {
		_ = b.Subscribe(ctx, "t", func(context.Context, broker.Message) error {
			handled <- struct{}{}
			return werrors.Unavailable("payments", stderrors.New("down"))
		})
	}()

	awaitDelivery(t, b, ctx, handled)
	if err := b.Publish(ctx, "t", broker.Message{ID: "evt-1", Type: "t"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}

	waitFor(t, &buf, "evt-1")
	logged := buf.String()
	for _, want := range []string{"evt-1", "UNAVAILABLE"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the dropped disposition does not mention %q:\n%s", want, logged)
		}
	}
}

// TestMessagesAbandonedAtShutdownAreCounted — Subscribe's doc says a
// cancelled context is "a drain, not an abort", but the buffered queue was
// abandoned with the goroutine: five messages published, one handled, four
// discarded, and the shutdown log said only "stopped".
func TestMessagesAbandonedAtShutdownAreCounted(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	b := memory.New(memory.WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))))

	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Subscribe(ctx, "t", func(context.Context, broker.Message) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		})
	}()

	// Wedge the handler, then queue more behind it. Published in a loop
	// because a topic with no LIVE subscription accepts and discards, so a
	// single publish can land before Subscribe has registered.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := b.Publish(context.Background(), "t", broker.Message{ID: "wedge", Type: "t"}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		select {
		case <-entered:
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("the handler never started")
			}
			continue
		}
		break
	}
	for i := range 4 {
		if err := b.Publish(context.Background(), "t", broker.Message{ID: "q-" + strconv.Itoa(i), Type: "t"}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return")
	}

	waitFor(t, &buf, "abandoned")
	if logged := buf.String(); !strings.Contains(logged, "abandoned") {
		t.Errorf("nothing said the queued messages were discarded:\n%s", logged)
	}
}

func waitFor(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitDelivery(t *testing.T, b *memory.Broker, ctx context.Context, handled <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := b.Publish(ctx, "t", broker.Message{ID: "probe", Type: "t"}); err != nil {
			t.Fatalf("probe publish: %v", err)
		}
		select {
		case <-handled:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("the subscription never started delivering")
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
