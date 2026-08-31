package persistence_test

import (
	"context"
	stderrors "errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/domain"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/persistence"
)

// --- fixture aggregate -----------------------------------------------------

type orderID string

func (id orderID) String() string { return string(id) }

type placed struct {
	ID orderID
	At time.Time
}

func (p placed) EventName() string     { return "order.placed" }
func (p placed) OccurredAt() time.Time { return p.At }
func (p placed) AggregateID() string   { return p.ID.String() }

type order struct {
	domain.AggregateRoot[orderID]
	Total int
}

func newOrder(id orderID, total int) *order {
	o := &order{AggregateRoot: domain.NewAggregateRoot(id), Total: total}
	o.Raise(placed{ID: id, At: time.Unix(1, 0)})
	return o
}

var _ domain.Aggregate = (*order)(nil)

// payment is the versioned fixture: the same aggregate opted into optimistic
// concurrency by embedding VersionedRoot instead of AggregateRoot.
type payment struct {
	domain.VersionedRoot[orderID]
	Total int
}

func newPayment(id orderID, total int) *payment {
	p := &payment{VersionedRoot: domain.NewVersionedRoot(id), Total: total}
	p.Raise(placed{ID: id, At: time.Unix(1, 0)})
	return p
}

var _ domain.Versioned = (*payment)(nil)

func TestTrackAndCollect(t *testing.T) {
	t.Parallel()

	t.Run("collect drains the events of every enlisted aggregate, in order", func(t *testing.T) {
		t.Parallel()
		ctx, drain := persistence.Collect(context.Background())
		a, b := newOrder("a", 1), newOrder("b", 2)
		persistence.Track(ctx, a)
		persistence.Track(ctx, b)

		events := drain()
		if len(events) != 2 {
			t.Fatalf("drained %d events, want 2", len(events))
		}
		if events[0].AggregateID() != "a" || events[1].AggregateID() != "b" {
			t.Errorf("order = %v, want enlistment order", events)
		}
		// Draining is destructive on the aggregates: a second drain is empty,
		// so no fact is published twice.
		if again := drain(); len(again) != 0 {
			t.Errorf("second drain returned %d events, want 0", len(again))
		}
	})

	t.Run("track outside a unit of work is a no-op that loses nothing", func(t *testing.T) {
		t.Parallel()
		o := newOrder("a", 1)
		persistence.Track(context.Background(), o) // no collector on ctx
		if len(o.PullEvents()) != 1 {
			t.Error("the aggregate's events were consumed outside a transaction — a later Do must still publish them")
		}
	})

	t.Run("nested collect returns the same context and drains only once", func(t *testing.T) {
		t.Parallel()
		outer, outerDrain := persistence.Collect(context.Background())
		inner, innerDrain := persistence.Collect(outer)
		if inner != outer {
			t.Error("a nested Collect built a second collector — only the outermost Do drains")
		}
		persistence.Track(inner, newOrder("a", 1))
		if got := innerDrain(); len(got) != 0 {
			t.Errorf("the inner drain returned %d events, want 0", len(got))
		}
		if got := outerDrain(); len(got) != 1 {
			t.Errorf("the outer drain returned %d events, want 1", len(got))
		}
	})

	t.Run("InTransaction reports the scope", func(t *testing.T) {
		t.Parallel()
		if persistence.InTransaction(context.Background()) {
			t.Error("InTransaction true outside any Do")
		}
		ctx, _ := persistence.Collect(context.Background())
		if !persistence.InTransaction(ctx) {
			t.Error("InTransaction false inside a Do")
		}
	})
}

func TestTransactionOptions(t *testing.T) {
	t.Parallel()

	tx := persistence.Configure()
	if tx.ReadOnly || tx.Isolation != "" {
		t.Errorf("default = %+v, want the driver's own defaults", tx)
	}
	tx = persistence.Configure(persistence.ReadOnly(), persistence.Isolation(persistence.Serializable))
	if !tx.ReadOnly || tx.Isolation != persistence.Serializable {
		t.Errorf("configured = %+v", tx)
	}
}

// --- the contract suite, run against the memory driver ---------------------

func TestMemoryDriverContract(t *testing.T) {
	t.Parallel()
	persistence.RunContract(t, func(*testing.T) (persistence.UnitOfWork, persistence.Repository[*order, orderID]) {
		uow := persistence.NewMemoryUnitOfWork()
		// This test is not about events, so it says so. Without a sink the
		// commit is now REFUSED — which is the point: dropping drained events
		// silently is the defect persistence.Deliver exists to prevent.
		uow.OnCommit(persistence.Discard)
		return uow, persistence.NewMemoryRepository[*order, orderID](uow)
	}, func(id orderID) *order { return newOrder(id, 1) }, orderID("first"), orderID("second"))
}

