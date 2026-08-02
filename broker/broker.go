// Package broker defines Warren's messaging ports: one driver-neutral message
// envelope, a publisher, a subscriber, and a message handler.
//
// Consumers written against these types run identically over Kafka, RabbitMQ,
// NATS, and the in-process broker. A consumer that touches a driver's record
// type loses that property, which is why the raw client is an explicit escape
// hatch and not the default path. The port carries zero implementations
// (invariant 5): the consumer middleware chain and the per-subscription
// options land with the runtime, and every driver runs the same exported
// contract suite.
package broker

import (
	"context"
	"time"
)

// Message is the driver-neutral envelope every adapter translates to and
// from.
type Message struct {
	// ID is the idempotency key. Inbox dedupe is keyed on it, so it must be
	// stable across redeliveries of the same fact.
	ID string

	// Type is the fact's name, such as "user.registered" — the same value a
	// domain.Event reports from EventName.
	Type string

	// Key is the partition or routing key.
	Key string

	// Payload is the encoded body. This package does not define its format.
	Payload []byte

	// Headers carries metadata across the broker; trace context propagates
	// here as strings, so a span survives the trip into the consumer without
	// the core module knowing a telemetry SDK exists.
	Headers map[string]string

	// OccurredAt is when the fact happened, not when it was published.
	OccurredAt time.Time
}

// Publisher sends messages to a topic. The outbox relay is its primary
// caller — it is variadic so a relay batch is one call; use cases publish
// through the unit of work, not directly.
type Publisher interface {
	Publish(ctx context.Context, topic string, msgs ...Message) error
}

// Subscriber consumes a topic, invoking the handler for each message. A
// subscription is a lifecycle component: it starts after its dependencies
// are ready and drains on shutdown — cancellation of ctx stops fetching,
// lets in-flight messages finish and ack, and returns. Never a goroutine
// someone forgot about.
type Subscriber interface {
	Subscribe(ctx context.Context, topic string, h MessageHandler) error
}

// MessageHandler processes one message. Returning nil acknowledges it;
// returning an error hands it to the retry and dead-letter middleware, which
// decide by the error's warren/errors code — the consumer column of the
// warren.md §2.6 table. A handler that maps a code to an ack decision itself
// has broken the ring: that decision belongs to the chain.
type MessageHandler func(ctx context.Context, msg Message) error
