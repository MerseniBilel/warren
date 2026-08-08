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
// are ready and drains on shutdown — cancellation of ctx stops fetching and
// lets in-flight messages finish and ack. Never a goroutine someone forgot
// about.
type Subscriber interface {
	// Subscribe registers h for topic and RETURNS ONCE THE SUBSCRIPTION IS
	// LIVE — once a Publish to that topic will reach h. It does not block
	// for the lifetime of the subscription; the driver owns the delivery
	// loop, which runs until ctx is cancelled.
	//
	// Returning early is the whole contract, and it exists because the
	// alternative silently loses messages. Subscribe used to block, so every
	// caller wrapped it in a goroutine:
	//
	//	OnStart: func(context.Context) error {
	//	    go func() { _ = sub.Subscribe(ctx, topic, h) }()
	//	    return nil          // "started" — but not yet subscribed
	//	}
	//
	// which reports success before the subscription exists. warren.md §1.3
	// step 6 promises consumers start before publishers, and that promise was
	// being defeated by the very code the framework generates: anything
	// published in the gap went to a topic with no subscriber and was
	// discarded — and the outbox relay, seeing a successful Publish, marked
	// the record published. Silent, permanent event loss on the path the
	// framework recommends. In tests it showed up as the first module test in
	// a process passing and every later one receiving nothing at all.
	//
	// A driver that cannot register synchronously must block until it can:
	// failing the boot because a broker is unreachable is the correct
	// outcome, and it is the reason this returns an error.
	//
	// THE CONTEXT HANDED TO h FOR EVERY DELIVERY MUST CARRY ctx's VALUES.
	// Cancellation is the driver's own — a shared poll loop may not stop
	// fetching for one subscription because another was cancelled — but the
	// values are the application's, and a driver that manufactures a delivery
	// context from context.Background() severs them.
	//
	// This is not a formality. app.Telemetry rides the context, and
	// TraceExtract and InjectTrace both reach it through
	// app.TelemetryFromContext: a driver that drops the values makes every
	// consumer span a new root and strips traceparent from every event the
	// consumer raises, with nothing failing anywhere. The two shipped drivers
	// disagreed about this until it was written down — memory passed the
	// context through, kafka rebuilt it from Background — so brokertest now
	// checks it, and a fix seeded through the context would otherwise have
	// passed on the in-process broker and done nothing in production.
	Subscribe(ctx context.Context, topic string, h MessageHandler) error
}

// Redeliverer is implemented by a driver that can say whether nacking a
// message returns it for another attempt.
//
// warren.md §2.6 gives UNAVAILABLE the consumer disposition "nack + backoff
// retry" and no dead-letter, which is right — but it rests on a PREMISE, and
// the premise is this method. A broker with a durable log and an
// acknowledgement protocol redelivers, so nacking is lossless and a DLQ
// would turn a transient blip into a queue of messages that would have
// succeeded. An in-process broker has neither, so a nack there is a DROP —
// and UNAVAILABLE, the code that means "try again", became the only lossy
// one while INVALID and INTERNAL were preserved.
//
// A driver that does not implement this is assumed to redeliver, which is
// true of every durable broker and keeps the documented behaviour for them.
//
// # The answer must be constant for a driver
//
// It is asked ONCE, when the pipeline is composed, and it is asked of the
// publisher rather than of a subscription — so a driver's answer must hold
// for every subscription it serves. Kafka (offset rewind), RabbitMQ (nack
// with requeue) and JetStream (Nak) are all constantly true; the in-process
// broker is constantly false.
//
// A driver that cannot promise that must expose TWO client values rather than
// one that answers differently per subscription — NATS Core and JetStream
// from a single connection is the case that forces this, and so is a RabbitMQ
// driver mixing requeue and no-requeue subscriptions. Moving this method onto
// Subscriber would make it per-subscription, and that is a breaking change to
// an exported interface, which is why the constraint is written down here
// rather than discovered later.
//
// It also follows that the dead-letter publisher handed to Pipeline must be
// the SAME driver as the subscription. If it is a different broker, the
// pipeline asks the wrong one whether a nack comes back.
type Redeliverer interface {
	// Redelivers reports whether a nacked message comes back. It must return
	// the same value for the lifetime of the driver.
	Redelivers() bool
}

// redelivers reports whether v is a driver that has declared it will not
// redeliver. Anything else — including a plain Publisher — is assumed to.
func redelivers(v any) bool {
	if r, ok := v.(Redeliverer); ok {
		return r.Redelivers()
	}
	return true
}

// MessageHandler processes one message. Returning nil acknowledges it;
// returning an error hands it to the retry and dead-letter middleware, which
// decide by the error's warren/errors code — the consumer column of the
// warren.md §2.6 table. A handler that maps a code to an ack decision itself
// has broken the ring: that decision belongs to the chain.
type MessageHandler func(ctx context.Context, msg Message) error