func TestMemoryDriverVersionedContract(t *testing.T) {
	t.Parallel()
	persistence.RunVersionedContract(t, func(*testing.T) (persistence.UnitOfWork, persistence.Repository[*payment, orderID]) {
		uow := persistence.NewMemoryUnitOfWork()
		// This test is not about events, so it says so. Without a sink the
		// commit is now REFUSED — which is the point: dropping drained events
		// silently is the defect persistence.Deliver exists to prevent.
		uow.OnCommit(persistence.Discard)
		return uow, persistence.NewMemoryRepository[*payment, orderID](uow)
	}, func(id orderID) *payment { return newPayment(id, 1) },
		orderID("v1"), orderID("v2"), orderID("v3"), orderID("v4"), orderID("v5"), orderID("v6"), orderID("v7"),
		orderID("v8"))
}

func TestMemoryUnitOfWorkCommitAndRollback(t *testing.T) {
	t.Parallel()

	t.Run("a committed Do drains events to the outbox sink", func(t *testing.T) {
		t.Parallel()
		uow := persistence.NewMemoryUnitOfWork()
		repo := persistence.NewMemoryRepository[*order, orderID](uow)
		var published []domain.Event
		uow.OnCommit(func(_ context.Context, events []domain.Event) error {
			published = append(published, events...)
			return nil
		})

		err := uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Save(ctx, newOrder("a", 1))
		})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if len(published) != 1 || published[0].AggregateID() != "a" {
			t.Errorf("published = %v, want the aggregate's one event", published)
		}
	})

	t.Run("a failing Do rolls the state back and publishes nothing", func(t *testing.T) {
		t.Parallel()
		uow := persistence.NewMemoryUnitOfWork()
		repo := persistence.NewMemoryRepository[*order, orderID](uow)
		published := 0
		uow.OnCommit(func(context.Context, []domain.Event) error { published++; return nil })

		boom := stderrors.New("business rule")
		err := uow.Do(context.Background(), func(ctx context.Context) error {
			if err := repo.Save(ctx, newOrder("a", 1)); err != nil {
				return err
			}
			return boom
		})
		if !stderrors.Is(err, boom) {
			t.Fatalf("Do = %v, want the handler's error", err)
		}
		if published != 0 {
			t.Error("events were published for a rolled-back transaction")
		}
		if _, err := repo.FindByID(context.Background(), "a"); !werrors.Is(err, werrors.CodeNotFound) {
			t.Error("the rolled-back aggregate is still readable")
		}
	})

	t.Run("a nested Do joins: one commit, one drain", func(t *testing.T) {
		t.Parallel()
		uow := persistence.NewMemoryUnitOfWork()
		repo := persistence.NewMemoryRepository[*order, orderID](uow)
		commits := 0
		uow.OnCommit(func(context.Context, []domain.Event) error { commits++; return nil })

		err := uow.Do(context.Background(), func(ctx context.Context) error {
			if err := repo.Save(ctx, newOrder("a", 1)); err != nil {
				return err
			}
			// §10's handler calls Do itself while app.Transactional may have
			// opened one already: the inner call must join, not nest.
			return uow.Do(ctx, func(ctx context.Context) error {
				return repo.Save(ctx, newOrder("b", 2))
			})
		})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if commits != 1 {
			t.Errorf("commits = %d, want 1 — the inner Do joined", commits)
		}
	})

	t.Run("options on a nested Do are an error, not a silent downgrade", func(t *testing.T) {
		t.Parallel()
		uow := persistence.NewMemoryUnitOfWork()
		// This test is not about events, so it says so. Without a sink the
		// commit is now REFUSED — which is the point: dropping drained events
		// silently is the defect persistence.Deliver exists to prevent.
		uow.OnCommit(persistence.Discard)
		err := uow.Do(context.Background(), func(ctx context.Context) error {
			return uow.Do(ctx, func(context.Context) error { return nil }, persistence.ReadOnly())
		})
		if !werrors.Is(err, werrors.CodeInvalid) {
			t.Fatalf("nested Do with options = %v, want INVALID", err)
		}
		if !strings.Contains(err.Error(), "outermost") {
			t.Errorf("error does not say where the options belong: %v", err)
		}
	})

	t.Run("a panic rolls back and re-panics", func(t *testing.T) {
		t.Parallel()
		uow := persistence.NewMemoryUnitOfWork()
		// This test is not about events, so it says so. Without a sink the
		// commit is now REFUSED — which is the point: dropping drained events
		// silently is the defect persistence.Deliver exists to prevent.
		uow.OnCommit(persistence.Discard)
		repo := persistence.NewMemoryRepository[*order, orderID](uow)

		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Error("Do swallowed a panic — the edge classifies it, and a swallowed panic loses the stack")
				}
			}()
			_ = uow.Do(context.Background(), func(ctx context.Context) error {
				_ = repo.Save(ctx, newOrder("a", 1))
				panic("bug")
			})
		}()

		if _, err := repo.FindByID(context.Background(), "a"); !werrors.Is(err, werrors.CodeNotFound) {
			t.Error("the panicking transaction was not rolled back — it would hold locks until the pool reaped it")
		}
	})

	t.Run("a commit failure surfaces as UNAVAILABLE", func(t *testing.T) {
		t.Parallel()
		uow := persistence.NewMemoryUnitOfWork()
		uow.OnCommit(func(context.Context, []domain.Event) error {
			return stderrors.New("outbox write failed")
		})
		// The transaction must actually RAISE something. Deliver does not
		// call sinks for a transaction that drained nothing — an empty
		// commit is not a delivery — so a body that writes no aggregate
		// would never reach the failing sink, and this test would assert on
		// a code path it did not execute.
		repo := persistence.NewMemoryRepository[*order, orderID](uow)
		err := uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Save(ctx, newOrder("a", 1))
		})
		if !werrors.Is(err, werrors.CodeUnavailable) {
			t.Errorf("commit failure = %v, want UNAVAILABLE", err)
		}
	})
}

