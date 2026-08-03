package inboxtest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/inbox"
)

// A contract suite nobody has watched fail proves nothing: every check here
// would also pass against a store that does the right thing by accident.
// So each broken store below violates ONE clause, and the test asserts that
// the check owning that clause is the one that catches it.
//
// The stores are written the way a driver author would get it wrong — DO
// NOTHING instead of DO UPDATE, an upsert on read, a truncated index
// column — not as arbitrary sabotage.

func TestEachCheckCatchesItsViolation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		check string
		store func() inbox.Store
		want  string // a distinctive fragment of the diagnostic
	}{
		{"mark then seen", func() inbox.Store { return &forgetfulStore{} }, "every redelivery would run the handler again"},
		{"Seen does not record", func() inbox.Store { return newRecordOnReadStore() }, "records on read"},
		{"ids are independent", func() inbox.Store { return &oneFlagStore{} }, "must not answer for another"},
		{"keys are opaque", func() inbox.Store { return newTruncatingStore(32) }, "collides distinct keys"},
		{"keys are opaque", func() inbox.Store { return newFoldingStore() }, "collides distinct keys"},
		{"a record expires", func() inbox.Store { return newNeverExpiresStore() }, "ignores the ttl argument"},
		{"re-marking extends the window", func() inbox.Store { return newDoNothingStore() }, "ON CONFLICT … DO UPDATE"},
		{"re-marking shortens the window", func() inbox.Store { return newLatestWinsStore() }, "does not take the later of the two"},
		{"marking twice is not an error", func() inbox.Store { return newConflictStore() }, "already recorded is normal"},
	}

	for _, c := range cases {
		t.Run(c.check+"/"+strings.Fields(c.want)[0], func(t *testing.T) {
			t.Parallel()
			fn := checkNamed(t, c.check)
			err := fn(t, c.store())
			if err == nil {
				t.Fatalf("check %q passed a store that violates it — the check does not test what it claims", c.check)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("check %q failed for the wrong reason:\n  got:  %v\n  want a message containing: %q", c.check, err, c.want)
			}
		})
	}
}

// The positive half — a correct store passing every check — is
// inbox.TestContract, which runs the suite the way a driver does.

// TestEveryCheckIsRegistered guards the list against a check written and
// never wired in — the failure mode that makes a suite quietly shrink.
func TestEveryCheckIsRegistered(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, c := range checks {
		if c.fn == nil {
			t.Errorf("check %q has no function", c.name)
		}
		if seen[c.name] {
			t.Errorf("two checks are named %q — the subtest names would collide", c.name)
		}
		seen[c.name] = true
	}
	if len(checks) < 10 {
		t.Errorf("the suite has %d checks; it had 10 when this test was written — a check was deleted, not added", len(checks))
	}
}

func checkNamed(t *testing.T, name string) func(*testing.T, inbox.Store) error {
	t.Helper()
	for _, c := range checks {
		if c.name == name {
			return c.fn
		}
	}
	t.Fatalf("no check named %q", name)
	return nil
}

// --- the broken stores ---------------------------------------------------

// base is the correct implementation the broken stores deviate from, so
// each one differs by exactly the clause it violates.
type base struct {
	mu     sync.Mutex
	expiry map[string]time.Time
}

func newBase() base { return base{expiry: map[string]time.Time{}} }

func (b *base) seen(key string) bool {
	deadline, ok := b.expiry[key]
	return ok && time.Now().Before(deadline)
}

// forgetfulStore never records anything: the MarkSeen that a driver wired
// to the wrong table, or swallowed the error from.
type forgetfulStore struct{}

func (*forgetfulStore) Seen(context.Context, string) (bool, error) { return false, nil }
func (*forgetfulStore) MarkSeen(context.Context, string, time.Duration) error {
	return nil
}

// recordOnReadStore upserts inside Seen — the "one round trip instead of
// two" optimisation that makes every first delivery its own duplicate.
type recordOnReadStore struct{ base }

func newRecordOnReadStore() *recordOnReadStore { return &recordOnReadStore{newBase()} }

