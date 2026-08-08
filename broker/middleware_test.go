package broker_test

import (
	"bytes"
	"context"
	stderrors "errors"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/log"
)

// capturePublisher records DLQ publishes; fail makes every publish fail.
type capturePublisher struct {
	mu     sync.Mutex
	byTop  map[string][]broker.Message
	failed error
}

func (p *capturePublisher) Publish(_ context.Context, topic string, msgs ...broker.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed != nil {
		return p.failed
	}
	if p.byTop == nil {
		p.byTop = map[string][]broker.Message{}
	}
	p.byTop[topic] = append(p.byTop[topic], msgs...)
	return nil
}

func (p *capturePublisher) dlq(topic string) []broker.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byTop[topic]
}

// zeroDelay retries up to max attempts with no wait — sleep-free tests.
type zeroDelay struct{ max int }

func (z zeroDelay) Next(attempt int) (time.Duration, bool) { return 0, attempt < z.max }

func pipelineFor(t *testing.T, h broker.MessageHandler, opts ...broker.SubscribeOption) (broker.MessageHandler, *capturePublisher) {
	t.Helper()
	dlq := &capturePublisher{}
	composed, _ := broker.Pipeline("sub-user-events", "user.events", h, inbox.NewMemoryStore(), dlq, opts...)
	return composed, dlq
}

func msg(id string) broker.Message {
	return broker.Message{ID: id, Type: "user.registered", Payload: []byte("{}"), Headers: map[string]string{"traceparent": "00-abc"}}
}

// TestDispositionTable is the §2.6 consumer column, one case per code plus
// nil and a non-Warren error — the single most valuable test in the suite.
func TestDispositionTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		err       error
		wantErr   bool // pipeline returns error = nack; nil = ack
		wantDLQ   bool
		wantCalls int // handler invocations (retries included)
	}{
		{"nil acks", nil, false, false, 1},
		{"INVALID goes to the DLQ, never retried", werrors.Invalid("payload", stderrors.New("bad")), false, true, 1},
		{"NOT_FOUND acks", werrors.NotFound("user", 42), false, false, 1},
		{"CONFLICT acks (idempotent replay)", werrors.Conflict("already applied"), false, false, 1},
		{"UNAUTHENTICATED goes to the DLQ, never retried", werrors.Unauthenticated("no identity"), false, true, 1},
		{"PERMISSION_DENIED goes to the DLQ, never retried", werrors.PermissionDenied("consume"), false, true, 1},
		{"UNAVAILABLE nacks after retries, never DLQ", werrors.Unavailable("db", stderrors.New("down")), true, false, 3},
		{"INTERNAL retries then goes to the DLQ", werrors.Internal(stderrors.New("boom")), false, true, 3},
		{"a non-Warren error is INTERNAL: retries then DLQ", stderrors.New("plain"), false, true, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			h, dlq := pipelineFor(t, func(context.Context, broker.Message) error {
				calls++
				return tc.err
			}, broker.WithRetry(zeroDelay{max: 3}))

			err := h(context.Background(), msg("evt-"+tc.name))
			if (err != nil) != tc.wantErr {
				t.Errorf("pipeline error = %v, want nack=%v", err, tc.wantErr)
			}
			if got := len(dlq.dlq("user.events.dlq")) > 0; got != tc.wantDLQ {
				t.Errorf("DLQ delivery = %v, want %v", got, tc.wantDLQ)
			}
			if calls != tc.wantCalls {
				t.Errorf("handler ran %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestDLQEnvelopeAndHeaders(t *testing.T) {
	t.Parallel()

	h, dlq := pipelineFor(t, func(context.Context, broker.Message) error {
		return werrors.Invalid("payload", stderrors.New("not json"))
	})
	if err := h(context.Background(), msg("evt-9")); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	dead := dlq.dlq("user.events.dlq")
	if len(dead) != 1 {
		t.Fatalf("DLQ has %d messages, want 1", len(dead))
	}
	d := dead[0]
	if d.ID != "evt-9" || string(d.Payload) != "{}" || d.Headers["traceparent"] != "00-abc" {
		t.Errorf("the original envelope did not survive intact: %+v", d)
	}
	if d.Headers["warren-origin-topic"] != "user.events" {
		t.Errorf("warren-origin-topic = %q", d.Headers["warren-origin-topic"])
	}
	if d.Headers["warren-error-code"] != "INVALID" {
		t.Errorf("warren-error-code = %q", d.Headers["warren-error-code"])
	}
	if d.Headers["warren-error"] == "" {
		t.Error("warren-error header is empty")
	}
	if d.Headers["warren-attempts"] != "1" {
		t.Errorf("warren-attempts = %q, want 1", d.Headers["warren-attempts"])
	}
}

func TestDLQAttemptsHeaderCountsRetries(t *testing.T) {
	t.Parallel()

	h, dlq := pipelineFor(t, func(context.Context, broker.Message) error {
		return werrors.Internal(stderrors.New("boom"))
	}, broker.WithRetry(zeroDelay{max: 4}))
	_ = h(context.Background(), msg("evt-r"))
	dead := dlq.dlq("user.events.dlq")
	if len(dead) != 1 || dead[0].Headers["warren-attempts"] != "4" {
		t.Fatalf("warren-attempts = %v, want 4", dead)
	}
}

func TestDLQPublishFailureNacksNeverAcks(t *testing.T) {
	t.Parallel()

	dlq := &capturePublisher{failed: stderrors.New("broker gone")}
	h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
		return werrors.Invalid("payload", stderrors.New("bad"))
	}, inbox.NewMemoryStore(), dlq)

	err := h(context.Background(), msg("evt-1"))
	if err == nil {
		t.Fatal("a failed DLQ publish was acked — silent loss, the one forbidden outcome")
	}
	if !werrors.Is(err, werrors.CodeUnavailable) {
		t.Errorf("nack code = %v, want UNAVAILABLE so the broker redelivers", err)
	}
}

func TestDedupe(t *testing.T) {
	t.Parallel()

	t.Run("the same ID delivered twice runs the handler once, acks both", func(t *testing.T) {
		t.Parallel()
		calls := 0
		store := inbox.NewMemoryStore()
		h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
			calls++
			return nil
		}, store, &capturePublisher{})
		if err := h(context.Background(), msg("evt-1")); err != nil {
			t.Fatalf("first delivery: %v", err)
		}
		if err := h(context.Background(), msg("evt-1")); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if calls != 1 {
			t.Errorf("handler ran %d times, want 1", calls)
		}
	})

	t.Run("a failed handler is not marked — redelivery runs it again", func(t *testing.T) {
		t.Parallel()
		calls := 0
		store := inbox.NewMemoryStore()
		h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
			calls++
			return werrors.Unavailable("db", stderrors.New("down"))
		}, store, &capturePublisher{}, broker.WithRetry(zeroDelay{max: 1}))
		_ = h(context.Background(), msg("evt-1"))
		_ = h(context.Background(), msg("evt-1"))
		if calls != 2 {
			t.Errorf("handler ran %d times, want 2 — a nacked message must not count as its own duplicate", calls)
		}
	})

	t.Run("a disposed message is marked: DLQ'd then redelivered is suppressed", func(t *testing.T) {
		t.Parallel()
		calls := 0
		store := inbox.NewMemoryStore()
		h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
			calls++
			return werrors.Invalid("payload", stderrors.New("bad"))
		}, store, &capturePublisher{})
		_ = h(context.Background(), msg("evt-1"))
		_ = h(context.Background(), msg("evt-1"))
		if calls != 1 {
			t.Errorf("handler ran %d times, want 1 — nil means disposed, never redo", calls)
		}
	})

	t.Run("a store error fails closed: nack, handler never runs", func(t *testing.T) {
		t.Parallel()
		calls := 0
		h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
			calls++
			return nil
		}, failingStore{}, &capturePublisher{})
		err := h(context.Background(), msg("evt-1"))
		if !werrors.Is(err, werrors.CodeUnavailable) {
			t.Errorf("store failure = %v, want UNAVAILABLE nack — duplicates over loss, but never silent", err)
		}
		if calls != 0 {
			t.Error("the handler ran while the dedupe store was down")
		}
	})

	t.Run("WithoutDedupe skips the store entirely", func(t *testing.T) {
		t.Parallel()
		calls := 0
		h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
			calls++
			return nil
		}, failingStore{}, &capturePublisher{}, broker.WithoutDedupe())
		if err := h(context.Background(), msg("evt-1")); err != nil {
			t.Fatalf("pipeline: %v", err)
		}
		if calls != 1 {
			t.Error("the handler did not run")
		}
	})
}

