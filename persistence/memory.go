package persistence

import (
	"context"
	"fmt"
	"reflect"
	"strings"
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
// It hands every reader its own copy of an aggregate, as a real driver does
// by materialising rows: two transactions cannot see each other's
// uncommitted mutations, a rolled-back transaction leaves the committed
// aggregate untouched, and a retried handler re-reads clean state instead of
// re-applying its change to the object it already mutated.
type MemoryUnitOfWork struct {
	// commitMu serializes the commit sequence — version check, sinks, apply
	// — so the check cannot be invalidated between passing and being acted
	// on. It is ordered BEFORE mu: never take it while holding mu.
	commitMu sync.Mutex

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
//
// expect records, per key, the version a versioned aggregate was loaded at.
// It is kept alongside the put rather than read off the aggregate at commit
// because the aggregate's own version is advanced on success, so by then the
// number the write was checked against is gone.
type staging struct {
	mu       sync.Mutex
	puts     map[string]any
	expect   map[string]int64
	deletes  map[string]bool
	readOnly bool
}

// Do runs fn in a transaction. A nested call joins: it runs fn on the
// ambient transaction and returns its error, opening and committing nothing.
func (u *MemoryUnitOfWork) Do(ctx context.Context, fn func(context.Context) error, opts ...Option) error {
	if s, ok := ctx.Value(stagingKey{}).(*staging); ok {
		if len(opts) > 0 {
			return ErrNestedOptions()
		}
		_ = s
		return fn(ctx)
	}

	// The options are HONOURED or REFUSED, never parsed and discarded.
	// warren.md §3.3: an unsupported Option is INVALID, never a silent
	// downgrade — and this line used to be `_ = Configure(opts...)`, so a
	// write inside ReadOnly() committed and Isolation(Serializable) was
	// accepted by a driver that cannot detect a write conflict at all.
	tx := Configure(opts...)
	if tx.Isolation != "" {
		return errUnsupportedIsolation(tx.Isolation)
	}
	s := &staging{puts: map[string]any{}, expect: map[string]int64{}, deletes: map[string]bool{}, readOnly: tx.ReadOnly}
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

	// COMMIT IS SERIALIZED FROM HERE.
	//
	// Everything below — the version check, the sinks, and the apply — is one
	// critical section, and this lock is what makes the check below
	// AUTHORITATIVE rather than advisory. Without it the sequence was:
	//
	//	check versions   → passes, another writer has not committed YET
	//	run sinks        → THE OUTBOX ROW IS WRITTEN HERE
	//	check again      → fails, because that writer committed in between
	//	return CONFLICT  → the row stays. It announces a change that was
	//	                   rolled back, and nothing ever removes it.
	//
	// outbox.Store.Append's contract is "if the transaction rolls back, the
	// records roll back with it — that atomicity is the entire pattern", and
	// a memory store cannot honour it after the fact: Append has already
	// happened and the Store interface has no undo. So the fix is to make the
	// window not exist. u.entities is mutated in exactly one place, the apply
	// below, which is inside this same critical section — so while commitMu
	// is held, no version this transaction checked can change.
	//
	// It costs commit concurrency, which for the in-process driver is the
	// right trade: correctness of the outbox is the property the driver
	// exists to demonstrate. Real drivers get this from the database — the
	// postgres UnitOfWork writes its sinks INSIDE the pgx transaction, so its
	// rollback takes the outbox rows with it, and it needs none of this.
	//
	// u.mu is NOT held across the sinks (see below), so a sink that reads or
	// writes through this unit of work still works. A sink that opens a
	// SEPARATE top-level transaction would deadlock here — but it would also
	// be escaping the transaction whose commit it is part of, which is
	// already the bug this pattern exists to prevent.
	u.commitMu.Lock()
	defer u.commitMu.Unlock()

	// Check the versions BEFORE draining and before the sinks run, so a
	// conflict costs nothing and — more importantly — publishes nothing.
	if err := u.checkVersions(s); err != nil {
		return err
	}

	events := drain()

	// Snapshot the sinks and RELEASE the lock before running them: a sink
	// legitimately reads or writes through this same unit of work — the
	// outbox writer is exactly that — and holding u.mu across the callback
	// would deadlock the commit against itself.
	u.mu.Lock()
	sinks := append([]func(context.Context, []domain.Event) error(nil), u.commit...)
	u.mu.Unlock()

	// The sinks run inside the transaction: txCtx, not ctx, so a sink's own
	// writes join it, and a failing sink fails the whole transaction — that
	// atomicity is what the outbox depends on.
	for _, sink := range sinks {
		if err := sink(txCtx, events); err != nil {
			return errors.Unavailable("unit of work commit", err)
		}
	}

	u.mu.Lock()
	s.mu.Lock()
	// The version check that shares ONE acquisition of u.mu with the apply
	// below, which is what makes "exactly one of N concurrent writers wins"
	// true: a check that released the lock before writing would let two
	// transactions both pass it and both commit.
	//
	// Under commitMu no OTHER transaction can invalidate the check above, so
	// what is left for this one to catch is a write THIS transaction's own
	// sinks staged — a sink that saves an aggregate joins the ambient
	// transaction, and those puts were not in s.puts when the first check
	// ran. That case does still roll back after a sink has run, and it is
	// the one case where that is the correct answer: a sink writing a stale
	// aggregate is a conflict, and committing it would be the lost update
	// the check exists to prevent.
	if err := versionConflict(u.entities, s); err != nil {
		s.mu.Unlock()
		u.mu.Unlock()
		return err
	}
	// Commit copies, so the handler's aggregate and committed state part
	// ways at commit: a later mutation of the handler's object cannot
	// rewrite what was committed.
	for k, v := range s.puts {
		// Advance the caller's aggregate BEFORE copying, so the stored copy
		// and the object the handler still holds agree — a handler that saves
		// the same aggregate twice in one request must not conflict with
		// itself.
		if ver, ok := v.(domain.Versioned); ok {
			ver.SetVersion(s.expect[k] + 1)
		}
		u.entities[k] = copyAggregate(v)
	}
	for k := range s.deletes {
		delete(u.entities, k)
	}
	s.mu.Unlock()
	u.mu.Unlock()
	committed = true
	return nil
}

// checkVersions takes the locks itself for the early, publish-nothing check.
func (u *MemoryUnitOfWork) checkVersions(s *staging) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return versionConflict(u.entities, s)
}

// versionConflict reports whether any staged write is working from a version
// the store has moved past. Callers hold both locks.
//
// The two cases are the two ways a writer can be stale: the row changed under
// it, or the row it expected to update is gone — a delete it did not see.
func versionConflict(entities map[string]any, s *staging) error {
	for k := range s.puts {
		expected, versioned := s.expect[k]
		if !versioned {
			continue
		}
		stored, exists := entities[k]
		switch {
		case !exists && expected != 0:
			return errStaleWrite(k, expected, -1)
		case exists && expected == 0:
			// An insert onto an existing row. Two requests both minted the
			// same identity, or one is replaying a create.
			return errStaleWrite(k, 0, currentVersion(stored))
		case exists:
			if got := currentVersion(stored); got != expected {
				return errStaleWrite(k, expected, got)
			}
		}
	}
	return nil
}

func currentVersion(v any) int64 {
	if ver, ok := v.(domain.Versioned); ok {
		return ver.Version()
	}
	return 0
}

// MemoryRepository is the in-process Repository for one aggregate type. Its
// Save calls Track, as the port's contract requires.
type MemoryRepository[T domain.Root[K], K domain.ID] struct {
	uow      *MemoryUnitOfWork
	kind     string // key namespace: fully qualified, never shown
	resource string // NOT_FOUND's noun: what a client is allowed to read
}

// NewMemoryRepository returns an in-process Repository backed by uow.
func NewMemoryRepository[T domain.Root[K], K domain.ID](uow *MemoryUnitOfWork) *MemoryRepository[T, K] {
	// The type name namespaces the keys, so two aggregate types sharing an
	// identifier value do not collide. It must stay fully qualified for that
	// — and must never be the string a 404 body carries, which is why the
	// resource noun is derived separately.
	t := reflect.TypeFor[T]()
	return &MemoryRepository[T, K]{uow: uow, kind: t.String(), resource: resourceName(t)}
}

// resourceName renders an aggregate type as the noun an API client sees:
// *domain.Invoice becomes "invoice". The memory driver is what `warren new`
// wires, so this string ships in every new application's 404 bodies — and
// "*domain.Invoice inv-1 not found" tells a caller the package layout, the
// Go type name, and that the aggregate is held by pointer. It matches the
// convention hand-written repositories already follow: errors.NotFound(
// "invoice", id).
func resourceName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		// An unnamed type has nothing safe to say; the code alone will have
		// to do.
		return "resource"
	}
	return strings.ToLower(name)
}

