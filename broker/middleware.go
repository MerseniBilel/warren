package broker

import (
	"context"
	stderrors "errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/log"
)

// Middleware decorates a MessageHandler — the consumer ring's mirror of
// app.Middleware, one ring over. Written once, it applies to Kafka, Rabbit,
// NATS, and the in-process broker identically; that property is the entire
// messaging pitch, and it holds because no stage ever sees a driver type.
type Middleware func(MessageHandler) MessageHandler

// Chain composes middleware around a handler; mw[0] is the outermost, the
// same convention as app.Chain. Like app.Chain it refuses nil at composition
// time — a boot-time panic naming the position instead of a request-time nil
// dereference (the AGENT.md-sanctioned guard).
func Chain(h MessageHandler, mw ...Middleware) MessageHandler {
	if h == nil {
		panic("broker: Chain composed around a nil handler")
	}
	for i := len(mw) - 1; i >= 0; i-- {
		if mw[i] == nil {
			panic(fmt.Sprintf("broker: Chain middleware %d of %d is nil — append a conditional middleware only when it is enabled", i+1, len(mw)))
		}
		h = mw[i](h)
		if h == nil {
			panic(fmt.Sprintf("broker: middleware %d of %d returned a nil handler", i+1, len(mw)))
		}
	}
	return h
}

// SubscribeOption configures one subscription's pipeline. It is opaque:
// options are minted here, collected at the registration site, and applied
// by Pipeline at boot — drivers only forward them.
type SubscribeOption struct{ apply func(*subscribeConfig) }

type subscribeConfig struct {
	retry       app.RetryPolicy
	dlqTopic    string
	concurrency int
	dedupe      bool
	dedupeTTL   time.Duration
}

// WithRetry sets the subscription's retry policy — the same app.RetryPolicy
// port the core Retrying middleware uses, so a policy written once is
// interchangeable with ExponentialBackoff at every site, inbound or
// consumer-side. One port, two rings, no library. The default is
// ExponentialBackoff(3).
func WithRetry(p app.RetryPolicy) SubscribeOption {
	return SubscribeOption{apply: func(c *subscribeConfig) { c.retry = p }}
}

// WithDeadLetter sets the topic exhausted and terminal messages are routed
// to. The default is "<topic>.dlq".
func WithDeadLetter(topic string) SubscribeOption {
	return SubscribeOption{apply: func(c *subscribeConfig) { c.dlqTopic = topic }}
}

// WithConcurrency caps the number of handler invocations executing
// concurrently. A message waiting out a retry backoff holds no slot. The
// default — omitting the option — is uncapped, bounded only by the driver's
// delivery parallelism; a cap of zero or less is a boot-time panic, because
// a subscription that can process no messages is a config typo, not intent.
func WithConcurrency(n int) SubscribeOption {
	if n <= 0 {
		panic(fmt.Sprintf("broker: WithConcurrency(%d) — a subscription cannot process %d messages; omit the option for uncapped", n, n))
	}
	return SubscribeOption{apply: func(c *subscribeConfig) { c.concurrency = n }}
}

// WithDedupeTTL sets how long a processed Message.ID is remembered. It must
// exceed the subscription's redelivery window. The default is 24 hours.
func WithDedupeTTL(d time.Duration) SubscribeOption {
	return SubscribeOption{apply: func(c *subscribeConfig) { c.dedupeTTL = d }}
}

// WithoutDedupe disables inbox dedupe for this subscription — the named
// opt-out from the on-by-default stance, for handlers that are naturally
// idempotent.
func WithoutDedupe() SubscribeOption {
	return SubscribeOption{apply: func(c *subscribeConfig) { c.dedupe = false }}
}

// ExponentialBackoff returns a bounded exponential app.RetryPolicy:
// attempts total attempts, base 100ms doubling per attempt, capped at 30s,
// with full jitter. Boot-time construction, request-path arithmetic only.
func ExponentialBackoff(attempts int) app.RetryPolicy {
	return expBackoff{attempts: attempts}
}

type expBackoff struct{ attempts int }

func (e expBackoff) Next(attempt int) (time.Duration, bool) {
	if attempt >= e.attempts {
		return 0, false
	}
	// The cap is reached by attempt 10 (100ms<<9 > 30s); computing the shift
	// past that would overflow int64 nanoseconds around attempt 38 and hand
	// rand.N a negative bound — a request-time panic in a stage, which the
	// inner Recover cannot catch.
	ceiling := 30 * time.Second
	if attempt <= 9 {
		ceiling = min(100*time.Millisecond*(1<<(attempt-1)), 30*time.Second)
	}
	return rand.N(ceiling + 1), true
}

