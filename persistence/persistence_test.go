package persistence_test

import (
	"context"
	stderrors "errors"
	"strings"
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
		return uow, persistence.NewMemoryRepository[*order, orderID](uow)
	}, func(id orderID) *order { return newOrder(id, 1) }, orderID("first"), orderID("second"))
}

func TestMemoryDriverVersionedContract(t *testing.T) {
	t.Parallel()
	persistence.RunVersionedContract(t, func(*testing.T) (persistence.UnitOfWork, persistence.Repository[*payment, orderID]) {
		uow := persistence.NewMemoryUnitOfWork()
		return uow, persistence.NewMemoryRepository[*payment, orderID](uow)
	}, func(id orderID) *payment { return newPayment(id, 1) },
		orderID("v1"), orderID("v2"), orderID("v3"), orderID("v4"), orderID("v5"), orderID("v6"), orderID("v7"))
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
		err := uow.Do(context.Background(), func(context.Context) error { return nil })
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
