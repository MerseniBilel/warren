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
	"github.com/MerseniBilel/warren/log"
	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
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
		recs := make([]Record, 0, len(events))
		for _, e := range events {
			rec, err := enc.Encode(e)
			if err != nil {
				return err
			}
			recs = append(recs, rec)
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

// Standalone is the default Elector: it always leads. Correct for a single
// instance and for the modular monolith; with several replicas and a durable
// store, every replica would drain, so the relay logs a warning naming the
// risk and the fix (postgres.AdvisoryLock).
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
				waited := make(chan struct{})
				go func() { waiter.Wait(ctx); close(waited) }()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(r.poll)
				select {
				case <-waited:
				case <-timer.C:
				case <-ctx.Done():
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
