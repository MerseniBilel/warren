package app

import "time"

// Clock is where a handler gets the current time.
//
// It is a FUNCTION TYPE, not an interface, for the same reason Middleware is:
// there is one method, so an interface would demand a struct per
// implementation and buy nothing. The system clock is `app.Clock(time.Now)`
// and a test clock is a closure over a variable — no fake type, no mocking
// library, which is the standard AGENT.md holds this repository to.
//
// It exists because every service needs it and, without a name here, every
// feature module invents its own — a `type Clock func() time.Time` per
// module, each private to that module because a provider is. Naming it once
// means a test substitutes one thing, and a handler's signature says what it
// depends on.
//
// Warren does NOT provide a default. A clock is a dependency like any other
// and the graph should say where it came from; `warren new` wires
// `app.Clock(time.Now)` in the platform module, which is the shape to copy.
//
//	func NewCollectParcel(clock app.Clock) app.Handler[…] {
//	    return &collectParcel{now: clock}
//	}
//
//	// in a test
//	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
//	h := NewCollectParcel(func() time.Time { return at })
type Clock func() time.Time

// Now returns the time, so a Clock reads as a clock at the call site when the
// caller prefers a method to a call.
func (c Clock) Now() time.Time { return c() }
