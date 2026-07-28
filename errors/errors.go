// Package errors is Warren's error model: one type, one semantic code, and
// enough structure that every transport maps an error without reading its text.
//
// An error is built from one of five constructors, each naming a [Code], and
// then decorated with context that survives being passed up through layers:
//
//	return errors.NotFound("no order %s", id).
//		Op("orders.Repository.Get").
//		Field("order_id", id).
//		Wrapping(err)
//
// A transport adapter maps the code without inspecting the message:
//
//	switch errors.CodeOf(err) {
//	case errors.CodeNotFound:
//		w.WriteHeader(http.StatusNotFound)
//	// ...
//	}
//
// The builders return a copy rather than mutating the receiver, so an error
// value may be shared, stored, or decorated by two callers independently.
//
// There is deliberately no New and no Errorf: every error made here carries a
// semantic code, because an uncoded error is one a transport maps to 500 by
// accident. Code that genuinely wants an uncoded sentinel imports the standard
// library's errors package directly.
package errors

import (
	"fmt"
	"slices"
	"strings"
)

// Field is one piece of structured context attached to an [Error].
//
// Value precedes Key so that the garbage collector scans 24 bytes rather than
// 32: a string's length word is not a pointer, and putting it last leaves it
// outside the scanned region. Always construct a Field with keyed fields.
type Field struct {
	// Value is the context itself. It is rendered with %v.
	Value any
	// Key names the context, for example "order_id". Machine-read context uses
	// snake_case; a key rendered by Detail for a human may read as prose.
	Key string
}

// Error is Warren's error type. Its zero value is not usable: construct one
// with [NotFound], [Conflict], [Invalid], [PermissionDenied], or [Internal].
//
// Every field is unexported, which is what makes the copy-on-write builders
// safe — a caller cannot reach in and mutate an error another caller holds.
//
// The field order is chosen for the garbage collector, not for reading: the two
// slices go last because their length and capacity words are not pointers, so
// trailing them shrinks the scanned region from 104 bytes to 80. code is a
// single byte and holds no pointer at all, so it goes after them.
type Error struct {
	msg    string
	fix    string
	err    error
	ops    []string
	fields []Field
	code   Code
}

// NotFound reports that the addressed thing does not exist.
func NotFound(format string, args ...any) *Error {
	return newError(CodeNotFound, format, args)
}

// Conflict reports that the request contradicts the current state.
func Conflict(format string, args ...any) *Error {
	return newError(CodeConflict, format, args)
}

// Invalid reports that the request is malformed or fails validation.
func Invalid(format string, args ...any) *Error {
	return newError(CodeInvalid, format, args)
}

// PermissionDenied reports that the caller is known and is not allowed.
func PermissionDenied(format string, args ...any) *Error {
	return newError(CodePermissionDenied, format, args)
}

// Internal reports that an invariant broke and the caller cannot fix it.
func Internal(format string, args ...any) *Error {
	return newError(CodeInternal, format, args)
}

// Wrapping records err as the cause of e and returns the result. The result
// unwraps to err, so [Is] and [As] reach through it.
//
// Wrapping(nil) returns the receiver unchanged, so a caller need not branch on
// whether it has a cause to attach.
func (e *Error) Wrapping(err error) *Error {
	if err == nil {
		return e
	}

	c := e.clone()
	c.err = err

	return c
}

// Field attaches structured context and returns the result. Fields are ordered
// and are not deduplicated: two calls with the same key produce two fields.
func (e *Error) Field(key string, value any) *Error {
	c := e.clone()
	c.fields = appended(e.fields, Field{Key: key, Value: value})

	return c
}

// Op names the operation that failed, for example "di.Resolve", and returns
// the result. Ops accumulate as an error travels outward: the last one added is
// the outermost, and is rendered first.
func (e *Error) Op(op string) *Error {
	c := e.clone()
	c.ops = appended(e.ops, op)

	return c
}

// Fix records a copy-pasteable remedy and returns the result, for example
// "add warren.Provide(NewDB) to internal/platform/module.go".
//
// It is a field rather than part of the message so that a renderer can present
// it distinctly and a test can assert that it exists at all.
func (e *Error) Fix(format string, args ...any) *Error {
	c := e.clone()
	c.fix = sprint(format, args)

	return c
}

// Error renders the operation chain, the message, and the cause, separated by
// ": ". Fields and the fix are not included; see [Detail] for those.
func (e *Error) Error() string {
	var b strings.Builder

	// Ops are stored in the order they were added, innermost first, and
	// rendered outermost first.
	for _, op := range slices.Backward(e.ops) {
		b.WriteString(op)
		b.WriteString(": ")
	}

	b.WriteString(e.msg)

	if e.err != nil {
		b.WriteString(": ")
		b.WriteString(e.err.Error())
	}

	return b.String()
}

// Unwrap returns the cause recorded by [Error.Wrapping], or nil.
func (e *Error) Unwrap() error { return e.err }

// newError builds an Error, formatting only when there is something to format.
// Skipping Sprintf for the no-argument case keeps construction to one
// allocation, which matters because errors are built on the request path.
func newError(code Code, format string, args []any) *Error {
	return &Error{code: code, msg: sprint(format, args)}
}

// clone copies the error so a builder never mutates its receiver.
func (e *Error) clone() *Error {
	c := *e

	return &c
}

// appended returns s with v added, always in a fresh backing array. Plain
// append would let two errors derived from the same parent write over each
// other's element when the parent had spare capacity.
func appended[T any](s []T, v T) []T {
	out := make([]T, len(s)+1)
	copy(out, s)
	out[len(s)] = v

	return out
}

// sprint formats only when args were supplied, so a literal message costs no
// formatting pass and no second allocation.
func sprint(format string, args []any) string {
	if len(args) == 0 {
		return format
	}

	return fmt.Sprintf(format, args...)
}
