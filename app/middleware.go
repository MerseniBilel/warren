package app

import (
	"context"
	stderrors "errors"
	"reflect"
	"time"

	"github.com/MerseniBilel/warren/errors"
)

// RetryPolicy decides whether a failed attempt is retried and how long to
// wait first. It is the port warren/resilience implements (wrapping backoff
// and circuit-breaker libraries); the kernel never sees those libraries.
type RetryPolicy interface {
	// Next reports whether a retry should follow the given completed attempt
	// (1-based) and how long to wait before it. Returning retry == false
	// ends the loop; the handler's last error is returned as-is.
	//
	// Termination is the policy's contract: the middleware imposes no
	// attempt ceiling of its own, so a policy that never returns
	// retry == false retries a persistent failure forever — with a zero
	// delay, as a hot loop. Every real policy bounds its attempts.
	Next(attempt int) (delay time.Duration, retry bool)
}

// AuthorizationPolicy decides whether the identity on the context may
// proceed. It is the port warren/auth implements; the edge ring
// authenticates and puts identity on the context, this policy authorizes.
//
// The contract: return nil to allow. Return an error from the warren/errors
// vocabulary to deny — errors.PermissionDenied(action) for a known caller
// that may not act, errors.Unauthenticated(reason) when identity is absent —
// and the middleware returns it unchanged, so the adapter maps the right
// status. An error outside the vocabulary maps downstream to INTERNAL, the
// safe default for the unknown.
type AuthorizationPolicy interface {
	Authorize(ctx context.Context) error
}

// Telemetry is the core-ring instrumentation seam Traced and Metered speak
// to. warren/observability implements it over OpenTelemetry and seeds it on
// the context at the edge; the kernel never imports a telemetry SDK. When no
// Telemetry is on the context, Traced and Metered are exact pass-throughs.
type Telemetry interface {
	// Span opens a span named name and returns the derived context and the
	// function that ends the span, recording err (nil on success). The
	// returned context MUST derive from ctx, preserving its values — Metered
	// and HandlerName downstream read through it, and a Span that returns a
	// fresh context silently disables them.
	Span(ctx context.Context, name string) (context.Context, func(err error))

	// Record records one handler invocation: its name, its duration, and its
	// error — nil on success. Implementations key error counters by the
	// warren/errors code.
	Record(name string, d time.Duration, err error)
}

type telemetryKey struct{}

type handlerNameKey struct{}

// WithTelemetry returns a copy of ctx carrying t. It is the seeding side of
// the instrumentation seam, called by observability's edge integration; a
// nil t — a typed-nil pointer inside the interface included — is not
// carried, so the never-instrumented path stays a pass-through instead of a
// request-time panic in Span.
//
// The typed-nil probe uses one constant-time reflect nil check. Invariant 7
// forbids type-driven dispatch and container consultation on the request
// path; a nil probe is neither, and the alternative is exactly the
// production panic the invariant exists to prevent.
func WithTelemetry(ctx context.Context, t Telemetry) context.Context {
	if t == nil || isTypedNil(t) {
		return ctx
	}
	return context.WithValue(ctx, telemetryKey{}, t)
}

func isTypedNil(t Telemetry) bool {
	v := reflect.ValueOf(t)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// TelemetryFromContext returns the Telemetry carried by ctx, or nil.
func TelemetryFromContext(ctx context.Context) Telemetry {
	t, _ := ctx.Value(telemetryKey{}).(Telemetry)
	return t
}

// WithHandlerName returns a copy of ctx carrying the handler's qualified
// name, "<module>.<handler>". The transport adapter seeds it in the route
// table's pre-built closure at boot step 5 — it is the one party that knows
// both names — and Traced and Metered read it back.
func WithHandlerName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, handlerNameKey{}, name)
}

// StampHandlerName returns a function that does what WithHandlerName does,
// with the boxing paid once here instead of on every call.
//
// context.WithValue takes an `any`, so passing a string boxes it — an
// allocation, per request, on every HTTP, gRPC and event route. A route's
// handler name is fixed at boot, so that allocation belongs at boot too.
// Measured on an M3 Pro: 25.4 ns and 2 allocs (64 B) becomes 14.2 ns and 1
// alloc (48 B).
//
// Use WithHandlerName anywhere the name is not fixed in advance; this exists
// for the request path, and the request path alone.
func StampHandlerName(name string) func(context.Context) context.Context {
	boxed := any(name)
	return func(ctx context.Context) context.Context {
		return context.WithValue(ctx, handlerNameKey{}, boxed)
	}
}

// HandlerName returns the qualified handler name carried by ctx, or "".
func HandlerName(ctx context.Context) string {
	name, _ := ctx.Value(handlerNameKey{}).(string)
	return name
}

