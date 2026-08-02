package persistence

import (
	"context"
	"maps"
	"reflect"
	"sync"

	"github.com/MerseniBilel/warren/domain"
	"github.com/MerseniBilel/warren/errors"
)

// MemoryUnitOfWork is the in-process driver: real staging, real rollback,
// real event draining, no database. It exists so the port is exercised by
// CI, so app.Transactional has something to be tested against, and so a test
// suite need not reach for Docker — the same reasons broker/memory and
// inbox.NewMemoryStore live in core.
//
// Its isolation is insert/delete visibility, not field-level snapshotting:
// it stores the aggregate pointers it was given, so a mutation to an
// already-saved aggregate is visible before commit. Real drivers snapshot;
// the contract suite asserts only what every driver can promise.
type MemoryUnitOfWork struct {
	mu       sync.Mutex
	entities map[string]any // committed state, keyed by type+id
	commit   []func(context.Context, []domain.Event) error
}

// NewMemoryUnitOfWork returns an in-process UnitOfWork.
func NewMemoryUnitOfWork() *MemoryUnitOfWork {
	return &MemoryUnitOfWork{entities: map[string]any{}}
}

var _ UnitOfWork = (*MemoryUnitOfWork)(nil)

// OnCommit registers a sink for the events drained at commit — how the
// outbox writer attaches in tests, and how a test asserts on what was
// published. Sinks run inside the commit: a failing sink fails the
// transaction, which is the atomicity the outbox depends on.
func (u *MemoryUnitOfWork) OnCommit(fn func(context.Context, []domain.Event) error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.commit = append(u.commit, fn)
}

type stagingKey struct{}

// staging is one transaction's pending writes and deletes.
type staging struct {
	mu      sync.Mutex
	puts    map[string]any
	deletes map[string]bool
}

// Do runs fn in a transaction. A nested call joins: it runs fn on the
// ambient transaction and returns its error, opening and committing nothing.
func (u *MemoryUnitOfWork) Do(ctx context.Context, fn func(context.Context) error, opts ...Option) error {
	if s, ok := ctx.Value(stagingKey{}).(*staging); ok {
		if len(opts) > 0 {
			return errNestedOptions()
		}
		_ = s
		return fn(ctx)
	}

	_ = Configure(opts...) // a real driver begins the transaction it describes
	s := &staging{puts: map[string]any{}, deletes: map[string]bool{}}
	txCtx := context.WithValue(ctx, stagingKey{}, s)
	txCtx, drain := Collect(txCtx)

	// A panic rolls back and re-panics: a leaked transaction holds locks
	// until the pool reaps it, and swallowing the panic would convert a
	// programming bug into a 503 and destroy the stack. The edge already
	// classifies panics.
	committed := false
	defer func() {
		if !committed {
			s.mu.Lock()
			s.puts, s.deletes = nil, nil
			s.mu.Unlock()
		}
	}()

	if err := fn(txCtx); err != nil {
		return err
	}

	events := drain()

	u.mu.Lock()
	defer u.mu.Unlock()
	for _, sink := range u.commit {
		if err := sink(ctx, events); err != nil {
			return errors.Unavailable("unit of work commit", err)
		}
	}
	s.mu.Lock()
	maps.Copy(u.entities, s.puts)
	for k := range s.deletes {
		delete(u.entities, k)
	}
	s.mu.Unlock()
	committed = true
	return nil
}

// MemoryRepository is the in-process Repository for one aggregate type. Its
// Save calls Track, as the port's contract requires.
type MemoryRepository[T domain.Root[K], K domain.ID] struct {
	uow  *MemoryUnitOfWork
	kind string
}

// NewMemoryRepository returns an in-process Repository backed by uow.
func NewMemoryRepository[T domain.Root[K], K domain.ID](uow *MemoryUnitOfWork) *MemoryRepository[T, K] {
	// The type name namespaces the keys, so two aggregate types sharing an
	// identifier value do not collide.
	return &MemoryRepository[T, K]{uow: uow, kind: reflect.TypeFor[T]().String()}
}

func (r *MemoryRepository[T, K]) key(id K) string { return r.kind + "/" + id.String() }

// FindByID returns the aggregate, reading the ambient transaction's pending
// writes first — a handler sees what it just saved.
func (r *MemoryRepository[T, K]) FindByID(ctx context.Context, id K) (T, error) {
	var zero T
	k := r.key(id)

	if s, ok := ctx.Value(stagingKey{}).(*staging); ok {
		s.mu.Lock()
		if s.deletes[k] {
			s.mu.Unlock()
			return zero, errors.NotFound("aggregate", id)
		}
		if v, ok := s.puts[k]; ok {
			s.mu.Unlock()
			return v.(T), nil
		}
		s.mu.Unlock()
	}

	r.uow.mu.Lock()
	defer r.uow.mu.Unlock()
	v, ok := r.uow.entities[k]
	if !ok {
		return zero, errors.NotFound("aggregate", id)
	}
	return v.(T), nil
}

// Save stages the aggregate and enlists it, so its events reach the outbox
// when the transaction commits.
func (r *MemoryRepository[T, K]) Save(ctx context.Context, root T) error {
	Track(ctx, root)

	k := r.key(root.ID())
	if s, ok := ctx.Value(stagingKey{}).(*staging); ok {
		s.mu.Lock()
		s.puts[k] = root
		delete(s.deletes, k)
		s.mu.Unlock()
		return nil
	}
	// Outside a transaction the write is immediate; the events stay pending
	// on the aggregate for a later Do.
	r.uow.mu.Lock()
	defer r.uow.mu.Unlock()
	r.uow.entities[k] = root
	return nil
}

// Delete removes the aggregate, or returns CodeNotFound.
func (r *MemoryRepository[T, K]) Delete(ctx context.Context, id K) error {
	if _, err := r.FindByID(ctx, id); err != nil {
		return err
	}
	k := r.key(id)
	if s, ok := ctx.Value(stagingKey{}).(*staging); ok {
		s.mu.Lock()
		s.deletes[k] = true
		delete(s.puts, k)
		s.mu.Unlock()
		return nil
	}
	r.uow.mu.Lock()
	defer r.uow.mu.Unlock()
	delete(r.uow.entities, k)
	return nil
}
