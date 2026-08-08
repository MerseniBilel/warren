// Package app defines Warren's central abstraction: a transport-agnostic use
// case, and the core-ring middleware shape that decorates it.
//
// A Handler is written once and exposed over HTTP, gRPC, and message
// consumers by adapters this package knows nothing about. A Middleware wraps
// the handler rather than the protocol, so a transaction decorator or retry
// policy is written once and applies everywhere — the core ring of the
// two-ring model in warren.md §1.4; transport-shaped concerns are the edge
// ring, owned by each adapter.
package app

import (
	"context"
	"fmt"
)

// Handler is a use case: one request in, one response out, plus an error
// drawn from the warren/errors vocabulary. It is the unit every transport
// adapter wraps and every core middleware decorates.
//
// A Handler imports no transport package. That is the framework's whole
// point.
type Handler[Req, Res any] interface {
	Handle(ctx context.Context, req Req) (Res, error)
}

// HandlerFunc adapts a bare function to Handler — how middleware wrap
// handlers without declaring a struct each time. Like net/http.HandlerFunc,
// a nil HandlerFunc panics when called; Chain refuses nil handlers at
// composition time, so a nil can only be called by bypassing Chain.
type HandlerFunc[Req, Res any] func(ctx context.Context, req Req) (Res, error)

// Handle calls f.
func (f HandlerFunc[Req, Res]) Handle(ctx context.Context, req Req) (Res, error) {
	return f(ctx, req)
}

// Middleware decorates a Handler with a cross-cutting concern. Because it
// wraps the handler rather than the protocol, one middleware applies
// identically to HTTP, gRPC, and consumers.
//
// A middleware must return the handler's error with its warren/errors code
// intact — unchanged or wrapped with %w — because the adapter downstream
// reads the code to pick a status. Flattening CodeConflict into CodeInternal
// silently turns a 409 into a 500 and a DLQ message into a nack.
type Middleware[Req, Res any] func(Handler[Req, Res]) Handler[Req, Res]

// Chain composes middleware around a handler and returns the composed
// handler. mw[0] is the outermost: the first to see the request and the last
// to see the response, so the argument order reads in execution order.
//
// Chain runs at boot, not per request — the result is stored in the route
// table as a pre-built closure, and invoking it allocates nothing.
//
// A nil handler, a nil middleware, or a middleware that returns nil panics
// here, at composition time, with a message naming the position — a startup
// crash instead of the request-time nil dereference each of them would
// otherwise become. This is the boot-time panic AGENT.md § General names as
// sanctioned alongside di.MustResolve: Chain cannot return an error (the
// §3.2 signature is fixed), and deferring the failure to request 1 is the
// exact outcome the boot-ordering rule exists to prevent. Note a conditional
// middleware belongs in the slice only when enabled — append it, don't leave
// a nil hole.
// THE FIRST MIDDLEWARE IS THE OUTERMOST ONE, and the loop below is why: it
// wraps from the end, so mw[0] is applied last and therefore sees the request
// first. Chain(h, A, B) is A(B(h)).
//
// That ordering is load-bearing for exactly one pair, and both spellings
// compile: Retrying BEFORE Transactional gives each attempt its own
// transaction, and the reverse wraps one transaction around every attempt. A
// field test wrote the reverse and measured zero retries across eight
// concurrent requests, with two callers refused for stock that existed.
func Chain[Req, Res any](h Handler[Req, Res], mw ...Middleware[Req, Res]) Handler[Req, Res] {
	if h == nil {
		panic("app: Chain composed around a nil handler — the handler must exist before boot step 5 builds the route table")
	}
	// Seeded from the handler we were GIVEN, because
	// Chain(Chain(h, Retrying(p)), Transactional(uow)) is the same mistake
	// spelled in two calls.
	sawRetry := containsRetrying[Req, Res](h)
	for i := len(mw) - 1; i >= 0; i-- {
		if mw[i] == nil {
			panic(fmt.Sprintf("app: Chain middleware %d of %d is nil — append a conditional middleware only when it is enabled", i+1, len(mw)))
		}
		h = mw[i](h)
		if h == nil {
			panic(fmt.Sprintf("app: middleware %d of %d returned a nil handler — a middleware must return a handler, usually by wrapping the one it was given", i+1, len(mw)))
		}
		switch h.(type) {
		case retryingHandler[Req, Res]:
			sawRetry = true
		case transactionalHandler[Req, Res]:
			// Transactional is being applied OUTSIDE a Retrying that is
			// already in the stack, so one transaction would wrap every
			// attempt. There is no correct reading of that — see the panic.
			if sawRetry {
				panic(errTransactionOutsideRetry(i+1, len(mw)))
			}
		}
	}
	return h
}

// containsRetrying walks a handler's wrappers looking for a Retrying. It sees
// Warren's own middleware and stops at a user's, which is the stated limit of
// the check.
func containsRetrying[Req, Res any](h Handler[Req, Res]) bool {
	for {
		if _, ok := h.(retryingHandler[Req, Res]); ok {
			return true
		}
		c, ok := h.(composed[Req, Res])
		if !ok {
			return false
		}
		h = c.inner()
	}
}

// errTransactionOutsideRetry refuses the one composition in Chain that has no
// correct reading.
//
// An architect ruling (2026-08-08) established that it is never legitimate,
// overturning an earlier decision of mine to document it and move on. The
// argument that changed it: Retrying re-invokes the whole HANDLER, not the
// one flaky call, so the "retry a blipping dependency inside the transaction"
// case only works when the handler has staged nothing yet — an unenforceable
// property — and even then the correct ordering does the same work without
// holding a transaction open across the backoff.
func errTransactionOutsideRetry(pos, total int) string {
	return fmt.Sprintf(`app: Transactional is composed OUTSIDE Retrying (middleware %d of %d), so ONE
transaction wraps every retry attempt instead of each attempt getting its own.

  Chain's FIRST middleware is the OUTERMOST one, so this:

      app.Chain(h, app.Transactional(uow), app.Retrying(policy))

  is written the other way round:

      app.Chain(h, app.Retrying(policy), app.Transactional(uow))

  As composed, a retry re-invokes the whole handler inside the transaction that
  is already open, with the writes the failed attempt staged still in it. On a
  driver that checks versions at COMMIT the conflict is raised outside the retry
  loop, so Retrying never sees it and retries ZERO times — a field test measured
  eight handler attempts for eight concurrent requests, and two callers refused
  for stock that existed. On Postgres the version check runs inside the handler,
  so the second attempt does run — in the same transaction, and it commits the
  first attempt's writes along with its own.

  To retry one flaky outbound call rather than the whole handler, retry it in the
  adapter behind its port (warren.md §7.3), not here.

  This check sees app.Transactional and app.Retrying. A hand-rolled transaction
  middleware carries no such marker and is not checked.`, pos, total)
}
