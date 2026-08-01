package log_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren/log"
)

// recorder is the in-memory slog.Handler the assertions run against: it
// records the attributes each record carries, With-attached ones included.
// The sink is shared by pointer so records logged through a WithAttrs-derived
// child are visible on the recorder the test holds.
type recorder struct {
	mu    *sync.Mutex
	attrs []slog.Attr
	sink  *[]map[string]any
}

func newRecorder() *recorder {
	return &recorder{mu: &sync.Mutex{}, sink: &[]map[string]any{}}
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	got := make(map[string]any)
	for _, a := range r.attrs {
		got[a.Key] = a.Value.Any()
	}
	rec.Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.Any()
		return true
	})
	r.mu.Lock()
	*r.sink = append(*r.sink, got)
	r.mu.Unlock()
	return nil
}

func (r *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recorder{mu: r.mu, sink: r.sink, attrs: append(append([]slog.Attr{}, r.attrs...), attrs...)}
}

func (r *recorder) WithGroup(string) slog.Handler { return r }

func (r *recorder) records() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.sink
}

func TestFromContext(t *testing.T) {
	t.Parallel()

	t.Run("returns the seeded logger", func(t *testing.T) {
		t.Parallel()
		seeded := slog.New(newRecorder())
		ctx := log.WithLogger(context.Background(), seeded)
		if got := log.FromContext(ctx); got != seeded {
			t.Error("FromContext did not return the logger WithLogger seeded")
		}
	})

	t.Run("a nil logger is not carried — the never-nil guarantee holds", func(t *testing.T) {
		t.Parallel()
		ctx := log.WithLogger(context.Background(), nil)
		got := log.FromContext(ctx)
		if got == nil {
			t.Fatal("FromContext returned nil after WithLogger(ctx, nil)")
		}
		if got != slog.Default() {
			t.Error("a nil-seeded context did not behave as unseeded")
		}
	})

	t.Run("falls back to slog.Default on an unseeded context", func(t *testing.T) {
		t.Parallel()
		got := log.FromContext(context.Background())
		if got == nil {
			t.Fatal("FromContext returned nil — §2.5 usage calls .Info on it unconditionally")
		}
		if got != slog.Default() {
			t.Error("FromContext on an unseeded context did not return slog.Default()")
		}
	})
}

func TestWith(t *testing.T) {
	t.Parallel()

	t.Run("attached fields appear on every subsequent record", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		ctx := log.WithLogger(context.Background(), slog.New(rec))
		ctx = log.With(ctx, "module", "user")

		log.FromContext(ctx).Info("registering", "email", "bob@example.com")

		if len(rec.records()) != 1 {
			t.Fatalf("recorded %d records, want 1", len(rec.records()))
		}
		got := rec.records()[0]
		if got["module"] != "user" || got["email"] != "bob@example.com" {
			t.Errorf("record carries %v, want the With field and the call-site field", got)
		}
	})

	t.Run("deriving twice accumulates attributes in order", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		ctx := log.WithLogger(context.Background(), slog.New(rec))
		ctx = log.With(ctx, "module", "user")
		ctx = log.With(ctx, "handler", "RegisterUser")

		log.FromContext(ctx).Info("hi")

		got := rec.records()[0]
		if got["module"] != "user" || got["handler"] != "RegisterUser" {
			t.Errorf("record carries %v, want both derived fields", got)
		}
	})

	t.Run("the parent context is unmodified", func(t *testing.T) {
		t.Parallel()
		rec := newRecorder()
		parent := log.WithLogger(context.Background(), slog.New(rec))
		_ = log.With(parent, "module", "user")

		log.FromContext(parent).Info("hi")

		if got := rec.records()[0]; got["module"] != nil {
			t.Errorf("parent context's logger gained the child's field: %v", got)
		}
	})

	t.Run("works on an unseeded context by deriving from the default", func(t *testing.T) {
		t.Parallel()
		ctx := log.With(context.Background(), "module", "user")
		if log.FromContext(ctx) == slog.Default() {
			t.Error("With on an unseeded context did not derive a new logger")
		}
	})

	t.Run("no args returns the context unchanged", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		if log.With(ctx) != ctx {
			t.Error("With(ctx) with no args did not return ctx itself")
		}
	})
}

func TestCorrelationID(t *testing.T) {
	t.Parallel()

	t.Run("round-trips through the context", func(t *testing.T) {
		t.Parallel()
		ctx := log.WithCorrelationID(context.Background(), "req-42")
		if got := log.CorrelationID(ctx); got != "req-42" {
			t.Errorf("CorrelationID() = %q, want %q", got, "req-42")
		}
	})

	t.Run("empty when none is carried", func(t *testing.T) {
		t.Parallel()
		if got := log.CorrelationID(context.Background()); got != "" {
			t.Errorf("CorrelationID() on an unseeded context = %q, want empty", got)
		}
	})
}

func BenchmarkFromContextSeeded(b *testing.B) {
	ctx := log.WithLogger(context.Background(), slog.New(newRecorder()))
	b.ReportAllocs()
	for b.Loop() {
		_ = log.FromContext(ctx)
	}
}

func BenchmarkWithTwoFields(b *testing.B) {
	ctx := log.WithLogger(context.Background(), slog.New(newRecorder()))
	b.ReportAllocs()
	for b.Loop() {
		_ = log.With(ctx, "module", "user", "handler", "RegisterUser")
	}
}

func BenchmarkCorrelationID(b *testing.B) {
	ctx := log.WithCorrelationID(context.Background(), "req-42")
	b.ReportAllocs()
	for b.Loop() {
		_ = log.CorrelationID(ctx)
	}
}
