package domain

import "uuid"

// IDs generates fresh aggregate identifiers.
//
// A function type rather than an interface, for the same reason app.Clock is:
// one method, so an interface would demand a struct and buy nothing.
//
// It returns a string because an identifier's TYPE belongs to the aggregate —
// ParcelID, UserID — and a shared generator cannot know it. A feature
// converts at the point of creation, which is one line and keeps the typed id
// where the type system can see it:
//
//	func NewParcel(ids domain.IDs, at time.Time) *Parcel {
//	    return &Parcel{AggregateRoot: domain.NewAggregateRoot(ParcelID(ids()))}
//	}
//
// Warren provides no default, for the reason app.Clock gives.
type IDs func() string

// UUIDs returns an IDs backed by UUID version 7.
//
// Version 7 rather than 4 because it is TIME-ORDERED: the first 48 bits are a
// millisecond timestamp, so ids sort by creation and a B-tree index on the
// primary key appends instead of writing into the middle of the tree. On a
// table that only grows, that is the difference between an index that stays
// dense and one that fragments.
//
// It costs no dependency. `uuid` entered the STANDARD LIBRARY in Go 1.27, so
// this satisfies AGENT.md invariant 1 — standard library plus dig — without
// argument. Before 1.27 this function could not have existed here, and
// Warren's own correlation IDs still use a cheaper counter scheme because
// they are minted per REQUEST rather than per aggregate; see
// transport/http/edge.go for that measurement.
func UUIDs() IDs {
	return func() string { return uuid.NewV7().String() }
}
