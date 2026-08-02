// Package memory is the in-process broker: the default in tests, and the
// driver a modular monolith runs in production before its modules are
// extracted into services. It is channels and goroutines — no third party,
// no network — which is why it lives in the core module rather than an
// adapter module of its own.
//
// It is what makes `warren extract module` viable: modules communicate
// through the broker port from day one, so extraction swaps the driver
// rather than rewriting call sites. Delivery is asynchronous and FIFO per
// subscription, so messages sharing a Key arrive in publish order — the
// guarantee a monolith may rely on and a partitioned driver preserves.
package memory

import (
	"context"
	"sync"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/errors"
)

// Option configures the broker.
type Option func(*Broker)

// WithBuffer sets the per-subscription queue depth. A publish to a full
// queue blocks until the subscriber catches up or the publishing context is
// cancelled — backpressure, not silent loss. The default is 1024.
func WithBuffer(n int) Option {
	if n <= 0 {
		panic("memory: WithBuffer requires a positive depth")
	}
	return func(b *Broker) { b.buffer = n }
}

// Broker is an in-process Publisher and Subscriber. The zero value is not
// usable; construct one with New. It is safe for concurrent use.
type Broker struct {
	buffer int

	mu     sync.RWMutex
	topics map[string][]*subscription
}

// New returns an in-process broker.
func New(opts ...Option) *Broker {
	b := &Broker{buffer: 1024, topics: map[string][]*subscription{}}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

var (
	_ broker.Publisher  = (*Broker)(nil)
	_ broker.Subscriber = (*Broker)(nil)
)

type subscription struct {
	queue chan broker.Message
	done  chan struct{}
}

// Publish delivers msgs to every subscription of topic, in order. It blocks
// while a subscription's queue is full and returns ctx.Err() wrapped as
// UNAVAILABLE if the context ends first — the caller's cue to retry, never
// a dropped message.
//
// A topic with no live subscription accepts messages and discards them: this
// is pub/sub without a log, so nothing retains a message for a subscriber
// that has not arrived yet. Boot order matters — consumers start before
// publishers (§1.3 step 6) — and a test that publishes before its
// subscription is delivering will see nothing.
func (b *Broker) Publish(ctx context.Context, topic string, msgs ...broker.Message) error {
	b.mu.RLock()
	subs := append([]*subscription(nil), b.topics[topic]...)
	b.mu.RUnlock()

	for _, msg := range msgs {
		for _, s := range subs {
			select {
			case s.queue <- msg:
			case <-s.done:
				// The subscription drained away mid-publish; its peers
				// still receive.
			case <-ctx.Done():
				return errors.Unavailable("memory broker", ctx.Err())
			}
		}
	}
	return nil
}

// Subscribe registers h against topic and delivers messages to it, one at a
// time, in publish order. It blocks until ctx is cancelled, then returns
// nil: the in-flight message finishes first, so a cancelled Subscribe is a
// drain, not an abort. A handler error does not end the subscription — the
// consumer chain owns dispositions.
func (b *Broker) Subscribe(ctx context.Context, topic string, h broker.MessageHandler) error {
	if h == nil {
		panic("memory: Subscribe with a nil handler")
	}
	s := &subscription{queue: make(chan broker.Message, b.buffer), done: make(chan struct{})}

	b.mu.Lock()
	b.topics[topic] = append(b.topics[topic], s)
	b.mu.Unlock()

	defer func() {
		close(s.done)
		b.mu.Lock()
		subs := b.topics[topic]
		for i, other := range subs {
			if other == s {
				b.topics[topic] = append(subs[:i:i], subs[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
	}()

	for {
		select {
		case msg := <-s.queue:
			// The handler's error is the chain's business; the subscription
			// survives it.
			_ = h(ctx, msg)
		case <-ctx.Done():
			return nil
		}
	}
}
