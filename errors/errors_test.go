package errors_test

import (
	stderrors "errors"
	"fmt"
	"slices"
	"testing"

	"github.com/MerseniBilel/warren/errors"
)

// msgNoOrder is the message under test wherever the case is about structure
// rather than about the text itself.
const msgNoOrder = "no order abc"

func TestCodeString(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		want string
		code errors.Code
	}{
		"internal":          {want: "Internal", code: errors.CodeInternal},
		"not found":         {want: "NotFound", code: errors.CodeNotFound},
		"conflict":          {want: "Conflict", code: errors.CodeConflict},
		"invalid":           {want: "Invalid", code: errors.CodeInvalid},
		"permission denied": {want: "PermissionDenied", code: errors.CodePermissionDenied},
		"outside the set":   {want: "Code(7)", code: errors.Code(7)},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tc.code.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestZeroCodeIsInternal(t *testing.T) {
	t.Parallel()

	var zero errors.Code
	if zero != errors.CodeInternal {
		t.Errorf("zero Code = %v, want CodeInternal", zero)
	}
}

func TestConstructorsSetTheirCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  *errors.Error
		want errors.Code
	}{
		"NotFound":         {errors.NotFound("x"), errors.CodeNotFound},
		"Conflict":         {errors.Conflict("x"), errors.CodeConflict},
		"Invalid":          {errors.Invalid("x"), errors.CodeInvalid},
		"PermissionDenied": {errors.PermissionDenied("x"), errors.CodePermissionDenied},
		"Internal":         {errors.Internal("x"), errors.CodeInternal},
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

func TestConstructorsFormatOnlyWithArguments(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  *errors.Error
		want string
	}{
		"no arguments leaves the message alone": {
			errors.NotFound("no order 100%"), "no order 100%",
		},
		"arguments are formatted": {
			errors.NotFound("no order %s", "abc"), msgNoOrder,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestErrorMessageFormat(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("connection refused")

	tests := map[string]struct {
		err  *errors.Error
		want string
	}{
		"message alone": {
			errors.NotFound(msgNoOrder),
			msgNoOrder,
		},
		"one op precedes the message": {
			errors.NotFound(msgNoOrder).Op("orders.Get"),
			"orders.Get: no order abc",
		},
		"ops render outermost first": {
			errors.NotFound(msgNoOrder).Op("orders.Get").Op("http.Handle"),
			"http.Handle: orders.Get: no order abc",
		},
		"the cause follows the message": {
			errors.Internal("query failed").Wrapping(cause),
			"query failed: connection refused",
		},
		"ops, message, then cause": {
			errors.Internal("query failed").Op("orders.Get").Wrapping(cause),
			"orders.Get: query failed: connection refused",
		},
		"fields and fix stay out of the message": {
			errors.NotFound(msgNoOrder).Field("order_id", "abc").Fix("create it first"),
			msgNoOrder,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFixFormatsItsArguments(t *testing.T) {
	t.Parallel()

	err := errors.Internal("no provider").Fix("add warren.Provide(%s) to %s", "NewDB", "module.go")

	const want = "add warren.Provide(NewDB) to module.go"
	if got := errors.Fix(err); got != want {
		t.Errorf("Fix() = %q, want %q", got, want)
	}
}

func TestBuildersDoNotMutateTheReceiver(t *testing.T) {
	t.Parallel()

	base := errors.NotFound(msgNoOrder).Op("orders.Get").Field("order_id", "abc")

	const (
		wantMessage = "orders.Get: no order abc"
		wantFields  = 1
		wantOps     = 1
	)

	// Derive several variants from the same parent. Each must be independent:
	// a shared backing array would let one write over another's element.
	_ = base.Op("http.Handle")
	_ = base.Field("tenant", "acme")
	_ = base.Fix("create it first")
	_ = base.Wrapping(stderrors.New("boom"))

	if got := base.Error(); got != wantMessage {
		t.Errorf("Error() = %q, want %q", got, wantMessage)
	}

	if got := len(errors.Fields(base)); got != wantFields {
		t.Errorf("len(Fields()) = %d, want %d", got, wantFields)
	}

	if got := len(errors.Ops(base)); got != wantOps {
		t.Errorf("len(Ops()) = %d, want %d", got, wantOps)
	}

	if got := errors.Fix(base); got != "" {
		t.Errorf("Fix() = %q, want empty", got)
	}

	unwrapped := stderrors.Unwrap(base)
	if unwrapped != nil {
		t.Errorf("Unwrap() = %v, want nil", unwrapped)
	}
}

func TestSiblingsDerivedFromOneParentDoNotShareStorage(t *testing.T) {
	t.Parallel()

	parent := errors.Internal("boom").Field("a", 1)

	left := parent.Field("b", 2)
	right := parent.Field("c", 3)

	wantLeft := []errors.Field{{Key: "a", Value: 1}, {Key: "b", Value: 2}}
	wantRight := []errors.Field{{Key: "a", Value: 1}, {Key: "c", Value: 3}}

	if got := errors.Fields(left); !slices.Equal(got, wantLeft) {
		t.Errorf("left Fields() = %v, want %v", got, wantLeft)
	}

	if got := errors.Fields(right); !slices.Equal(got, wantRight) {
		t.Errorf("right Fields() = %v, want %v", got, wantRight)
	}
}

func TestWrappingNilReturnsTheReceiver(t *testing.T) {
	t.Parallel()

	base := errors.NotFound(msgNoOrder)

	same := base.Wrapping(nil)
	if same != base {
		t.Errorf("Wrapping(nil) returned a different error: %v", same)
	}
}

func TestUnwrapReturnsTheCause(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("connection refused")
	err := errors.Internal("query failed").Wrapping(cause)

	unwrapped := err.Unwrap()
	if !stderrors.Is(unwrapped, cause) {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, cause)
	}
}

func TestErrorIsAnOrdinaryGoError(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("connection refused")

	tests := map[string]error{
		"through Wrapping":      errors.Internal("query failed").Wrapping(cause),
		"through two Wrappings": errors.Internal("outer").Wrapping(errors.Internal("inner").Wrapping(cause)),
		"through fmt.Errorf":    fmt.Errorf("boot: %w", errors.Internal("query failed").Wrapping(cause)),
		"through Join":          errors.Join(stderrors.New("other"), errors.Internal("q").Wrapping(cause)),
		"through Wrapping a fmt.Errorf": errors.Internal("outer").
			Wrapping(fmt.Errorf("mid: %w", cause)),
	}

	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if !errors.Is(err, cause) {
				t.Errorf("Is(err, cause) = false, want true")
			}
		})
	}
}

func TestAsReachesTheWarrenError(t *testing.T) {
	t.Parallel()

	inner := errors.NotFound(msgNoOrder)

	tests := map[string]error{
		"the error itself":   inner,
		"through fmt.Errorf": fmt.Errorf("boot: %w", inner),
		"through Wrapping":   errors.Internal("outer").Wrapping(inner),
	}

	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var target *errors.Error
			if !errors.As(err, &target) {
				t.Fatal("As() = false, want true")
			}
		})
	}
}

func TestErrorDoesNotMatchByCode(t *testing.T) {
	t.Parallel()

	// Two NotFound errors are different errors. Making Is match on the code
	// would make every NotFound equal to every other, which is not what a
	// reader expects errors.Is to mean.
	one := errors.NotFound(msgNoOrder)
	two := errors.NotFound("no order xyz")

	if errors.Is(one, two) {
		t.Error("Is(one, two) = true, want false")
	}
}
