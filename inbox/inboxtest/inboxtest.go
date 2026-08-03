// Package inboxtest is the contract suite every inbox.Store must pass: the
// in-process one, and the durable stores that arrive with the persistence
// adapters. It is written BEFORE the second store on purpose — otherwise
// Postgres and Redis each re-derive the semantics from the doc comment, and
// the two that catch people are silent:
//
//   - Re-marking an id REFRESHES its window, in both directions. Redis
//     `SET … EX` does it natively; Postgres needs an explicit
//     `ON CONFLICT … DO UPDATE`, and the store that forgets it expires a
//     hot id in the middle of its own redelivery window.
//   - `Seen` must not RECORD. A store that upserts on read marks every id
//     it is asked about, so the first delivery of a message is treated as
//     its own duplicate and the handler never runs.
//
// A driver runs the suite in one line:
//
//	func TestContract(t *testing.T) {
//	    inboxtest.Run(t, func(t *testing.T) inbox.Store {
//	        return inbox.NewMemoryStore()
//	    })
//	}
//
// # What a key is
//
// A key is an opaque, case-sensitive string. A store must not truncate,
// normalise, lower-case, or otherwise collide distinct keys — the suite
// passes quotes, per-cent and underscore (SQL LIKE wildcards), unicode, and
// a key longer than any index prefix, because every one of those has broken
// a real dedupe table.
//
// A store MAY assume a key holds no NUL byte: Postgres `text` rejects 0x00
// outright, so a key containing one would be unstorable there through no
// fault of the driver. Warren's own key builder upholds that — broker's
// scope separator is U+001F and RequireMessageID refuses an id carrying a
// NUL — so the assumption is a guarantee, not a hope.
//
// # Time
//
// The expiry checks use real time with a short TTL, because a durable store
// reads its server's clock and cannot be handed a fake one. They cost a few
// hundred milliseconds and no sleep longer than the TTL under test.
package inboxtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/inbox"
)

// NewStore builds a fresh, empty Store for one subtest. Drivers clean up
// with t.Cleanup; the suite never shares a store between checks, so a
// driver may return a per-test schema, database, or key prefix.
type NewStore func(t *testing.T) inbox.Store

// shortTTL is the window the expiry checks mark with. It is long enough
// that a store doing real I/O writes and reads inside it, and short enough
// that the suite stays under a second.
const shortTTL = 150 * time.Millisecond

// longTTL is the window a refresh check extends to — far enough past
// shortTTL that no scheduling delay can be mistaken for an expiry.
const longTTL = time.Minute

// Run executes the whole contract suite against the driver.
func Run(t *testing.T, newStore NewStore) {
	t.Helper()
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if err := c.fn(t, newStore(t)); err != nil {
				t.Error(err)
			}
		})
	}
}

// check is one contract property. It returns an error rather than failing
// the test directly so that this package's own tests can point a knowingly
// broken store at each property and assert the property CATCHES it — a
// suite nobody has watched fail is a suite that proves nothing.
type check struct {
	name string
	fn   func(t *testing.T, s inbox.Store) error
}

var checks = []check{
	{"an unrecorded id is not seen", checkUnseen},
	{"mark then seen", checkMarkThenSeen},
	{"Seen does not record", checkSeenDoesNotRecord},
	{"ids are independent", checkIndependent},
	{"keys are opaque", checkOpaqueKeys},
	{"a record expires", checkExpires},
	{"re-marking extends the window", checkRefreshExtends},
	{"re-marking shortens the window", checkRefreshShortens},
	{"marking twice is not an error", checkMarkTwice},
	{"concurrent use is safe", checkConcurrent},
}

func checkUnseen(t *testing.T, s inbox.Store) error {
	seen, err := s.Seen(t.Context(), "never-marked")
	if err != nil {
		return fmt.Errorf("Seen on an empty store: %w", err)
	}
	if seen {
		return fmt.Errorf("Seen(%q) = true on an empty store, want false", "never-marked")
	}
	return nil
}

func checkMarkThenSeen(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	if err := s.MarkSeen(ctx, "id-1", longTTL); err != nil {
		return fmt.Errorf("MarkSeen: %w", err)
	}
	seen, err := s.Seen(ctx, "id-1")
	if err != nil {
		return fmt.Errorf("Seen after MarkSeen: %w", err)
	}
	if !seen {
		return fmt.Errorf("Seen after MarkSeen = false, want true — every redelivery would run the handler again")
	}
	return nil
}

