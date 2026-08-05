//go:build integration

package kafka_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/broker"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/inbox"
)

// syncBuf collects log output from several goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// replaceDefaultLogger points slog at w and returns the undo.
func replaceDefaultLogger(w *syncBuf) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	return func() { slog.SetDefault(prev) }
}

// TestDeadLetterReachesItsTopic runs the disposition path against a real
// broker: a handler that fails terminally must end with the envelope on
// <topic>.dlq, not looping and not lost.
func TestDeadLetterReachesItsTopic(t *testing.T) {
	addr := brokers(t)
	topic := freshTopic(t, addr, "warren-dlq")
	dlqTopic := topic + ".dlq"
	freshPartitions(t, addr, dlqTopic) // provisioned, as production must

	pub, sub := newProbeBroker(t, addr)

	var calls atomic.Int32
	handler := func(context.Context, broker.Message) error {
		calls.Add(1)
		return werrors.Invalid("payload", nil) // terminal: never retried
	}
	chain, _ := broker.Pipeline("dlq-probe", topic, handler, inbox.NewMemoryStore(), pub)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := sub.Subscribe(ctx, topic, chain); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := pub.Publish(context.Background(), topic, broker.Message{
		ID: "bad-1", Type: "probe", Key: "k", Payload: []byte(`{"bad":true}`),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The envelope must appear on the DLQ topic.
	deadline := time.Now().Add(30 * time.Second)
	for {
		counts := endOffsets(t, addr, dlqTopic)
		total := int64(0)
		for _, n := range counts {
			total += n
		}
		if total >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing reached %s after 30s; the message is looping or lost (handler ran %d times)",
				dlqTopic, calls.Load())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// INVALID is terminal, so the handler must have run exactly once — a
	// retried terminal error is the §2.6 table being ignored.
	if n := calls.Load(); n != 1 {
		t.Errorf("handler ran %d times for a terminal error, want 1", n)
	}

	// And the forensic headers must survive, or the DLQ is a pile of
	// payloads nobody can trace.
	got := readOne(t, addr, dlqTopic)
	if got.ID != "bad-1" {
		t.Errorf("dead-lettered ID = %q, want %q", got.ID, "bad-1")
	}
	var named []string
	for k := range got.Headers {
		named = append(named, k)
	}
	if !hasHeaderNaming(got.Headers, topic) {
		t.Errorf("no header names the origin topic %q; headers=%v", topic, named)
	}
}

// hasHeaderNaming reports whether any header value mentions the origin topic.
func hasHeaderNaming(h map[string]string, topic string) bool {
	for _, v := range h {
		if strings.Contains(v, topic) {
			return true
		}
	}
	return false
}

// readOne returns the first message on a topic, through the PORT rather than
// the driver, so the assertions are about the envelope Warren promises.
func readOne(t *testing.T, addr, topic string) broker.Message {
	t.Helper()
	_, sub := newProbeBroker(t, addr)
	got := make(chan broker.Message, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	err := sub.Subscribe(ctx, topic, func(_ context.Context, m broker.Message) error {
		select {
		case got <- m:
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe %s: %v", topic, err)
	}
	select {
	case m := <-got:
		return m
	case <-time.After(30 * time.Second):
		t.Fatalf("nothing readable on %s", topic)
		return broker.Message{}
	}
}

// TestMissingDeadLetterTopicIsLoud covers the trap a production cluster sets:
// auto-creation is off, the operator provisioned the topics their handlers
// consume, and nobody told them each one also needs a <topic>.dlq.
//
// The message must NOT be silently dropped — that is the one forbidden
// outcome — so it nacks and redelivers. What matters is whether anyone can
// find out why: a loop with no diagnostic naming the missing topic is a
// consumer that looks busy and achieves nothing.
func TestMissingDeadLetterTopicIsLoud(t *testing.T) {
	addr := brokers(t)
	topic := freshTopic(t, addr, "warren-nodlq")
	// Deliberately do NOT create topic + ".dlq".

	var logs syncBuf
	restore := replaceDefaultLogger(&logs)
	t.Cleanup(restore)

	pub, sub := newProbeBroker(t, addr)
	var calls atomic.Int32
	handler := func(context.Context, broker.Message) error {
		calls.Add(1)
		return werrors.Invalid("payload", nil)
	}
	chain, _ := broker.Pipeline("nodlq-probe", topic, handler, inbox.NewMemoryStore(), pub)

	ctx, cancel := context.WithCancel(context.Background())
	if err := sub.Subscribe(ctx, topic, chain); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := pub.Publish(context.Background(), topic, broker.Message{
		ID: "bad-2", Type: "probe", Key: "k", Payload: []byte(`{"bad":true}`),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	started := time.Now()

	// Wait for the PUBLISH VERDICT, not for the topic name. The alert line
	// is emitted BEFORE the publish is attempted and names the dlq topic
	// either way, so matching on that passes whether the message was
	// preserved or not — which is exactly the mistake this test existed to
	// catch, and made once.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "dead-letter publish failed") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	elapsed := time.Since(started)
	cancel()

	out := logs.String()
	if !strings.Contains(out, "dead-letter publish failed") {
		t.Fatalf("the failed dead-letter publish was never reported; handler ran %d times.\n%s",
			calls.Load(), tail(out))
	}
	// The operator must learn the TOPIC is missing, not that their handler
	// is flaky: the diagnostic carries kafka's own unknown-topic advice.
	if !strings.Contains(out, "has no topic") {
		t.Errorf("the diagnostic never says the topic is missing:\n%s", tail(out))
	}
	// And it must NOT claim the message was dead-lettered. That line is the
	// alert a human is paged by; firing it here reports a poison message as
	// safely parked while the consumer loops on it for ever.
	if strings.Contains(out, "message dead-lettered") {
		t.Errorf("the alert claims the message was dead-lettered, but the publish failed:\n%s", tail(out))
	}
	t.Logf("one terminal message blocked the consumer for %v before nacking", elapsed)
}

func tail(s string) string {
	if len(s) > 3000 {
		return s[len(s)-3000:]
	}
	return s
}
