package inbox_test

import (
	"context"
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
