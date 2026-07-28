package log_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/MerseniBilel/warren/log"
)

// newLogger returns a logger writing deterministic JSON to buf: the timestamp
// is dropped, because a record containing one cannot be compared.
func newLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	}))
}

func TestIntoAndFromRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := newLogger(&buf)
	ctx := log.Into(t.Context(), logger)

	if got := log.From(ctx); got != logger {
		t.Errorf("From() returned a different logger than Into() stored")
	}
}

func TestFromFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	tests := map[string]context.Context{
		"a context carrying no logger": t.Context(),
		"a nil context":                nil,
	}

	for name, ctx := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := log.From(ctx); got != slog.Default() {
				t.Errorf("From() = %v, want slog.Default()", got)
			}
		})
	}
}

func TestIntoANilLoggerReturnsTheContextUnchanged(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	if got := log.Into(ctx, nil); got != ctx {
		t.Error("Into(ctx, nil) returned a different context")
	}
}

func TestWithNoArgumentsReturnsTheContextUnchanged(t *testing.T) {
	t.Parallel()

	ctx := log.Into(t.Context(), newLogger(&bytes.Buffer{}))

	if got := log.With(ctx); got != ctx {
		t.Error("With(ctx) with no arguments returned a different context")
	}

	if got := log.WithAttrs(ctx); got != ctx {
		t.Error("WithAttrs(ctx) with no attributes returned a different context")
	}
}

func TestWithAccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	ctx := log.Into(t.Context(), newLogger(&buf))
	ctx = log.With(ctx, "request_id", "r-1")
	ctx = log.WithAttrs(ctx, slog.String("tenant", "acme"))

	log.From(ctx).InfoContext(ctx, "handled")

	const want = `{"level":"INFO","msg":"handled","request_id":"r-1","tenant":"acme"}` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("record = %s, want %s", got, want)
	}
}

func TestWithDoesNotMutateTheParentContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	parent := log.Into(t.Context(), newLogger(&buf))
	_ = log.With(parent, "request_id", "r-1")

	log.From(parent).InfoContext(parent, "handled")

	const want = `{"level":"INFO","msg":"handled"}` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("record = %s, want %s", got, want)
	}
}

func TestWithUsesTheDefaultWhenTheContextCarriesNoLogger(t *testing.T) {
	t.Parallel()

	// With must not panic or drop the attributes when there is no logger to
	// derive from; it derives from slog.Default() instead.
	ctx := log.With(t.Context(), "request_id", "r-1")

	if log.From(ctx) == slog.Default() {
		t.Error("With() did not derive a new logger from the default")
	}
}
