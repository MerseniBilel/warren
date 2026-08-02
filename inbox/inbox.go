// Package inbox defines the dedupe-store port the consumer chain's
// Deduplicate stage records processed Message.IDs in, and ships the
// stdlib-only memory store that makes dedupe-by-default cost neither Docker
// nor a database. Durable stores arrive with the persistence adapters
// (Postgres joining the unit of work, Redis with native TTLs); this package
// never imports them.
//
// The protocol Deduplicate executes — Seen before the handler, MarkSeen only
// after success, fail closed on store errors — is the broker package's;
// this package only answers "have I recorded this key".
//
// What that buys is the SUPPRESSION OF REDELIVERY of a message already
// disposed of. It is not exactly-once. Seen and MarkSeen are separate calls
// and nothing claims, so the same ID delivered to two workers concurrently
// runs the handler twice; at-least-once remains the guarantee. That is
// deliberate: an atomic claim-and-release would trade a rare duplicate for
// a rare loss, because a crash between claim and release suppresses the
// redelivery and the message is gone.
//
// Keys are scoped by subscription — broker.Pipeline builds them — so two
// features consuming one topic through one store do not suppress each
// other.
package inbox

import (
	"context"
	"slices"
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

// WithMaxRecords bounds how many records the store holds. Past the cap it
// evicts the earliest deadline first. Zero or less means unbounded, which is
// only ever right for a test that asserts on growth.
func WithMaxRecords(n int) MemoryOption {
	return func(s *memoryStore) { s.max = n }
}

// Len reports how many records a memory store holds. It exists for the
// tests that assert the bound holds; a Store from another adapter reports 0.
func Len(s Store) int {
	m, ok := s.(*memoryStore)
	if !ok {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.expiry)
}

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
//
// Expired records are reclaimed lazily on their own reads and by an
// incremental sweep on every write (a few random probes, Redis-style), so
// memory tracks the live TTL window rather than everything ever consumed.
//
// The sweep can only reclaim what has ALREADY expired, so the live set is
// throughput times TTL — with the 24h default that is tens of millions of
// entries at a modest rate. The store is therefore bounded: past
// WithMaxRecords it evicts the oldest deadline first, so an immediate
// redelivery, which is what deduplication exists to catch, is still caught
// while an unbounded map is not left in a default-on middleware.
//
// It remains a dev/test-grade store: sustained high-volume production use
// wants a durable adapter, and eviction means a bounded store trades
// certainty for memory — a redelivery arriving after eviction runs the
// handler again.
func NewMemoryStore(opts ...MemoryOption) Store {
	s := &memoryStore{expiry: map[string]time.Time{}, now: time.Now, max: defaultMaxRecords}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// defaultMaxRecords bounds the in-memory store. At 200k entries the map
// costs a few tens of megabytes — large enough that a dev or test workload
// never reaches it, small enough that a production misconfiguration is a
// slow handler rather than an OOM.
const defaultMaxRecords = 200_000

type memoryStore struct {
	mu     sync.Mutex
	expiry map[string]time.Time
	now    func() time.Time
	max    int
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
	now := s.now()
	s.expiry[id] = now.Add(ttl)
	// Incremental sweep: probe a few entries per write (map iteration order
	// is randomized) so expired records are reclaimed even when never
	// re-queried.
	probed := 0
	for k, deadline := range s.expiry {
		if probed >= 4 {
			break
		}
		probed++
		if now.After(deadline) {
			delete(s.expiry, k)
		}
	}
	s.evict()
	return nil
}

// evict drops the earliest deadlines until the store is inside its bound.
//
// Oldest-first is the right order: the redelivery deduplication exists to
// catch arrives within seconds, so the youngest records carry the value.
//
// It evicts down to 90% of the cap IN ONE PASS rather than one record at a
// time. Dropping a single oldest entry costs a full scan, so under
// sustained overflow — every write past the cap evicting one — that would
// be O(max) per message: 200,000 comparisons each, at whatever rate the
// topic delivers. Batching makes it O(n log n) once every max/10 writes,
// which is amortised O(log n) per write.
func (s *memoryStore) evict() {
	if s.max <= 0 || len(s.expiry) <= s.max {
		return
	}
	target := s.max - s.max/10
	if target < 1 {
		target = 1
	}

	deadlines := make([]time.Time, 0, len(s.expiry))
	for _, deadline := range s.expiry {
		deadlines = append(deadlines, deadline)
	}
	slices.SortFunc(deadlines, func(a, b time.Time) int { return a.Compare(b) })

	// Everything at or before this deadline goes. Ties can carry the count
	// below the target, which is harmless — the bound is a ceiling.
	cutoff := deadlines[len(deadlines)-target]
	for k, deadline := range s.expiry {
		if !deadline.After(cutoff) {
			delete(s.expiry, k)
		}
	}
}
