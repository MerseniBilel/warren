package inbox

import (
	"context"
	"testing"
	"time"
)

// TestSweepReclaimsExpiredRecords is white-box on purpose: eviction is
// invisible through the port, and unbounded growth was the review finding.
func TestSweepReclaimsExpiredRecords(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	s := NewMemoryStore(WithClock(func() time.Time { return now })).(*memoryStore)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		_ = s.MarkSeen(context.Background(), id, time.Hour)
	}
	now = now.Add(2 * time.Hour) // everything above is expired

	// Each write probes a few entries; enough writes reclaim them all.
	for range 20 {
		_ = s.MarkSeen(context.Background(), "fresh", time.Hour)
	}
	s.mu.Lock()
	size := len(s.expiry)
	s.mu.Unlock()
	if size != 1 {
		t.Errorf("store holds %d records, want 1 — expired records must be reclaimed without being re-queried", size)
	}
}