// Pipeline assembles the full consumer chain around h for one subscription,
// in the fixed wrapping order:
//
//	Recover → Drain → TraceExtract → Deduplicate → DeadLetter → RequireMessageID → Retry → ConcurrencyLimit → handler
//
// and returns the composed handler plus the drain-wait the lifecycle calls
// at shutdown step 3. Recover guards twice: innermost around the handler, so
// a handler panic becomes INTERNAL and takes the §2.6 path — retried, then
// dead-lettered — and outermost as a safety net, so a bug in a stage itself
// cannot kill the subscription either. topic is the subscription's topic —
// it names the default DLQ ("<topic>.dlq") and the origin-topic header on
// dead-lettered messages. A nil handler, store (unless WithoutDedupe), or
// dead-letter publisher panics here, at boot.
//
// subscription names THIS subscription and must be unique in the process.
// It scopes the deduplication key, which is what lets two features consume
// one topic: keyed on the message id alone, whichever handler ran first
// marked the message seen and the second never saw it.
func Pipeline(subscription, topic string, h MessageHandler, store inbox.Store, dlq Publisher, opts ...SubscribeOption) (MessageHandler, func(context.Context) error) {
	if h == nil {
		panic("broker: Pipeline composed around a nil handler")
	}
	if subscription == "" {
		panic("broker: Pipeline composed with an empty subscription name — it scopes the deduplication key and must be unique in the process")
	}
	if dlq == nil {
		panic("broker: Pipeline composed with a nil dead-letter publisher — terminal messages must be preserved, never dropped")
	}
	cfg := subscribeConfig{
		retry:     ExponentialBackoff(3),
		dlqTopic:  topic + ".dlq",
		dedupe:    true,
		dedupeTTL: 24 * time.Hour,
	}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.dedupe && store == nil {
		panic("broker: Pipeline composed with a nil inbox store — pass one, or opt out with WithoutDedupe()")
	}

	inner := Recover()(h)
	if cfg.concurrency > 0 {
		inner = ConcurrencyLimit(cfg.concurrency)(inner)
	}
	inner = Retry(cfg.retry)(inner)
	if cfg.dedupe {
		// Inside DeadLetter, so a message with no id is preserved rather
		// than returned to the broker for ever.
		inner = RequireMessageID()(inner)
	}
	inner = DeadLetter(dlq, topic, cfg.dlqTopic)(inner)
	if cfg.dedupe {
		inner = Deduplicate(subscription, store, cfg.dedupeTTL)(inner)
	}
	inner = TraceExtract()(inner)
	// Outside TraceExtract, so that everything downstream — the retry's
	// logging, the dead-letter's, the handler's own — writes under the
	// correlation ID of the request that published the message.
	inner = correlate()(inner)
	drainMw, wait := Drain()
	inner = drainMw(inner)
	inner = Recover()(inner)
	return inner, wait
}

// Recover converts a panic into errors.Internal. Pipeline applies it twice:
// innermost — so a handler panic is classified INTERNAL and retried, then
// dead-lettered, per §2.6 — and outermost, so a bug in a stage itself cannot
// kill the subscription.
func Recover() Middleware {
	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) (err error) {
			defer func() {
				if r := recover(); r != nil {
					// Logged HERE, with the stack, because this is the only
					// frame that still has one. Converting the panic to an
					// error and returning it discards the stack, and a field
					// test found that a panicking consumer produced no record
					// anywhere: no error, no trace, no sign the message had
					// even arrived. transport/http has logged panics with a
					// stack from the start; the consumer ring is where nobody
					// is watching, so it needs it more.
					log.FromContext(ctx).ErrorContext(ctx, "consumer panicked",
						"panic", r,
						"message_id", msg.ID,
						"type", msg.Type,
						"stack", string(debug.Stack()))
					err = errors.Internal(fmt.Errorf("handler panic: %v", r))
				}
			}()
			return next(ctx, msg)
		}
	}
}

// Drain returns the admission stage and the wait the lifecycle calls at
// shutdown step 3: wait refuses new deliveries (UNAVAILABLE nack — the
// broker redelivers them elsewhere) and returns when every in-flight
// message, its retries and its DLQ publish included, has finished.
func Drain() (Middleware, func(context.Context) error) {
	d := &drainState{idle: make(chan struct{})}
	mw := func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			if !d.admit() {
				return errors.Unavailable("subscription", stderrors.New("draining for shutdown"))
			}
			defer d.done()
			return next(ctx, msg)
		}
	}
	return mw, d.wait
}