func (r *MemoryRepository[T, K]) key(id K) string { return r.kind + "/" + id.String() }

// FindByID returns a COPY of the aggregate, reading the ambient
// transaction's pending writes first — a handler sees what it just saved,
// and never the object another transaction is mutating.
func (r *MemoryRepository[T, K]) FindByID(ctx context.Context, id K) (T, error) {
	var zero T
	k := r.key(id)

	if s, ok := ctx.Value(stagingKey{}).(*staging); ok {
		s.mu.Lock()
		if s.deletes[k] {
			s.mu.Unlock()
			return zero, errors.NotFound(r.resource, id)
		}
		if v, ok := s.puts[k]; ok {
			s.mu.Unlock()
			// Within one transaction the handler owns its aggregate: hand
			// back the same object so its own mutations accumulate.
			return v.(T), nil
		}
		s.mu.Unlock()
	}

	r.uow.mu.Lock()
	defer r.uow.mu.Unlock()
	v, ok := r.uow.entities[k]
	if !ok {
		return zero, errors.NotFound(r.resource, id)
	}
	return copyAggregate(v).(T), nil
}

// copyAggregate shallow-copies the pointed-to struct, which is what gives
// each transaction its own aggregate. A real driver gets this for free by
// materialising rows; the memory driver must do it deliberately, or every
// caller shares one object and the "confined to one goroutine" contract
// domain.AggregateRoot states becomes unkeepable through the repository.
// copyAggregate returns a DEEP copy, so staged state and committed state
// share no memory at all.
//
// A shallow copy — reflect.New plus Elem().Set — duplicates the struct and
// aliases every slice, map and pointer inside it. That made the driver's own
// promise false in the ordinary case: an aggregate with line items, mutated
// inside a transaction that then failed, kept the mutation after the
// rollback, and two concurrent readers saw each other's uncommitted edits.
// An invoice with a []Line is the canonical aggregate, not an exotic one.
//
// The cost is confined to the in-process driver, which is dev/test grade and
// already copies on every read and write; a real driver's isolation comes
// from the database.
func copyAggregate(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return v
	}
	return deepCopy(rv, map[uintptr]reflect.Value{}).Interface()
}