type failingStore struct{}

func (failingStore) Seen(context.Context, string) (bool, error) {
	return false, stderrors.New("store down")
}

func (failingStore) MarkSeen(context.Context, string, time.Duration) error {
	return stderrors.New("store down")
}

// markOnlyFailingStore answers Seen fine and fails only the MARK, which is the
// asymmetry that made the failure invisible: the handler runs, the disposition
// is correct, and idempotency is silently off.
type markOnlyFailingStore struct{}

func (markOnlyFailingStore) Seen(context.Context, string) (bool, error) { return false, nil }

func (markOnlyFailingStore) MarkSeen(context.Context, string, time.Duration) error {
	return stderrors.New("relation \"warren_inbox\" does not exist")
}

// TestAMarkSeenFailureIsReported — field test #11, defect 4. Deduplicate
// discards a MarkSeen error under a comment claiming "the store's own
// instrumentation" logs it. Neither shipped store logs anything, so a broken
// inbox degraded to no deduplication at all with no signal anywhere: the
// handler ran on every redelivery, and only an idempotent aggregate hid it.
func TestAMarkSeenFailureIsReported(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := log.WithLogger(context.Background(),
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	calls := 0
	h := broker.Deduplicate("fulfillment/on-order-placed", markOnlyFailingStore{}, time.Hour)(
		func(context.Context, broker.Message) error {
			calls++
			return nil
		})

	// The disposition still stands: the work is done, so the message acks.
	if err := h(ctx, broker.Message{ID: "msg-A", Type: "order.placed"}); err != nil {
		t.Fatalf("a MarkSeen failure must not nack work that succeeded: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}

	out := buf.String()
	if !strings.Contains(out, "inbox mark failed") {
		t.Errorf("a MarkSeen failure was swallowed — nothing was logged:\n%s", out)
	}
	// Naming both is the whole point: without them an operator cannot tell
	// WHICH subscription lost idempotency, and every subscription shares a
	// store.
	for _, want := range []string{"fulfillment/on-order-placed", "msg-A"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
}

// TestASuppressedDuplicateSaysSo — the other half of defect 4's silence. A
// duplicate is acked without invoking the handler and, until now, without a
// word, so "my consumer never ran" and "my consumer ran and was suppressed"
// looked identical from the outside. DEBUG, not INFO: on a redelivering
// broker this is routine.
func TestASuppressedDuplicateSaysSo(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := log.WithLogger(context.Background(),
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	store := inbox.NewMemoryStore()
	calls := 0
	h := broker.Deduplicate("fulfillment/on-order-placed", store, time.Hour)(
		func(context.Context, broker.Message) error {
			calls++
			return nil
		})

	msg := broker.Message{ID: "msg-B", Type: "order.placed"}
	if err := h(ctx, msg); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := h(ctx, msg); err != nil {
		t.Fatalf("redelivery must ack: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1 — dedupe did not suppress", calls)
	}

	out := buf.String()
	if !strings.Contains(out, "duplicate suppressed") {
		t.Errorf("the suppression was silent:\n%s", out)
	}
	for _, want := range []string{"fulfillment/on-order-placed", "msg-B"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
}

func TestRetryObservesCancellationDuringWaits(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	h, _ := pipelineFor(t, func(context.Context, broker.Message) error {
		calls++
		return werrors.Unavailable("db", stderrors.New("down"))
	}, broker.WithRetry(hourDelay{}))
	err := h(ctx, msg("evt-1"))
	if calls != 1 {
		t.Errorf("handler ran %d times — a cancelled context must abort the backoff wait", calls)
	}
	if !werrors.Is(err, werrors.CodeUnavailable) {
		t.Errorf("the last error did not surface: %v", err)
	}
}

type hourDelay struct{}

func (hourDelay) Next(int) (time.Duration, bool) { return time.Hour, true }

// TestConcurrencySlotFreeDuringBackoff pins the D6 semantics: a message
// waiting out a backoff holds no slot, so WithConcurrency(1) cannot be
// starved by a retrying message.
func TestConcurrencySlotFreeDuringBackoff(t *testing.T) {
	t.Parallel()

	aWaiting := make(chan struct{})
	release := make(chan struct{})
	var calls sync.Map

	h, _ := pipelineFor(t, func(_ context.Context, m broker.Message) error {
		if m.ID == "a" {
			if _, loaded := calls.LoadOrStore("a", true); !loaded {
				close(aWaiting) // first attempt: fail, then the policy makes it wait
			}
			return werrors.Unavailable("db", stderrors.New("down"))
		}
		calls.Store("b", true)
		return nil
	}, broker.WithConcurrency(1), broker.WithRetry(gatedDelay{release: release}))

	done := make(chan error, 1)
	go func() { done <- h(context.Background(), msg("a")) }()
	<-aWaiting // message a has failed once and is now inside its backoff wait

	// With the slot released during the wait, message b proceeds under
	// WithConcurrency(1).
	if err := h(context.Background(), msg("b")); err != nil {
		t.Fatalf("message b: %v", err)
	}
	if _, ok := calls.Load("b"); !ok {
		t.Fatal("message b never ran — the backoff wait is holding the concurrency slot")
	}
	close(release)
	<-done
}

// gatedDelay waits on release inside Next — deterministically parking the
// retry loop without sleeps.
type gatedDelay struct{ release chan struct{} }

func (g gatedDelay) Next(attempt int) (time.Duration, bool) {
	if attempt >= 2 {
		return 0, false
	}
	<-g.release // park the retrying message here, outside any slot
	return 0, true
}

func TestConcurrencyLimitCapsInFlight(t *testing.T) {
	t.Parallel()

	const n = 3
	var mu sync.Mutex
	inFlight, peak := 0, 0
	gate := make(chan struct{})

	h, _ := pipelineFor(t, func(context.Context, broker.Message) error {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		<-gate
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}, broker.WithConcurrency(n), broker.WithoutDedupe())

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() { _ = h(context.Background(), msg("evt-"+strconv.Itoa(i))) })
	}
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return inFlight == n })
	close(gate)
	wg.Wait()

	if peak != n {
		t.Errorf("peak in-flight = %d, want exactly %d", peak, n)
	}
}

// waitUntil spins on cond, yielding the scheduler each miss — an
// observable-state wait, no sleeps.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for range 1_000_000 {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("condition never held")
}

func TestDrain(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	h, wait := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
		once.Do(func() { close(started) })
		<-gate
		return nil
	}, inbox.NewMemoryStore(), &capturePublisher{}, broker.WithoutDedupe())

	inFlight := make(chan error, 1)
	go func() { inFlight <- h(context.Background(), msg("evt-1")) }()
	<-started // evt-1 is admitted and executing

	waitReturned := make(chan error, 1)
	go func() { waitReturned <- wait(context.Background()) }()

	// The drain must not return while evt-1 is in flight.
	for range 1_000 {
		select {
		case <-waitReturned:
			t.Fatal("wait returned while a message was in flight")
		default:
		}
	}

	close(gate)
	if err := <-inFlight; err != nil {
		t.Fatalf("in-flight message: %v", err)
	}
	if err := <-waitReturned; err != nil {
		t.Fatalf("wait: %v", err)
	}

	// After the drain, new deliveries are refused with a nack — the broker
	// redelivers them to another consumer instance.
	err := h(context.Background(), msg("evt-2"))
	if !werrors.Is(err, werrors.CodeUnavailable) {
		t.Errorf("delivery during drain = %v, want UNAVAILABLE nack", err)
	}
}

