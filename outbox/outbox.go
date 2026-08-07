// Package outbox implements the transactional outbox: the pattern that makes
// a state change and the events announcing it atomic without a distributed
// transaction.
//
// A unit of work writes aggregate state and outbox rows in ONE database
// transaction; a separate relay — a lifecycle participant, leader-elected —
// drains those rows to the broker afterwards. If the transaction rolls back
// the rows roll back with it, and if the process dies between commit and
// publish the rows are still there for the next drain. The honest guarantee
// is at-least-once publication, which is why the inbox dedupes on
// Message.ID.
package outbox

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// Record is one outbox row: a broker.Message plus the topic it publishes to.
// broker.Message deliberately carries no topic — a subscription's topic is
// boot-time state — but an outbox row is the one place the topic must travel
// with the message.
type Record struct {
	Topic   string
	Message broker.Message
}

// Store is the outbox's one port. Append is the writer: a role the unit of
// work plays through this method, not a second type. Persistence adapters
// implement it, because the rows live in their database.
type Store interface {
	// Append writes records in the caller's ambient transaction. It opens no
	// connection of its own: if the transaction rolls back, the records roll
	// back with it — that atomicity is the entire pattern. A record whose
	// Message.ID is empty is assigned one by the store, stable across
	// republishes, which is the key the inbox dedupes on.
	Append(ctx context.Context, recs ...Record) error

	// Pending returns up to limit undispatched records in insertion order,
	// parked records excluded.
	Pending(ctx context.Context, limit int) ([]Record, error)

	// MarkPublished marks records dispatched.
	MarkPublished(ctx context.Context, ids ...string) error

	// MarkFailed parks a record: kept for inspection, never returned by
	// Pending again.
	MarkFailed(ctx context.Context, id string, cause error) error
}

// Waiter is the optional low-latency seam a Store may implement: Wait blocks
// until a record is appended or ctx is done. Relay.Run asserts for it once at
// start and falls back to PollInterval, so a missed signal delays publication
// and never loses it. Appending is the signal — Postgres delivers it with
// LISTEN/NOTIFY, the memory store with a channel.
type Waiter interface {
	Wait(ctx context.Context)
}

// Encoder turns a domain event into the record the outbox stores.
type Encoder interface {
	Encode(e domain.Event) (Record, error)
}

// EncodeOption configures JSONEncoder.
type EncodeOption func(*jsonEncoder)

// Topic overrides the topic an event publishes to. The default is the
// event's own name.
func Topic(fn func(domain.Event) string) EncodeOption {
	return func(e *jsonEncoder) { e.topic = fn }
}

// MessageID sets the message's idempotency key. The default leaves it to the
// store, whose row identity is stable across republishes.
func MessageID(fn func(domain.Event) string) EncodeOption {
	return func(e *jsonEncoder) { e.id = fn }
}

// JSONEncoder encodes the concrete event value with encoding/json. The
// message's Key is the event's AggregateID — the reason domain.Event
// flattens identity to a string, and what preserves per-aggregate order
// through a partitioned broker.
func JSONEncoder(opts ...EncodeOption) Encoder {
	e := &jsonEncoder{}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

type jsonEncoder struct {
	topic func(domain.Event) string
	id    func(domain.Event) string
}

func (e *jsonEncoder) Encode(ev domain.Event) (Record, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return Record{}, errors.Internal(fmt.Errorf("outbox: encoding %s: %w", ev.EventName(), err))
	}
	topic := ev.EventName()
	if e.topic != nil {
		topic = e.topic(ev)
	}
	id := ""
	if e.id != nil {
		id = e.id(ev)
	}
	return Record{
		Topic: topic,
		Message: broker.Message{
			ID:         id,
			Type:       ev.EventName(),
			Key:        ev.AggregateID(),
			Payload:    payload,
			OccurredAt: ev.OccurredAt(),
		},
	}, nil
}