type drainState struct {
	mu       sync.Mutex
	inFlight int
	draining bool
	idle     chan struct{}
	closed   bool
}

func (d *drainState) admit() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return false
	}
	d.inFlight++
	return true
}

func (d *drainState) done() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inFlight--
	if d.draining && d.inFlight == 0 && !d.closed {
		d.closed = true
		close(d.idle)
	}
}

func (d *drainState) wait(ctx context.Context) error {
	d.mu.Lock()
	d.draining = true
	if d.inFlight == 0 {
		if !d.closed {
			d.closed = true
			close(d.idle)
		}
	}
	d.mu.Unlock()
	select {
	case <-d.idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TraceExtract seeds the delivery's headers on the context, so downstream
// instrumentation — and the observability adapter's span extraction — can
// continue the producer's trace without this package knowing a telemetry
// SDK exists.
func TraceExtract() Middleware {
	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			if msg.Headers != nil {
				ctx = WithDeliveryHeaders(ctx, msg.Headers)
			}
			return next(ctx, msg)
		}
	}
}

type deliveryHeadersKey struct{}

// WithDeliveryHeaders returns a copy of ctx carrying the delivery's headers.
// TraceExtract seeds it; adapters and instrumentation read it back with
// DeliveryHeaders.
func WithDeliveryHeaders(ctx context.Context, h map[string]string) context.Context {
	return context.WithValue(ctx, deliveryHeadersKey{}, h)
}

// DeliveryHeaders returns the headers of the message being processed, or nil
// when the context carries none. The returned map is the delivery's own —
// read it, don't mutate it.
func DeliveryHeaders(ctx context.Context) map[string]string {
	h, _ := ctx.Value(deliveryHeadersKey{}).(map[string]string)
	return h
}

// Deduplicate suppresses redeliveries of already-disposed messages, keyed on
// Message.ID: Seen before the handler (seen → ack without invoking it),
// MarkSeen only after the inner chain returns nil — a nacked message must
// not count as its own duplicate. A store error fails CLOSED: UNAVAILABLE
// nack, duplicates over loss, but never silently. A MarkSeen failure after
// success is acked — the work is done; refusing the ack would guarantee the
// duplicate it failed to prevent.
func Deduplicate(subscription string, store inbox.Store, ttl time.Duration) Middleware {
	if store == nil {
		panic("broker: Deduplicate composed with a nil inbox store")
	}
	if subscription == "" {
		panic("broker: Deduplicate composed with an empty subscription name — the dedupe key is scoped by it, and an empty scope makes two subscriptions suppress each other")
	}
	if ttl <= 0 {
		panic("broker: Deduplicate composed with a non-positive TTL — pass a positive one, or opt out with WithoutDedupe()")
	}
	// The scope is fixed at composition, so the request path concatenates
	// rather than formats.
	//
	// The separator is U+001F UNIT SEPARATOR, not NUL, and the difference is
	// load-bearing: the joined string becomes a key in an inbox.Store, and
	// Postgres `text` rejects 0x00 outright — a NUL-separated key is
	// unstorable in the first durable store anyone writes. Neither half
	// carries U+001F (a subscription name is a developer-chosen identifier;
	// RequireMessageID refuses an ID with a NUL and nothing generates a
	// separator byte), so the join stays unambiguous without escaping.
	prefix := subscription + keySeparator

	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			// A message with no ID has no key to dedupe on, so this stage
			// passes it through untouched — RequireMessageID, which sits
			// inside the dead-letter ring, is what refuses it. Touching the
			// store here would file every such message under the empty key
			// and destroy all but the first.
			if msg.ID == "" {
				return next(ctx, msg)
			}
			key := prefix + msg.ID

			seen, err := store.Seen(ctx, key)
			if err != nil {
				return errors.Unavailable("inbox store", err)
			}
			if seen {
				return nil
			}
			if err := next(ctx, msg); err != nil {
				return err
			}
			// Marking failure is logged by the store's own instrumentation;
			// the disposition stands.
			_ = store.MarkSeen(ctx, key, ttl)
			return nil
		}
	}
}

