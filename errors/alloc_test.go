//go:build !race

// The race detector adds its own heap allocations, which makes an exact
// allocation count meaningless under `go test -race`. This file therefore runs
// under `make test-short` and `make bench`, not under `make test`.

package errors_test

import (
	stderrors "errors"
	"testing"

	"github.com/MerseniBilel/warren/errors"
)

// allocRuns is the sample size for each measurement. Large enough that a
// one-off allocation during warm-up cannot round the average up past a budget.
const allocRuns = 100

// TestAllocationBudget enforces the budget in errors/SPEC.md §5.6. An error is
// constructed on the request path, so a regression here is a regression in
// every service built on Warren.
//
//nolint:paralleltest // testing.AllocsPerRun panics if called from a parallel test.
func TestAllocationBudget(t *testing.T) {
	base := errors.NotFound("no order abc")
	cause := stderrors.New("connection refused")
	chain := errors.Internal("outer").Op("a").Wrapping(
		errors.NotFound("inner").Op("b").Wrapping(cause),
	)

	tests := map[string]struct {
		run  func()
		want float64
	}{
		"construction without arguments": {func() { _ = errors.NotFound("no order abc") }, 1},
		"Op":                             {func() { _ = base.Op("orders.Get") }, 2},
		"Field":                          {func() { _ = base.Field("order_id", "abc") }, 2},
		"Fix":                            {func() { _ = base.Fix("create it first") }, 1},
		"Wrapping":                       {func() { _ = base.Wrapping(cause) }, 1},
		"CodeOf on a deep chain":         {func() { _ = errors.CodeOf(chain) }, 0},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := testing.AllocsPerRun(allocRuns, tc.run); got > tc.want {
				t.Errorf("allocations = %v, want at most %v", got, tc.want)
			}
		})
	}
}
