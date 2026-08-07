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

// FromContext returns the logger carried by ctx, or slog.Default() when ctx
// was never seeded — a unit test, a detached goroutine — so the result is
// always usable and never nil.
//
// USE THE *Context METHODS ON IT: InfoContext, ErrorContext, and so on.
//
//	log.FromContext(ctx).InfoContext(ctx, "saved", "id", id)   // correct
//	log.FromContext(ctx).Info("saved", "id", id)               // loses everything
//
// Passing the context twice looks redundant and is not. Correlation and
// trace IDs are resolved from the context when a record is EMITTED — see
// Handler — which is what makes a request that logs nothing cost nothing.
// slog's non-Context methods pass context.Background(), so they resolve
// nothing, and the record is silently missing every field that would let you
// join it to the request.
//
// An earlier version of this comment claimed the fields were "already
// attached" by the transport adapter. They are not, deliberately: attaching
// them at the edge costs 8 allocations on every request, measured, whether or
// not the request ever logs.
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
//
// The ID lands at the TOP level even on a logger derived with WithGroup: it
// is the key a log store is queried by, and nested under a group it is
// present, plausible and unmatchable. The caller's own attributes stay in
// the group they were added to. A logger with no group pays nothing for
// this — one AddAttrs, the same as before there was a rule about it.
func Handler(h slog.Handler, extra ...ContextAttrs) slog.Handler {
	if h == nil {
		panic("warren/log: Handler wraps a nil slog.Handler")
	}
	return &contextHandler{inner: h, extra: extra}
}

type contextHandler struct {
	inner slog.Handler
	extra []ContextAttrs

	// base and deferred exist only once a group is open. A handler nests
	// everything it is given inside its open groups, record attributes and
	// injected ones alike — so a logger built with WithGroup("http") emitted
	// http.correlation_id, which is present, plausible, and matches nothing:
	// the ID is the key you query a log store BY, and the records look
	// perfectly healthy while the search comes back empty.
	//
	// base is the inner handler as it stood before the first group opened —
	// the last point at which an attribute lands at the top level — and
	// deferred is what has been applied to it since. Handle replays them
	// AFTER injecting, which is what puts the ID above the group and leaves
	// the caller's own attributes inside it.
	base     slog.Handler
	deferred []deferredOp
}

// deferredOp is one WithAttrs or WithGroup call held back from base. Exactly
// one field is set.
type deferredOp struct {
	group string
	attrs []slog.Attr
}

func (c *contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return c.inner.Enabled(ctx, l)
}

func (c *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	// Below the threshold nothing reaches here, so the extractors never run
	// for a record that would be dropped.
	if len(c.deferred) == 0 {
		// No group is open, so the record's own attribute list IS the top
		// level. This is the path every Warren service logs through and it
		// costs one AddAttrs.
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

	var injected []slog.Attr
	if id := CorrelationID(ctx); id != "" {
		injected = append(injected, slog.String("correlation_id", id))
	}
	for _, fn := range c.extra {
		if fn != nil {
			fn(ctx, func(a slog.Attr) { injected = append(injected, a) })
		}
	}
	if len(injected) == 0 {
		// Nothing to lift, so the eagerly built handler is already right and
		// a grouped logger with no correlation ID pays nothing extra.
		return c.inner.Handle(ctx, r)
	}
	// Re-nest the record rather than re-deriving the handler chain per line.
	// Replaying base.WithAttrs + WithGroup for every record measured 11
	// allocations against this shape's 5, because a handler pre-formats what
	// WithAttrs gives it and throws the result away one record later.
	nested := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	nested.AddAttrs(injected...)
	nested.AddAttrs(c.regroup(r))
	return c.base.Handle(ctx, nested)
}

// regroup rebuilds the deferred WithGroup/WithAttrs chain as one nested
// attribute, with the record's own attributes innermost — the shape base
// would have produced had the groups been open when it received them.
//
// It is built from the inside out because a group's value is complete before
// the group that encloses it exists.
func (c *contextHandler) regroup(r slog.Record) slog.Attr {
	inner := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		inner = append(inner, a)
		return true
	})
	for i := len(c.deferred) - 1; i >= 0; i-- {
		op := c.deferred[i]
		if op.group == "" {
			// Attributes added under the group they follow, so they sit
			// beside the record's own rather than around them.
			inner = append(append(make([]slog.Attr, 0, len(op.attrs)+len(inner)), op.attrs...), inner...)
			continue
		}
		inner = []slog.Attr{{Key: op.group, Value: slog.GroupValue(inner...)}}
	}
	// The outermost op is always a group — deferral starts at one — so this
	// is that group, not a bare list.
	return inner[0]
}

func (c *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return c
	}
	next := &contextHandler{inner: c.inner.WithAttrs(attrs), extra: c.extra}
	if len(c.deferred) > 0 {
		// These belong to the group they were added under, so they are
		// replayed after it rather than folded into base.
		next.base = c.base
		next.deferred = appendOp(c.deferred, deferredOp{attrs: attrs})
	}
	return next
}

func (c *contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return c // slog.Handler's contract: an empty name is not a group
	}
	next := &contextHandler{inner: c.inner.WithGroup(name), extra: c.extra}
	next.base = c.base
	if len(c.deferred) == 0 {
		next.base = c.inner // the last handler an attribute reaches unnested
	}
	next.deferred = appendOp(c.deferred, deferredOp{group: name})
	return next
}

// appendOp copies rather than appending in place: two loggers derived from
// one parent must not share the array, or the second's group overwrites the
// first's and records go into a group their logger never opened.
func appendOp(ops []deferredOp, op deferredOp) []deferredOp {
	next := make([]deferredOp, len(ops), len(ops)+1)
	copy(next, ops)
	return append(next, op)
}