// Sink returns the commit hook that turns an aggregate's drained events
// into outbox records: encode each one, append them all in the caller's
// transaction. It is the bridge between persistence and outbox, and every
// application was writing it by hand.
//
//	uow.OnCommit(outbox.Sink(store, outbox.JSONEncoder()))
//
// Appending inside the transaction is the whole pattern: the aggregate's
// new state and the rows announcing it commit together or not at all.
func Sink(store Store, enc Encoder) func(context.Context, []domain.Event) error {
	if store == nil {
		panic("outbox: Sink with a nil store")
	}
	if enc == nil {
		enc = JSONEncoder()
	}
	return func(ctx context.Context, events []domain.Event) error {
		if len(events) == 0 {
			return nil
		}
		// Capture the correlation ID HERE, not at publish time. Sink runs
		// inside the request, at commit; the relay publishes minutes later
		// from a background context that carries no correlation ID, so a
		// publisher decorator alone would stamp nothing on the outbox path —
		// which is the path every Warren service actually emits events on.
		// broker.Correlating then leaves these headers alone.
		id := log.CorrelationID(ctx)
		recs := make([]Record, 0, len(events))
		for _, e := range events {
			rec, err := enc.Encode(e)
			if err != nil {
				return err
			}
			if id != "" && rec.Message.Headers[broker.CorrelationHeader] == "" {
				if rec.Message.Headers == nil {
					rec.Message.Headers = make(map[string]string, 1)
				}
				rec.Message.Headers[broker.CorrelationHeader] = id
			}
			recs = append(recs, rec)
		}
		// The trace goes on HERE, at Append, for the reason the correlation
		// ID does: the relay publishes minutes later from a context whose
		// span — if it has one at all — is the drain's, not the request's.
		// Guarded, so an uninstrumented service pays one nil check per
		// commit rather than a slice per commit.
		if app.TelemetryFromContext(ctx) != nil {
			msgs := make([]broker.Message, len(recs))
			for i := range recs {
				msgs[i] = recs[i].Message
			}
			broker.InjectTrace(ctx, msgs)
			for i := range recs {
				recs[i].Message = msgs[i]
			}
		}
		return store.Append(ctx, recs...)
	}
}

// Elector grants the exclusive right to drain the outbox. Lead acquires
// leadership, runs fn with a context cancelled when leadership is lost, and
// returns when fn returns.
type Elector interface {
	Lead(ctx context.Context, fn func(context.Context) error) error
}

// Durable is implemented by a Store that survives the process — one backed by
// a database rather than by memory.
//
// It exists so the relay can tell apart the two configurations that use the
// Standalone elector: a modular monolith, where always-leading is right, and
// several replicas over one table, where it silently duplicates every event.
// A Store that does not implement this is assumed NOT durable, which is true
// of the in-process store and makes the warning impossible to trigger
// spuriously.
type Durable interface {
	// Durable reports whether records outlive this process.
	Durable() bool
}

// publisherRedelivers reports whether the publisher's driver brings a nacked
// message back. A publisher that does not implement broker.Redeliverer is
// assumed to, which is true of every durable broker — so the warning below
// cannot fire spuriously on Kafka.
func publisherRedelivers(p broker.Publisher) bool {
	r, ok := p.(broker.Redeliverer)
	return !ok || r.Redelivers()
}

// durable reports whether s has declared itself durable.
func durable(s Store) bool {
	d, ok := s.(Durable)
	return ok && d.Durable()
}

// Standalone is the default Elector: it always leads. Correct for a single
// instance and for the modular monolith.
//
// With several replicas and a DURABLE store it is wrong, and quietly: every
// replica drains the same table, each marks records published, and each
// delivers to its own broker. A field test lost 50% of its events to exactly
// this, with no error anywhere — the rows said published, and they were.
//
// So the relay warns at startup when this elector is paired with a store that
// reports itself durable, naming the fix. It cannot know how many replicas
// there are; it can know that the combination is only safe at one.
func Standalone() Elector { return standalone{} }