func TestRecover(t *testing.T) {
	t.Parallel()

	h, dlq := pipelineFor(t, func(context.Context, broker.Message) error {
		panic("handler bug")
	}, broker.WithRetry(zeroDelay{max: 2}))

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the panic escaped the pipeline: %v", r)
			}
		}()
		err = h(context.Background(), msg("evt-1"))
	}()
	if err != nil {
		t.Fatalf("pipeline = %v — a panic is INTERNAL: retried, then DLQ'd (ack)", err)
	}
	if len(dlq.dlq("user.events.dlq")) != 1 {
		t.Error("the panicking message did not reach the DLQ")
	}
}

// TestExponentialBackoffNeverOverflows pins the review's HIGH finding: high
// attempt numbers must clamp to the cap, never overflow into a negative
// bound that panics rand.N at request time.
func TestExponentialBackoffNeverOverflows(t *testing.T) {
	t.Parallel()

	p := broker.ExponentialBackoff(1000)
	for _, attempt := range []int{10, 37, 38, 63, 64, 200, 999} {
		delay, retry := p.Next(attempt)
		if !retry {
			t.Fatalf("attempt %d: retry = false before the bound", attempt)
		}
		if delay < 0 || delay > 30*time.Second {
			t.Errorf("attempt %d: delay %v outside [0, 30s]", attempt, delay)
		}
	}
}

