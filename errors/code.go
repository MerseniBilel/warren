package errors

import "strconv"

// Code classifies an error semantically, independently of any transport.
//
// Its zero value is [CodeInternal], so an error that never states a code — and
// any error originating outside Warren — is treated as internal. That is the
// safe default: an unmapped failure becomes a 500 rather than a misleading 404.
//
// The set is deliberately small and closed. Every transport adapter switches
// over it exhaustively, checked by the exhaustive linter, so adding a code is a
// breaking change for every adapter and is decided in docs/architecture.md
// rather than in a pull request that needed one.
type Code uint8

const (
	// CodeInternal reports that an invariant broke and the caller cannot fix it.
	CodeInternal Code = iota
	// CodeNotFound reports that the addressed thing does not exist.
	CodeNotFound
	// CodeConflict reports that the request contradicts the current state.
	CodeConflict
	// CodeInvalid reports that the request is malformed or fails validation.
	CodeInvalid
	// CodePermissionDenied reports that the caller is known and is not allowed.
	CodePermissionDenied
)

// String returns the code's name, for example "NotFound". A value outside the
// defined set renders as "Code(7)", so a bad conversion is visible rather than
// blank.
func (c Code) String() string {
	switch c {
	case CodeInternal:
		return "Internal"
	case CodeNotFound:
		return "NotFound"
	case CodeConflict:
		return "Conflict"
	case CodeInvalid:
		return "Invalid"
	case CodePermissionDenied:
		return "PermissionDenied"
	default:
		return "Code(" + strconv.Itoa(int(c)) + ")"
	}
}
