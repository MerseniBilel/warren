package broker

import (
	"context"
	"maps"

	"github.com/MerseniBilel/warren/log"
)

// CorrelationHeader is the message header the correlation ID travels in. It
// is the broker-side counterpart of transport/http's X-Correlation-Id, and it
// is lower-case because a broker header map is a plain map of strings with no
// canonicalisation rules — a driver that round-trips keys verbatim and one
// that lower-cases them must agree, so the wire form is fixed here.
const CorrelationHeader = "correlation-id"

// Correlating returns a Publisher that copies the context's correlation ID
// into every message's headers, so the work a consumer does on the other side
// belongs to the request that caused it.
//
// Without it the trail ends at the broker: a request logged under one ID
// published an event, and every line the consumer wrote while handling that
// event belonged to no request at all. Nothing in broker/ or outbox/ carried
// the ID, so the two halves of one causal chain could not be joined.
//
// It never OVERWRITES a header that is already there. That rule is what makes
// the outbox work: outbox.Sink stamps the ID at Append, inside the request,
// and the relay publishes minutes later from a background context that has no
// correlation ID of its own. A decorator that overwrote would replace the one
// true value with nothing at exactly the moment it mattered.
//
// It never stamps an EMPTY id either. A blank header reads as a correlation
// ID that is genuinely blank, and the consumer would seed one.
//
// Correlating(nil) is nil, so wrapping whatever boot resolved is safe in a
// module that may have no broker configured.
func Correlating(next Publisher) Publisher {
	if next == nil {
		return nil
	}
	return correlatingPublisher{next: next}
}

type correlatingPublisher struct{ next Publisher }

// Redelivers forwards the wrapped driver's answer.
//
// A decorator that swallows it is worse than one that never existed: the
// driver says "I cannot redeliver", the wrapper reports the default "I can",
// and DeadLetter nacks an exhausted message into nothing — in exactly the
// configuration the scaffold ships, since platform hands Pipeline a
// Correlating publisher and never the raw broker. Every decorator added to
// this package has to forward it.
func (p correlatingPublisher) Redelivers() bool { return redelivers(p.next) }

func (p correlatingPublisher) Publish(ctx context.Context, topic string, msgs ...Message) error {
	id := log.CorrelationID(ctx)
	if id == "" {
		return p.next.Publish(ctx, topic, msgs...)
	}

	// Copy before writing. The relay hands the same records to a retry and,
	// with several topics in flight, to concurrent publishes — writing into
	// the caller's map would be a data race on someone else's value.
	var stamped []Message
	for i, m := range msgs {
		if _, ok := m.Headers[CorrelationHeader]; ok {
			continue // already carries one; leave the batch untouched so far
		}
		if stamped == nil {
			stamped = make([]Message, len(msgs))
			copy(stamped, msgs)
		}
		h := make(map[string]string, len(m.Headers)+1)
		maps.Copy(h, m.Headers)
		h[CorrelationHeader] = id
		stamped[i].Headers = h
	}
	if stamped == nil {
		return p.next.Publish(ctx, topic, msgs...)
	}
	return p.next.Publish(ctx, topic, stamped...)
}

// correlate seeds log.CorrelationID from the delivery's header, so a
// consumer's own log lines — and anything it publishes onward — carry the ID
// of the request that started the chain.
//
// It seeds nothing when the header is absent. A message from something that
// is not a Warren service has no correlation header, and inventing one would
// tie unrelated work together under an identifier that means nothing.
func correlate() Middleware {
	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			if id := msg.Headers[CorrelationHeader]; id != "" {
				ctx = log.WithCorrelationID(ctx, id)
			}
			return next(ctx, msg)
		}
	}
}
