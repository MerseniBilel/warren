package memory_test

import (
	"context"
	"runtime"
	"strconv"
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

func BenchmarkPublishDeliver(b *testing.B) {
	br := memory.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan struct{}, 1<<20)
	go func() {
		_ = br.Subscribe(ctx, "t", func(context.Context, broker.Message) error {
			received <- struct{}{}
			return nil
		})
	}()
	m := broker.Message{ID: "evt", Type: "t"}
	b.ReportAllocs()
	for b.Loop() {
		_ = br.Publish(ctx, "t", m)
	}
}
