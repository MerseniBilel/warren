package inbox_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/inbox"
)

func TestMemoryStore(t *testing.T) {
	t.Parallel()

	t.Run("an unseen id is not seen; a marked id is", func(t *testing.T) {
		t.Parallel()
		s := inbox.NewMemoryStore()
		seen, err := s.Seen(context.Background(), "evt-1")
		if err != nil || seen {
			t.Fatalf("Seen(new) = %v, %v — want false, nil", seen, err)
		}
		if err := s.MarkSeen(context.Background(), "evt-1", time.Hour); err != nil {
			t.Fatalf("MarkSeen: %v", err)
		}
		seen, err = s.Seen(context.Background(), "evt-1")
		if err != nil || !seen {
			t.Errorf("Seen(marked) = %v, %v — want true, nil", seen, err)
		}
	})

	t.Run("a record expires after its ttl — clock injected, no sleeps", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
		s := inbox.NewMemoryStore(inbox.WithClock(func() time.Time { return now }))
		if err := s.MarkSeen(context.Background(), "evt-1", time.Hour); err != nil {
			t.Fatalf("MarkSeen: %v", err)
		}
		now = now.Add(59 * time.Minute)
		if seen, _ := s.Seen(context.Background(), "evt-1"); !seen {
			t.Error("record expired before its ttl")
		}
		now = now.Add(2 * time.Minute)
		if seen, _ := s.Seen(context.Background(), "evt-1"); seen {
			t.Error("record survived past its ttl — the dedupe window must close")
		}
	})

	t.Run("re-marking refreshes the ttl", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
		s := inbox.NewMemoryStore(inbox.WithClock(func() time.Time { return now }))
		_ = s.MarkSeen(context.Background(), "evt-1", time.Hour)
		now = now.Add(50 * time.Minute)
		_ = s.MarkSeen(context.Background(), "evt-1", time.Hour)
		now = now.Add(50 * time.Minute)
		if seen, _ := s.Seen(context.Background(), "evt-1"); !seen {
			t.Error("refresh did not extend the window")
		}
	})

	t.Run("safe under concurrent use", func(t *testing.T) {
		t.Parallel()
		s := inbox.NewMemoryStore()
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for i := range 100 {
					id := string(rune('a' + i%26))
					_, _ = s.Seen(context.Background(), id)
					_ = s.MarkSeen(context.Background(), id, time.Minute)
				}
			})
		}
		wg.Wait()
	})
}

// TestMemoryStoreIsBounded — the sweep can only reclaim records that have
// already expired, so with the 24h default TTL the live set is throughput ×
// 24h. At a thousand messages a second that is ~86 million map entries: an
// OOM in someone's production, from a middleware that is on by default.
func TestMemoryStoreIsBounded(t *testing.T) {
	t.Parallel()

	store := inbox.NewMemoryStore(inbox.WithMaxRecords(100))
	ctx := context.Background()

	for i := range 1000 {
		if err := store.MarkSeen(ctx, strconv.Itoa(i), time.Hour); err != nil {
			t.Fatalf("MarkSeen: %v", err)
		}
	}
	if n := inbox.Len(store); n > 100 {
		t.Errorf("the store holds %d records with a cap of 100", n)
	}

	// The most recent marks survive: eviction drops the oldest, so an
	// immediate redelivery — the case dedupe exists for — is still caught.
	seen, err := store.Seen(ctx, "999")
	if err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if !seen {
		t.Error("the most recently marked record was evicted first")
	}
}

// TestMemoryStoreDefaultIsBounded — the default must be finite too, or the
// fix only helps people who already knew about it.
func TestMemoryStoreDefaultIsBounded(t *testing.T) {
	t.Parallel()

	store := inbox.NewMemoryStore()
	ctx := context.Background()
	for i := range 200_001 {
		if err := store.MarkSeen(ctx, strconv.Itoa(i), time.Hour); err != nil {
			t.Fatalf("MarkSeen: %v", err)
		}
	}
	if n := inbox.Len(store); n > 200_000 {
		t.Errorf("the default store grew to %d records", n)
	}
}

// BenchmarkMarkSeenOverCapacity is the reason eviction batches. Dropping one
// record per write costs a full scan of the map, so sustained overflow would
// be O(max) per message.
func BenchmarkMarkSeenOverCapacity(b *testing.B) {
	store := inbox.NewMemoryStore(inbox.WithMaxRecords(10_000))
	ctx := context.Background()
	for i := range 10_000 {
		_ = store.MarkSeen(ctx, strconv.Itoa(i), time.Hour)
	}
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		i++
		_ = store.MarkSeen(ctx, strconv.Itoa(1_000_000+i), time.Hour)
	}
}