func TestWithConcurrencyRejectsNonPositive(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "WithConcurrency(0)") {
			t.Errorf("panic = %v, want the named boot guard", r)
		}
	}()
	broker.WithConcurrency(0)
}

// TestDrainWaitsForDLQPublish pins that wait covers a DLQ publish in flight,
// not just the handler — shutdown step 5 must not close the connection
// under the one message the DLQ exists to keep.
func TestDrainWaitsForDLQPublish(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	slow := &slowPublisher{gate: gate, started: func() { once.Do(func() { close(started) }) }}
	h, wait := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error {
		return werrors.Invalid("payload", stderrors.New("bad"))
	}, inbox.NewMemoryStore(), slow)

	inFlight := make(chan error, 1)
	go func() { inFlight <- h(context.Background(), msg("evt-1")) }()
	<-started // the DLQ publish is mid-flight

	waitReturned := make(chan error, 1)
	go func() { waitReturned <- wait(context.Background()) }()
	for range 1_000 {
		select {
		case <-waitReturned:
			t.Fatal("wait returned while a DLQ publish was in flight")
		default:
			runtime.Gosched()
		}
	}
	close(gate)
	if err := <-inFlight; err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if err := <-waitReturned; err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestExponentialBackoff(t *testing.T) {
	t.Parallel()

	p := broker.ExponentialBackoff(4)
	for attempt := 1; attempt <= 3; attempt++ {
		delay, retry := p.Next(attempt)
		if !retry {
			t.Fatalf("attempt %d: retry = false before the bound", attempt)
		}
		if delay < 0 || delay > 30*time.Second {
			t.Errorf("attempt %d: delay %v outside [0, 30s]", attempt, delay)
		}
	}
	if _, retry := p.Next(4); retry {
		t.Error("attempt 4 of 4: retry = true — attempts is a total bound")
	}
}

func TestPipelineCompositionGuards(t *testing.T) {
	t.Parallel()

	assertPanics := func(t *testing.T, want string, fn func()) {
		t.Helper()
		defer func() {
			r := recover()
			msg, ok := r.(string)
			if !ok || !strings.Contains(msg, want) {
				t.Errorf("panic = %v, want a message containing %q", r, want)
			}
		}()
		fn()
	}

	assertPanics(t, "nil handler", func() {
		_, _ = broker.Pipeline("sub-t", "t", nil, inbox.NewMemoryStore(), &capturePublisher{})
	})
	assertPanics(t, "nil dead-letter publisher", func() {
		_, _ = broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error { return nil }, inbox.NewMemoryStore(), nil)
	})
	assertPanics(t, "nil inbox store", func() {
		_, _ = broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error { return nil }, nil, &capturePublisher{})
	})
}

func TestDeliveryHeadersSeam(t *testing.T) {
	t.Parallel()

	var got map[string]string
	h, _ := pipelineFor(t, func(ctx context.Context, _ broker.Message) error {
		got = broker.DeliveryHeaders(ctx)
		return nil
	})
	if err := h(context.Background(), msg("evt-1")); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if got["traceparent"] != "00-abc" {
		t.Errorf("DeliveryHeaders = %v — TraceExtract did not seed the context", got)
	}
}

type slowPublisher struct {
	gate    chan struct{}
	started func()
}

func (p *slowPublisher) Publish(context.Context, string, ...broker.Message) error {
	p.started()
	<-p.gate
	return nil
}

func BenchmarkPipelineFullChain(b *testing.B) {
	// Dedupe on, unique IDs — the real per-message cost.
	store := inbox.NewMemoryStore()
	h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error { return nil },
		store, &capturePublisher{})
	ctx := context.Background()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		_ = h(ctx, broker.Message{ID: strconv.Itoa(i), Type: "t"})
	}
}

func BenchmarkPipelineDedupeSuppressed(b *testing.B) {
	store := inbox.NewMemoryStore()
	h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error { return nil },
		store, &capturePublisher{})
	ctx := context.Background()
	m := broker.Message{ID: "same", Type: "t"}
	_ = h(ctx, m)
	b.ReportAllocs()
	for b.Loop() {
		_ = h(ctx, m)
	}
}

func BenchmarkPipelineSuccessPath(b *testing.B) {
	store := inbox.NewMemoryStore()
	h, _ := broker.Pipeline("sub-t", "t", func(context.Context, broker.Message) error { return nil },
		store, &capturePublisher{}, broker.WithoutDedupe())
	ctx := context.Background()
	m := broker.Message{ID: "evt", Type: "t"}
	b.ReportAllocs()
	for b.Loop() {
		_ = h(ctx, m)
	}
}

