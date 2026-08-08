package persistence

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"
	"time"

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
// one pending event. The suite calls it with DISTINCT, non-zero identities —
// a suite that only ever used the zero value could not catch a driver
// storing everything under one key, and would build the very
// zero-identity aggregate domain.NewAggregateRoot exists to prevent.
func RunContract[T domain.Root[K], K domain.ID](t *testing.T, newDriver NewDriver[T, K], newAggregate func(K) T, ids ...K) {
	t.Helper()
	if len(ids) < 2 {
		t.Fatalf("persistence.RunContract needs at least two distinct identities to certify a driver, got %d", len(ids))
	}
	first, second := ids[0], ids[1]
	var zero K
	if first == zero || second == zero || first == second {
		t.Fatal("persistence.RunContract needs two DISTINCT, non-zero identities — the zero value cannot catch a driver storing everything under one key")
	}

	// A ROLLED-BACK TRANSACTION LEAVES NOTHING BEHIND.
	//
	// RunContract's own doc comment has always claimed this suite asserts it.
	// It did not — fourteen subtests and not one rolled back — so a driver
	// that committed on the way out of a failing Do passed certification
	// while breaking the property the unit of work exists for. Found by the
	// adapter ruling of 2026-08-05, in the same stale-doc class as the jobs
	// premise.
	//
	// It matters most for the drivers not yet written. A Mongo driver on a
	// standalone topology cannot open a transaction at all; without this
	// subtest it would write non-transactionally and pass everything Warren
	// exports.
	t.Run("a rolled-back transaction leaves nothing behind", func(t *testing.T) {
		uow, repo := newDriver(t)
		id := second
		boom := stderrors.New("the handler changed its mind")

		err := uow.Do(context.Background(), func(ctx context.Context) error {
			if err := repo.Save(ctx, newAggregate(id)); err != nil {
				return err
			}
			// Saved, and then the transaction fails. Nothing may survive.
			return boom
		})
		if !stderrors.Is(err, boom) {
			t.Fatalf("Do = %v, want the handler's own error", err)
		}
		if _, err := repo.FindByID(context.Background(), id); !errors.Is(err, errors.CodeNotFound) {
			t.Errorf("the aggregate survived a rolled-back transaction: FindByID = %v, want NOT_FOUND", err)
		}
	})

	t.Run("save then find round-trips identity", func(t *testing.T) {
		uow, repo := newDriver(t)
		agg := newAggregate(first)
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
		_, err := repo.FindByID(context.Background(), second)
		if !errors.Is(err, errors.CodeNotFound) {
			t.Errorf("FindByID = %v, want CodeNotFound — the adapter maps it to 404 / NotFound / ack", err)
		}
	})

	t.Run("a handler reads its own writes inside the transaction", func(t *testing.T) {
		uow, repo := newDriver(t)
		agg := newAggregate(first)
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
		agg := newAggregate(first)
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

	t.Run("two aggregates are stored under distinct keys", func(t *testing.T) {
		uow, repo := newDriver(t)
		a, b := newAggregate(first), newAggregate(second)
		if err := uow.Do(context.Background(), func(ctx context.Context) error {
			if err := repo.Save(ctx, a); err != nil {
				return err
			}
			return repo.Save(ctx, b)
		}); err != nil {
			t.Fatalf("Do: %v", err)
		}
		gotA, err := repo.FindByID(context.Background(), first)
		if err != nil {
			t.Fatalf("FindByID(first): %v", err)
		}
		gotB, err := repo.FindByID(context.Background(), second)
		if err != nil {
			t.Fatalf("FindByID(second): %v", err)
		}
		if gotA.ID() != first || gotB.ID() != second {
			t.Errorf("ids = %v/%v, want %v/%v — one key for every aggregate would pass a weaker suite", gotA.ID(), gotB.ID(), first, second)
		}
	})

	t.Run("readers do not share one mutable aggregate", func(t *testing.T) {
		uow, repo := newDriver(t)
		agg := newAggregate(first)
		if err := uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Save(ctx, agg)
		}); err != nil {
			t.Fatalf("Do: %v", err)
		}
		one, err := repo.FindByID(context.Background(), first)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		two, err := repo.FindByID(context.Background(), first)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if any(one) == any(two) {
			t.Error("two reads returned the SAME aggregate — concurrent transactions would mutate one object, and domain.AggregateRoot's single-goroutine contract becomes unkeepable through the repository")
		}
	})

	t.Run("Save enlists the aggregate with the unit of work", func(t *testing.T) {
		uow, repo := newDriver(t)
		agg := newAggregate(first)
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

// versionedContractIDs is how many distinct identities RunVersionedContract
// needs: one per subtest.
const versionedContractIDs = 7

// RunVersionedContract certifies a driver's optimistic concurrency, and is
// run IN ADDITION to RunContract by drivers whose aggregates embed
// domain.VersionedRoot. It is separate because the version is opt-in: a
// driver that stores unversioned aggregates is not failing anything by not
// implementing it.
//
// The suite exists because the framework shipped without it. warren.md §3.3
// promised CodeConflict, Repository.Save had no expected-version seam, and a
// field test paying one invoice four times concurrently got four 201s and
// four published events. Every subtest here is that defect in miniature.
//
// newAggregate must return an aggregate implementing domain.Versioned at
// version 0.
//
// ids must hold at least versionedContractIDs DISTINCT, non-zero identities:
// one per subtest. Unlike RunContract, this suite cannot reuse an identity
// across subtests, because a store that newDriver does not actually reset —
// a shared Postgres table, say — would leave the row behind and every later
// insert would conflict for the wrong reason. Its own diagnostic would then
// be indistinguishable from the defect it is testing for.
func RunVersionedContract[T domain.Root[K], K domain.ID](t *testing.T, newDriver NewDriver[T, K], newAggregate func(K) T, ids ...K) {
	t.Helper()
	if len(ids) < versionedContractIDs {
		t.Fatalf("persistence.RunVersionedContract needs %d distinct identities, one per subtest, got %d", versionedContractIDs, len(ids))
	}
	var zero K
	seen := map[K]bool{}
	for _, id := range ids {
		if id == zero {
			t.Fatal("persistence.RunVersionedContract was given the zero identity")
		}
		if seen[id] {
			t.Fatalf("persistence.RunVersionedContract was given %v twice — each subtest needs its own row", id)
		}
		seen[id] = true
	}
	if _, ok := any(newAggregate(ids[0])).(domain.Versioned); !ok {
		t.Fatal("persistence.RunVersionedContract was given an aggregate that does not implement domain.Versioned — embed domain.VersionedRoot")
	}
	next := func() K {
		id := ids[0]
		ids = ids[1:]
		return id
	}

	save := func(uow UnitOfWork, repo Repository[T, K], agg T) error {
		return uow.Do(context.Background(), func(ctx context.Context) error {
			return repo.Save(ctx, agg)
		})
	}

	t.Run("a first save starts the aggregate at version 1", func(t *testing.T) {
		uow, repo := newDriver(t)
		agg := newAggregate(next())
		if err := save(uow, repo, agg); err != nil {
			t.Fatalf("Do: %v", err)
		}
		// The driver must ADVANCE the caller's aggregate. Leaving it at 0
		// makes the next save in the same request look like an insert.
		if got := any(agg).(domain.Versioned).Version(); got != 1 {
			t.Errorf("version = %d after the first save, want 1 — the driver did not advance the aggregate it just wrote", got)
		}
	})

	t.Run("a loaded aggregate carries its stored version", func(t *testing.T) {
		uow, repo := newDriver(t)
		id := next()
		if err := save(uow, repo, newAggregate(id)); err != nil {
			t.Fatalf("Do: %v", err)
		}
		got, err := repo.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if v := any(got).(domain.Versioned).Version(); v != 1 {
			t.Errorf("loaded version = %d, want 1 — reconstituting at 0 turns every update into an insert", v)
		}
	})

	t.Run("a stale write is CONFLICT, not a silent overwrite", func(t *testing.T) {
		uow, repo := newDriver(t)
		id := next()
		if err := save(uow, repo, newAggregate(id)); err != nil {
			t.Fatalf("Do: %v", err)
		}
		// Two readers load the same version — the shape of two concurrent
		// requests paying one invoice.
		one, err := repo.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		two, err := repo.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if err := save(uow, repo, one); err != nil {
			t.Fatalf("the first writer must win: %v", err)
		}
		err = save(uow, repo, two)
		// CodeContention, not CodeConflict: nothing was written, and the
		// next attempt re-reads and usually wins. A driver returning
		// CONFLICT here fails this suite on purpose — on a CONSUMER that
		// code ACKS, so the message would be destroyed with its work not
		// done.
		if !errors.Is(err, errors.CodeContention) {
			t.Errorf("the second writer got %v, want CodeContention — a blind upsert here is the lost update the version exists to prevent", err)
		}
	})

	// A COMMIT SINK MUST NOT FIRE FOR A TRANSACTION THAT THEN FAILS.
	//
	// This is the outbox half of the subtest below, and it is the assertion
	// this suite was one line short of. "The winner's write survives the
	// conflict" proves the loser left no trace in the STATE; it says nothing
	// about what the loser announced. outbox.Store.Append's contract is the
	// specification being checked here:
	//
	//	"Append writes records in the caller's ambient transaction. It opens
	//	 no connection of its own: if the transaction rolls back, the records
	//	 roll back with it — that atomicity is the entire pattern."
	//
	// A driver that runs its sinks before the write is certain publishes
	// events for state that never existed. Downstream consumers then invoice
	// for orders nobody placed, and no error is reported anywhere.
	t.Run("a conflicted transaction announces nothing", func(t *testing.T) {
		uow, repo := newDriver(t)
		id := next()
		if err := save(uow, repo, newAggregate(id)); err != nil {
			t.Fatalf("Do: %v", err)
		}

		// OnCommit is on the concrete drivers, not on the port — the port
		// deliberately does not expose sink registration to a handler, so a
		// driver without it cannot have this defect.
		sinker, ok := uow.(interface {
			OnCommit(func(context.Context, []domain.Event) error)
		})
		if !ok {
			t.Skip("driver exposes no OnCommit; there is no sink to fire early")
		}

		var mu sync.Mutex
		announced, parked := 0, false
		release := make(chan struct{})

		// The sink is what makes the window observable. The FIRST commit's
		// sink dawdles; a second, conflicting transaction runs meanwhile. A
		// driver that writes its outbox rows before the write is certain
		// will have announced TWO changes and committed one.
		//
		// The park is bounded rather than a rendezvous: a correct driver
		// serializes the two commits, so a hard rendezvous would deadlock on
		// the very implementation this is asserting.
		sinker.OnCommit(func(context.Context, []domain.Event) error {
			mu.Lock()
			announced++
			first := !parked
			parked = true
			mu.Unlock()
			if first {
				select {
				case <-release:
				case <-time.After(500 * time.Millisecond):
				}
			}
			return nil
		})

		// Both writers load the SAME version: exactly one may win.
		one, err := repo.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		two, err := repo.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(1)
		var firstErr error
		go func() {
			defer wg.Done()
			firstErr = save(uow, repo, one)
		}()
		// Give the first transaction time to reach its sink and park there.
		time.Sleep(50 * time.Millisecond)
		secondErr := save(uow, repo, two)
		close(release)
		wg.Wait()

		wins := 0
		for _, e := range []error{firstErr, secondErr} {
			if e == nil {
				wins++
			}
		}
		mu.Lock()
		got := announced
		mu.Unlock()

		if wins != 1 {
			t.Fatalf("%d of 2 writers from one version committed, want exactly 1 (first=%v second=%v)",
				wins, firstErr, secondErr)
		}
		// outbox.Store.Append's contract, stated as a number: "if the
		// transaction rolls back, the records roll back with it — that
		// atomicity is the entire pattern". A conflicted transaction that
		// still announced its change invoices for orders nobody placed, and
		// reports no error anywhere.
		if got != wins {
			t.Errorf("commit sinks ran %d time(s) for %d committed transaction(s) — "+
				"a conflicted transaction announced a change it did not make", got, wins)
		}
	})

	t.Run("the winner's write survives the conflict", func(t *testing.T) {
		uow, repo := newDriver(t)
		id := next()
		if err := save(uow, repo, newAggregate(id)); err != nil {
			t.Fatalf("Do: %v", err)
		}
		one, _ := repo.FindByID(context.Background(), id)
		two, _ := repo.FindByID(context.Background(), id)
		if err := save(uow, repo, one); err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = save(uow, repo, two)
		got, err := repo.FindByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		// A conflict that still wrote is worse than no check at all: the
		// caller is told the write failed while the data says otherwise.
		if v := any(got).(domain.Versioned).Version(); v != 2 {
			t.Errorf("stored version = %d after one win and one conflict, want 2 — the rejected write left a trace", v)
		}
	})

	t.Run("sequential saves of one aggregate keep working", func(t *testing.T) {
		uow, repo := newDriver(t)
		agg := newAggregate(next())
		for i := range 3 {
			if err := save(uow, repo, agg); err != nil {
				t.Fatalf("save %d of the same aggregate failed: %v — the driver reads the version but never advances it", i+1, err)
			}
		}
		if v := any(agg).(domain.Versioned).Version(); v != 3 {
			t.Errorf("version = %d after three saves, want 3", v)
		}
	})

	t.Run("concurrent writers produce exactly one winner", func(t *testing.T) {
		uow, repo := newDriver(t)
		id := next()
		if err := save(uow, repo, newAggregate(id)); err != nil {
			t.Fatalf("Do: %v", err)
		}
		const writers = 8
		loaded := make([]T, writers)
		for i := range loaded {
			got, err := repo.FindByID(context.Background(), id)
			if err != nil {
				t.Fatalf("FindByID: %v", err)
			}
			loaded[i] = got
		}
		var wg sync.WaitGroup
		errs := make([]error, writers)
		start := make(chan struct{})
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = save(uow, repo, loaded[i])
			}()
		}
		close(start)
		wg.Wait()

		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, errors.CodeContention):
			default:
				t.Errorf("writer %d = %v, want nil or CodeContention", i, err)
			}
		}
		// This is the field-test defect exactly: 8 writers all loaded version
		// 1, so 7 of them are working from state that no longer exists.
		if won != 1 {
			t.Errorf("%d of %d concurrent writers succeeded, want exactly 1 — the rest silently overwrote each other", won, writers)
		}
	})
}
