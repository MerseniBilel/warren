// Package domain provides the DDD building blocks Warren's other contracts are
// expressed in terms of: identity, aggregates, events, and specifications.
//
// It contains no persistence, no transport, and no publication. An aggregate
// records events; the persistence.UnitOfWork drains and stores them.
package domain

import (
	"fmt"
	"time"
)

// ID constrains the identifier type of an entity. An identifier must be usable
// as a map key and must render itself for logs, URLs, and message keys.
type ID interface {
	comparable
	fmt.Stringer
}

// Entity is the identity-carrying base of a domain entity. Two entities are the
// same entity when their identifiers are equal, regardless of their other
// fields.
type Entity[T ID] struct{ id T }

// ID returns the entity's identifier. It is the identity accessor: the
// identifier itself is set at construction and never reassigned.
func (e *Entity[T]) ID() T { return e.id }

// Root is the constraint repositories are generic over. AggregateRoot
// satisfies it. (A struct cannot serve as a Go type constraint — only this
// interface makes Repository[T Root[K], K ID] expressible.)
type Root[T ID] interface {
	ID() T
	PullEvents() []Event
}

// AggregateRoot is the consistency boundary of a cluster of entities and the
// only object a repository loads or saves. It accumulates the domain events
// raised while its invariants were enforced; those events are not published
// until the unit of work commits.
//
// An aggregate is used through a pointer and confined to one goroutine at a
// time — it is a consistency boundary, not a shared structure. Copying an
// aggregate value after events are raised aliases the pending-event array,
// and two copies then corrupt each other's events; §3.1's pattern (*User
// everywhere after construction) is the contract, not a style choice.
type AggregateRoot[T ID] struct {
	Entity[T]
	events []Event
}

// NewAggregateRoot returns an aggregate root with its identity set at
// construction. It is called by the aggregate's own constructor — the
// aggregate mints its identifier before the first event is raised — and by
// the reconstitution path a repository loads through.
//
// It is the only way identity is set: an aggregate assembled without it — the
// zero value — silently carries the zero identifier, which a repository would
// save under an empty key. Repositories reconstitute through this
// constructor, never by filling in the struct.
func NewAggregateRoot[T ID](id T) AggregateRoot[T] {
	return AggregateRoot[T]{Entity: Entity[T]{id: id}}
}

// Raise records a domain event on the aggregate. It publishes nothing. Like
// every aggregate method it assumes single-goroutine confinement; concurrent
// Raise is a data race by design, not an oversight.
func (a *AggregateRoot[T]) Raise(e Event) {
	a.events = append(a.events, e)
}

// PullEvents returns the events raised since the last call and clears them from
// the aggregate. It is drained by the unit of work inside the business
// transaction; calling it elsewhere loses events.
func (a *AggregateRoot[T]) PullEvents() []Event {
	events := a.events
	a.events = nil
	return events
}

// Event is a fact that has already happened in the domain. Implementations are
// values: named, timestamped, and attributable to one aggregate.
type Event interface {
	// EventName returns the stable, dotted name of the fact, such as
	// "user.registered". It is the topic and type key for messaging.
	EventName() string
	// OccurredAt returns when the fact happened, not when it was published.
	OccurredAt() time.Time
	// AggregateID returns the identity of the aggregate the fact belongs to.
	AggregateID() string
}

// Specification is a reusable predicate over T, expressed once and usable both
// in memory and, where a repository chooses to translate it, in a query.
type Specification[T any] interface {
	// IsSatisfiedBy reports whether the candidate matches the specification.
	IsSatisfiedBy(T) bool
	// ToSQL renders the specification as a SQL fragment and its arguments.
	// Repositories may translate it; nothing requires them to.
	ToSQL() (clause string, args []any)
}