type standalone struct{}

func (standalone) Lead(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// Relay drains the outbox to the broker. It is constructed by the module and
// driven by the lifecycle; DrainOnce is exported so a test can drive one
// pass deterministically, with no goroutine and no clock.
type Relay struct {
	store   Store
	pub     broker.Publisher
	batch   int
	backoff app.RetryPolicy
	poll    time.Duration
	flush   time.Duration
	elector Elector

	report *slog.Logger

	mu       sync.Mutex
	attempts map[string]int
}

// ReportTo sets where the relay reports a drain it could not complete —
// most importantly a PARKED record, whose diagnostic names the record, the
// topic, the key, the attempt count, and the fact that later records for
// that key are now out of order.
//
// It exists because Run used to discard that error. Its inner loop was
// `if err != nil || n == 0 { break }`, so the best diagnostic in the
// package was written and then dropped, and the scaffolded platform module
// swallows Run's own return as well. The net effect in every new Warren app
// was an event parked for ever with nothing printed anywhere.
//
// The default is slog.Default() at emit time, so an application that
// installs its handler in main is covered without wiring anything.
func ReportTo(l *slog.Logger) RelayOption {
	return func(r *Relay) { r.report = l }
}

// RelayOption configures a Relay.
type RelayOption func(*Relay)

// PollInterval is how long Run waits between drains when the store offers no
// Waiter, and the safety net when it does — a missed signal delays
// publication, never loses it. Default 1s.
func PollInterval(d time.Duration) RelayOption {
	if d <= 0 {
		panic(fmt.Sprintf("outbox: PollInterval(%v) — a relay that never waits is a busy loop", d))
	}
	return func(r *Relay) { r.poll = d }
}

// LeaderElection sets who may drain. The default is Standalone(), which
// always leads — correct for one instance and for the modular monolith, and
// wrong for several replicas over a durable store, where every replica would
// drain.
func LeaderElection(e Elector) RelayOption {
	if e == nil {
		panic("outbox: LeaderElection(nil) — pass an Elector, or omit the option for Standalone()")
	}
	return func(r *Relay) { r.elector = e }
}

// FlushTimeout bounds the final drain at shutdown, inside the lifecycle's
// force-exit budget. Default 10s.
func FlushTimeout(d time.Duration) RelayOption {
	return func(r *Relay) { r.flush = d }
}

// BatchSize caps how many records one drain publishes. Default 100.
func BatchSize(n int) RelayOption {
	if n <= 0 {
		panic(fmt.Sprintf("outbox: BatchSize(%d) — a drain that publishes no records makes no progress", n))
	}
	return func(r *Relay) { r.batch = n }
}

// Backoff sets the retry policy for records whose publish failed
// transiently; when it stops, the record is parked. Default
// ExponentialBackoff(10).
func Backoff(p app.RetryPolicy) RelayOption {
	return func(r *Relay) { r.backoff = p }
}

// reporter returns the logger to report through.
func (r *Relay) reporter(ctx context.Context) *slog.Logger {
	if r.report != nil {
		return r.report
	}
	return log.FromContext(ctx)
}

// NewRelay returns a relay over store and pub.
func NewRelay(store Store, pub broker.Publisher, opts ...RelayOption) *Relay {
	if store == nil {
		panic("outbox: NewRelay with a nil store")
	}
	if pub == nil {
		panic("outbox: NewRelay with a nil publisher")
	}
	r := &Relay{
		store:    store,
		pub:      pub,
		batch:    100,
		backoff:  broker.ExponentialBackoff(10),
		poll:     time.Second,
		flush:    10 * time.Second,
		elector:  Standalone(),
		attempts: map[string]int{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run drains the outbox until ctx is cancelled: it acquires leadership,
// then loops — drain everything pending, then wait for the store's signal
// (or the poll interval) and drain again. It returns when leadership ends or
// ctx is cancelled, which is what makes it a lifecycle participant rather
// than a goroutine someone forgot about.
//
// Register it from a constructor that injects lifecycle.Lifecycle:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	lc.Append(lifecycle.Hook{
//	    Name:    "outbox relay",
//	    OnStart: func(context.Context) error { go relay.Run(ctx); return nil },
//	    OnStop:  func(c context.Context) error { cancel(); return relay.Flush(c) },
//	})
//
// The loop's context must NOT be OnStart's: that one is the boot context,
// which outlives boot only under Run and is cancelled immediately under
// Start/Stop — so a relay started on it would leak the goroutine in every
// test.
//
// A drain error does not end the loop — a broker outage is transient by
// definition, and the disposition rules already decide what happens to the
// records.
func (r *Relay) Run(ctx context.Context) error {
	// The warning Standalone's doc has always promised and never emitted.
	// Once, at startup, not per drain.
	if _, standalone := r.elector.(standalone); standalone && durable(r.store) {
		log.FromContext(ctx).WarnContext(ctx,
			"outbox relay is leading unconditionally over a durable store",
			"risk", "with more than one replica every replica drains the same table, so each event is published once PER REPLICA and each marks the row published — silent duplication, no error anywhere",
			// BOTH halves, spelled out. The option alone only PROVIDES an
			// Elector; the relay ignores it unless the elector it is given is
			// that one. A field test applied the one-line version of this
			// advice and still duplicated every event.
			//
			// Step 2 has two shapes and naming only one sends half the
			// readers looking for work already done: a scaffolded app's
			// newRelay ALREADY injects outbox.Elector and passes
			// LeaderElection, so what is left there is deleting the local
			// provider that returns Standalone.
			"fix", "TWO steps, and the first alone does nothing: (1) add postgres.WithAdvisoryLock(), so the adapter provides an outbox.Elector; (2) make that the elector this relay receives — if a local provider returns outbox.Standalone(), delete it and its Providers entry (leaving both is an ambiguous binding at boot, which names them); if nothing injects an Elector yet, add outbox.Elector to the relay's constructor and pass outbox.LeaderElection(e)",
			"safe_if", "this service runs as exactly one instance")
	}
	// A durable store draining into a broker that cannot redeliver loses
	// everything the broker still holds when the process stops.
	//
	// The relay marks a row published when Publish RETURNS, and an in-process
	// broker's Publish only enqueues in memory. So on shutdown the queue is
	// discarded, the row still says published, and there is no retry path — a
	// restart recovers nothing. Measured in a field test: 350 events
	// committed, 350 rows marked published, ZERO delivered.
	//
	// This is the configuration `warren new --db postgres` produces by
	// default, and it is strictly worse than the multi-replica case the relay
	// already warned about, so staying silent about it was the wrong way
	// round. Both capabilities needed to see it already ship.
	if durable(r.store) && !publisherRedelivers(r.pub) {
		log.FromContext(ctx).WarnContext(ctx,
			"durable outbox is publishing to a broker that cannot redeliver",
			"risk", "a record is marked published when Publish returns, and this broker holds its queue in memory — anything undelivered when the process stops is lost, the row still says published, and a restart recovers nothing",
			"fix", "use a durable broker for anything that must survive a restart; broker/memory is for one process and for tests",
			"safe_if", "this service is a modular monolith where the consumer is in the same process and a lost event on shutdown is acceptable")
	}
	return r.elector.Lead(ctx, func(ctx context.Context) error {
		waiter, _ := r.store.(Waiter)
		timer := time.NewTimer(r.poll)
		defer timer.Stop()

		for {
			if ctx.Err() != nil {
				return nil
			}
			// Drain everything currently pending, not just one batch.
			for {
				n, err := r.DrainOnce(ctx)
				if err != nil {
					// Reported, not returned: one unpublishable record must
					// not stop the relay, and silence is what let a parked
					// record go unnoticed for ever. ctx cancellation during
					// shutdown is the loop ending, not a failure.
					if ctx.Err() == nil {
						r.reporter(ctx).ErrorContext(ctx, "outbox drain failed", "error", err.Error())
					}
					break
				}
				if n == 0 {
					break
				}
			}
			if ctx.Err() != nil {
				return nil
			}

			if waiter != nil {
				// Appending is the signal; the timer is the safety net for a
				// signal that never arrives.
				//
				// The Wait gets its OWN cancellable context, and is cancelled
				// and JOINED before the next iteration. Both halves matter.
				// This loop used to hand Wait the relay-lifetime context and
				// `continue` the moment the timer won, leaving that Wait
				// blocked for ever and starting another on the next tick.
				//
				// The memory store's Wait blocks on a channel, so the leak
				// there was only goroutines. The Postgres store's Wait holds a
				// POOLED CONNECTION for its whole duration: an idle service
				// consumed one connection per poll interval, exhausted the
				// pool in MaxConns ticks, and then every request blocked for
				// ever — with one INFO line in the log to explain it. It could
				// not recover, either, because releasing a waiter needs a
				// NOTIFY, which is only issued inside Append, which needs a
				// connection.
				waitCtx, cancelWait := context.WithCancel(ctx)
				waited := make(chan struct{})
				go func() { waiter.Wait(waitCtx); close(waited) }()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(r.poll)
				stopping := false
				select {
				case <-waited:
				case <-timer.C:
				case <-ctx.Done():
					stopping = true
				}
				// Joining is what makes "exactly one waiter" true rather than
				// merely intended. A Waiter must return when its context is
				// done — that is the contract Waiter states — so this cannot
				// outlast the cancellation.
				cancelWait()
				<-waited
				if stopping {
					return nil
				}
				continue
			}

			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(r.poll)
			select {
			case <-timer.C:
			case <-ctx.Done():
				return nil
			}
		}
	})
}

// Flush drains what is pending one last time, bounded by FlushTimeout. It is
// the relay's OnStop: shutdown step 4, after consumers stop and before
// connections close. Rows written after it are simply published by the next
// process — which is precisely what an outbox is for.
func (r *Relay) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.flush)
	defer cancel()
	for {
		n, err := r.DrainOnce(ctx)
		if err != nil {
			return err
		}
		if n == 0 || ctx.Err() != nil {
			return nil
		}
	}
}

// DrainOnce publishes one batch and returns how many records were
// dispatched. It publishes in insertion order, batching consecutive records
// that share a topic into one call, and stops at the first failure without
// publishing anything behind it: global order is a stronger guarantee than
// per-aggregate order and it is what makes the promise one sentence long.
//
// Head-of-line blocking is the accepted cost — a publish failure is nearly
// always broker-wide, so in the common case nothing is behind it — and the
// disposition rules bound the pathological case: a transient failure leaves
// the record for the next poll, a rejection parks it immediately, and an
// unknown failure retries until the policy stops and then parks.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	recs, err := r.store.Pending(ctx, r.batch)
	if err != nil {
		return 0, fmt.Errorf("outbox: reading pending records: %w", err)
	}
	if len(recs) == 0 {
		return 0, nil
	}

	published := 0
	for i := 0; i < len(recs); {
		topic := recs[i].Topic
		j := i
		for j < len(recs) && recs[j].Topic == topic {
			j++
		}
		group := recs[i:j]

		msgs := make([]broker.Message, len(group))
		ids := make([]string, len(group))
		for k, rec := range group {
			msgs[k] = rec.Message
			ids[k] = rec.Message.ID
		}

		if err := r.pub.Publish(ctx, topic, msgs...); err != nil {
			// A REJECTION IS ABOUT ONE RECORD, and a batch is how records
			// were sent, not what they are. The broker accepts the rest of
			// the batch and refuses that one: measured against real Kafka, a
			// five-record batch with one oversized message returned
			// MESSAGE_TOO_LARGE and left FOUR of the five in the topic.
			//
			// Parking the head on that error parked records the broker had
			// already delivered — the outbox then reports a delivered event
			// as failed for ever, and replaying parked rows re-sends
			// something that already went out. So the batch is retried one
			// record at a time, and each gets its own verdict.
			//
			// Only for INVALID. UNAVAILABLE is the broker being down, which
			// is not about any record, and isolating there would be N
			// pointless publishes instead of one.
			if len(group) > 1 && codeOf(err) == errors.CodeInvalid {
				n, isoErr := r.publishEach(ctx, topic, group)
				published += n
				if isoErr != nil {
					return published, isoErr
				}
				i = j
				continue
			}
			return published, r.disposition(ctx, group, err)
		}
		if err := r.store.MarkPublished(ctx, ids...); err != nil {
			// The messages are out; failing to mark them means the next
			// drain republishes, which at-least-once already allows for.
			return published, fmt.Errorf("outbox: marking %d records published: %w", len(ids), err)
		}
		r.forget(ids)
		published += len(group)
		i = j
	}
	return published, nil
}

// publishEach republishes a rejected batch one record at a time, so the
// broker's verdict on each is its own.
//
// THE RECORDS THE BROKER ALREADY ACCEPTED ARE SENT AGAIN. That is the price
// of a port whose Publish returns one error for many messages, and it is the
// cheaper mistake: at-least-once already permits a duplicate and the inbox
// dedupes it, whereas parking a delivered record loses the truth about it
// permanently. It happens only on the failure path.
//
// It stops at the first record that does not publish. A parked record ends
// this drain; the ones behind it are pending and the next drain — a poll
// interval away — takes them.
func (r *Relay) publishEach(ctx context.Context, topic string, group []Record) (int, error) {
	published := 0
	for _, rec := range group {
		if err := r.pub.Publish(ctx, topic, rec.Message); err != nil {
			return published, r.disposition(ctx, []Record{rec}, err)
		}
		if err := r.store.MarkPublished(ctx, rec.Message.ID); err != nil {
			return published, fmt.Errorf("outbox: marking record %s published: %w", rec.Message.ID, err)
		}
		r.forget([]string{rec.Message.ID})
		published++
	}
	return published, nil
}

// disposition decides what happens to the batch that failed, by the
// outermost §2.6 code of the publish error.
//
// A rejection parks the batch's HEAD only, not the whole batch: the broker
// rejected one message and the batch is how they were sent, not what they
// are. Parking head-first converges — each drain retries the batch minus the
// parked record, so a rejection caused by the third message parks the first
// two only if they are rejected in turn, and every record gets its own
// verdict.
func (r *Relay) disposition(ctx context.Context, group []Record, cause error) error {
	head := group[0]
	switch codeOf(cause) {
	case errors.CodeUnavailable:
		// The broker is down. Not the record's fault: leave everything for
		// the next poll and never park — a misconfigured broker should stall
		// and alert, not discard.
		return fmt.Errorf("outbox: publishing to %q: %w", head.Topic, cause)

	case errors.CodeInvalid:
		// The broker rejected the message itself. Retrying a deterministic
		// rejection would stall the queue forever.
		if err := r.store.MarkFailed(ctx, head.Message.ID, cause); err != nil {
			return fmt.Errorf("outbox: parking record %s: %w", head.Message.ID, err)
		}
		r.forget([]string{head.Message.ID})
		return errParked(head, cause, 1)

	default:
		// Unknown: retry under the policy, then park — §2.6's
		// "nack + retry, then DLQ", where the outbox's DLQ is a parked row.
		attempts := r.bump(head.Message.ID)
		if _, retry := r.backoff.Next(attempts); retry {
			return fmt.Errorf("outbox: publishing to %q (attempt %d): %w", head.Topic, attempts, cause)
		}
		if err := r.store.MarkFailed(ctx, head.Message.ID, cause); err != nil {
			return fmt.Errorf("outbox: parking record %s: %w", head.Message.ID, err)
		}
		r.forget([]string{head.Message.ID})
		return errParked(head, cause, attempts)
	}
}

// bump counts attempts in memory: the relay is the only drainer, so a
// restart resetting the count is harmless and it saves a write per failure.
func (r *Relay) bump(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts[id]++
	return r.attempts[id]
}

func (r *Relay) forget(ids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		delete(r.attempts, id)
	}
}

