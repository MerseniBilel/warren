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
	}, func(id orderID) *order { return newOrder(id, 1) })
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