// checkSeenDoesNotRecord is the property a store that upserts on read
// breaks. The consumer chain calls Seen on the FIRST delivery too; a store
// that records what it is asked about reports every message as its own
// duplicate, and the handler never runs at all.
func checkSeenDoesNotRecord(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	for i := range 2 {
		seen, err := s.Seen(ctx, "asked-about")
		if err != nil {
			return fmt.Errorf("Seen call %d: %w", i+1, err)
		}
		if seen {
			return fmt.Errorf("Seen(%q) = true on call %d without any MarkSeen — the store records on read, so a first delivery is treated as its own duplicate and the handler never runs", "asked-about", i+1)
		}
	}
	return nil
}

func checkIndependent(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	if err := s.MarkSeen(ctx, "marked", longTTL); err != nil {
		return fmt.Errorf("MarkSeen: %w", err)
	}
	seen, err := s.Seen(ctx, "unmarked")
	if err != nil {
		return fmt.Errorf("Seen: %w", err)
	}
	if seen {
		return fmt.Errorf("marking %q made %q seen — one id's record must not answer for another", "marked", "unmarked")
	}
	// The empty key is an ordinary key, not a wildcard. A store that files
	// it as NULL, or that treats "" as "any", suppresses every message.
	if err := s.MarkSeen(ctx, "", longTTL); err != nil {
		return fmt.Errorf("MarkSeen with an empty key: %w", err)
	}
	seen, err = s.Seen(ctx, "still-unmarked")
	if err != nil {
		return fmt.Errorf("Seen after marking the empty key: %w", err)
	}
	if seen {
		return fmt.Errorf("marking the empty key made %q seen — the empty key is an ordinary key, not a wildcard", "still-unmarked")
	}
	return nil
}

// opaqueKeys are the shapes that have broken a real dedupe table: SQL LIKE
// wildcards, quote characters, unicode, case-only differences, the scope
// separator broker joins with, and a key past the 255-byte index prefix
// MySQL's utf8mb4 default imposes.
var opaqueKeys = []string{
	"plain",
	"with space",
	"with'quote",
	`with"double`,
	"with%percent",
	"with_underscore",
	"with-dash.and.dots",
	"with/slash\\backslash",
	"unicode-héllo-世界-🐇",
	"orders\x1fevt-1", // the scope separator broker joins with
	"CaseSensitive",
	"casesensitive",
	strings.Repeat("long", 200),
}

func checkOpaqueKeys(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	// Marked one at a time, each checked unseen FIRST: marking the whole
	// set and then reading it back cannot see a collision, because both
	// halves of a colliding pair get marked. A case-folding store passes
	// that version and fails this one.
	for i, k := range opaqueKeys {
		seen, err := s.Seen(ctx, k)
		if err != nil {
			return fmt.Errorf("Seen(%q): %w", short(k), err)
		}
		if seen {
			return fmt.Errorf("Seen(%q) = true before it was ever marked, after marking %d other distinct keys — the store collides distinct keys (folded case? normalised unicode? truncated?)", short(k), i)
		}
		if err := s.MarkSeen(ctx, k, longTTL); err != nil {
			return fmt.Errorf("MarkSeen(%q): %w", short(k), err)
		}
		if seen, err = s.Seen(ctx, k); err != nil {
			return fmt.Errorf("Seen(%q) after MarkSeen: %w", short(k), err)
		} else if !seen {
			return fmt.Errorf("Seen(%q) = false after MarkSeen — the store altered, truncated or normalised the key", short(k))
		}
	}
	// Every key above is distinct, so a near-miss of each must still be
	// unseen: that is what catches truncation and case folding.
	for _, k := range opaqueKeys {
		near := k + "x"
		seen, err := s.Seen(ctx, near)
		if err != nil {
			return fmt.Errorf("Seen(%q): %w", short(near), err)
		}
		if seen {
			return fmt.Errorf("Seen(%q) = true, but only %q was marked — the store collides distinct keys", short(near), short(k))
		}
	}
	return nil
}

func checkExpires(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	if err := s.MarkSeen(ctx, "expiring", shortTTL); err != nil {
		return fmt.Errorf("MarkSeen: %w", err)
	}
	seen, err := s.Seen(ctx, "expiring")
	if err != nil {
		return fmt.Errorf("Seen: %w", err)
	}
	if !seen {
		return fmt.Errorf("Seen = false immediately after MarkSeen with a %s TTL", shortTTL)
	}
	return awaitGone(ctx, s, "expiring", "a record must not outlive its TTL — the store ignores the ttl argument")
}

