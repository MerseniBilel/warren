// Package inbox defines the dedupe-store port the consumer chain's
// Deduplicate stage records processed Message.IDs in, and ships the
// stdlib-only memory store that makes dedupe-by-default cost neither Docker
// nor a database. Durable stores arrive with the persistence adapters
// (Postgres joining the unit of work, Redis with native TTLs); this package
// never imports them.
//
// The protocol Deduplicate executes — Seen before the handler, MarkSeen only
// after success, fail closed on store errors — is the broker package's;
// this package only answers "have I recorded this id".
package inbox

import (
	"context"
	"sync"
	"time"
)

// Store records which Message.IDs a subscription has fully processed.
// Implementations must be safe for concurrent use.
type Store interface {
	// Seen reports whether id has been recorded and not yet expired.
	Seen(ctx context.Context, id string) (bool, error)

	// MarkSeen records id, expiring after ttl. Marking an existing id
	// refreshes its window.
	MarkSeen(ctx context.Context, id string, ttl time.Duration) error
}

// MemoryOption configures NewMemoryStore.
type MemoryOption func(*memoryStore)

// WithClock injects the time source — how tests drive expiry without
// sleeping. The default is time.Now.
func WithClock(now func() time.Time) MemoryOption {
	return func(s *memoryStore) { s.now = now }
}

// NewMemoryStore returns an in-process Store: a map, a mutex, and a clock.
// It is the default store when no other is provided — dedupe is on by
// default because at-least-once delivery makes duplicates certain, not
// hypothetical. Its records do not survive a restart; a durable store is the
// persistence adapters' business.
func NewMemoryStore(opts ...MemoryOption) Store {
	s := &memoryStore{expiry: map[string]time.Time{}, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type memoryStore struct {
	mu     sync.Mutex
	expiry map[string]time.Time
	now    func() time.Time
}

func (s *memoryStore) Seen(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, ok := s.expiry[id]
	if !ok {
		return false, nil
	}
	if s.now().After(deadline) {
		delete(s.expiry, id)
		return false, nil
	}
	return true, nil
}

func (s *memoryStore) MarkSeen(_ context.Context, id string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiry[id] = s.now().Add(ttl)
	return nil
}
