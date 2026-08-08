// Package errors defines Warren's semantic error vocabulary: a closed set of
// codes that describe what went wrong in terms a domain expert would use, with
// no reference to any transport.
//
// Domain and application code returns these errors. Each transport adapter
// owns the translation from a Code into its own protocol — HTTP status, gRPC
// code, or consumer ack semantics. Nothing in this package knows those
// mappings exist.
//
// Code that also touches driver sentinels imports this package under the alias
// werrors, keeping the bare errors identifier for the standard library.
package errors

import (
	stderrors "errors"
	"fmt"
	"maps"
	"strings"
)

// Code is the semantic classification of a failure. The set is closed: these
// eight codes are the whole vocabulary, and every adapter maps every one of
// them. An adapter treats a code it does not know as CodeInternal — the safe
// default for the unknown.
type Code string

const (
	// CodeInvalid means the request was malformed or violated a constraint.
	// The caller must change the request before retrying.
	CodeInvalid Code = "INVALID"

	// CodeNotFound means the addressed resource does not exist.
	CodeNotFound Code = "NOT_FOUND"

	// CodeConflict means the request collided with the current state — a
	// duplicate, or a transition the aggregate does not allow from here.
	//
	// It is TERMINAL. Retrying cannot change the answer, and app.RetryingOn
	// refuses to be composed on it. A write refused because someone else
	// committed first is NOT this — that is CodeContention, and it retries.
	CodeConflict Code = "CONFLICT"

	// CodeContention means a conditional write lost a race: another writer
	// committed first, so this request's read-modify-write sequence was
	// refused rather than allowed to overwrite theirs. Nothing was written.
	//
	// It is the second retryable code, and it retries DIFFERENTLY from
	// CodeUnavailable. Unavailable means "make the same call again";
	// Contention means "start the sequence again" — re-read the aggregate,
	// re-decide, re-write. That is exactly what app.Retrying does, because it
	// re-invokes the HANDLER and not the transaction.
	//
	// It is gRPC's ABORTED, whose own guidance is the test to apply:
	// "use ABORTED if the client should retry at a higher level … restart a
	// read-modify-write sequence".
	//
	// It is not CodeConflict. A conflict is arithmetic — three units in stock
	// and a request for five is refused identically on the tenth attempt.
	// Contention is a race, and the race is usually won on the next attempt.
	CodeContention Code = "CONTENTION"

	// CodeUnauthenticated means the caller's identity was absent or could not
	// be established.
	//
	// It describes the caller's identity, not yours. A service that fails to
	// authenticate to something downstream — Postgres, S3, another API —
	// returns CodeUnavailable, never CodeUnauthenticated.
	CodeUnauthenticated Code = "UNAUTHENTICATED"

	// CodePermissionDenied means the caller is known but is not allowed to
	// perform this operation.
	CodePermissionDenied Code = "PERMISSION_DENIED"

	// CodeUnavailable means a dependency was temporarily unreachable: the same
	// request may succeed later unchanged. It is one of the two retryable
	// codes: this one means "make the same call again", where CodeContention
	// means "start the read-modify-write sequence again".
	//
	// This is the code for failing to authenticate to a downstream dependency
	// — that failure is about your service's credentials, not the caller's
	// identity, so it retries; it is never CodeUnauthenticated.
	CodeUnavailable Code = "UNAVAILABLE"

	// CodeInternal means the failure was not anticipated. It carries no
	// promise that retrying helps.
	CodeInternal Code = "INTERNAL"
)

// Error is Warren's semantic error. It carries a Code, a message, an optional
// wrapped cause, and any details attached with WithDetail.
type Error struct {
	code    Code
	msg     string
	cause   error
	details map[string]any
}

// Invalid reports that field failed validation or conversion, wrapping err as
// the reason. The resulting Error carries CodeInvalid.
//
// The reason is ALSO recorded as a detail under field, so it reaches the
// caller. CodeInvalid describes the caller's own input — telling them only
// "field email is invalid" while the reason sits in a wrapped cause nothing
// renders is a 400 nobody can act on, and it made hand-written validation
// less informative than warren/validate's, which fills the same detail.
//
// The detail is skipped when field names SEVERAL fields at once — when it
// contains a comma or a space. A details key of "customer, cents" is not a
// field name, and a client mapping details onto form controls would render
// an error against a control that does not exist. Callers reporting several
// fields add a detail per field themselves, which is what validate does.
//
// A cause too sensitive to return is not a CodeInvalid: use Internal, which
// renders nothing and logs everything.
func Invalid(field string, err error) *Error {
	e := &Error{code: CodeInvalid, msg: "field " + field + " is invalid", cause: err}
	if err != nil && !strings.ContainsAny(field, ", ") {
		e.details = map[string]any{field: err.Error()}
	}
	return e
}

// NotFound reports that no resource of the named kind exists with this id.
// The resulting Error carries CodeNotFound.
func NotFound(resource string, id any) *Error {
	return &Error{code: CodeNotFound, msg: fmt.Sprintf("%s %v not found", resource, id)}
}

// Conflict reports that the request collided with current state. The
// resulting Error carries CodeConflict. args are fmt operands for msg; when
// args is empty, msg is used verbatim.
func Conflict(msg string, args ...any) *Error {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	return &Error{code: CodeConflict, msg: msg}
}

