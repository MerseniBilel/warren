// Package log carries a *slog.Logger on the context and propagates the
// correlation ID that ties one request's records together.
//
// Transport adapters seed the context with WithLogger and WithCorrelationID at
// the edge. Handlers read the logger back out with FromContext; they never
// construct a logger and never take one as a constructor dependency.
//
// This is not a logging abstraction: the logger is log/slog, used directly.
package log

import (
	"context"
	"log/slog"
)

type loggerKey struct{}

type correlationKey struct{}

// FromContext returns the logger carried by ctx. The trace ID, span ID,
// correlation ID, module, and handler are already attached to it by the
// transport adapter that seeded the context. When ctx was never seeded —
// a unit test, a detached goroutine — it returns slog.Default(), so the
// result is always usable and never nil.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// WithLogger returns a copy of ctx carrying l. It is the seeding side of
// FromContext, called by transport adapters at the edge of a request; handler
// code has no reason to call it.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// With returns a copy of ctx carrying a logger with args appended to the
// logger already on ctx — or to slog.Default() when ctx was never seeded.
// args are slog key/value pairs. Use it to add request-scoped fields that
// every subsequent record in this call tree should carry.
func With(ctx context.Context, args ...any) context.Context {
	if len(args) == 0 {
		return ctx
	}
	return WithLogger(ctx, FromContext(ctx).With(args...))
}

// CorrelationID returns the correlation ID carried by ctx — the identifier
// that ties together every record emitted while handling one request, across
// the transports and consumers it passes through. It returns "" when ctx
// carries none.
func CorrelationID(ctx context.Context) string {
	if id, ok := ctx.Value(correlationKey{}).(string); ok {
		return id
	}
	return ""
}

// WithCorrelationID returns a copy of ctx carrying id. The correlation ID is
// minted at the edge: a transport adapter reuses the identifier arriving on
// the wire or mints a fresh one, seeds it here, and attaches it to the logger.
// Attaching is the adapter's step — this function only carries the value.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}
