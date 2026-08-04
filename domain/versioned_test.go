package domain_test

import (
	"testing"

	"github.com/MerseniBilel/warren/domain"
)

// invoice is the versioned aggregate: the same shape as order, plus the
// version a repository checks on write.
type invoice struct {
	domain.VersionedRoot[orderID]
}

// A versioned root is still a Root — Repository[T Root[K], K ID] is generic
// over the same constraint, which is what makes the version opt-in rather
// than a second repository interface.
var (
	_ domain.Root[orderID] = (*invoice)(nil)
	_ domain.Versioned     = (*invoice)(nil)
	_ domain.Aggregate     = (*invoice)(nil)
)

func newInvoice(id orderID) *invoice {
	return &invoice{VersionedRoot: domain.NewVersionedRoot(id)}
}

// TestANewAggregateStartsAtVersionZero — zero is what tells a repository the
// row does not exist yet, so it INSERTs rather than issuing an UPDATE whose
// WHERE matches nothing and looks like a conflict.
func TestANewAggregateStartsAtVersionZero(t *testing.T) {
	t.Parallel()

	inv := newInvoice("inv-1")
	if got := inv.Version(); got != 0 {
		t.Fatalf("Version() = %d on a never-persisted aggregate, want 0", got)
	}
	if got := inv.ID(); got != "inv-1" {
		t.Errorf("ID() = %q — the embedded identity must survive", got)
	}
}

// TestReconstituteCarriesTheStoredVersion — the load path is where the
// expected version comes from. A repository that reconstitutes at version 0
// would turn every update into an insert.
func TestReconstituteCarriesTheStoredVersion(t *testing.T) {
	t.Parallel()

	inv := &invoice{VersionedRoot: domain.ReconstituteVersionedRoot[orderID]("inv-1", 7)}
	if got := inv.Version(); got != 7 {
		t.Fatalf("Version() = %d after reconstitution at 7", got)
	}
	if got := inv.ID(); got != "inv-1" {
		t.Errorf("ID() = %q", got)
	}
	// Reconstitution replays history that already happened; it must not look
	// like a fact the caller has yet to publish.
	if evs := inv.PullEvents(); len(evs) != 0 {
		t.Errorf("reconstitution raised %d events, want 0", len(evs))
	}
}

// TestSetVersionIsHowADriverAdvancesIt — after a successful write the
// repository advances the in-memory aggregate, so a second Save in the same
// request checks against the version it just wrote rather than the stale one.
func TestSetVersionIsHowADriverAdvancesIt(t *testing.T) {
	t.Parallel()

	inv := newInvoice("inv-1")
	inv.SetVersion(inv.Version() + 1)
	if got := inv.Version(); got != 1 {
		t.Fatalf("Version() = %d after SetVersion(1)", got)
	}
}

// TestVersionedRootRaisesEventsLikeAnyRoot — VersionedRoot embeds
// AggregateRoot, and the version must not disturb the event machinery the
// unit of work drains.
func TestVersionedRootRaisesEventsLikeAnyRoot(t *testing.T) {
	t.Parallel()

	inv := newInvoice("inv-1")
	inv.Raise(placed{ID: "inv-1"})
	evs := inv.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("PullEvents() returned %d events, want 1", len(evs))
	}
	if got := evs[0].EventName(); got != "order.placed" {
		t.Errorf("EventName() = %q", got)
	}
	if again := inv.PullEvents(); len(again) != 0 {
		t.Errorf("PullEvents() is not draining: %d events on the second call", len(again))
	}
}

// TestAnUnversionedAggregateIsNotVersioned — the interface is the opt-in
// signal every driver switches on. If a plain AggregateRoot satisfied it,
// every aggregate would silently acquire a version column it has no schema
// for.
func TestAnUnversionedAggregateIsNotVersioned(t *testing.T) {
	t.Parallel()

	var a domain.Aggregate = newOrder("ord-1")
	if _, ok := a.(domain.Versioned); ok {
		t.Fatal("a plain AggregateRoot satisfies domain.Versioned — the opt-in is not opt-in")
	}
}