func TestSaveEnlistsAutomatically(t *testing.T) {
	t.Parallel()

	// The contract's load-bearing line: a driver whose Save does not Track
	// loses events, so the suite asserts it rather than trusting it.
	uow := persistence.NewMemoryUnitOfWork()
	repo := persistence.NewMemoryRepository[*order, orderID](uow)
	var drained []domain.Event
	uow.OnCommit(func(_ context.Context, e []domain.Event) error { drained = e; return nil })

	if err := uow.Do(context.Background(), func(ctx context.Context) error {
		return repo.Save(ctx, newOrder("a", 1))
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("drained %d events, want 1 — Save must call persistence.Track", len(drained))
	}
}

func BenchmarkTrackCollect(b *testing.B) {
	o := newOrder("a", 1)
	b.ReportAllocs()
	for b.Loop() {
		ctx, drain := persistence.Collect(context.Background())
		persistence.Track(ctx, o)
		_ = drain()
	}
}

// TestCommitSinkMayUseTheUnitOfWork pins the deadlock the 2026-08-02 review
// reproduced: the outbox writer is a commit sink that writes through this
// same unit of work, so the commit must not hold its lock across the
// callback.
func TestCommitSinkMayUseTheUnitOfWork(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	repo := persistence.NewMemoryRepository[*order, orderID](uow)
	sinkSaw := 0
	uow.OnCommit(func(ctx context.Context, events []domain.Event) error {
		// Reads and writes through the same uow — what an outbox sink does.
		if _, err := repo.FindByID(ctx, "a"); err == nil {
			sinkSaw++
		}
		return repo.Save(ctx, newOrder("audit", 0))
	})

	done := make(chan error, 1)
	go func() {
		done <- uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Save(ctx, newOrder("a", 1))
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a commit sink touching the repository deadlocked the commit")
	}
	if sinkSaw != 1 {
		t.Error("the sink could not see the transaction's own writes — it must run inside the transaction")
	}
	// The sink's own write committed with everything else.
	if _, err := repo.FindByID(context.Background(), "audit"); err != nil {
		t.Errorf("the sink's write did not commit: %v", err)
	}
}

// --- reference-typed fields ------------------------------------------------

// lineItem makes the fixture aggregate look like a real one. An invoice with
// line items, an order with lines, a cart with entries — a slice on the
// aggregate is the canonical shape, not an exotic one.
type lineItem struct {
	SKU      string
	Quantity int
}

type invoice struct {
	domain.AggregateRoot[orderID]
	Lines []lineItem
	Meta  map[string]string
}

func newInvoice(id orderID) *invoice {
	return &invoice{
		AggregateRoot: domain.NewAggregateRoot(id),
		Lines:         []lineItem{{SKU: "a", Quantity: 2}},
		Meta:          map[string]string{"channel": "web"},
	}
}

var _ domain.Aggregate = (*invoice)(nil)

// TestRollbackDoesNotReachThroughAReferenceField — MemoryUnitOfWork's doc
// promises "a rolled-back transaction leaves the committed aggregate
// untouched". The staging copy was SHALLOW (reflect.New + Elem().Set), so
// every slice, map and pointer field was shared with committed state: a
// transaction that returned an error still rewrote what it had mutated
// through the alias, and the invoice total was silently wrong afterwards.
func TestRollbackDoesNotReachThroughAReferenceField(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*invoice, orderID](uow)
	ctx := context.Background()

	if err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newInvoice("inv-1"))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	boom := stderrors.New("rolled back")
	err := uow.Do(ctx, func(ctx context.Context) error {
		got, ferr := repo.FindByID(ctx, "inv-1")
		if ferr != nil {
			return ferr
		}
		got.Lines[0].Quantity = 999
		got.Meta["channel"] = "tampered"
		got.Lines = append(got.Lines, lineItem{SKU: "b", Quantity: 1})
		return boom
	})
	if !stderrors.Is(err, boom) {
		t.Fatalf("Do = %v, want the rolled-back error", err)
	}

	after, err := repo.FindByID(ctx, "inv-1")
	if err != nil {
		t.Fatalf("FindByID after rollback: %v", err)
	}
	if got := after.Lines[0].Quantity; got != 2 {
		t.Errorf("committed line quantity = %d, want 2 — the rollback reached through the shared slice", got)
	}
	if got := after.Meta["channel"]; got != "web" {
		t.Errorf("committed meta = %q, want %q — the rollback reached through the shared map", got, "web")
	}
	if len(after.Lines) != 1 {
		t.Errorf("committed line count = %d, want 1", len(after.Lines))
	}
}

