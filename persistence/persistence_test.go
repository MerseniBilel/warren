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
