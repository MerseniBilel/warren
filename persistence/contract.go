package persistence

import (
	"context"
	"testing"

	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
)

// NewDriver builds a fresh unit of work and repository for one subtest.
type NewDriver[T domain.Root[K], K domain.ID] func(t *testing.T) (UnitOfWork, Repository[T, K])

// RunContract is the suite every persistence driver must pass — the memory
// driver here, Postgres and Mongo behind a build tag. It asserts only what
// every driver can promise: identity round-trips, NOT_FOUND is a code and
// not a nil, a rolled-back transaction leaves nothing behind, and Save
// enlists the aggregate so its events reach the outbox.
//
// newAggregate builds a fresh aggregate with the given identity and at least
// one pending event.
func RunContract[T domain.Root[K], K domain.ID](t *testing.T, newDriver NewDriver[T, K], newAggregate func(K) T) {
	t.Helper()

	t.Run("save then find round-trips identity", func(t *testing.T) {
		uow, repo := newDriver(t)
		var id K
		agg := newAggregate(id)
		if err := uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Save(ctx, agg)
		}); err != nil {
			t.Fatalf("Do: %v", err)
		}
		got, err := repo.FindByID(context.Background(), agg.ID())
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.ID() != agg.ID() {
			t.Errorf("id = %v, want %v", got.ID(), agg.ID())
		}
	})

	t.Run("a missing aggregate is NOT_FOUND, not a nil", func(t *testing.T) {
		_, repo := newDriver(t)
		var missing K
		_, err := repo.FindByID(context.Background(), missing)
		if !errors.Is(err, errors.CodeNotFound) {
			t.Errorf("FindByID = %v, want CodeNotFound — the adapter maps it to 404 / NotFound / ack", err)
		}
	})

	t.Run("a handler reads its own writes inside the transaction", func(t *testing.T) {
		uow, repo := newDriver(t)
		var id K
		agg := newAggregate(id)
		err := uow.Do(context.Background(), func(ctx context.Context) error {
			if err := repo.Save(ctx, agg); err != nil {
				return err
			}
			_, err := repo.FindByID(ctx, agg.ID())
			return err
		})
		if err != nil {
			t.Errorf("a saved aggregate was invisible to its own transaction: %v", err)
		}
	})

	t.Run("delete removes it and deleting again is NOT_FOUND", func(t *testing.T) {
		uow, repo := newDriver(t)
		var id K
		agg := newAggregate(id)
		if err := uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Save(ctx, agg)
		}); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if err := uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Delete(ctx, agg.ID())
		}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		err := uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Delete(ctx, agg.ID())
		})
		if !errors.Is(err, errors.CodeNotFound) {
			t.Errorf("second Delete = %v, want CodeNotFound — silent success hides bugs", err)
		}
	})

	t.Run("Save enlists the aggregate with the unit of work", func(t *testing.T) {
		uow, repo := newDriver(t)
		var id K
		agg := newAggregate(id)
		var drained []domain.Event
		err := uow.Do(context.Background(), func(ctx context.Context) error {
			if err := repo.Save(ctx, agg); err != nil {
				return err
			}
			// The events must still be pending here: draining is the unit of
			// work's job, at commit, inside the transaction.
			return nil
		})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		drained = agg.PullEvents()
		if len(drained) != 0 {
			t.Errorf("%d events were still pending after commit — Save did not Track, so they never reached the outbox", len(drained))
		}
	})
}
