package broker_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/log"
)

// syncBuf is a concurrency-safe sink for the pipeline's own log records.
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

// pipelineLogger returns a context carrying a logger writing into buf, at
// debug level so nothing is filtered out of the assertions.
//
// It wraps the handler in log.Handler, which is how a real Warren main is
// configured: the correlation ID is attached by that handler at Handle time,
// from the context, rather than by each call site remembering to pass it.
// Asserting on the ID here therefore also pins that the pipeline logs through
// the CONTEXT-aware path — a stage calling slog.Default() or a logger
// captured at boot would lose it.
func pipelineLogger(buf *syncBuf) context.Context {
	h := log.Handler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return log.WithLogger(context.Background(), slog.New(h))
}

// TestAPanickingConsumerSaysSo — the field test made a consumer panic and
// Warren emitted NOTHING: no error, no stack, no record of three failed
// attempts or of the dead-letter that followed. The only way to see where the
// message went was to attach a raw subscriber to the DLQ topic.
//
// transport/http logs a panic with a full stack and the correlation ID. The
// consumer ring is where nobody is watching, so it needs it more.
func TestAPanickingConsumerSaysSo(t *testing.T) {
	t.Parallel()

	buf := &syncBuf{}
	ctx := log.WithCorrelationID(pipelineLogger(buf), "corr-1")

	h, _ := broker.Pipeline("subs", "orders", func(context.Context, broker.Message) error {
		panic("consumer exploded")
	}, inbox.NewMemoryStore(), &recordingPublisher{}, broker.WithRetry(broker.ExponentialBackoff(1)))

	_ = h(ctx, broker.Message{ID: "m-1"})

	out := buf.String()
	for _, want := range []string{"consumer exploded", "stack", "corr-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the pipeline's log does not mention %q — a panic nobody can see is a panic nobody fixes:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("a panic was not logged at ERROR:\n%s", out)
	}
}

// TestADeadLetteredMessageSaysSo — warren.md §2.6: the DLQ "stops the
// message, keeps it for inspection, and fires the DLQ alert. Which is
// correct, because this should wake someone up." Nothing woke up.
//
// With the scaffold's memory broker and nobody subscribed to <topic>.dlq, a
// poison message vanished leaving no trace in logs or in storage.
func TestADeadLetteredMessageSaysSo(t *testing.T) {
	t.Parallel()

	buf := &syncBuf{}
	ctx := log.WithCorrelationID(pipelineLogger(buf), "corr-2")

	h, _ := broker.Pipeline("subs", "orders", func(context.Context, broker.Message) error {
		return errors.Invalid("payload", stderrorsNew("not JSON"))
	}, inbox.NewMemoryStore(), &recordingPublisher{}, broker.WithRetry(broker.ExponentialBackoff(1)))

	if err := h(ctx, broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("a dead-lettered message must still be acked: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"dead-letter", "orders.dlq", "m-1", "corr-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dead-letter log does not mention %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("a dead-letter was not logged at ERROR — it is the one event that should page someone:\n%s", out)
	}
}

// TestARetriedMessageSaysSo — three attempts happened and the log was empty,
// so a consumer retrying steadily against a broken dependency looked exactly
// like a consumer doing nothing.
func TestARetriedMessageSaysSo(t *testing.T) {
	t.Parallel()

	buf := &syncBuf{}
	ctx := pipelineLogger(buf)

	calls := 0
	h, _ := broker.Pipeline("subs", "orders", func(context.Context, broker.Message) error {
		calls++
		if calls < 2 {
			return errors.Unavailable("downstream", stderrorsNew("connection refused"))
		}
		return nil
	}, inbox.NewMemoryStore(), &recordingPublisher{})

	if err := h(ctx, broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("the retry should have succeeded on attempt 2: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "retrying") {
		t.Errorf("a retry was not logged:\n%s", out)
	}
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("a retry was not logged at WARN — it is a warning, not a failure:\n%s", out)
	}
}

// TestTheHappyPathStaysQuiet — a consumer that works must not log per
// message. The point of the three tests above is failure visibility, and a
// framework that narrates success is a framework whose logs get filtered out.
func TestTheHappyPathStaysQuiet(t *testing.T) {
	t.Parallel()

	buf := &syncBuf{}
	ctx := pipelineLogger(buf)

	h, _ := broker.Pipeline("subs", "orders", func(context.Context, broker.Message) error {
		return nil
	}, inbox.NewMemoryStore(), &recordingPublisher{})

	if err := h(ctx, broker.Message{ID: "m-1"}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out := buf.String(); out != "" {
		t.Errorf("a successful delivery logged something:\n%s", out)
	}
}

func stderrorsNew(s string) error { return errorString(s) }

type errorString string

func (e errorString) Error() string { return string(e) }
