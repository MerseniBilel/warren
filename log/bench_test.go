package log_test

import (
	"log/slog"
	"testing"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// nullWriter accepts output and drops it.
//
// It is deliberately not io.Discard: sloglint rightly suggests
// slog.DiscardHandler for that, and a discarding handler skips the attribute
// preformatting these benchmarks exist to measure. What is wanted is a real
// JSONHandler doing real encoding, without the write itself.
type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }

// jsonLogger reports the cost a service actually pays, preformatting included.
func jsonLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(nullWriter{}, nil))
}

func BenchmarkFrom(b *testing.B) {
	ctx := log.Into(b.Context(), jsonLogger())

	b.ReportAllocs()

	for b.Loop() {
		_ = log.From(ctx)
	}
}

func BenchmarkFromMissing(b *testing.B) {
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		_ = log.From(ctx)
	}
}

func BenchmarkInto(b *testing.B) {
	ctx := b.Context()
	logger := jsonLogger()

	b.ReportAllocs()

	for b.Loop() {
		_ = log.Into(ctx, logger)
	}
}

func BenchmarkWith(b *testing.B) {
	ctx := log.Into(b.Context(), jsonLogger())

	b.ReportAllocs()

	for b.Loop() {
		_ = log.With(ctx, "request_id", "r-1")
	}
}

func BenchmarkWithAttrs(b *testing.B) {
	ctx := log.Into(b.Context(), jsonLogger())
	attr := slog.String("request_id", "r-1")

	b.ReportAllocs()

	for b.Loop() {
		_ = log.WithAttrs(ctx, attr)
	}
}

func BenchmarkErr(b *testing.B) {
	err := errors.NotFound("no order abc").Op("orders.Get")

	b.ReportAllocs()

	for b.Loop() {
		_ = log.Err(err)
	}
}

func BenchmarkErrWithFields(b *testing.B) {
	err := errors.NotFound("no order abc").
		Op("orders.Get").
		Field("order_id", "abc").
		Field("tenant", "acme").
		Fix("create it first")

	b.ReportAllocs()

	for b.Loop() {
		_ = log.Err(err)
	}
}
