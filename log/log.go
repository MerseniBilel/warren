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
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithLogger returns a copy of ctx carrying l. It is the seeding side of
// FromContext, called by transport adapters at the edge of a request; handler
// code has no reason to call it. A nil l is not carried: the context behaves
// as unseeded, so FromContext's never-nil guarantee holds even against an
// adapter seeding an uninitialized logger field.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	if l == nil {
		return ctx
	}
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

// ContextAttrs derives log attributes from a context at the moment a record
// is emitted. It is the seam an adapter uses to put values core cannot
// compute — trace IDs, a tenant — on every record without core importing
// that adapter's SDK.
type ContextAttrs func(ctx context.Context, add func(slog.Attr))

// Handler wraps h so that every record carries the correlation ID on its
// context, plus whatever extra derives.
//
// The resolution happens in Handle, which is where slog is designed to do it
// and is the whole point: a request that logs nothing pays nothing, and a
// request that logs ten lines pays no per-request logger derivation.
// Deriving one at the edge instead — log.With(ctx, "correlation_id", id) —
// costs 8 allocations on EVERY request, measured, which is more than a
// transport's entire decode-validate-handle-encode path. That is why
// warren/transport/http seeds the ID and does not build a logger, and this
// is the other half of that decision.
//
// Install it once, in main:
//
//	slog.SetDefault(slog.New(log.Handler(
//	    slog.NewJSONHandler(os.Stdout, nil), observability.LogAttrs())))
//
// warren/observability does this for you unless you tell it not to.
func Handler(h slog.Handler, extra ...ContextAttrs) slog.Handler {
	if h == nil {
		panic("warren/log: Handler wraps a nil slog.Handler")
	}
	return &contextHandler{inner: h, extra: extra}
}

type contextHandler struct {
	inner slog.Handler
	extra []ContextAttrs
}

func (c *contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return c.inner.Enabled(ctx, l)
}

func (c *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	// Below the threshold nothing reaches here, so the extractors never run
	// for a record that would be dropped.
	if id := CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlation_id", id))
	}
	for _, fn := range c.extra {
		if fn != nil {
			fn(ctx, func(a slog.Attr) { r.AddAttrs(a) })
		}
	}
	return c.inner.Handle(ctx, r)
}

func (c *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{inner: c.inner.WithAttrs(attrs), extra: c.extra}
}

func (c *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{inner: c.inner.WithGroup(name), extra: c.extra}
}