func (s *recordOnReadStore) Seen(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.seen(key)
	s.expiry[key] = time.Now().Add(longTTL)
	return was, nil
}

func (s *recordOnReadStore) MarkSeen(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiry[key] = time.Now().Add(ttl)
	return nil
}

// oneFlagStore keeps a single "have I seen anything" flag — the store
// written against a per-subscription cursor rather than a per-id record.
type oneFlagStore struct {
	mu  sync.Mutex
	any bool
}

func (s *oneFlagStore) Seen(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.any, nil
}

func (s *oneFlagStore) MarkSeen(context.Context, string, time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.any = true
	return nil
}

// truncatingStore cuts the key to n bytes — a VARCHAR(n) primary key, or
// MySQL's 255-byte utf8mb4 index prefix.
type truncatingStore struct {
	base
	n int
}

func newTruncatingStore(n int) *truncatingStore { return &truncatingStore{newBase(), n} }

func (s *truncatingStore) cut(key string) string {
	if len(key) <= s.n {
		return key
	}
	return key[:s.n]
}

func (s *truncatingStore) Seen(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen(s.cut(key)), nil
}

func (s *truncatingStore) MarkSeen(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiry[s.cut(key)] = time.Now().Add(ttl)
	return nil
}

// foldingStore lower-cases the key — a MySQL column on a *_ci collation,
// which is the default.
type foldingStore struct{ base }

func newFoldingStore() *foldingStore { return &foldingStore{newBase()} }

func (s *foldingStore) Seen(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen(strings.ToLower(key)), nil
}

func (s *foldingStore) MarkSeen(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiry[strings.ToLower(key)] = time.Now().Add(ttl)
	return nil
}

// neverExpiresStore ignores the TTL — the table with no expiry column and
// no sweeper, which is correct until it is enormous and wrong the first
// time an id is legitimately redelivered a day later.
type neverExpiresStore struct{ base }

func newNeverExpiresStore() *neverExpiresStore { return &neverExpiresStore{newBase()} }

func (s *neverExpiresStore) Seen(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.expiry[key]
	return ok, nil
}

func (s *neverExpiresStore) MarkSeen(_ context.Context, key string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiry[key] = time.Time{}
	return nil
}

// doNothingStore keeps the first deadline: INSERT … ON CONFLICT DO NOTHING.
type doNothingStore struct{ base }

func newDoNothingStore() *doNothingStore { return &doNothingStore{newBase()} }

func (s *doNothingStore) Seen(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen(key), nil
}

func (s *doNothingStore) MarkSeen(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.expiry[key]; !ok {
		s.expiry[key] = time.Now().Add(ttl)
	}
	return nil
}

// latestWinsStore takes the later of the two deadlines — GREATEST(…) in the
// DO UPDATE, which passes "extends" and fails "shortens".
type latestWinsStore struct{ base }

func newLatestWinsStore() *latestWinsStore { return &latestWinsStore{newBase()} }

func (s *latestWinsStore) Seen(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen(key), nil
}

func (s *latestWinsStore) MarkSeen(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := time.Now().Add(ttl)
	if cur, ok := s.expiry[key]; !ok || next.After(cur) {
		s.expiry[key] = next
	}
	return nil
}

// conflictStore errors on a duplicate key — a bare INSERT with no ON
// CONFLICT clause, which turns an ordinary re-mark into a nack.
type conflictStore struct{ base }

func newConflictStore() *conflictStore { return &conflictStore{newBase()} }

func (s *conflictStore) Seen(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen(key), nil
}

func (s *conflictStore) MarkSeen(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.expiry[key]; ok {
		return errDuplicateKey
	}
	s.expiry[key] = time.Now().Add(ttl)
	return nil
}

var errDuplicateKey = duplicateKeyError{}

type duplicateKeyError struct{}

func (duplicateKeyError) Error() string {
	return `ERROR: duplicate key value violates unique constraint "inbox_pkey" (SQLSTATE 23505)`
}