// TestDedupeIsScopedToTheSubscription is the fan-out defect: two features
// subscribing to one topic through one inbox store. Keyed on the message id
// alone, whichever handler ran first marked the message seen and the second
// never saw it — silent message loss in the exact shape §5.6 documents as
// supported.
func TestDedupeIsScopedToTheSubscription(t *testing.T) {
	t.Parallel()

	store := inbox.NewMemoryStore()
	dlq := &capturePublisher{}
	var billing, email atomic.Int32

	b, _ := broker.Pipeline("billing", "order.placed", func(context.Context, broker.Message) error {
		billing.Add(1)
		return nil
	}, store, dlq)
	e, _ := broker.Pipeline("notification", "order.placed", func(context.Context, broker.Message) error {
		email.Add(1)
		return nil
	}, store, dlq)

	msg := broker.Message{ID: "evt-1", Type: "order.placed"}
	for _, h := range []broker.MessageHandler{b, e} {
		if err := h(context.Background(), msg); err != nil {
			t.Fatalf("handling: %v", err)
		}
	}
	if got := billing.Load(); got != 1 {
		t.Errorf("billing ran %d times, want 1", got)
	}
	if got := email.Load(); got != 1 {
		t.Errorf("notification ran %d times, want 1 — the other subscription's mark suppressed it", got)
	}

	// And redelivery to the SAME subscription is still suppressed.
	if err := b(context.Background(), msg); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := billing.Load(); got != 1 {
		t.Errorf("billing ran %d times after redelivery, want 1", got)
	}
}

// TestMessageWithoutAnIDIsDeadLettered — Message.ID is the idempotency key,
// and nothing enforced it. Five distinct messages carrying no id collapsed
// to one: the first was handled, the other four were silently acked and
// destroyed.
func TestMessageWithoutAnIDIsDeadLettered(t *testing.T) {
	t.Parallel()

	store := inbox.NewMemoryStore()
	dlq := &capturePublisher{}
	var handled atomic.Int32

	h, _ := broker.Pipeline("billing", "t", func(context.Context, broker.Message) error {
		handled.Add(1)
		return nil
	}, store, dlq)

	for i := range 5 {
		if err := h(context.Background(), broker.Message{Type: "t", Payload: []byte{byte(i)}}); err != nil {
			t.Fatalf("handling %d: %v", i, err)
		}
	}
	if got := handled.Load(); got != 0 {
		t.Errorf("the handler ran %d times on messages with no id, want 0", got)
	}
	// Dead-lettered, never dropped: an unusable message is preserved.
	if got := len(dlq.dlq("t.dlq")); got != 5 {
		t.Errorf("%d messages reached the dead-letter topic, want 5", got)
	}
}

// TestDedupeKeysAreStorableByADurableStore — the dedupe key is written to
// an inbox.Store, and Postgres `text` rejects a NUL byte outright, so a
// NUL-separated key is unstorable in the first durable store anyone writes.
// The memory store swallows it, which is the worst pairing: it works in
// development and fails in production.
//
// This test watches the key the pipeline actually hands to a Store rather
// than asserting on the separator constant, so it still holds if the
// scoping scheme is rewritten.
func TestDedupeKeysAreStorableByADurableStore(t *testing.T) {
	t.Parallel()

	store := &keySpy{Store: inbox.NewMemoryStore()}
	dlq := &capturePublisher{}
	h, _ := broker.Pipeline("billing", "order.placed", func(context.Context, broker.Message) error {
		return nil
	}, store, dlq)

	if err := h(context.Background(), broker.Message{ID: "evt-1", Type: "order.placed"}); err != nil {
		t.Fatalf("handling: %v", err)
	}

	keys := store.seen()
	if len(keys) == 0 {
		t.Fatal("the pipeline never touched the store")
	}
	for _, k := range keys {
		if strings.ContainsRune(k, 0) {
			t.Errorf("the dedupe key %q contains a NUL byte — Postgres text cannot store it, so a durable inbox store would error where the memory store does not", k)
		}
		if !strings.Contains(k, "evt-1") || !strings.Contains(k, "billing") {
			t.Errorf("dedupe key %q no longer carries both the subscription and the message id", k)
		}
	}
}

// TestMessageIDWithANULIsDeadLettered — the other half of the guarantee
// above. The separator is Warren's to choose; the id comes off the wire, so
// a producer can put a NUL in it, and then the key is unstorable through no
// fault of the store. It is refused as INVALID — terminal, so preserved.
func TestMessageIDWithANULIsDeadLettered(t *testing.T) {
	t.Parallel()

	store := inbox.NewMemoryStore()
	dlq := &capturePublisher{}
	var handled atomic.Int32

	h, _ := broker.Pipeline("billing", "t", func(context.Context, broker.Message) error {
		handled.Add(1)
		return nil
	}, store, dlq)

	if err := h(context.Background(), broker.Message{ID: "evt\x001", Type: "t"}); err != nil {
		t.Fatalf("handling: %v", err)
	}
	if got := handled.Load(); got != 0 {
		t.Errorf("the handler ran %d times on a message whose id holds a NUL, want 0", got)
	}
	if got := len(dlq.dlq("t.dlq")); got != 1 {
		t.Errorf("%d messages reached the dead-letter topic, want 1 — it must be preserved, not dropped", got)
	}
}

// keySpy records every key the pipeline builds, without changing what the
// wrapped store does with it.
type keySpy struct {
	inbox.Store
	mu   sync.Mutex
	keys []string
}

func (s *keySpy) Seen(ctx context.Context, key string) (bool, error) {
	s.record(key)
	return s.Store.Seen(ctx, key)
}