// TestTwoReadersDoNotShareMutableState — the same aliasing let one
// transaction observe another's uncommitted edits, which contradicts the
// isolation the driver's own comment claims.
func TestTwoReadersDoNotShareMutableState(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*invoice, orderID](uow)
	ctx := context.Background()

	if err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newInvoice("inv-2"))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	first, err := repo.FindByID(ctx, "inv-2")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	first.Lines[0].Quantity = 42

	second, err := repo.FindByID(ctx, "inv-2")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := second.Lines[0].Quantity; got != 2 {
		t.Errorf("second reader saw %d, want 2 — the two reads share one backing array", got)
	}
}

// TestUnsupportedTransactionOptionsAreRefused — warren.md §3.3 says "An
// unsupported Option is INVALID, never a silent downgrade", and
// MemoryUnitOfWork.Do contained `_ = Configure(opts...)` with the comment
// "a real driver begins the transaction it describes". It described nothing:
// a write inside persistence.ReadOnly() committed, and
// Isolation(Serializable) was accepted by a driver that cannot detect a
// write conflict at all.
//
// Refusing is the honest answer for the in-process driver. Claiming
// Serializable while two concurrent writers both win would be worse than
// refusing.
func TestUnsupportedTransactionOptionsAreRefused(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	ctx := context.Background()

	err := uow.Do(ctx, func(context.Context) error { return nil },
		persistence.Isolation(persistence.Serializable))
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Errorf("Isolation(Serializable) = %v, want INVALID — the in-process driver cannot honour it", err)
	}
	if err != nil && !strings.Contains(err.Error(), "serializable") {
		t.Errorf("the diagnostic does not name the level asked for:\n%v", err)
	}
}

// TestReadOnlyRefusesAWrite — persistence.ReadOnly() accepted a write and
// committed it. A read-only transaction that commits is not a downgrade, it
// is the opposite of what was asked for.
func TestReadOnlyRefusesAWrite(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*order, orderID](uow)
	ctx := context.Background()

	err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newOrder("ro-1", 1))
	}, persistence.ReadOnly())
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Fatalf("a write inside ReadOnly() = %v, want INVALID", err)
	}

	// And it did not commit.
	if _, ferr := repo.FindByID(ctx, "ro-1"); !werrors.Is(ferr, werrors.CodeNotFound) {
		t.Errorf("FindByID = %v, want NOT_FOUND — the read-only transaction committed", ferr)
	}
}

// TestAReadOnlyTransactionStillReads is the other half: refusing writes must
// not refuse the thing read-only transactions exist for.
func TestAReadOnlyTransactionStillReads(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*order, orderID](uow)
	ctx := context.Background()

	if err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newOrder("ro-2", 7))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := uow.Do(ctx, func(ctx context.Context) error {
		got, ferr := repo.FindByID(ctx, "ro-2")
		if ferr != nil {
			return ferr
		}
		if got.Total != 7 {
			t.Errorf("Total = %d, want 7", got.Total)
		}
		return nil
	}, persistence.ReadOnly()); err != nil {
		t.Errorf("a read inside ReadOnly() was refused: %v", err)
	}
}

// TestSavingAnAggregateWithNoIDIsRefused — removing
// `AggregateRoot: domain.NewAggregateRoot(id)` from a constructor is a
// plausible refactor mistake and there is no compile error. The write then
// "succeeded", filed under the empty key, and the aggregate 404'd for ever:
//
//	POST /invoices      → 201 {"id":"x1","status":"draft"}
//	POST /invoices/x1/issue → 404 "*domain.Invoice x1 not found"
//	GET  /invoices/x1       → 404 "*domain.Invoice x1 not found"
//
// The event was published too, so a downstream consumer learned about an
// invoice nothing can ever load.
func TestSavingAnAggregateWithNoIDIsRefused(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*order, orderID](uow)
	ctx := context.Background()

	// An aggregate whose embedded root was never initialised: its ID is the
	// zero value, and nothing in the type system says so.
	orphan := &order{Total: 1}

	err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, orphan)
	})
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Fatalf("Save of an aggregate with no ID = %v, want INVALID", err)
	}
	if err != nil && !strings.Contains(err.Error(), "NewAggregateRoot") {
		t.Errorf("the diagnostic does not name the omission that causes it:\n%v", err)
	}

	// Outside a transaction too — the immediate-write path is the same bug.
	if err := repo.Save(ctx, orphan); !werrors.Is(err, werrors.CodeInvalid) {
		t.Errorf("Save outside a transaction = %v, want INVALID", err)
	}
}

