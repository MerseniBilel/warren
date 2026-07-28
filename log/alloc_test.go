//go:build !race

// The race detector adds its own heap allocations, which makes an exact
// allocation count meaningless under `go test -race`. This file therefore runs
// under `make test-short` and `make bench`, not under `make test`.

package log_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// allocRuns is the sample size for each measurement. Large enough that a
// one-off allocation during warm-up cannot round the average up past a budget.
const allocRuns = 100

// TestAllocationBudget enforces the budget in log/SPEC.md §5.4. From and Into
// are on the request path: a request that logs nothing still pays for them.
//
// It measures against slog.DiscardHandler, whose WithAttrs returns itself, so
// what is asserted is this package's own cost. A real handler adds its own on
// top — slog.JSONHandler preformats, taking With from 3 allocations to 8 — and
// that is the handler's trade, paid once per request to make every subsequent
// record cheaper. Benchmarks use a JSONHandler and report the realistic figure.
//
//nolint:paralleltest // testing.AllocsPerRun panics if called from a parallel test.
func TestAllocationBudget(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	ctx := log.Into(context.Background(), logger)
	attr := slog.String("request_id", "r-1")
	err := errors.NotFound("no order abc").Op("orders.Get")
	withFields := err.Field("order_id", "abc").Fix("create it first")

	tests := map[string]struct {
		run  func()
		want float64
	}{
		"From with a logger":    {func() { _ = log.From(ctx) }, 0},
		"From without a logger": {func() { _ = log.From(context.Background()) }, 0},
		"Into":                  {func() { _ = log.Into(ctx, logger) }, 1},
		"With":                  {func() { _ = log.With(ctx, "request_id", "r-1") }, 3},
		"WithAttrs":             {func() { _ = log.WithAttrs(ctx, attr) }, 3},
		"Err without fields":    {func() { _ = log.Err(err) }, 5},
		"Err with fields":       {func() { _ = log.Err(withFields) }, 6},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := testing.AllocsPerRun(allocRuns, tc.run); got > tc.want {
				t.Errorf("allocations = %v, want at most %v", got, tc.want)
			}
		})
	}
}
