package errors

import (
	stderrors "errors"
	"slices"
)

// CodeOf reports the semantic code of err.
//
// It returns the code of the outermost [Error] in the chain, and [CodeInternal]
// if there is none — including when err is nil. An outer layer that
// deliberately reclassifies therefore wins over what it wrapped, which is the
// intended direction: the outer layer knows what the caller asked for.
func CodeOf(err error) Code {
	// Written as an explicit walk rather than with As, which would force the
	// target variable onto the heap. Every transport calls CodeOf on every
	// error it maps, so the budget in SPEC.md §5.6 is zero allocations.
	for err != nil {
		//nolint:errorlint // the walk below is the traversal As would do; asserting here is what keeps it allocation-free.
		if e, ok := err.(*Error); ok {
			return e.code
		}

		err = Unwrap(err)
	}

	return CodeInternal
}

// Ops returns the operation chain recorded by [Error.Op], outermost first.
func Ops(err error) []string {
	var out []string

	forEach(err, func(e *Error) {
		for _, op := range slices.Backward(e.ops) {
			out = append(out, op)
		}
	})

	return out
}

// Fields returns every field recorded by [Error.Field], outermost error first
// and, within one error, in the order they were added.
func Fields(err error) []Field {
	var out []Field

	forEach(err, func(e *Error) {
		out = append(out, e.fields...)
	})

	return out
}

// Fix returns the innermost remedy recorded by [Error.Fix], or "" if there is
// none. Innermost wins: the layer closest to the failure knows the real fix,
// and an outer layer that also has one is offering a more general guess.
func Fix(err error) string {
	var fix string

	forEach(err, func(e *Error) {
		if e.fix != "" {
			fix = e.fix
		}
	})

	return fix
}

// Is reports whether any error in err's chain matches target.
//
// It is the standard library's errors.Is, re-exported so that importing
// warren/errors never also requires importing errors.
func Is(err, target error) bool { return stderrors.Is(err, target) }

// As finds the first error in err's chain that matches target, and if one is
// found, sets target to that error value.
//
// It is the standard library's errors.As, re-exported.
func As(err error, target any) bool { return stderrors.As(err, target) }

// Unwrap returns the result of calling the Unwrap method on err, or nil.
//
// It is the standard library's errors.Unwrap, re-exported.
func Unwrap(err error) error { return stderrors.Unwrap(err) }

// Join returns an error wrapping the given errors, omitting any that are nil.
//
// It is the standard library's errors.Join, re-exported.
func Join(errs ...error) error { return stderrors.Join(errs...) }

// forEach visits every [Error] in err's chain, outermost first.
//
// A foreign error in the middle of the chain is stepped over rather than
// treated as the end, so a caller who wrapped with fmt.Errorf("%w") instead of
// [Error.Wrapping] is degraded, not truncated.
//
// The walk follows single-error unwrapping only. An error built by [Join] has
// no single operation chain, set of fields, or fix, so it contributes none —
// consistent with [CodeOf] reporting it as [CodeInternal].
func forEach(err error, visit func(*Error)) {
	for err != nil {
		//nolint:errorlint // deliberate single-chain walk; see the doc comment above.
		if e, ok := err.(*Error); ok {
			visit(e)

			err = e.err

			continue
		}

		err = Unwrap(err)
	}
}