// TestNotFoundDoesNotLeakTheGoTypeName — the memory driver is what `warren
// new` wires, so its NotFound message is the one every new application ships
// to its clients. It namespaced its keys with reflect.TypeFor[T]().String()
// and then reused that string as the error's resource, so a 404 body read
//
//	{"error":{"code":"NOT_FOUND","message":"*persistence_test.order o-1 not found"}}
//
// which tells a client the aggregate's Go package, its Go type name, and that
// it is held by pointer. The hand-written convention — and the one the
// postgres adapter's own doc comment shows — is errors.NotFound("order", id).
// Keys still need the fully qualified name; the message does not.
func TestNotFoundDoesNotLeakTheGoTypeName(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*order, orderID](uow)

	_, err := repo.FindByID(context.Background(), "o-1")
	if !werrors.Is(err, werrors.CodeNotFound) {
		t.Fatalf("FindByID of an absent aggregate = %v, want NOT_FOUND", err)
	}
	// Message is what the transport puts in the response body.
	var werr *werrors.Error
	if !stderrors.As(err, &werr) {
		t.Fatalf("FindByID error is not a *errors.Error: %T", err)
	}
	msg := werr.Message()
	for _, leak := range []string{"*", "persistence_test", "."} {
		if strings.Contains(msg, leak) {
			t.Errorf("the 404 body leaks the Go type (%q): %q", leak, msg)
		}
	}
	if msg != "order o-1 not found" {
		t.Errorf("FindByID message = %q, want %q", msg, "order o-1 not found")
	}
}

// TestTwoAggregateTypesWithOneIDDoNotCollide — the resource noun in the
// message is now a different string from the key prefix, so the property the
// key prefix exists for needs its own test.
func TestTwoAggregateTypesWithOneIDDoNotCollide(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	orders := persistence.NewMemoryRepository[*order, orderID](uow)
	invoices := persistence.NewMemoryRepository[*invoice, orderID](uow)
	ctx := context.Background()

	if err := uow.Do(ctx, func(ctx context.Context) error {
		return orders.Save(ctx, newOrder("shared-1", 7))
	}); err != nil {
		t.Fatalf("saving the order: %v", err)
	}

	// Same identifier value, different aggregate type: still absent.
	if _, err := invoices.FindByID(ctx, "shared-1"); !werrors.Is(err, werrors.CodeNotFound) {
		t.Fatalf("an invoice resolved from an order's key = %v, want NOT_FOUND", err)
	}
	if _, err := orders.FindByID(ctx, "shared-1"); err != nil {
		t.Fatalf("the order itself no longer loads: %v", err)
	}
}