// Retrying re-invokes the handler when the error's OUTERMOST code is
// CodeUnavailable — the one retryable code. The outermost code decides,
// exactly as an adapter's status mapping does, because wrapping is
// recategorization: a handler that wraps an Unavailable inside Internal has
// declared the failure non-retryable, and this middleware agrees. A plain
// %w wrap (fmt.Errorf) leaves the outermost Warren error untouched and
// stays retryable. Every other code returns unretried.
//
// When retries exhaust, or the context is cancelled during a wait, the
// handler's LAST error is returned: it is the freshest and still carries the
// code the adapter maps. A context that is already cancelled still reaches
// the handler once — the handler owns its own context checks; this
// middleware observes cancellation between attempts and during waits. A nil
// policy panics at composition time, like Chain's guards.
func Retrying[Req, Res any](policy RetryPolicy) Middleware[Req, Res] {
	if policy == nil {
		panic("app: Retrying composed with a nil policy — construct the policy before boot step 5 composes the route table")
	}
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return HandlerFunc[Req, Res](func(ctx context.Context, req Req) (Res, error) {
			for attempt := 1; ; attempt++ {
				res, err := next.Handle(ctx, req)
				if err == nil || !retryable(err) {
					return res, err
				}
				delay, retry := policy.Next(attempt)
				if !retry {
					return res, err
				}
				if delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						return res, err
					}
				} else if ctx.Err() != nil {
					return res, err
				}
			}
		})
	}
}

// retryable reports whether the OUTERMOST Warren code is CodeUnavailable —
// the adapter's own reading of the error, not a chain search: errors.Is
// would find an Unavailable buried under a recategorizing Internal wrap and
// retry a failure the handler declared final.
func retryable(err error) bool {
	var e *errors.Error
	if stderrors.As(err, &e) {
		return e.Code() == errors.CodeUnavailable
	}
	return false
}

// Authorized runs the policy before invoking the handler; a denial
// short-circuits — the handler is never called — and the policy's error
// returns unchanged, code intact. Because it is core-ring, the same check
// applies to HTTP, gRPC, and consumers. A nil policy panics at composition
// time, like Chain's guards.
func Authorized[Req, Res any](policy AuthorizationPolicy) Middleware[Req, Res] {
	if policy == nil {
		panic("app: Authorized composed with a nil policy — construct the policy before boot step 5 composes the route table")
	}
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return HandlerFunc[Req, Res](func(ctx context.Context, req Req) (Res, error) {
			if err := policy.Authorize(ctx); err != nil {
				var zero Res
				return zero, err
			}
			return next.Handle(ctx, req)
		})
	}
}

// UnitOfWork is the transaction seam Transactional speaks to — the shape
// warren/persistence's UnitOfWork already has. It is declared here rather
// than imported so app stays free of every sibling contract: the middleware
// needs one method, and a port with one method costs less than a coupling.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(context.Context) error) error
}

// Transactional wraps Handle in a unit of work, so the aggregate state the
// handler wrote and the outbox rows for the events it raised commit in one
// transaction — or neither does.
//
// A handler that calls uow.Do itself under this middleware is joined, not
// nested: warren.md §10 shows both patterns, and the unit of work's own
// contract makes the inner call join the transaction in scope. The
// transaction's error passes through with its code intact, so a serialization
// failure arrives as UNAVAILABLE and app.Retrying — composed OUTSIDE this
// middleware — re-runs the whole transaction rather than retrying inside a
// doomed one. A nil unit of work panics at composition time, like Chain's
// guards.
func Transactional[Req, Res any](uow UnitOfWork) Middleware[Req, Res] {
	if uow == nil {
		panic("app: Transactional composed with a nil unit of work — construct it before boot step 5 composes the route table")
	}
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return HandlerFunc[Req, Res](func(ctx context.Context, req Req) (Res, error) {
			var res Res
			err := uow.Do(ctx, func(ctx context.Context) error {
				var err error
				res, err = next.Handle(ctx, req)
				return err
			})
			if err != nil {
				var zero Res
				return zero, err
			}
			return res, nil
		})
	}
}

// Traced opens one span per handler invocation, named with the
// "<module>.<handler>" the adapter seeded via WithHandlerName. The Telemetry
// rides the context (WithTelemetry); on a context carrying none, Traced is
// an exact pass-through.
func Traced[Req, Res any]() Middleware[Req, Res] {
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return HandlerFunc[Req, Res](func(ctx context.Context, req Req) (Res, error) {
			t := TelemetryFromContext(ctx)
			if t == nil {
				return next.Handle(ctx, req)
			}
			sctx, end := t.Span(ctx, HandlerName(ctx))
			res, err := next.Handle(sctx, req)
			end(err)
			return res, err
		})
	}
}

// Metered records a duration histogram per handler and an error counter
// keyed by the warren/errors code, through the context-carried Telemetry —
// once per invocation, the error path included. On a context carrying no
// Telemetry it is an exact pass-through.
func Metered[Req, Res any]() Middleware[Req, Res] {
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return HandlerFunc[Req, Res](func(ctx context.Context, req Req) (Res, error) {
			t := TelemetryFromContext(ctx)
			if t == nil {
				return next.Handle(ctx, req)
			}
			start := time.Now()
			res, err := next.Handle(ctx, req)
			t.Record(HandlerName(ctx), time.Since(start), err)
			return res, err
		})
	}
}
