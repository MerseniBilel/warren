package log

import (
	"log/slog"

	"github.com/MerseniBilel/warren/errors"
)

// Group and attribute keys for [Err]. They are fixed rather than configurable:
// a configurable key is one setting nobody changes and every log query has to
// account for.
const (
	errGroupKey = "error"
	messageKey  = "message"
	codeKey     = "code"
	opsKey      = "ops"
	fixKey      = "fix"
)

// errAttrCount is the number of attributes Err emits before the error's own
// fields: message, code, ops, and fix.
const errAttrCount = 4

// Err returns a grouped attribute describing err — its message, semantic code,
// operation chain, fields, and suggested fix:
//
//	log.From(ctx).ErrorContext(ctx, "cannot create order", log.Err(err))
//
// The error's fields are emitted as siblings inside the group, under their own
// keys, so that error.order_id is queryable rather than buried inside a
// rendered string.
//
// An error from outside Warren yields a message and code=Internal, which is
// what [errors.CodeOf] reports for it. Err never inspects an error's text.
//
// Err(nil) returns the empty [slog.Attr], which slog discards, so a caller need
// not branch on whether it has an error.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}

	fields := errors.Fields(err)

	// Built as []slog.Attr and closed with GroupValue rather than passed to
	// slog.Group, whose variadic ...any boxes every attribute individually.
	attrs := make([]slog.Attr, 0, errAttrCount+len(fields))
	attrs = append(attrs,
		slog.String(messageKey, err.Error()),
		slog.String(codeKey, errors.CodeOf(err).String()),
	)

	if ops := errors.Ops(err); len(ops) > 0 {
		attrs = append(attrs, slog.Any(opsKey, ops))
	}

	if fix := errors.Fix(err); fix != "" {
		attrs = append(attrs, slog.String(fixKey, fix))
	}

	// A field whose key collides with one above is emitted anyway: slog permits
	// duplicate keys, and silently dropping a caller's context is worse than a
	// duplicate.
	for _, f := range fields {
		attrs = append(attrs, slog.Any(f.Key, f.Value))
	}

	return slog.Attr{Key: errGroupKey, Value: slog.GroupValue(attrs...)}
}
