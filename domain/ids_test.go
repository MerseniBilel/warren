package domain_test

import (
	"testing"

	"github.com/MerseniBilel/warren/domain"
)

// TestUUIDsAreTimeOrdered is the whole reason UUIDs returns v7 rather than v4.
// A v4 id sorts randomly, so a primary-key index writes into the middle of
// the tree and fragments; a v7 id appends. If this ever regresses to v4 the
// change is invisible in every other test.
func TestUUIDsAreTimeOrdered(t *testing.T) {
	t.Parallel()

	ids := domain.UUIDs()
	prev := ids()
	for range 100 {
		next := ids()
		if next <= prev {
			t.Fatalf("ids are not ascending: %q then %q — v7 encodes a millisecond timestamp in its leading bits", prev, next)
		}
		prev = next
	}
}

func TestUUIDsAreDistinct(t *testing.T) {
	t.Parallel()

	ids := domain.UUIDs()
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := ids()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
