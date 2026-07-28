package errors_test

import (
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/MerseniBilel/warren/errors"
)

// deepChain builds a five-deep chain alternating Warren and foreign errors,
// which is what an error looks like by the time it reaches a transport.
func deepChain() error {
	return errors.Internal("boot failed").Op("warren.Run").Field("phase", "start").
		Wrapping(fmt.Errorf("module orders: %w",
			errors.NotFound("no provider for *sql.DB").Op("di.Resolve").
				Wrapping(stderrors.New("connection refused"))))
}

func BenchmarkNotFound(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		_ = errors.NotFound("no order abc")
	}
}

func BenchmarkNotFoundFormatted(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		_ = errors.NotFound("no order %s", "abc")
	}
}

func BenchmarkOp(b *testing.B) {
	err := errors.NotFound("no order abc")

	b.ReportAllocs()

	for b.Loop() {
		_ = err.Op("orders.Get")
	}
}

func BenchmarkField(b *testing.B) {
	err := errors.NotFound("no order abc")

	b.ReportAllocs()

	for b.Loop() {
		_ = err.Field("order_id", "abc")
	}
}

func BenchmarkFix(b *testing.B) {
	err := errors.NotFound("no order abc")

	b.ReportAllocs()

	for b.Loop() {
		_ = err.Fix("create it first")
	}
}

func BenchmarkWrapping(b *testing.B) {
	err := errors.Internal("query failed")
	cause := stderrors.New("connection refused")

	b.ReportAllocs()

	for b.Loop() {
		_ = err.Wrapping(cause)
	}
}

func BenchmarkCodeOfDeepChain(b *testing.B) {
	err := deepChain()

	b.ReportAllocs()

	for b.Loop() {
		_ = errors.CodeOf(err)
	}
}

func BenchmarkErrorString(b *testing.B) {
	err := deepChain()

	b.ReportAllocs()

	for b.Loop() {
		_ = err.Error()
	}
}

func BenchmarkDetail(b *testing.B) {
	err := deepChain()

	b.ReportAllocs()

	for b.Loop() {
		_ = errors.Detail(err)
	}
}