// TestAWriteThatSucceedsCannotLeaveItsEventsBehind is field test #12's D2,
// pinned. The tester deleted ONE line — persistence.Track(ctx, u) — from a
// generated repository's Save. The user row committed, POST /users returned
// 201, and the UserRegistered event evaporated: no outbox row, no error, no
// log line, and the consumer never fired. That is the exact loss the whole
// outbox subsystem exists to make impossible, and it was one deletable
// statement away.
//
// persistence.Write removes the statement. The enlistment is no longer
// something the repository remembers to do after the write; it is what Write
// does when the write returns nil.
func TestAWriteThatSucceedsCannotLeaveItsEventsBehind(t *testing.T) {
	t.Parallel()

	ctx, drain := persistence.Collect(context.Background())
	o := newOrder("a", 1)

	wrote := false
	err := persistence.Write(ctx, "order.Save", o, func(context.Context) error {
		wrote = true
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !wrote {
		t.Fatal("Write did not run the write function")
	}
	if events := drain(); len(events) != 1 {
		t.Errorf("drained %d events, want 1 — the write succeeded and the aggregate was not enlisted", len(events))
	}
}

func TestWriteRefusesAndEnlists(t *testing.T) {
	t.Parallel()

	t.Run("outside a unit of work it refuses and does not write", func(t *testing.T) {
		t.Parallel()
		o := newOrder("a", 1)
		ran := false
		err := persistence.Write(context.Background(), "order.Save", o, func(context.Context) error {
			ran = true
			return nil
		})
		if err == nil {
			t.Fatal("Write outside a unit of work succeeded — the row would autocommit and the events would be lost")
		}
		if ran {
			t.Error("the write function ran anyway — the refusal must come BEFORE the row is touched")
		}
		if !strings.Contains(err.Error(), "order.Save") {
			t.Errorf("the diagnostic does not name the operation: %v", err)
		}
		// The aggregate keeps its events: a later Do must still be able to
		// publish them, exactly as Track outside a transaction does.
		if len(o.PullEvents()) != 1 {
			t.Error("a refused Write consumed the aggregate's events")
		}
	})

	t.Run("a write that fails enlists nothing", func(t *testing.T) {
		t.Parallel()
		ctx, drain := persistence.Collect(context.Background())
		o := newOrder("a", 1)
		boom := stderrors.New("duplicate key")
		err := persistence.Write(ctx, "order.Save", o, func(context.Context) error { return boom })
		if !stderrors.Is(err, boom) {
			t.Errorf("Write = %v, want the write function's own error unchanged", err)
		}
		if events := drain(); len(events) != 0 {
			t.Errorf("drained %d events after a FAILED write, want 0", len(events))
		}
	})

	t.Run("it enlists exactly once", func(t *testing.T) {
		t.Parallel()
		ctx, drain := persistence.Collect(context.Background())
		o := newOrder("a", 1)
		if err := persistence.Write(ctx, "order.Save", o, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if events := drain(); len(events) != 1 {
			t.Errorf("drained %d events, want exactly 1", len(events))
		}
	})

	t.Run("a nil aggregate is named, not dereferenced", func(t *testing.T) {
		t.Parallel()
		ctx, _ := persistence.Collect(context.Background())
		err := persistence.Write(ctx, "order.Save", nil, func(context.Context) error { return nil })
		if err == nil {
			t.Fatal("Write with a nil aggregate succeeded")
		}
		if !strings.Contains(err.Error(), "order.Save") {
			t.Errorf("the diagnostic does not name the operation: %v", err)
		}
	})
}

// TestVersionedContractReturnsContention pins the CODE a lost versioned write
// carries, not merely that it errored.
//
// The port's own doc comment promised CodeConflict while errors.go, the
// postgres driver, the memory driver and warren.md §3.3 all said CONTENTION —
// and the doc comment is the first place a driver author reads. The two codes
// are different columns of the error table, so getting it wrong is not
// cosmetic: app.Retrying covers CONTENTION and not CONFLICT, and on a consumer
// the CONFLICT column ACKS, destroying the message with its work not done.
func TestVersionedContractReturnsContention(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	uow := persistence.NewMemoryUnitOfWork()
	// This test is not about events, so it says so. Without a sink the
	// commit is now REFUSED — which is the point: dropping drained events
	// silently is the defect persistence.Deliver exists to prevent.
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*payment, orderID](uow)
	save := func(p *payment) error {
		return uow.Do(ctx, func(ctx context.Context) error { return repo.Save(ctx, p) })
	}

	if err := save(newPayment("lost-update", 1)); err != nil {
		t.Fatalf("first save: %v", err)
	}
	one, err := repo.FindByID(ctx, "lost-update")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	two, err := repo.FindByID(ctx, "lost-update")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if err := save(one); err != nil {
		t.Fatalf("the first writer must win: %v", err)
	}

	err = save(two)
	if err == nil {
		t.Fatal("the second writer committed from a version the store had moved past — that is the lost update the version exists to prevent")
	}
	if !werrors.Is(err, werrors.CodeContention) {
		t.Errorf("the loser got %v, want CodeContention — nothing was written, so app.Retrying must re-run the handler", err)
	}
	// The negative half is the one that has teeth. Asserting CONTENTION alone
	// would still pass if the two codes were aliases, and CONFLICT is exactly
	// what the port doc used to promise.
	if werrors.Is(err, werrors.CodeConflict) {
		t.Errorf("the loser got %v, which is ALSO CodeConflict — a consumer acks that code, so the message would be destroyed with its work not done", err)
	}
}

// --- the event-sink refusal (FT14-2) ---------------------------------------
//
// Every test below fails on the code as it stood before this change, where a
// unit of work with no sink drained an aggregate's events and dropped them:
// the row committed, warren_outbox held nothing, the aggregate's pending queue
// was empty, and no error or log line said so.

func TestDeliverRefusesEventsWithNoSink(t *testing.T) {
	t.Parallel()

	events := []domain.Event{placed{ID: "a", At: time.Unix(1, 0)}}
	err := persistence.Deliver(context.Background(), events, nil)
	if err == nil {
		t.Fatal("Deliver dropped the events and returned nil — the whole defect")
	}
	// CodeOf, not Is: this is a bare diagnostic, exactly like
	// ErrNoTransaction, because errors.Internal would prefix "unexpected
	// failure:" and bury the first line of a message whose text is its whole
	// value. CodeOf is what the transport edge calls, and it maps an
	// unclassified error to INTERNAL — so the caller sees 500, which is
	// right: nothing about the next attempt is different.
	if got := werrors.CodeOf(err); got != werrors.CodeInternal {
		t.Errorf("code = %s, want INTERNAL — a wiring mistake is not retryable", got)
	}
	for _, want := range []string{"nowhere to put them", "persistence.Discard", "WithOutbox()"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, err)
		}
	}
}