func (s *keySpy) MarkSeen(ctx context.Context, key string, ttl time.Duration) error {
	s.record(key)
	return s.Store.MarkSeen(ctx, key, ttl)
}

func (s *keySpy) record(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, key)
}

func (s *keySpy) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

// TestNonPositiveDedupeTTLPanics — WithDedupeTTL(0) silently disabled
// deduplication, while WithConcurrency(0) panics. A boot-time refusal is
// the documented shape for both.
func TestNonPositiveDedupeTTLPanics(t *testing.T) {
	t.Parallel()

	for _, ttl := range []time.Duration{0, -time.Hour} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithDedupeTTL(%v) did not panic — it silently disabled dedupe", ttl)
				}
			}()
			_, _ = broker.Pipeline("s", "t", func(context.Context, broker.Message) error { return nil },
				inbox.NewMemoryStore(), &capturePublisher{}, broker.WithDedupeTTL(ttl))
		}()
	}
}

// nonRedelivering is a Publisher that also declares it will not redeliver a
// nacked message — the shape broker/memory has.
type nonRedelivering struct {
	mu   sync.Mutex
	sent map[string][]broker.Message
}

func (p *nonRedelivering) Publish(_ context.Context, topic string, msgs ...broker.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sent == nil {
		p.sent = map[string][]broker.Message{}
	}
	p.sent[topic] = append(p.sent[topic], msgs...)
	return nil
}

func (p *nonRedelivering) Redelivers() bool { return false }

func (p *nonRedelivering) count(topic string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent[topic])
}

// TestExhaustedUnavailableIsDeadLetteredOnANonRedeliveringBroker — field test
// #4, defect A3. §2.6 says the consumer disposition for UNAVAILABLE is "nack
// + backoff retry" with no DLQ, and that is right — but it rests on a
// PREMISE: that nacking returns the message to the broker for another
// attempt. An in-process broker has no durable log and no acknowledgement
// protocol, so a nack there is a drop.
//
// The result was that UNAVAILABLE — the TRANSIENT code, the one that means
// "try again" — was the only lossy one, while INVALID and INTERNAL were
// preserved. A downstream unavailable for longer than the retry budget lost
// the event permanently, in the driver every scaffolded app runs.
func TestExhaustedUnavailableIsDeadLetteredOnANonRedeliveringBroker(t *testing.T) {
	t.Parallel()

	dlq := &nonRedelivering{}
	attempts := 0
	handle := func(context.Context, broker.Message) error {
		attempts++
		return werrors.Unavailable("downstream", stderrors.New("connection refused"))
	}

	pipeline, _ := broker.Pipeline("test.sub", "orders", handle,
		inbox.NewMemoryStore(), dlq,
		broker.WithRetry(broker.ExponentialBackoff(2)),
	)

	err := pipeline(context.Background(), broker.Message{ID: "m1", Type: "order.placed"})
	if err != nil {
		t.Errorf("the message was nacked back to a broker that cannot redeliver: %v", err)
	}
	if attempts < 2 {
		t.Errorf("handler ran %d times; the retry budget was not spent first", attempts)
	}
	if n := dlq.count("orders.dlq"); n != 1 {
		t.Errorf("dead-lettered %d messages, want 1 — the event was dropped", n)
	}
}

// TestExhaustedUnavailableIsStillNackedOnARedeliveringBroker — the change
// must not cost the case §2.6 was written for. A real broker redelivers, and
// dead-lettering there would turn a transient blip into a DLQ full of
// messages that would have succeeded on the next attempt.
func TestExhaustedUnavailableIsStillNackedOnARedeliveringBroker(t *testing.T) {
	t.Parallel()

	dlq := &recordingPublisher{} // no Redelivers method: assumed to redeliver
	handle := func(context.Context, broker.Message) error {
		return werrors.Unavailable("downstream", stderrors.New("connection refused"))
	}

	pipeline, _ := broker.Pipeline("test.sub2", "orders", handle,
		inbox.NewMemoryStore(), dlq,
		broker.WithRetry(broker.ExponentialBackoff(2)),
	)

	err := pipeline(context.Background(), broker.Message{ID: "m2", Type: "order.placed"})
	if !werrors.Is(err, werrors.CodeUnavailable) {
		t.Errorf("err = %v, want UNAVAILABLE nacked back for redelivery", err)
	}
	if n := len(dlq.msgs); n != 0 {
		t.Errorf("dead-lettered %d messages on a redelivering broker, want 0", n)
	}
}

// TestWrappersDoNotHideNonRedelivery — the scaffold does not hand the raw
// broker to Pipeline; it hands broker.Correlating(b). A wrapper that does not
// forward Redelivers turns the driver's honest "I cannot redeliver" into the
// default assumption that it can, and the fix above silently stops applying
// in the only configuration that ships.
//
// This is the failure this test exists to catch, because everything still
// compiles and every other test still passes.
func TestWrappersDoNotHideNonRedelivery(t *testing.T) {
	t.Parallel()

	dlq := &nonRedelivering{}
	handle := func(context.Context, broker.Message) error {
		return werrors.Unavailable("downstream", stderrors.New("connection refused"))
	}

	// Exactly what platform hands Pipeline: never the raw broker.
	pipeline, _ := broker.Pipeline("wrap.sub", "orders", handle,
		inbox.NewMemoryStore(), broker.Correlating(dlq),
		broker.WithRetry(broker.ExponentialBackoff(2)),
	)
	if err := pipeline(context.Background(), broker.Message{ID: "w1", Type: "order.placed"}); err != nil {
		t.Errorf("Correlating hid the driver's non-redelivery; the message was nacked into nothing: %v", err)
	}
	if n := dlq.count("orders.dlq"); n != 1 {
		t.Errorf("dead-lettered %d messages through a Correlating wrapper, want 1", n)
	}
}

