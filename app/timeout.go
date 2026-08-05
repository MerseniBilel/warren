package app

import (
	"context"
	"time"
)

// Timeout bounds the handler with a derived deadline.
//
// # Where you put it decides what it bounds
//
// Composed INSIDE Retrying it bounds each ATTEMPT; composed OUTSIDE it bounds
// the whole retried SEQUENCE. Chain applies its middleware so that the last
// argument is nearest the handler:
//
//	app.Chain(h, app.Retrying(p), app.Timeout(3*time.Second)) // per attempt
//	app.Chain(h, app.Timeout(3*time.Second), app.Retrying(p)) // per sequence
//
// Those are materially different systems — three attempts of up to three
// seconds each, against three attempts sharing three seconds — and that is
// exactly why there is no Policy(Timeout(...), Retry(...)) combinator. A
// combinator would hide, inside a DSL, a choice Chain already makes visible
// on one line.
//
// # It signals; it does not interrupt
//
// A handler that ignores its context runs to completion, exactly as
// transport/http's ShutdownTimeout documents for the drain. Go has no way to
// kill a goroutine. What the deadline does is reach every adapter that
// respects a context — postgres, kafka, any http.Client you build — so the
// work a handler delegates is bounded even when the handler itself is not.
//
// A caller's SHORTER deadline always wins: context.WithTimeout never extends
// one, so a request already bounded at 100ms stays bounded at 100ms.
//
// The expiry surfaces as whatever the handler returns. An adapter that
// respects the context typically returns CodeUnavailable — postgres does —
// which app.Retrying then retries and every transport maps to 503.
func Timeout[Req, Res any](d time.Duration) Middleware[Req, Res] {
	if d <= 0 {
		panic("app: Timeout composed with a non-positive duration — a deadline already in the past would fail every request; omit the middleware instead")
	}
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return HandlerFunc[Req, Res](func(ctx context.Context, req Req) (Res, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next.Handle(ctx, req)
		})
	}
}
