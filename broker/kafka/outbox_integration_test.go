//go:build integration

package kafka_test

import (
	"context"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/outbox"
)

// TestRelayParksOnlyTheRejectedRecord runs the relay against a REAL broker
// with one record the broker will refuse.
//
// The unit test in warren/outbox models this with a double. This is the
// measurement it was modelled on: a five-record batch with one 2 MiB message
// returns MESSAGE_TOO_LARGE and leaves FOUR of the five in the topic, so
// parking the batch's head parks records the broker already delivered.
func TestRelayParksOnlyTheRejectedRecord(t *testing.T) {
	addr := brokers(t)
	topic := freshTopic(t, addr, "warren-relay")
	pub, _ := newProbeBroker(t, addr)

	store := &countingStore{Store: outbox.NewMemoryStore()}
	ctx := context.Background()
	small := []byte(`{"ok":true}`)
	huge := make([]byte, 2<<20) // over the broker's 1 MiB default
	for i := range huge {
		huge[i] = 'x'
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		payload := small
		if id == "c" {
			payload = huge
		}
		if err := store.Append(ctx, outbox.Record{
			Topic:   topic,
			Message: broker.Message{ID: id, Type: "probe", Key: "key-" + id, Payload: payload},
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	relay := outbox.NewRelay(store, pub)
	for range 8 {
		n, _ := relay.DrainOnce(ctx)
		pending, _ := store.Pending(ctx, 10)
		if n == 0 && len(pending) == 0 {
			break
		}
	}

	store.mu.Lock()
	parked := append([]string(nil), store.parked...)
	published := append([]string(nil), store.published...)
	store.mu.Unlock()

	if len(parked) != 1 || parked[0] != "c" {
		t.Errorf("parked %v, want only [c] — the broker accepted every other record", parked)
	}
	for _, id := range []string{"a", "b", "d", "e"} {
		if !contains(published, id) {
			t.Errorf("%s was accepted by the broker but never marked published; published=%v parked=%v",
				id, published, parked)
		}
	}
	if pending, _ := store.Pending(ctx, 10); len(pending) != 0 {
		t.Errorf("%d record(s) still pending, want none", len(pending))
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// countingStore reports what was parked and what was marked published;
// neither is visible through the Store interface.
type countingStore struct {
	outbox.Store
	mu        sync.Mutex
	parked    []string
	published []string
}

func (s *countingStore) MarkFailed(ctx context.Context, id string, cause error) error {
	s.mu.Lock()
	s.parked = append(s.parked, id)
	s.mu.Unlock()
	return s.Store.MarkFailed(ctx, id, cause)
}

func (s *countingStore) MarkPublished(ctx context.Context, ids ...string) error {
	s.mu.Lock()
	s.published = append(s.published, ids...)
	s.mu.Unlock()
	return s.Store.MarkPublished(ctx, ids...)
}