func codeOf(err error) errors.Code {
	if e, ok := stderrors.AsType[*errors.Error](err); ok {
		return e.Code()
	}
	return errors.CodeInternal
}

// errParked is loud on purpose: parking is the one operation that breaks
// ordering for a key permanently.
func errParked(rec Record, cause error, attempts int) error {
	return diagnostic(fmt.Sprintf(
		"✗ outbox record parked\n\n"+
			"    record %s (topic %q, key %q) failed to publish after %s and was\n"+
			"    parked: %v\n\n"+
			"  It will not be retried, and records for key %q published after it are\n"+
			"  now out of order. Inspect the row, fix the cause, and republish it by\n"+
			"  hand — or delete it if the fact no longer matters.",
		rec.Message.ID, rec.Topic, rec.Message.Key, attemptWord(attempts), cause, rec.Message.Key))
}

func attemptWord(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return strconv.Itoa(n) + " attempts"
}

type diagnostic string

func (d diagnostic) Error() string { return string(d) }

// --- the in-process store --------------------------------------------------

// MemoryOption configures NewMemoryStore.
type MemoryOption func(*memoryStore)

// WithClock injects the time source — how tests drive the store without
// sleeping.
func WithClock(now func() time.Time) MemoryOption {
	return func(s *memoryStore) { s.now = now }
}