// RequireMessageID refuses a message whose ID cannot serve as a dedupe key,
// as INVALID — which is terminal, so the message is dead-lettered and
// preserved rather than dropped or redelivered for ever.
//
// Two IDs are refused. An EMPTY one, because it is not a key: without this
// every message on the topic shares it, the first is handled, and every one
// after it is silently acked and destroyed. And one carrying a NUL byte,
// because the key must be storable by any inbox.Store — Postgres `text`
// rejects 0x00 outright, so such an ID would be a store error on a durable
// deployment and no error at all on the memory store, which is the worst
// pairing there is. Refusing it here makes the guarantee inboxtest states
// (keys hold no NUL) true by construction rather than by hope.
//
// Pipeline installs it only when deduplication is on, because that is when
// the ID is load-bearing: it IS the key.
//
// It sits INSIDE the dead-letter ring and outside Retry: retrying a message
// whose ID will never change achieves nothing.
func RequireMessageID() Middleware {
	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			if msg.ID == "" {
				return errors.Invalid("message id", errNoMessageID)
			}
			if strings.ContainsRune(msg.ID, 0) {
				return errors.Invalid("message id", errNULInMessageID)
			}
			return next(ctx, msg)
		}
	}
}

// keySeparator joins the subscription scope to the message ID in a dedupe
// key. See Deduplicate for why it is U+001F and not NUL.
const keySeparator = "\x1f"

// errNoMessageID is the cause under the INVALID a message with no id gets.
var errNoMessageID = stderrors.New(
	"a message carries no ID, and the ID is the deduplication key — without one every message on the topic would share a key and all but the first would be discarded")

// errNULInMessageID is the cause under the INVALID a message whose id holds
// a NUL byte gets.
var errNULInMessageID = stderrors.New(
	"a message ID contains a NUL byte, and the ID is the deduplication key — a durable inbox store cannot hold one (Postgres text rejects 0x00), so the message would be deduplicated in development and fail in production")

// DeadLetter is the disposition stage: it maps the inner chain's final error
// by its outermost warren/errors code to the §2.6 consumer column. Terminal
// codes (INVALID, UNAUTHENTICATED, PERMISSION_DENIED, exhausted INTERNAL —
// and everything unknown, INTERNAL being the safe default) publish the
// original envelope to the dead-letter topic with forensic headers and ack.
// NOT_FOUND and CONFLICT ack. UNAVAILABLE nacks so the broker redelivers —
// never dead-lettered. A failed DLQ publish nacks: silent loss is the one
// forbidden outcome. Note what that nack implies: the chain holds no state,
// so the redelivery re-runs the FULL pipeline — the handler and its side
// effects included — before the DLQ publish is re-attempted. Handlers are
// idempotent in an at-least-once world; this is one more reason why.
func DeadLetter(pub Publisher, originTopic, dlqTopic string) Middleware {
	if pub == nil {
		panic("broker: DeadLetter composed with a nil publisher")
	}
	// Whether a nack is lossless is a property of the DRIVER, not of this
	// code — see Redeliverer. Asked once, at composition time, because the
	// answer cannot change while the process runs.
	nackComesBack := redelivers(pub)
	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			err := next(ctx, msg)
			if err == nil {
				return nil
			}
			switch codeOf(err) {
			case errors.CodeNotFound, errors.CodeConflict:
				return nil
			case errors.CodeUnavailable:
				// Nack, so the broker brings it back — §2.6's disposition,
				// and the right one whenever the premise holds.
				//
				// It does not hold on a broker that cannot redeliver, and
				// there a nack is a silent drop. Once the local retry budget
				// is spent there is nothing left to try, so the message is
				// preserved instead: a DLQ entry is inspectable and
				// replayable, which "gone" is not.
				if nackComesBack || attemptsOf(err) <= 1 {
					return err
				}
			}
			// Terminal: INVALID, UNAUTHENTICATED, PERMISSION_DENIED,
			// INTERNAL (retries exhausted), and every unknown code.
			dead := msg
			dead.Headers = make(map[string]string, len(msg.Headers)+4)
			maps.Copy(dead.Headers, msg.Headers)
			dead.Headers["warren-origin-topic"] = originTopic
			dead.Headers["warren-error-code"] = string(codeOf(err))
			dead.Headers["warren-error"] = err.Error()
			dead.Headers["warren-attempts"] = strconv.Itoa(attemptsOf(err))
			// warren.md §2.6: the DLQ "stops the message, keeps it for
			// inspection, and fires the DLQ alert. Which is correct, because
			// this should wake someone up." Nothing woke up — with the
			// scaffold's memory broker and nobody subscribed to <topic>.dlq,
			// a poison message vanished leaving no trace in logs or storage.
			// This line IS the alert: it is the one consumer event that
			// should page a human.
			log.FromContext(ctx).ErrorContext(ctx, "message dead-lettered",
				"message_id", msg.ID,
				"type", msg.Type,
				"origin_topic", originTopic,
				"dlq_topic", dlqTopic,
				"attempts", attemptsOf(err),
				"code", string(codeOf(err)),
				"error", err.Error())
			if perr := pub.Publish(ctx, dlqTopic, dead); perr != nil {
				// Worse than a dead-letter: the message is now neither handled
				// nor preserved, and the nack sends it round again.
				log.FromContext(ctx).ErrorContext(ctx, "dead-letter publish failed",
					"message_id", msg.ID,
					"dlq_topic", dlqTopic,
					"error", perr.Error())
				return errors.Unavailable("dead-letter publish to "+dlqTopic, perr)
			}
			return nil
		}
	}
}

