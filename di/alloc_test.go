//go:build !race

// The race detector adds its own heap allocations, which makes an exact
// allocation count meaningless under `go test -race`. This file therefore runs
// under `make test-short` and `make bench`, not under `make test`.

package di_test

import (
	"database/sql"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/di/internal/orders"
)

// allocRuns is the sample size for each measurement. Large enough that a one-off
// allocation during warm-up cannot round the average up past a budget.
const allocRuns = 100

// TestAllocationBudget enforces the budget in di/SPEC.md §5.6. Resolution is the
// only part of this package a request can reach, so it is the only part with a
// budget — and after Build the instance map is never written again, which is what
// makes zero achievable.
//
//nolint:paralleltest // testing.AllocsPerRun panics if called from a parallel test.
func TestAllocationBudget(t *testing.T) {
	c := di.New()
	di.Provide[*sql.DB](c, newDB)
	di.Provide[*orders.Repository](c, orders.NewRepository)
	di.Provide[reader](c, newTagged)
	di.Contribute[string](c, func() string { return "a" })
	di.Contribute[string](c, func() string { return "b" })

	mustBuild(t, c)

	var (
		repo *orders.Repository
		port reader
		tags []string
	)

	tests := map[string]struct {
		run  func()
		want float64
	}{
		"Resolve of a pointer":    {func() { _ = di.Resolve(c, &repo) }, 0},
		"Resolve of an interface": {func() { _ = di.Resolve(c, &port) }, 0},
		"Group":                   {func() { _ = di.Group(c, &tags) }, 0},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := testing.AllocsPerRun(allocRuns, tc.run); got > tc.want {
				t.Errorf("allocations = %v, want at most %v", got, tc.want)
			}
		})
	}
}
