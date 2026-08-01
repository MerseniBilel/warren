# `github.com/MerseniBilel/warren/domain` — SPEC

| | |
|---|---|
| **Status** | **Approved (2026-08-01)** — the type system compiles as amended; conditions: `Specification.ToSQL` placement (Open question 1) and the event-payload seam (Open question 4) settled before the persistence and outbox seams are built |
| **Source** | [warren.md §3.1](../warren.md) |
| **Module** | core |
| **Mode** | Build |
| **Wraps** | — |

## Problem

Warren's third claim is "**DDD as real types**, not folder naming conventions"
(AGENT.md § What Warren is). A framework that only prescribes a directory layout
gives the compiler nothing to check: an "aggregate" is a struct like any other,
a "domain event" is whatever the author remembered to publish, and the atomicity
between a state change and the event announcing it is a convention that decays
on the first busy afternoon.

`warren/domain` is the package that turns those concepts into types the rest of
the framework can name. It sits in the CONTRACTS ring — "pure interfaces, zero
implementations, in the core module. This is what lets an adapter and a user's
domain package depend on the same type without meeting" (§3 preamble). The
concrete consequence: `persistence.Repository` is parameterised over a domain
type, `UnitOfWork.Do` drains `PullEvents()` from aggregates it never imported,
and the user's `package domain` and `warren/persistence/postgres` share a
vocabulary without either one importing the other.

Two properties are load-bearing:

- **Events accumulate on the aggregate.** "Nothing is published until the
  `UnitOfWork` commits — that's what makes state changes and event publication
  atomic" (§3.1). `Raise` records; it does not publish.
- **Domain code returns semantic errors.** `errors.Conflict("user already
  active")` in the §3.1 example, never an HTTP status. The error table (§2.6) is
  what lets that hold.

## Goals

1. Give an identity-carrying entity base and an aggregate root base as **real
   generic types**, so `domain.ID` can constrain a repository's key type.
2. Make event collection a property of the aggregate, drained exactly once, by a
   caller that the domain does not know about (§3.1, §3.3 step 3).
3. Define `Event` as the minimum a fact must expose to be routed, correlated,
   and stored: name, time, aggregate identity.
4. Define `Specification[T]` so a query predicate is a domain concept that a
   repository *may* translate, rather than SQL smeared through the use cases.
5. Import nothing above the KERNEL ring. §10 fixes the budget exactly: the
   user's domain package imports `warren/domain` and `warren/errors` and nothing
   else.

## Non-goals

- **No persistence.** No mapping, no ORM, no struct tags, no table names. The
  ORM is a deliberate omission framework-wide (§3.3).
- **No event publication.** `Raise` does not reach a broker. Publication is the
  `UnitOfWork` → outbox → relay path (§1.5, §3.3, §5.5).
- **No event bus, dispatcher, or in-process handler registry.** `Event` is a
  value; routing belongs to `broker` and the messaging runtime.
- **No event sourcing.** §3.1's aggregate carries current state plus a pending
  event slice; nothing in warren.md rebuilds state from an event log.
- **No validation.** Struct-tag validation is `warren/validate` (§2.7), invoked
  by transport adapters after decode — "handlers never invoke it", and neither
  does the domain.
- **No base-class service layer.** Use cases are `app.Handler` (§3.2), not
  methods hung off an aggregate.

## Public API

Taken from warren.md §3.1 verbatim; doc comments added.

```go
// Package domain provides the DDD building blocks Warren's other contracts are
// expressed in terms of: identity, aggregates, events, and specifications.
//
// It contains no persistence, no transport, and no publication. An aggregate
// records events; the persistence.UnitOfWork drains and stores them.
package domain

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
func (e *Entity[T]) ID() T

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
type AggregateRoot[T ID] struct {
	Entity[T]
	events []Event
}

// NewAggregateRoot returns an aggregate root with its identity set at
// construction. It is called by the aggregate's own constructor — the
// aggregate mints its identifier before the first event is raised — and by
// the reconstitution path a repository loads through.
func NewAggregateRoot[T ID](id T) AggregateRoot[T]

// Raise records a domain event on the aggregate. It publishes nothing.
func (a *AggregateRoot[T]) Raise(e Event)

// PullEvents returns the events raised since the last call and clears them from
// the aggregate. It is drained by the unit of work inside the business
// transaction; calling it elsewhere loses events.
func (a *AggregateRoot[T]) PullEvents() []Event

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
```

Usage, from §3.1 — the shape every generated aggregate follows:

```go
package domain // the user's package, not warren's

type User struct {
	domain.AggregateRoot[UserID]
	Email  Email
	Name   string
	Status Status
}

func NewUser(email Email, name string) *User {
	u := &User{
		AggregateRoot: domain.NewAggregateRoot(NewUserID()),
		Email:  email,
		Name:   name,
		Status: StatusPending,
	}
	u.Raise(UserRegistered{UserID: u.ID(), Email: email.String(), At: time.Now()})
	return u
}

func (u *User) Activate() error {
	if u.Status == StatusActive {
		return errors.Conflict("user already active")
	}
	u.Status = StatusActive
	u.Raise(UserActivated{UserID: u.ID(), At: time.Now()})
	return nil
}
```