// Retry re-invokes the inner chain on the two §2.6 retry rows — UNAVAILABLE
// and INTERNAL, judged by the OUTERMOST code, a non-Warren error counting as
// INTERNAL — under the policy. Waits observe context cancellation, so
// shutdown never sits out a backoff. The final error is wrapped with the
// attempt count for the DLQ's forensic header; the code stays reachable.
func Retry(policy app.RetryPolicy) Middleware {
	if policy == nil {
		panic("broker: Retry composed with a nil policy")
	}
	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			for attempt := 1; ; attempt++ {
				err := next(ctx, msg)
				if err == nil {
					return nil
				}
				code := codeOf(err)
				if code != errors.CodeUnavailable && code != errors.CodeInternal {
					return &attemptedError{err: err, attempts: attempt}
				}
				delay, retry := policy.Next(attempt)
				if !retry {
					return &attemptedError{err: err, attempts: attempt}
				}
				// WARN, not ERROR: a retry that succeeds is not a failure. But
				// it must be audible — a consumer retrying steadily against a
				// broken dependency used to look exactly like a consumer doing
				// nothing at all.
				log.FromContext(ctx).WarnContext(ctx, "consumer retrying",
					"message_id", msg.ID,
					"type", msg.Type,
					"attempt", attempt,
					"delay", delay.String(),
					"code", string(code),
					"error", err.Error())
				if delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						return &attemptedError{err: err, attempts: attempt}
					}
				} else if ctx.Err() != nil {
					return &attemptedError{err: err, attempts: attempt}
				}
			}
		}
	}
}

// ConcurrencyLimit caps concurrently executing handler invocations. It sits
// innermost — inside Retry — so the semaphore is held per attempt and a
// message sleeping in backoff starves nobody.
func ConcurrencyLimit(n int) Middleware {
	if n <= 0 {
		panic("broker: ConcurrencyLimit composed with a non-positive cap")
	}
	sem := make(chan struct{}, n)
	return func(next MessageHandler) MessageHandler {
		return func(ctx context.Context, msg Message) error {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return errors.Unavailable("subscription", ctx.Err())
			}
			defer func() { <-sem }()
			return next(ctx, msg)
		}
	}
}

// attemptedError carries the attempt count to the DLQ stage without
// disturbing the outermost warren/errors code — errors.As skips it.
type attemptedError struct {
	err      error
	attempts int
}

func (e *attemptedError) Error() string { return e.err.Error() }

func (e *attemptedError) Unwrap() error { return e.err }

// codeOf reads the outermost warren/errors code; anything else — a plain
// error included — is INTERNAL, the safe default for the unknown.
// codeOf is errors.CodeOf, kept as a one-line alias so the call sites below
// stay short. The behaviour is the exported one: outermost code, INTERNAL for
// anything foreign.
func codeOf(err error) errors.Code { return errors.CodeOf(err) }

func attemptsOf(err error) int {
	if a, ok := stderrors.AsType[*attemptedError](err); ok {
		return a.attempts
	}
	return 1
}

// InjectTrace writes the trace context on ctx into each message's Headers,
// allocating a map only for a message that has none. A publisher adapter
// calls it as its first act, and it is what makes a span survive the trip
// through a broker into the consumer — the other half of the chain's
// TraceExtract stage.
//
// It is a no-op when no telemetry is bound, so an uninstrumented service
// pays one nil check per publish.
//
// The OUTBOX calls it when the row is WRITTEN, not when it is relayed: the
// relay runs long after the request's span ended, and a span parented to the
// relay's own context is a trace nobody can follow back to the request that
// caused it.
func InjectTrace(ctx context.Context, msgs []Message) {
	tel := app.TelemetryFromContext(ctx)
	if tel == nil {
		return
	}
	for i := range msgs {
		tel.Inject(ctx, func(k, v string) {
			if msgs[i].Headers == nil {
				msgs[i].Headers = map[string]string{}
			}
			msgs[i].Headers[k] = v
		})
	}
}
