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
	stderrors "errors"
	"log/slog"
	"sync"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
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
	logger *slog.Logger

	mu     sync.RWMutex
	topics map[string][]*subscription
}

// WithLogger sets where a dropped disposition and an abandoned queue are
// reported. The default is slog.Default() at the moment the record is
// emitted, so an application that installs its handler in main is covered
// without wiring anything.
func WithLogger(l *slog.Logger) Option {
	return func(b *Broker) { b.logger = l }
}

// at returns the logger to report through.
func (b *Broker) at(ctx context.Context) *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return log.FromContext(ctx)
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

// Subscribe registers h against topic and RETURNS ONCE IT IS LIVE — the
// registration below happens before this function returns, so a Publish that
// is ordered after it cannot miss the subscription. Delivery then runs in the
// background, one message at a time, in publish order, until ctx is
// cancelled; the in-flight message finishes first, so cancellation is a
// drain, not an abort.
//
// The registration was always synchronous and always first. What changed is
// that the caller can now RELY on it: while Subscribe blocked for the
// subscription's lifetime, every caller had to spawn a goroutine and had no
// way to know when the line below had run — so a publish racing boot went to
// a topic that, as far as this map was concerned, nobody was listening to.
//
// A handler error does not end the subscription — the consumer chain owns
// dispositions, and a disposition this driver cannot honour is logged rather
// than dropped in silence.
func (b *Broker) Subscribe(ctx context.Context, topic string, h broker.MessageHandler) error {
	if h == nil {
		panic("memory: Subscribe with a nil handler")
	}
	s := &subscription{queue: make(chan broker.Message, b.buffer), done: make(chan struct{})}

	b.mu.Lock()
	b.topics[topic] = append(b.topics[topic], s)
	b.mu.Unlock()

	go b.deliver(ctx, topic, s, h)
	return nil
}

// deliver is the subscription's loop, owned by the driver rather than by the
// caller's goroutine.
func (b *Broker) deliver(ctx context.Context, topic string, s *subscription, h broker.MessageHandler) {
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
			// The handler's error is the chain's business and the
			// subscription survives it — but it must not vanish.
			//
			// broker.DeadLetter nacks UNAVAILABLE on the stated grounds that
			// "the broker redelivers". This driver does not: there is no
			// durable log to re-read and no acknowledgement protocol. So an
			// exhausted retry used to be neither handled, dead-lettered,
			// redelivered nor logged — the silent loss warren.md §5 forbids.
			//
			// Whether an in-process broker SHOULD redeliver is a design
			// decision with real consequences (a poison message would loop
			// for ever). Saying so out loud is not.
			if err := h(ctx, msg); err != nil {
				b.at(ctx).ErrorContext(ctx, "message dropped: the in-process broker does not redeliver",
					"topic", topic,
					"message_id", msg.ID,
					"code", string(codeOf(err)),
					"error", err.Error())
			}
		case <-ctx.Done():
			b.abandon(ctx, topic, s)
			return
		}
	}
}

// abandon reports the messages still queued when the subscription's context
// was cancelled.
//
// They are lost: this driver holds them in a channel, not on disk, so a
// process that is going away takes them with it. What is fixable is the
// silence — five messages published, one handled and four discarded used to
// produce a shutdown log reading only "stopped".
func (b *Broker) abandon(ctx context.Context, topic string, s *subscription) {
	n := len(s.queue)
	if n == 0 {
		return
	}
	ids := make([]string, 0, n)
	for range n {
		select {
		case msg := <-s.queue:
			ids = append(ids, msg.ID)
		default:
		}
	}
	// The context is already cancelled, so this record is emitted against a
	// background one: a cancelled ctx is not a reason to lose the report of
	// what was lost.
	b.at(ctx).ErrorContext(context.WithoutCancel(ctx),
		"messages abandoned: the in-process broker holds its queue in memory",
		"topic", topic,
		"count", n,
		"message_ids", ids)
}

// codeOf reads the OUTERMOST warren/errors code, which is the one an adapter
// maps — wrapping is recategorization. An error from outside the vocabulary
// is INTERNAL, the same default the §2.6 table uses.
func codeOf(err error) errors.Code {
	var e *errors.Error
	if stderrors.As(err, &e) && e != nil {
		return e.Code()
	}
	return errors.CodeInternal
}
