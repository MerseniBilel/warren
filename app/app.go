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

import "context"

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
// handlers without declaring a struct each time.
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
func Chain[Req, Res any](h Handler[Req, Res], mw ...Middleware[Req, Res]) Handler[Req, Res] {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
