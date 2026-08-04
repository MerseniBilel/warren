package domain

// Versioned is the optional interface an aggregate implements to get
// optimistic concurrency. A repository that finds it on a root writes the
// version into the WHERE clause and reports errors.Conflict when the write
// matches no row — which is what makes §3.3's CodeConflict reachable for a
// real write conflict rather than only for a uniqueness violation.
//
// It is optional, and deliberately so: the version costs a column, and an
// aggregate whose writes are already serialised by something else — a single
// consumer, a lock, an append-only log — does not need one. A plain
// AggregateRoot does NOT satisfy this interface, so nothing acquires a
// version column it has no schema for.
//
// The two methods are the two halves of one protocol. A driver READS the
// version to build the write, and SETS it after the write succeeds so a
// second Save in the same request checks against what it just wrote instead
// of a stale number. A driver that reads but never advances turns the second
// save of one request into a spurious conflict.
type Versioned interface {
	// Version reports the version this aggregate was loaded at. Zero means it
	// has never been persisted, so the write is an insert.
	Version() int64

	// SetVersion records the version the aggregate now holds. Repositories
	// call it at reconstitution and after a successful write; domain code
	// does not.
	SetVersion(v int64)
}

// VersionedRoot is an AggregateRoot that carries a version. Embed it instead
// of AggregateRoot to opt an aggregate into optimistic concurrency:
//
//	type Invoice struct {
//	    domain.VersionedRoot[InvoiceID]
//	    Total Money
//	}
//
// Everything AggregateRoot promises still holds — identity set once at
// construction, events accumulated by Raise and drained by PullEvents. The
// version is the only addition, and it is meaningful only to repositories.
//
// The concurrency note on AggregateRoot applies unchanged: an aggregate is
// confined to one goroutine at a time. The version does not make it safe to
// share — it makes concurrent *writers* of the same row detect each other,
// which is a different problem.
type VersionedRoot[T ID] struct {
	AggregateRoot[T]
	version int64
}

// NewVersionedRoot returns a versioned root at version 0 — never persisted.
// It is the constructor an aggregate's own constructor calls, exactly as
// NewAggregateRoot is for the unversioned case.
func NewVersionedRoot[T ID](id T) VersionedRoot[T] {
	return VersionedRoot[T]{AggregateRoot: NewAggregateRoot(id)}
}

// ReconstituteVersionedRoot returns a versioned root as it was stored: its
// identity and the version the row carried. It is the LOAD path, and the
// version it takes is the one a later Save checks against — reconstituting at
// 0 would turn every update into an insert.
//
// It raises no events. Reconstitution replays what already happened; the
// facts were published when they occurred.
func ReconstituteVersionedRoot[T ID](id T, version int64) VersionedRoot[T] {
	return VersionedRoot[T]{AggregateRoot: NewAggregateRoot(id), version: version}
}

// Version reports the version this aggregate was loaded at, 0 if it has never
// been persisted.
func (a *VersionedRoot[T]) Version() int64 { return a.version }

// SetVersion records the version the aggregate now holds. It is called by
// repositories — at reconstitution, and after a write succeeds.
func (a *VersionedRoot[T]) SetVersion(v int64) { a.version = v }