// Contention reports that a conditional write was refused because another
// writer committed first. The resulting Error carries CodeContention. args
// are fmt operands for msg; when args is empty, msg is used verbatim.
//
// It is written by REPOSITORIES, not by aggregates. If you are about to
// return it from a method that enforces a business rule, you want Conflict:
// the test is whether the same request would succeed if the caller sent it
// again ten seconds later with nothing else changed. Yes is Contention, no
// is Conflict.
func Contention(msg string, args ...any) *Error {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	return &Error{code: CodeContention, msg: msg}
}

// Codes returns the closed set, in the order warren.md §2.6 tables them.
//
// It exists so an adapter's mapping can be TESTED for exhaustiveness rather
// than asserted to be. Every switch over Code in this repository has a
// default arm answering INTERNAL — correct policy for a code it has never
// heard of, and a silent trap for one that was just added, because the
// forgotten adapter renders 500 with no compile error and no test failure.
func Codes() []Code {
	return []Code{
		CodeInvalid, CodeNotFound, CodeConflict, CodeContention,
		CodeUnauthenticated, CodePermissionDenied, CodeUnavailable, CodeInternal,
	}
}

// Unauthenticated reports that the caller's identity was absent or could not
// be established; reason is the message verbatim. It describes the caller's
// identity — a downstream auth failure is Unavailable, never this.
func Unauthenticated(reason string) *Error {
	return &Error{code: CodeUnauthenticated, msg: reason}
}

// PermissionDenied reports that the known caller may not perform action.
func PermissionDenied(action string) *Error {
	return &Error{code: CodePermissionDenied, msg: "not allowed to " + action}
}

// Unavailable reports that dependency was temporarily unreachable, wrapping
// err as the cause. The resulting Error carries CodeUnavailable, the
// retryable code.
func Unavailable(dependency string, err error) *Error {
	return &Error{code: CodeUnavailable, msg: dependency + " is unavailable", cause: err}
}

// Internal reports an unanticipated failure, wrapping err as the cause.
func Internal(err error) *Error {
	return &Error{code: CodeInternal, msg: "unexpected failure", cause: err}
}

// Code returns the semantic classification. On a nil receiver it returns the
// zero Code, which every adapter treats as unknown and maps to INTERNAL.
func (e *Error) Code() Code {
	if e == nil {
		return ""
	}
	return e.code
}

// Message returns the human-readable message, without the code prefix and
// without the cause. It returns "" on a nil receiver.
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.msg
}

// Details returns a copy of the details attached with WithDetail; mutating
// the returned map does not touch e. The copy is shallow: the map is copied,
// the values are shared, so a mutable value (a slice, a map) attached as a
// detail is still reachable through it. It returns nil when no detail was
// attached, and on a nil receiver.
func (e *Error) Details() map[string]any {
	if e == nil {
		return nil
	}
	return maps.Clone(e.details)
}

// Error renders "CODE: message", with ": cause" appended when a cause is
// wrapped. Details are not rendered — they are structured payload for
// adapters, not log text. On a nil receiver — the classic typed-nil slip —
// it returns "<nil>" rather than panicking.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause != nil {
		return string(e.code) + ": " + e.msg + ": " + e.cause.Error()
	}
	return string(e.code) + ": " + e.msg
}

// Unwrap returns the wrapped cause, or nil — so the standard library's
// errors.Is and errors.As see through a Warren error. It is nil-receiver
// safe.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// WithDetail attaches the key/value pair to e and returns e, so adapters have
// structured context to put in a response body. It mutates e — errors are
// constructed, decorated, and returned on one failure path, never shared. On
// a nil receiver it is a no-op returning nil.
func (e *Error) WithDetail(k string, v any) *Error {
	if e == nil {
		return nil
	}
	if e.details == nil {
		e.details = make(map[string]any)
	}
	e.details[k] = v
	return e
}

// CodeOf returns the code an adapter would map err by: the OUTERMOST Warren
// code in the chain, CodeInternal for an error carrying none, and the empty
// code for nil.
//
// It answers a different question from Is, and the difference is the one
// already documented there. Wrapping is RECATEGORISATION — an error wrapped
// as Invalid is invalid, whatever it wrapped — so a status mapping reads the
// outermost code, while Is asks whether a meaning appears anywhere in the
// chain. CodeOf is the mapping's question.
//
// A foreign error is CodeInternal, matching §2.6's rule that a code the table
// does not list is treated as the safe default for the unknown. nil is the
// EMPTY code and deliberately not CodeInternal: a nil error is not a failure,
// and answering INTERNAL would make `if CodeOf(err) == CodeInternal` fire on
// success.
//
// It exists because reading a code back otherwise meant a linear scan over
// the vocabulary in user code — and because two private copies of it already
// lived in the framework, in transport/http and in broker.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if stderrors.As(err, &e) {
		// Code() is nil-safe and returns the empty code, so a typed-nil
		// *Error answers the same as nil rather than being miscategorised as
		// a genuine INTERNAL failure.
		return e.Code()
	}
	return CodeInternal
}

// Is reports whether err, or any error it wraps, carries code — the whole
// chain is searched, so a Warren error wrapped inside another Warren error
// is still found. It is how callers ask about meaning without a type
// assertion, and it never panics, a typed-nil *Error included.
//
// Note the asymmetry with an adapter's status mapping: an adapter translates
// the OUTERMOST code (the standard library's errors.AsType finds it, Code()
// reads it), because wrapping is recategorization; Is answers whether the
// meaning appears anywhere in the chain.
func Is(err error, code Code) bool {
	for err != nil {
		if e, ok := err.(*Error); ok && e != nil && e.code == code {
			return true
		}
		switch x := err.(type) {
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		case interface{ Unwrap() []error }:
			for _, u := range x.Unwrap() {
				if Is(u, code) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}
