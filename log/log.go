// Package log carries a [slog.Logger] on a [context.Context], and turns a
// warren/errors error into structured attributes.
//
// It defines no logger type of its own. A handler is transport-agnostic, so it
// cannot be handed a logger by the HTTP layer without importing it; the one
// thing that crosses every layer is the context, so the logger travels there:
//
//	ctx = log.Into(ctx, logger)                        // at the edge, once
//	ctx = log.With(ctx, "request_id", id)              // request-scoped context
//
//	log.From(ctx).InfoContext(ctx, "order created", "order_id", order.ID)
//
// [From] returns a *slog.Logger and callers use slog's own API. That is
// deliberate: the moment this package defines a Logger type it grows methods,
// and a thin layer becomes a logging framework. Levels, formatting, output, and
// handler choice are all slog's, and choosing among them belongs to a service's
// generated main.go where a user can read it and change it.
//
// Logging an error keeps its structure rather than flattening it to a string:
//
//	log.From(ctx).ErrorContext(ctx, "cannot create order", log.Err(err))
package log

import (
	"context"
	"log/slog"
)

// contextKey is the key under which the logger is stored. It is an unexported
// zero-size type, so nothing outside this package can collide with it or read
// the logger out except through [From].
type contextKey struct{}

// Into returns a copy of ctx carrying logger.
//
// A transport adapter calls this once per request, after attaching the
// attributes that identify the request. A nil logger returns ctx unchanged, so
// a caller need not branch on whether it has one.
func Into(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}

	return context.WithValue(ctx, contextKey{}, logger)
}

// From returns the logger carried by ctx.
//
// It returns [slog.Default] when ctx carries no logger, and when ctx is nil, so
// a call site never has to check and never panics. That fallback is the
// standard library's global, not one of Warren's: this package holds no state.
func From(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}

	if logger, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		return logger
	}

	return slog.Default()
}

// With returns a copy of ctx carrying [From](ctx) with args attached, in slog's
// alternating key/value form:
//
//	ctx = log.With(ctx, "request_id", id, "tenant", tenant)
//
// Every record logged through From(ctx) downstream carries them. Prefer
// [WithAttrs] where the attributes are already typed: an odd number of
// arguments here degrades at runtime rather than failing to compile.
func With(ctx context.Context, args ...any) context.Context {
	if len(args) == 0 {
		return ctx
	}

	return Into(ctx, From(ctx).With(args...))
}

// WithAttrs returns a copy of ctx carrying [From](ctx) with attrs attached.
//
// It is [With] for callers that already hold typed attributes, and avoids both
// the alternating form's runtime cost and its failure mode.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}

	// slog.Logger.With does exactly this internally, after converting its
	// arguments to attributes. Going through the handler skips that conversion.
	return Into(ctx, slog.New(From(ctx).Handler().WithAttrs(attrs)))
}