## Behaviour

**Identity.** `Entity[T]` holds its identifier in an unexported field, read
through the `ID()` accessor. The identifier is minted by the aggregate's own
constructor: `NewUser` calls `domain.NewAggregateRoot(NewUserID())` (§3.1,
§10), so identity exists before the first event is raised and is never
reassigned afterwards. Repositories reconstitute a loaded aggregate through
that same constructor path — `NewAggregateRoot` with the stored identifier —
never by writing a field. The `ID` constraint requires `comparable` so
identifiers work as map keys and as `Repository[T, ID]`'s key parameter
(§3.3), and `fmt.Stringer` so an identifier can be logged, put in a URL path
(`/users/{id}`, §3.5), or used as a `Message.Key` (§3.4) without the caller
knowing its concrete type. `Root[T]` exists because a struct cannot serve as a
Go type constraint: it is the interface `Repository[T domain.Root[ID],
ID domain.ID]` is generic over, and `AggregateRoot` satisfies it.

**Aggregates are pointers, confined to one goroutine.** Two contracts the
2026-08-01 review made explicit, stated in the doc comments where they are
read: copying an aggregate *value* after events are raised aliases the
pending-event array — two copies then clobber each other's `Raise`d events —
so §3.1's `*User`-everywhere pattern is the contract, not a style choice; and
an aggregate is a consistency boundary, not a shared structure — concurrent
`Raise` is a data race by design. Mechanical enforcement (a `noCopy` field)
was considered and rejected: `go vet` would then flag the legitimate
`AggregateRoot: domain.NewAggregateRoot(...)` construction copy that §3.1
itself prescribes. Likewise, identity exists only through `NewAggregateRoot`:
the zero-value aggregate silently carries the zero identifier, so
repositories must reconstitute through the constructor — the generated
repository template (Open question 3) must respect this.

**Event accumulation.** `Raise` appends to the aggregate's pending events.
Nothing observes them until something drains them. `PullEvents` is
destructive — the name and the "drained by UnitOfWork" comment in §3.1 both say
so, and step 3 of `UnitOfWork.Do` (§3.3) is the only drain warren.md names.

**Atomicity is not this package's mechanism, but it is this package's reason.**
The sequence that matters spans three packages and is stated in §3.3 and shown
in §10: `Handle` calls `uow.Do`; inside the transaction the repository saves the
aggregate; the unit of work drains `PullEvents()` and inserts those events into
the outbox in the same transaction; commit makes state and events atomic. An
aggregate that published on `Raise` would break that guarantee, which is why
`Raise` does not publish.

**Specifications.** `IsSatisfiedBy` is the in-memory evaluation and is always
available. `ToSQL` is the translation hook — "repositories may translate"
(§3.1). warren.md states no obligation on a non-SQL repository, and this spec
adds none; see Open questions.

**Ring position.** `domain` sits in CONTRACTS and may import KERNEL packages
only. §10 pins the actual import list for user domain code to `warren/domain`
and `warren/errors`. Nothing in this package may import `app`, `persistence`,
`broker`, or `transport`, and nothing may import an adapter.

## Errors

No function in this package returns an `error`. Nothing here can fail: `Raise`
appends, `PullEvents` drains.

Domain code written *against* this package returns the `warren/errors` semantic
vocabulary — §3.1's `Activate` returns `errors.Conflict("user already active")`,
and §10's use case returns `errors.Invalid("email", err)`. Each adapter owns the
translation to a status code via the table in §2.6; a domain method that maps a
code to HTTP 409 itself has broken ring 2 (AGENT.md § The error table is
load-bearing).

## Testing

**Contract suite.** `domain` has no driver, so the reusable suite here is an
**aggregate-semantics suite** that any user or generated aggregate can be run
through, plus an **event conformance suite** for `Event` implementations. Per
AGENT.md, it is written and updated before anything that consumes it — in this
case before `persistence`'s unit-of-work suite, which depends on drain
semantics. It must cover:

- `Raise` then `PullEvents` returns the events in the order raised.
- `PullEvents` is destructive: a second call with no intervening `Raise` returns
  no events. This is the property `UnitOfWork.Do` relies on to avoid publishing
  a fact twice.
- `Raise` publishes nothing and touches no external state — asserted by running
  an aggregate with no broker, no repository, and no context in scope at all.
- Identity: two aggregates with equal identifiers compare equal on identity;
  identifiers work as map keys (the `comparable` requirement) and round-trip
  through `String()`.
- `Event` conformance: `EventName()` is stable and non-empty across calls,
  `AggregateID()` matches the raising aggregate's identifier, `OccurredAt()` is
  the time the fact happened and does not change on later calls.
- `Specification`: `IsSatisfiedBy` and the predicate implied by `ToSQL` agree on
  a shared table of candidates — the only check that keeps the two renderings of
  one specification from drifting.

**Constraints.** Unit tests only: no Docker, no network, no sleeps (AGENT.md
§ Testing). No mocking framework; hand-written fakes only. `t.Parallel()` and
table-driven subtests named for behaviour.