func TestDeliverWithNoEventsIsNotAMisconfiguration(t *testing.T) {
	t.Parallel()

	// Most transactions raise nothing. Refusing those would make the check
	// useless by making it fire everywhere.
	if err := persistence.Deliver(context.Background(), nil, nil); err != nil {
		t.Errorf("Deliver with no events and no sink = %v, want nil", err)
	}
}

func TestDeliverRunsSinksInRegistrationOrderWithTheSameSlice(t *testing.T) {
	t.Parallel()

	events := []domain.Event{placed{ID: "a", At: time.Unix(1, 0)}, placed{ID: "b", At: time.Unix(1, 0)}}
	var order []string
	sink := func(name string) persistence.EventSink {
		return func(_ context.Context, got []domain.Event) error {
			order = append(order, name)
			if len(got) != len(events) {
				t.Errorf("%s saw %d events, want %d", name, len(got), len(events))
			}
			return nil
		}
	}
	if err := persistence.Deliver(context.Background(), events,
		[]persistence.EventSink{sink("first"), nil, sink("second")}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("sinks ran %v, want first,second — and the nil skipped, not panicked", order)
	}
}

func TestDeliverWrapsAFailingSinkAsUnavailable(t *testing.T) {
	t.Parallel()

	err := persistence.Deliver(context.Background(),
		[]domain.Event{placed{ID: "a", At: time.Unix(1, 0)}},
		[]persistence.EventSink{func(context.Context, []domain.Event) error {
			return stderrors.New("outbox write failed")
		}})
	if !werrors.Is(err, werrors.CodeUnavailable) {
		t.Errorf("failing sink = %v, want UNAVAILABLE — the next attempt may succeed", err)
	}
}

// TestErrNoEventSinkIsGolden pins the text, because the text IS the value of
// this error: an operator meeting it has already lost the events that
// transaction raised, and the two ways out have to be in front of them.
//
// Regenerate with:  go test ./persistence -run Golden -update
func TestErrNoEventSinkIsGolden(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString("── one event\n")
	b.WriteString(persistence.ErrNoEventSink(1).Error())
	b.WriteString("\n\n── three events\n")
	b.WriteString(persistence.ErrNoEventSink(3).Error())
	b.WriteString("\n")

	path := filepath.Join("testdata", "no_event_sink.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden file — run: go test ./persistence -run Golden -update\n%v", err)
	}
	if b.String() != string(want) {
		t.Errorf("diagnostic drifted from the golden:\n--- got\n%s\n--- want\n%s", b.String(), want)
	}
}

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func TestDiscardIsAWorkingOptOut(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork()
	uow.OnCommit(persistence.Discard)
	repo := persistence.NewMemoryRepository[*order, orderID](uow)

	ctx := context.Background()
	if err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newOrder("a", 1))
	}); err != nil {
		t.Fatalf("a commit with Discard registered must succeed: %v", err)
	}
	// The write is real. Discard says the EVENTS go nowhere, not the row.
	if _, err := repo.FindByID(ctx, "a"); err != nil {
		t.Errorf("the row is not there after a Discard commit: %v", err)
	}
}

func TestMemoryCommitWithNoSinkRollsBackTheWrite(t *testing.T) {
	t.Parallel()

	uow := persistence.NewMemoryUnitOfWork() // deliberately no OnCommit
	repo := persistence.NewMemoryRepository[*order, orderID](uow)

	ctx := context.Background()
	err := uow.Do(ctx, func(ctx context.Context) error {
		return repo.Save(ctx, newOrder("a", 1))
	})
	if err == nil {
		t.Fatal("the commit succeeded and the events were destroyed — the defect")
	}
	if got := werrors.CodeOf(err); got != werrors.CodeInternal {
		t.Errorf("code = %s, want INTERNAL", got)
	}
	// The refusal must roll back. A row that committed while its events were
	// destroyed is the half-done state the whole check exists to prevent.
	if _, err := repo.FindByID(ctx, "a"); !werrors.Is(err, werrors.CodeNotFound) {
		t.Errorf("FindByID after the refusal = %v, want NOT_FOUND — the write was not rolled back", err)
	}
}

// --- the canaries: proving the CONTRACT SUITE can fail ---------------------
//
// FT14-2 was not only a driver defect. RunContract certified the drivers and
// could not see it, because its enlistment subtests assert the aggregate's
// pending queue is EMPTY after commit — equally true of events that reached
// the outbox and events that reached the floor. A suite that cannot fail on
// the thing it exists to certify is worse than no suite: it converts an
// unchecked property into a checked-looking one.
//
// These two run the suite against deliberately broken drivers in a SUBPROCESS,
// because a failing subtest fails its parent — the only way to assert that a
// test suite fails is to run it as a test binary and read the exit code.

// droppingUnitOfWork drains the enlisted aggregates and throws the events
// away. It is what every driver would look like with persistence.Deliver's
// refusal removed.
type droppingUnitOfWork struct{}