// deepCopy duplicates v. seen carries pointers already copied, so a cyclic
// aggregate terminates and keeps its shape rather than recursing for ever.
func deepCopy(v reflect.Value, seen map[uintptr]reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		if prev, ok := seen[v.Pointer()]; ok {
			return prev
		}
		dup := reflect.New(v.Type().Elem())
		seen[v.Pointer()] = dup
		dup.Elem().Set(deepCopy(v.Elem(), seen))
		return dup

	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		dup := reflect.MakeSlice(v.Type(), v.Len(), v.Cap())
		for i := range v.Len() {
			dup.Index(i).Set(deepCopy(v.Index(i), seen))
		}
		return dup

	case reflect.Map:
		if v.IsNil() {
			return v
		}
		dup := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			dup.SetMapIndex(deepCopy(iter.Key(), seen), deepCopy(iter.Value(), seen))
		}
		return dup

	case reflect.Array:
		dup := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			dup.Index(i).Set(deepCopy(v.Index(i), seen))
		}
		return dup

	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		dup := reflect.New(v.Type()).Elem()
		dup.Set(deepCopy(v.Elem(), seen))
		return dup

	case reflect.Struct:
		dup := reflect.New(v.Type()).Elem()
		for i := range v.NumField() {
			// An unexported field cannot be Set through reflection. Copying
			// the whole struct first means it keeps its shallow value —
			// which is what it had before — and every exported field is then
			// deep-copied over the top.
			if !v.Type().Field(i).IsExported() {
				continue
			}
			dup.Field(i).Set(deepCopy(v.Field(i), seen))
		}
		copyUnexported(dup, v)
		return dup

	default:
		return v
	}
}

// copyUnexported fills in the fields deepCopy could not Set, by copying the
// whole struct value first and then leaving the deep-copied exported fields
// in place. domain.AggregateRoot keeps its events this way.
func copyUnexported(dst, src reflect.Value) {
	shallow := reflect.New(src.Type()).Elem()
	shallow.Set(src)
	for i := range src.NumField() {
		if src.Type().Field(i).IsExported() {
			shallow.Field(i).Set(dst.Field(i))
		}
	}
	dst.Set(shallow)
}

