package errors_test

import (
	stderrors "errors"
	"fmt"
	"slices"
	"testing"

	"github.com/MerseniBilel/warren/errors"
)

// Operation names used across several tables. Named because goconst counts
// their repetition, and because they are the two the DI boot path really uses.
const (
	opResolve = "di.Resolve"
	opRun     = "warren.Run"
)

func TestCodeOf(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want errors.Code
	}{
		"nil is internal": {
			nil, errors.CodeInternal,
		},
		"a foreign error is internal": {
			stderrors.New("connection refused"), errors.CodeInternal,
		},
		"a warren error reports its code": {
			errors.NotFound("no order abc"), errors.CodeNotFound,
		},
		"reads through fmt.Errorf": {
			fmt.Errorf("boot: %w", errors.Conflict("already exists")), errors.CodeConflict,
		},
		"the outermost warren error wins": {
			errors.Invalid("bad request").Wrapping(errors.NotFound("no order abc")),
			errors.CodeInvalid,
		},
		"reads through a foreign error to the warren error below": {
			fmt.Errorf("layer: %w", fmt.Errorf("layer: %w", errors.PermissionDenied("denied"))),
			errors.CodePermissionDenied,
		},
		"a joined error has no single code": {
			errors.Join(errors.NotFound("a"), errors.Conflict("b")), errors.CodeInternal,
		},
		"a five deep mixed chain": {
			errors.Internal("a").Wrapping(
				fmt.Errorf("b: %w", errors.NotFound("c").Wrapping(
					fmt.Errorf("d: %w", stderrors.New("e")),
				)),
			),
			errors.CodeInternal,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := errors.CodeOf(tc.err); got != tc.want {
				t.Errorf("CodeOf() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOps(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want []string
	}{
		"no ops": {
			errors.Internal("boom"), nil,
		},
		"one op": {
			errors.Internal("boom").Op(opResolve), []string{opResolve},
		},
		"outermost first within one error": {
			errors.Internal("boom").Op(opResolve).Op("di.Build").Op(opRun),
			[]string{opRun, "di.Build", opResolve},
		},
		"outermost first across a chain": {
			errors.Internal("outer").Op(opRun).Wrapping(
				errors.Internal("inner").Op(opResolve),
			),
			[]string{opRun, opResolve},
		},
		"traverses a foreign error in the middle": {
			errors.Internal("outer").Op(opRun).Wrapping(
				fmt.Errorf("mid: %w", errors.Internal("inner").Op(opResolve)),
			),
			[]string{opRun, opResolve},
		},
		"a foreign error contributes none": {
			stderrors.New("connection refused"), nil,
		},
		"a joined error contributes none": {
			errors.Join(
				errors.Internal("a").Op("one"),
				errors.Internal("b").Op("two"),
			),
			nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := errors.Ops(tc.err); !slices.Equal(got, tc.want) {
				t.Errorf("Ops() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFields(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want []errors.Field
	}{
		"no fields": {
			errors.Internal("boom"), nil,
		},
		"added order is kept within one error": {
			errors.Internal("boom").Field("a", 1).Field("b", 2),
			[]errors.Field{{Key: "a", Value: 1}, {Key: "b", Value: 2}},
		},
		"duplicates are not merged": {
			errors.Internal("boom").Field("a", 1).Field("a", 2),
			[]errors.Field{{Key: "a", Value: 1}, {Key: "a", Value: 2}},
		},
		"outermost error first across a chain": {
			errors.Internal("outer").Field("outer", 1).Wrapping(
				errors.Internal("inner").Field("inner", 2),
			),
			[]errors.Field{{Key: "outer", Value: 1}, {Key: "inner", Value: 2}},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := errors.Fields(tc.err); !slices.Equal(got, tc.want) {
				t.Errorf("Fields() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFixInnermostWins(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want string
	}{
		"no fix": {
			errors.Internal("boom"), "",
		},
		"one fix": {
			errors.Internal("boom").Fix("run make tools"), "run make tools",
		},
		"the innermost of two": {
			errors.Internal("outer").Fix("read the docs").Wrapping(
				errors.Internal("inner").Fix("run make tools"),
			),
			"run make tools",
		},
		"an inner error with no fix does not clear the outer one": {
			errors.Internal("outer").Fix("read the docs").Wrapping(
				errors.Internal("inner"),
			),
			"read the docs",
		},
		"a foreign error has none": {
			stderrors.New("connection refused"), "",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := errors.Fix(tc.err); got != tc.want {
				t.Errorf("Fix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInspectionIsDeterministic guards the reason fields are an ordered slice
// and not a map: a golden test on a message containing randomly ordered fields
// fails intermittently, which is worse than having no golden test at all.
func TestInspectionIsDeterministic(t *testing.T) {
	t.Parallel()

	const runs = 1000

	err := errors.Internal("outer").Op(opRun).Field("a", 1).Field("b", 2).Wrapping(
		errors.NotFound("inner").Op(opResolve).Field("c", 3),
	)

	wantOps := errors.Ops(err)
	wantFields := errors.Fields(err)
	wantDetail := errors.Detail(err)

	for range runs {
		if got := errors.Ops(err); !slices.Equal(got, wantOps) {
			t.Fatalf("Ops() = %v, want %v", got, wantOps)
		}

		if got := errors.Fields(err); !slices.Equal(got, wantFields) {
			t.Fatalf("Fields() = %v, want %v", got, wantFields)
		}

		if got := errors.Detail(err); got != wantDetail {
			t.Fatalf("Detail() = %q, want %q", got, wantDetail)
		}
	}
}

func TestReExportsMatchTheStandardLibrary(t *testing.T) {
	t.Parallel()

	target := stderrors.New("target")
	wrapped := fmt.Errorf("outer: %w", target)

	if !errors.Is(wrapped, target) {
		t.Error("Is() = false, want true")
	}

	var got *errors.Error
	if errors.As(wrapped, &got) {
		t.Error("As() = true, want false for a chain with no warren error")
	}

	unwrapped := errors.Unwrap(wrapped)
	if !stderrors.Is(unwrapped, target) {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, target)
	}

	joined := errors.Join(target, stderrors.New("other"))
	if !errors.Is(joined, target) {
		t.Error("Is(joined, target) = false, want true")
	}

	if errors.Join() != nil {
		t.Error("Join() with no errors = non-nil, want nil")
	}
}