func (u *droppingUnitOfWork) Do(ctx context.Context, fn func(context.Context) error, _ ...persistence.Option) error {
	ctx, drain := persistence.Collect(ctx)
	if err := fn(ctx); err != nil {
		return err
	}
	drain() // the whole defect, in one line: destructive, and nothing receives it
	return nil
}

// sinkableDroppingUnitOfWork is CORRECT IN EVERY RESPECT BUT ONE. It is the
// real MemoryUnitOfWork — real transactions, real rollback, real optimistic
// concurrency — wrapped so that a sink registered through OnCommit is
// swallowed instead of forwarded.
//
// That isolation is the whole point. A fake that is broken in several ways
// would fail RunContract for some other reason, and the canary would prove
// only that the suite rejects rubbish — not that the DELIVERY assertion is
// what caught it. This one passes every other subtest.
type sinkableDroppingUnitOfWork struct {
	*persistence.MemoryUnitOfWork
	swallowed []persistence.EventSink
}

func newSinkableDroppingUnitOfWork() *sinkableDroppingUnitOfWork {
	inner := persistence.NewMemoryUnitOfWork()
	// Registered on the INNER unit of work so commits still succeed: the
	// modelled defect is "the sink the caller registered never runs", not
	// "the unit of work refuses".
	inner.OnCommit(persistence.Discard)
	return &sinkableDroppingUnitOfWork{MemoryUnitOfWork: inner}
}

// OnCommit accepts the sink and never calls it — a driver that says it
// delivers and does not.
func (u *sinkableDroppingUnitOfWork) OnCommit(fn persistence.EventSink) {
	u.swallowed = append(u.swallowed, fn)
}

const canaryEnv = "WARREN_CONTRACT_CANARY"

// runCanary re-executes this test binary with only the named test enabled and
// returns whether it passed. A canary must FAIL.
func runCanary(t *testing.T, name string) (passed bool, output string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), canaryEnv+"=1")
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func TestRunContractRefusesADriverThatIsNotAnEventSource(t *testing.T) {
	if os.Getenv(canaryEnv) != "" {
		persistence.RunContract(t, func(*testing.T) (persistence.UnitOfWork, persistence.Repository[*order, orderID]) {
			uow := &droppingUnitOfWork{}
			return uow, newRecordingRepo()
		}, func(id orderID) *order { return newOrder(id, 1) }, orderID("first"), orderID("second"))
		return
	}

	passed, out := runCanary(t, t.Name())
	if passed {
		t.Errorf("RunContract PASSED a unit of work with nowhere to deliver events:\n%s", out)
	}
	if !strings.Contains(out, "persistence.EventSource") {
		t.Errorf("the failure does not name the missing seam:\n%s", out)
	}
}

func TestRunContractRefusesADriverThatDrainsAndDiscards(t *testing.T) {
	if os.Getenv(canaryEnv) != "" {
		persistence.RunContract(t, func(*testing.T) (persistence.UnitOfWork, persistence.Repository[*order, orderID]) {
			uow := newSinkableDroppingUnitOfWork()
			return uow, persistence.NewMemoryRepository[*order, orderID](uow.MemoryUnitOfWork)
		}, func(id orderID) *order { return newOrder(id, 1) }, orderID("first"), orderID("second"))
		return
	}

	passed, out := runCanary(t, t.Name())
	if passed {
		t.Errorf("RunContract PASSED a driver that registers sinks and never calls them —\n"+
			"which is FT14-2 exactly, and the suite could not see it:\n%s", out)
	}
	if !strings.Contains(out, "destroyed at commit") && !strings.Contains(out, "never ran") {
		t.Errorf("the failure does not say the events were destroyed:\n%s", out)
	}
}

// recordingRepo is the smallest Repository the contract suite accepts. It
// exists so a canary can pair a BROKEN unit of work with a CORRECT repository
// — otherwise a failure could be blamed on the repository and the canary would
// prove nothing.
type recordingRepo struct {
	mu    sync.Mutex
	items map[orderID]*order
}

func newRecordingRepo() *recordingRepo {
	return &recordingRepo{items: map[orderID]*order{}}
}

func (r *recordingRepo) FindByID(_ context.Context, id orderID) (*order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.items[id]
	if !ok {
		return nil, werrors.NotFound("order", id)
	}
	return o, nil
}

func (r *recordingRepo) Save(ctx context.Context, root *order) error {
	// Through persistence.Write, so the aggregate is enlisted exactly as a
	// real repository enlists it. The canary's defect is in the unit of work,
	// not here.
	return persistence.Write(ctx, "order.save", root, func(context.Context) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.items[root.ID()] = root
		return nil
	})
}

func (r *recordingRepo) Delete(ctx context.Context, root *order) error {
	return persistence.Write(ctx, "order.delete", root, func(context.Context) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.items, root.ID())
		return nil
	})
}
