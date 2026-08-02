package broker_test

import (
	"context"
	stderrors "errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/broker"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/inbox"
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