// checkRefreshExtends is the trap warren.md §5.6 names: a store whose write
// is an INSERT … ON CONFLICT DO NOTHING keeps the FIRST deadline, so a hot
// id expires mid-redelivery-window even though it is being re-marked.
func checkRefreshExtends(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	if err := s.MarkSeen(ctx, "refreshed", shortTTL); err != nil {
		return fmt.Errorf("MarkSeen with the short TTL: %w", err)
	}
	if err := s.MarkSeen(ctx, "refreshed", longTTL); err != nil {
		return fmt.Errorf("MarkSeen with the long TTL: %w", err)
	}
	// Well past the first window, comfortably inside the second.
	time.Sleep(3 * shortTTL)
	seen, err := s.Seen(ctx, "refreshed")
	if err != nil {
		return fmt.Errorf("Seen: %w", err)
	}
	if !seen {
		return fmt.Errorf("re-marking with a longer TTL did not extend the window: the id expired on its FIRST deadline. Re-marking must refresh — Postgres needs ON CONFLICT … DO UPDATE, not DO NOTHING")
	}
	return nil
}

// checkRefreshShortens is the same property from the other side. A store
// that keeps whichever deadline is later still passes checkRefreshExtends,
// so without this one "refreshes" and "extends only" are indistinguishable.
func checkRefreshShortens(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	if err := s.MarkSeen(ctx, "shortened", longTTL); err != nil {
		return fmt.Errorf("MarkSeen with the long TTL: %w", err)
	}
	if err := s.MarkSeen(ctx, "shortened", shortTTL); err != nil {
		return fmt.Errorf("MarkSeen with the short TTL: %w", err)
	}
	return awaitGone(ctx, s, "shortened", "re-marking with a shorter TTL did not shorten the window — MarkSeen sets the window, it does not take the later of the two")
}

func checkMarkTwice(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	for i := range 3 {
		if err := s.MarkSeen(ctx, "repeated", longTTL); err != nil {
			return fmt.Errorf("MarkSeen call %d: %w — marking an id already recorded is normal, not a conflict", i+1, err)
		}
	}
	seen, err := s.Seen(ctx, "repeated")
	if err != nil {
		return fmt.Errorf("Seen: %w", err)
	}
	if !seen {
		return fmt.Errorf("Seen = false after three MarkSeen calls")
	}
	return nil
}

// checkConcurrent is what -race turns into an assertion. The consumer chain
// runs this store from every worker of every subscription at once.
func checkConcurrent(t *testing.T, s inbox.Store) error {
	ctx := t.Context()
	const workers, each = 8, 25

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				key := fmt.Sprintf("w%d-%d", w, i)
				if _, err := s.Seen(ctx, key); err != nil {
					errs <- fmt.Errorf("concurrent Seen: %w", err)
					return
				}
				if err := s.MarkSeen(ctx, key, longTTL); err != nil {
					errs <- fmt.Errorf("concurrent MarkSeen: %w", err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}

	// Every key written concurrently must still be there: a store whose
	// writes race loses records, and a lost record is a duplicate delivery.
	for w := range workers {
		for i := range each {
			key := fmt.Sprintf("w%d-%d", w, i)
			seen, err := s.Seen(ctx, key)
			if err != nil {
				return fmt.Errorf("Seen(%q) after the concurrent run: %w", key, err)
			}
			if !seen {
				return fmt.Errorf("Seen(%q) = false after the concurrent run — a concurrent MarkSeen was lost", key)
			}
		}
	}
	return nil
}

// awaitGone polls until key stops being seen, so a store whose expiry is
// second-granular or swept in the background is not failed for latency —
// only for never expiring at all.
func awaitGone(ctx context.Context, s inbox.Store, key, why string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		seen, err := s.Seen(ctx, key)
		if err != nil {
			return fmt.Errorf("Seen while waiting for expiry: %w", err)
		}
		if !seen {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %q was still seen 5s after a %s TTL", why, key, shortTTL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// short keeps a failure message readable when the key under test is the
// 800-byte one.
func short(k string) string {
	if len(k) <= 40 {
		return k
	}
	return k[:40] + "…"
}