**Benchmarks.** `Raise` and `PullEvents` sit inside `Handle`, which is on the
request path, so both get allocation benchmarks — invariant 7 is a performance
claim and needs a number behind it. An aggregate that raises no events must not
allocate an event slice.

## Definition of done

- [x] Every identifier above compiles with the doc comments shown, in the core
      module, importing only `fmt` and `time` — `domain/domain.go`, 2026-08-01.
- [x] `go list -deps` on this package shows standard library only — verified
      2026-08-01: `fmt`, `time`, and their transitive closure; no `dig`, no
      adapter, no sibling contract package.
- [x] The aggregate-semantics and event conformance suites exist and pass —
      `domain/domain_test.go`. **Not yet exported:** an exported suite needs a
      package (`domain/domaintest` or `warren/testing`) that warren.md does not
      yet describe, and adding one requires a manifest decision first. Same
      question as errors/SPEC.md Open question 8; decide before `persistence`'s
      unit-of-work suite is written, and export the suites then.
- [x] Allocation benchmarks for `Raise` and `PullEvents` are committed with
      their recorded numbers — 2026-08-01, Apple M-series (`-11`):
      `BenchmarkRaiseAndPull` 43 ns/op, 64 B/op, 2 allocs/op;
      `BenchmarkPullEventsEmpty` 2.7 ns/op, 0 allocs/op; a quiet aggregate
      allocates no event slice (asserted by test).
- [x] The §3.1 usage example compiles verbatim as a test —
      `domain/example_section31_test.go`: `NewUser` and `Activate` are
      warren.md's bodies unchanged, and the double-`Activate` path is asserted
      to return a `CONFLICT` error.
- [ ] Open questions 1, 3, 4, and 6 below are answered by the human and this
      spec is corrected in the same change. None of them blocked this package:
      1 and 4 gate the persistence/outbox seams, 3 gates the generated
      repository template, 6 is a wording decision on warren.md §3.1.

## Open questions

1. **`Specification.ToSQL()` puts a persistence concern in the domain contract
   package.** §3.1 defines `ToSQL() (clause string, args []any)` on a type in
   `warren/domain`, while §3.3's headline is "the deliberate omission is an ORM"
   and AGENT.md's application-layer rule is "domain imports NOTHING from the
   other three". A domain specification that renders SQL knows its persistence
   technology by name. It is also unusable by the Mongo and Redis adapters
   (§6.2–6.4), which have no SQL. **Flagged, not resolved.** The options warren.md
   leaves open: keep it and accept SQL as a domain-visible detail; move the
   translation to a persistence-side `SpecificationTranslator`; or make
   `Specification` a marker with translation entirely in the adapter. This is a
   port-shape decision and belongs to the human.

2. **RESOLVED — `Entity[T]` identity surface.** §3.1 as amended declares
   `func (e *Entity[T]) ID() T` as the accessor and
   `func NewAggregateRoot[T ID](id T) AggregateRoot[T]` as the way identity is
   set — at construction, by the aggregate's own constructor
   (`domain.NewAggregateRoot(NewUserID())` in both §3.1 and §10). The
   identifier is minted by the aggregate's constructor, not by the repository
   or the database. The field stays unexported; there is no setter.

3. **§6.1's generated repository still scans into an exported field.** The
   accessor question is settled — `ID()` exists and identity is set through
   `NewAggregateRoot` — but §6.1's generated code still reads
   `row.Scan(&u.ID, &u.Email, &u.Name, &u.Status)`, treating `ID` as an
   addressable exported field on `domain.User`, which the §3.1 definition makes
   impossible: the field is `Entity[T].id`, unexported and in a different
   package. The generated-repository template needs to be redesigned around
   reconstitution — loading through the constructor path
   (`NewAggregateRoot` with the stored identifier) rather than scanning into
   fields. warren.md has not yet amended §6.1 to match.

4. **Does an `Event` carry a payload, and how does it become
   `broker.Message.Payload []byte`?** `Event` exposes only name, time, and
   aggregate ID. §3.3 step 4 inserts events into the outbox and §5.5's relay
   drains them to the broker as messages with a `[]byte` payload. Nothing in
   warren.md defines the serialisation boundary — whether `Event` gains a
   marshalling method, whether the outbox writer JSON-encodes the concrete type,
   or whether a codec is registered at boot. This is the seam between §3.1 and
   §3.4 and it is currently unspecified.

5. **RESOLVED — `AggregateRoot` does not violate invariant 5.** AGENT.md now
   words the invariant as contract packages being "interfaces, types, and pure
   functions", with one named exception for the concrete transport registrars
   (§3.5). Under that wording `Raise` and `PullEvents` — methods on a type,
   holding no driver — are cleanly permitted; the "zero implementations"
   phrasing is the shorthand, not the rule `warren lint arch` will encode.

6. **`Event.AggregateID() string` versus `ID` being a generic type parameter.**
   Everything else in the package is parameterised over `T ID`; the event
   interface flattens identity to `string`. Deliberate (so the outbox and broker
   handle one shape), or an oversight? If deliberate it is worth stating,
   because it makes `fmt.Stringer` on `ID` load-bearing rather than
   conventional.