// Save stages the aggregate and enlists it, so its events reach the outbox
// when the transaction commits.
func (r *MemoryRepository[T, K]) Save(ctx context.Context, root T) error {
	var zero K
	if root.ID() == zero {
		return errAggregateHasNoID(r.kind)
	}
	Track(ctx, root)

	k := r.key(root.ID())
	ver, versioned := any(root).(domain.Versioned)
	if s, ok := ctx.Value(stagingKey{}).(*staging); ok {
		if s.readOnly {
			return errReadOnlyWrite("Save")
		}
		s.mu.Lock()
		s.puts[k] = root
		if versioned {
			// Record the expected version only on the FIRST save of this key
			// in this transaction. A handler that saves twice checks against
			// the version it read, not against the one its own first save
			// would have implied.
			if _, seen := s.expect[k]; !seen {
				s.expect[k] = ver.Version()
			}
		}
		delete(s.deletes, k)
		s.mu.Unlock()
		return nil
	}
	// Outside a transaction the write is immediate; the events stay pending
	// on the aggregate for a later Do. The stored value is a copy, so a
	// later mutation of the caller's object does not silently rewrite
	// committed state.
	r.uow.mu.Lock()
	defer r.uow.mu.Unlock()
	if versioned {
		expected := ver.Version()
		stored, exists := r.uow.entities[k]
		switch {
		case !exists && expected != 0:
			return errStaleWrite(k, expected, -1)
		case exists && expected == 0:
			return errStaleWrite(k, 0, currentVersion(stored))
		case exists:
			if got := currentVersion(stored); got != expected {
				return errStaleWrite(k, expected, got)
			}
		}
		ver.SetVersion(expected + 1)
	}
	r.uow.entities[k] = copyAggregate(root)
	return nil
}

// Delete removes the aggregate, or returns CodeNotFound.
func (r *MemoryRepository[T, K]) Delete(ctx context.Context, root T) error {
	if s, ok := ctx.Value(stagingKey{}).(*staging); ok && s.readOnly {
		return errReadOnlyWrite("Delete")
	}
	id := root.ID()
	if _, err := r.FindByID(ctx, id); err != nil {
		return err
	}
	// Enlist for the same reason Save does, and only once the row is known to
	// be there: the farewell fact a handler raised before asking for the
	// delete lives on THIS object, and nothing else will ever drain it.
	// Outside a transaction Track is a no-op and the events stay pending, as
	// they do after an untransacted Save.
	Track(ctx, root)

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

// errUnsupportedIsolation refuses a level this driver cannot provide.
//
// It refuses rather than downgrades because the downgrade would be a lie
// with money attached. The driver detects a conflict only where an aggregate
// opted in by embedding domain.VersionedRoot; an isolation level is a promise
// about EVERY read and write in the transaction, including the unversioned
// ones and the reads, and this driver cannot make it.
func errUnsupportedIsolation(level Level) error {
	return errors.Invalid("isolation",
		fmt.Errorf("the in-process driver cannot honour %s: it stages writes behind a mutex, and the only write conflict it detects is a version mismatch on an aggregate that embeds domain.VersionedRoot. Accepting the level would be a silent downgrade — use a real driver, or drop the option", level))
}

// errStaleWrite reports a lost update that did NOT happen. current is the
// version the store holds, or -1 when the row is gone.
//
// It carries CodeConflict, which §3.3 promises and which nothing could
// produce before: Save had no expected-version seam at all, so a field test
// paying one invoice four times concurrently got four 201s and published four
// events. The message names both versions because "conflict" alone leaves the
// caller unable to tell a stale read from a vanished row.
// The three cases are NOT one answer to the caller, and saying they were is
// what the 2026-08-08 ruling corrected. Two of them are races that the next
// attempt usually wins; the third is two requests minting one identity,
// which is arithmetic and true for ever.
func errStaleWrite(key string, expected, current int64) error {
	if current < 0 {
		return errors.Contention("%s was loaded at version %d, but it no longer exists — it was deleted after this request read it", key, expected)
	}
	if expected == 0 {
		// CONFLICT, not CONTENTION: a retry re-reads and finds the row still
		// there, on this attempt and on every one after it.
		return errors.Conflict("%s already exists at version %d, and this write expected to create it", key, current)
	}
	return errors.Contention("%s was loaded at version %d and is now at version %d — another writer committed first, so this update was refused rather than silently overwriting theirs", key, expected, current)
}

// errReadOnlyWrite refuses a write in a transaction the caller declared
// read-only — which used to commit.
func errReadOnlyWrite(op string) error {
	return errors.Invalid("read-only transaction",
		fmt.Errorf("%s was called inside persistence.ReadOnly(): a read-only transaction that writes is not a downgrade, it is the opposite of what was asked for", op))
}

// errAggregateHasNoID refuses a write whose key would be the zero value.
//
// Dropping `AggregateRoot: domain.NewAggregateRoot(id)` from a constructor
// is a plausible refactor mistake with no compile error, and the
// consequences were entirely silent: the row was filed under the empty key,
// the write reported success, the creation event was published, and every
// later load of that aggregate returned NOT_FOUND for ever. A consumer
// downstream learned about an entity nothing can load.
func errAggregateHasNoID(kind string) error {
	return errors.Invalid("aggregate id",
		fmt.Errorf("a %s was saved with the zero identifier, so it would be filed under the empty key and could never be loaded again. Its constructor most likely omits AggregateRoot: domain.NewAggregateRoot(id)", kind))
}