// TestAGuardedConsumerDeadLettersEveryMessage pins the warning warren.md §7.2
// gives, which was prose with nothing enforcing it.
//
// Event routes carry no identity in v0.1 — there is no header convention and
// no propagating decorator — so a policy composed into a consumer chain
// denies EVERY message. §2.6 sends UNAUTHENTICATED to the dead-letter queue
// without retrying, because a message will not get a better token by waiting.
//
// That is fail-closed and therefore correct. It is also a 3 a.m. incident if
// someone reaches for app.Authorized in a consumer expecting it to work, so
// the behaviour is asserted rather than described: the handler must never
// run, the message must be preserved, and it must NOT be nacked back to the
// broker for a retry that cannot help.
func TestAGuardedConsumerDeadLettersEveryMessage(t *testing.T) {
	t.Parallel()

	dlq := &nonRedelivering{}
	handled := 0
	guarded := func(ctx context.Context, _ broker.Message) error {
		if err := app.RequireAuthenticated().Authorize(ctx); err != nil {
			return err
		}
		handled++
		return nil
	}

	pipeline, _ := broker.Pipeline("audit.sub", "doc.created", guarded,
		inbox.NewMemoryStore(), dlq)

	err := pipeline(context.Background(), broker.Message{ID: "m1", Type: "doc.created"})
	if err != nil {
		t.Errorf("a dead-lettered message was also nacked: %v — it would be redelivered for ever", err)
	}
	if handled != 0 {
		t.Errorf("the guarded handler ran %d times with no identity on the context", handled)
	}
	if n := dlq.count("doc.created.dlq"); n != 1 {
		t.Errorf("dead-lettered %d messages, want 1 — §7.2 promises the message is preserved", n)
	}
}

// TestTheDeadLetterAlertFollowsThePublish pins WHEN the alert fires.
//
// "message dead-lettered" is the one consumer event meant to page a human,
// and it used to be logged BEFORE the publish that preserves the message. So
// on the ordinary production failure — a missing <topic>.dlq, because
// somebody provisioned the topics their handlers consume and knew nothing of
// the shadow set — it announced a preservation that never happened, on every
// redelivery of a message the consumer was looping on for ever.
func TestTheDeadLetterAlertFollowsThePublish(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := log.WithLogger(context.Background(), logger)

	pub := &capturePublisher{failed: werrors.Unavailable("kafka", stderrors.New("no such topic"))}
	h := broker.DeadLetter(pub, "orders", "orders.dlq")(
		func(context.Context, broker.Message) error {
			return werrors.Invalid("payload", nil) // terminal
		})

	err := h(ctx, broker.Message{ID: "m-1", Type: "t"})
	if err == nil {
		t.Fatal("a failed dead-letter publish must nack, not ack")
	}
	if code := werrors.CodeOf(err); code != werrors.CodeUnavailable {
		t.Errorf("code = %v, want UNAVAILABLE so the broker redelivers", code)
	}

	out := buf.String()
	if !strings.Contains(out, "dead-letter publish failed") {
		t.Errorf("the failure was not reported:\n%s", out)
	}
	if strings.Contains(out, "message dead-lettered") {
		t.Errorf("the alert claims the message was dead-lettered, but the publish failed:\n%s", out)
	}

	// And on the happy path it must still fire — an alert that never
	// arrives is the defect this line exists to prevent.
	var ok bytes.Buffer
	okCtx := log.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&ok, nil)))
	good := &capturePublisher{}
	h2 := broker.DeadLetter(good, "orders", "orders.dlq")(
		func(context.Context, broker.Message) error { return werrors.Invalid("payload", nil) })
	if err := h2(okCtx, broker.Message{ID: "m-2", Type: "t"}); err != nil {
		t.Fatalf("a successful dead-letter must ack: %v", err)
	}
	if !strings.Contains(ok.String(), "message dead-lettered") {
		t.Errorf("the alert never fired on a successful dead-letter:\n%s", ok.String())
	}
	if len(good.dlq("orders.dlq")) != 1 {
		t.Errorf("the envelope did not reach the dead-letter topic")
	}
}

// TestContentionIsNackedNotAcked — the disposition that fixes a live silent
// data loss. Today a consumer handler that loses an optimistic-lock race
// returns errors.Conflict, which Retry does not retry and DeadLetter ACKS:
//
//	case errors.CodeNotFound, errors.CodeConflict:
//	    return nil
//
// so the message is destroyed with the work NOT done — no log at ERROR, no
// DLQ entry, nothing distinguishing it from success.
//
// CONFLICT keeps acking, and rightly: its justification is "idempotent
// replay", meaning the work was already applied. CONTENTION asserts the
// opposite — the conditional write matched zero rows, so nothing was
// applied — and a message whose work was never done has to come back.
func TestContentionIsNackedNotAcked(t *testing.T) {
	t.Parallel()

	h, dlq := pipelineFor(t, func(context.Context, broker.Message) error {
		return werrors.Contention("stock s-1 was changed by another request")
	}, broker.WithRetry(zeroDelay{max: 2}))

	err := h(context.Background(), msg("m-1"))
	if err == nil {
		t.Fatal("a contended message was ACKED — its work was never done, and it is now gone")
	}
	if got := werrors.CodeOf(err); got != werrors.CodeContention {
		t.Errorf("code = %s, want CONTENTION", got)
	}
	// Not dead-lettered: redelivery is overwhelmingly likely to succeed, and
	// dead-lettering ordinary contention turns a busy aggregate into an
	// operational incident and a page.
	if n := len(dlq.dlq("user.events")); n != 0 {
		t.Errorf("%d message(s) dead-lettered — contention is expected to succeed on redelivery", n)
	}
}