// NewMemoryStore returns an in-process Store implementing Waiter: a slice, a
// mutex, and a clock.
//
// Test and modular-monolith use only. Undispatched records do not survive a
// restart, which is the one guarantee an outbox exists to give — a durable
// store is the persistence adapters' business.
func NewMemoryStore(opts ...MemoryOption) Store {
	s := &memoryStore{now: time.Now, signal: make(chan struct{}, 1)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type row struct {
	rec       Record
	published bool
	parked    bool
	cause     error
	at        time.Time
}

type memoryStore struct {
	mu     sync.Mutex
	rows   []*row
	seq    int
	now    func() time.Time
	signal chan struct{}
}

func (s *memoryStore) Append(_ context.Context, recs ...Record) error {
	s.mu.Lock()
	for _, rec := range recs {
		s.seq++
		if rec.Message.ID == "" {
			rec.Message.ID = "outbox-" + strconv.Itoa(s.seq)
		}
		s.rows = append(s.rows, &row{rec: rec, at: s.now()})
	}
	s.mu.Unlock()

	// Appending is the signal; a full buffer means one is already pending.
	select {
	case s.signal <- struct{}{}:
	default:
	}
	return nil
}

func (s *memoryStore) Pending(_ context.Context, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.rows {
		if r.published || r.parked {
			continue
		}
		out = append(out, r.rec)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *memoryStore) MarkPublished(_ context.Context, ids ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, r := range s.rows {
		if want[r.rec.Message.ID] {
			r.published = true
		}
	}
	return nil
}

func (s *memoryStore) MarkFailed(_ context.Context, id string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.rec.Message.ID == id {
			r.parked = true
			r.cause = cause
			return nil
		}
	}
	return errors.NotFound("outbox record", id)
}

func (s *memoryStore) Wait(ctx context.Context) {
	select {
	case <-s.signal:
	case <-ctx.Done():
	}
}