// TestContentionIsRetriedInProcessFirst — Pipeline composes Retry inside
// DeadLetter, so ordinary contention is absorbed by backoff and the nack is
// the fallback rather than the first answer.
func TestContentionIsRetriedInProcessFirst(t *testing.T) {
	t.Parallel()

	attempts := 0
	h, _ := pipelineFor(t, func(context.Context, broker.Message) error {
		attempts++
		if attempts < 3 {
			return werrors.Contention("lost the race")
		}
		return nil
	}, broker.WithRetry(zeroDelay{max: 5}))

	if err := h(context.Background(), msg("m-1")); err != nil {
		t.Fatalf("a contended message that would have succeeded was not retried: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// TestConflictStillAcks — the control. A business refusal is arithmetic: the
// tenth delivery is refused exactly like the first, so redelivering it for
// ever is the wrong answer and always was.
func TestConflictStillAcks(t *testing.T) {
	t.Parallel()

	h, dlq := pipelineFor(t, func(context.Context, broker.Message) error {
		return werrors.Conflict("already applied")
	}, broker.WithRetry(zeroDelay{max: 2}))

	if err := h(context.Background(), msg("m-1")); err != nil {
		t.Errorf("CONFLICT was not acked: %v", err)
	}
	if n := len(dlq.dlq("user.events")); n != 0 {
		t.Errorf("CONFLICT was dead-lettered: %d", n)
	}
}

// TestADiscardedMessageIsLogged — field test #10, defect 3. §2.6's row for
// NOT_FOUND says "ack + log" and there was no log call on that branch, so a
// consumer discarding messages because a row is missing produced no record
// anywhere. That is the class of silence the CONTENTION split was introduced
// to kill, promised in the table and not delivered.
//
// CONFLICT is discarded too and its row honestly says "ack (idempotent
// replay)" — but a message dropped because the work was already done is
// still a message dropped, and an operator watching a queue drain to nothing
// deserves to know which of the two it was.
func TestADiscardedMessageIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"NOT_FOUND", werrors.NotFound("order", "o-9"), "NOT_FOUND"},
		{"CONFLICT", werrors.Conflict("already applied"), "CONFLICT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			h, _ := pipelineFor(t, func(context.Context, broker.Message) error {
				return tc.err
			})
			if err := h(context.Background(), msg("m-1")); err != nil {
				t.Fatalf("the message was not acked: %v", err)
			}

			out := buf.String()
			if !strings.Contains(out, "message discarded") {
				t.Errorf("a discarded message left no trace in the log: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the line does not carry the code %s: %q", tc.want, out)
			}
			if !strings.Contains(out, "m-1") {
				t.Errorf("the line does not name the message: %q", out)
			}
		})
	}
}

// TestReplayingADeadLetterNeedsANewMessageID — field test #10, defect 2.
// GETTING_STARTED says a DLQ topic is ordinary and "that is how you inspect
// and replay". Republishing the preserved envelope to its ORIGIN topic with
// its original id is silently discarded: Deduplicate sits outside DeadLetter
// and DeadLetter returns nil once it has published, so the id is marked seen
// for a message whose work never happened.
//
// That suppression is CORRECT for a broker redelivery — the message is
// already preserved in the DLQ, and handling it twice is what the inbox
// exists to prevent. It is wrong only for a deliberate replay, and the
// difference is the id. So the contract is: replay under a NEW message id.
func TestReplayingADeadLetterNeedsANewMessageID(t *testing.T) {
	t.Parallel()

	handled := 0
	fail := true
	store := inbox.NewMemoryStore()
	// A publisher that cannot redeliver — broker/memory's shape, and the
	// scaffold's default. On one that CAN, an exhausted CONTENTION nacks
	// instead and never reaches the DLQ.
	dlq := &droppingPublisher{capturePublisher: &capturePublisher{}}
	h, _ := broker.Pipeline("sub-user-events", "user.events",
		func(context.Context, broker.Message) error {
			if fail {
				return werrors.Contention("lost the race")
			}
			handled++
			return nil
		}, store, dlq, broker.WithRetry(zeroDelay{max: 2}))

	// Dead-letter it.
	if err := h(context.Background(), msg("m-1")); err != nil {
		t.Fatalf("the message was not preserved: %v", err)
	}
	if n := len(dlq.dlq("user.events.dlq")); n != 1 {
		t.Fatalf("dead-lettered %d, want 1", n)
	}

	// The cause is fixed. Replaying the SAME id does nothing, silently.
	fail = false
	if err := h(context.Background(), msg("m-1")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if handled != 0 {
		t.Errorf("a same-id replay ran the handler %d time(s) — the inbox is supposed to suppress it", handled)
	}

	// A NEW id is the replay that works, and it is what the docs must say.
	if err := h(context.Background(), msg("m-1-replay")); err != nil {
		t.Fatalf("replay under a new id: %v", err)
	}
	if handled != 1 {
		t.Errorf("replay under a new id ran the handler %d time(s), want 1", handled)
	}
}

// droppingPublisher declares that a nack is a drop, so DeadLetter preserves
// instead of returning the message to a broker that would lose it.
type droppingPublisher struct{ *capturePublisher }

func (droppingPublisher) Redelivers() bool { return false }
