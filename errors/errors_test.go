package errors_test

import (
	stderrors "errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	werrors "github.com/MerseniBilel/warren/errors"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestErrorRendersGolden(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		golden string
		err    *werrors.Error
	}{
		{
			name:   "invalid names the field and carries the constraint",
			golden: "invalid",
			err:    werrors.Invalid("email", stderrors.New(`missing "@"`)),
		},
		{
			name:   "not found names the resource and the id",
			golden: "notfound",
			err:    werrors.NotFound("user", 42),
		},
		{
			name:   "conflict uses the message verbatim without args",
			golden: "conflict_plain",
			err:    werrors.Conflict("user already exists"),
		},
		{
			name:   "conflict formats args printf style",
			golden: "conflict_formatted",
			err:    werrors.Conflict("user %s already exists", "bob"),
		},
		{
			name:   "unauthenticated uses the reason verbatim",
			golden: "unauthenticated",
			err:    werrors.Unauthenticated("token expired"),
		},
		{
			name:   "permission denied names the action",
			golden: "permissiondenied",
			err:    werrors.PermissionDenied("delete user 42"),
		},
		{
			name:   "unavailable names the dependency and carries the cause",
			golden: "unavailable",
			err:    werrors.Unavailable("postgres", stderrors.New("connection refused")),
		},
		{
			name:   "internal carries the cause",
			golden: "internal",
			err:    werrors.Internal(stderrors.New("boom")),
		},
		{
			name:   "details are not rendered",
			golden: "with_details",
			err:    werrors.NotFound("user", 42).WithDetail("id", 42).WithDetail("kind", "user"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.err.Error()
			path := filepath.Join("testdata", tc.golden+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("writing golden file: %v", err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden file: %v", err)
			}
			if got != string(want) {
				t.Errorf("Error() = %q, golden file has %q", got, want)
			}
		})
	}
}

func TestIs(t *testing.T) {
	t.Parallel()

	notFound := werrors.NotFound("user", 42)

	cases := []struct {
		name string
		err  error
		code werrors.Code
		want bool
	}{
		{"direct error carries its code", notFound, werrors.CodeNotFound, true},
		{"direct error does not carry another code", notFound, werrors.CodeConflict, false},
		{"wrapped once with %w", fmt.Errorf("loading profile: %w", notFound), werrors.CodeNotFound, true},
		{"wrapped twice with %w", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", notFound)), werrors.CodeNotFound, true},
		{"non-Warren error", stderrors.New("plain"), werrors.CodeInternal, false},
		{"nil error", nil, werrors.CodeNotFound, false},
		{"a Warren error inside another Warren error is still found", werrors.Internal(notFound), werrors.CodeNotFound, true},
		{"the outer code of a rewrapped error is found too", werrors.Internal(notFound), werrors.CodeInternal, true},
		{"joined errors are searched", stderrors.Join(stderrors.New("plain"), notFound), werrors.CodeNotFound, true},
		{"typed-nil *Error does not panic and does not match", (*werrors.Error)(nil), werrors.CodeNotFound, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := werrors.Is(tc.err, tc.code); got != tc.want {
				t.Errorf("Is(%v, %s) = %v, want %v", tc.err, tc.code, got, tc.want)
			}
		})
	}
}

func TestUnwrapExposesTheCauseToTheStandardLibrary(t *testing.T) {
	t.Parallel()

	sentinel := stderrors.New("connection refused")
	err := werrors.Unavailable("postgres", sentinel)

	if !stderrors.Is(err, sentinel) {
		t.Error("stdlib errors.Is does not see the wrapped cause through a Warren error")
	}
	if got := err.Unwrap(); got != sentinel {
		t.Errorf("Unwrap() = %v, want the wrapped sentinel", got)
	}
	if got := werrors.NotFound("user", 42).Unwrap(); got != nil {
		t.Errorf("Unwrap() on a cause-less error = %v, want nil", got)
	}
}

func TestAccessors(t *testing.T) {
	t.Parallel()

	err := werrors.NotFound("user", 42)
	if got := err.Code(); got != werrors.CodeNotFound {
		t.Errorf("Code() = %s, want %s", got, werrors.CodeNotFound)
	}
	if got := err.Message(); got != "user 42 not found" {
		t.Errorf("Message() = %q, want %q", got, "user 42 not found")
	}
}

func TestWithDetail(t *testing.T) {
	t.Parallel()

	t.Run("attaches pairs and returns the same error for chaining", func(t *testing.T) {
		t.Parallel()
		err := werrors.Conflict("user already exists")
		if got := err.WithDetail("email", "bob@example.com"); got != err {
			t.Error("WithDetail did not return the receiver")
		}
		details := err.Details()
		if details["email"] != "bob@example.com" {
			t.Errorf("Details() = %v, want the attached pair", details)
		}
	})

	t.Run("details returns a copy", func(t *testing.T) {
		t.Parallel()
		err := werrors.Conflict("taken").WithDetail("k", "v")
		err.Details()["k"] = "mutated"
		if got := err.Details()["k"]; got != "v" {
			t.Errorf("mutating the returned map reached the error: Details()[k] = %v", got)
		}
	})

	t.Run("no details means nil", func(t *testing.T) {
		t.Parallel()
		if got := werrors.Conflict("taken").Details(); got != nil {
			t.Errorf("Details() = %v, want nil", got)
		}
	})
}

// TestTypedNilIsSafe covers the classic slip: a nil *Error escaping through
// a non-nil error interface. No method may panic on it.
func TestTypedNilIsSafe(t *testing.T) {
	t.Parallel()

	var e *werrors.Error
	if got := e.Error(); got != "<nil>" {
		t.Errorf("nil Error() = %q, want %q", got, "<nil>")
	}
	if got := e.Code(); got != "" {
		t.Errorf("nil Code() = %q, want the zero Code (adapters map it to INTERNAL)", got)
	}
	if e.Message() != "" || e.Details() != nil || e.Unwrap() != nil {
		t.Error("nil accessors did not return zero values")
	}
	if e.WithDetail("k", "v") != nil {
		t.Error("nil WithDetail did not return nil")
	}
}

func TestDetailsCopyIsShallow(t *testing.T) {
	t.Parallel()

	// The documented contract: the map is copied, the values are shared.
	err := werrors.Conflict("taken").WithDetail("fields", []string{"email"})
	err.Details()["fields"].([]string)[0] = "mutated"
	if got := err.Details()["fields"].([]string)[0]; got != "mutated" {
		t.Errorf("value sharing changed: Details()[fields][0] = %q — update the doc if the copy became deep", got)
	}
}

func TestSevenCodesAreDistinct(t *testing.T) {
	t.Parallel()

	codes := []werrors.Code{
		werrors.CodeInvalid,
		werrors.CodeNotFound,
		werrors.CodeConflict,
		werrors.CodeUnauthenticated,
		werrors.CodePermissionDenied,
		werrors.CodeUnavailable,
		werrors.CodeInternal,
	}
	seen := make(map[werrors.Code]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Errorf("code %s appears twice", c)
		}
		seen[c] = true
	}
	if len(seen) != 7 {
		t.Errorf("vocabulary has %d distinct codes, want 7", len(seen))
	}
}

func BenchmarkNotFound(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = werrors.NotFound("user", 42)
	}
}

func BenchmarkInvalid(b *testing.B) {
	cause := stderrors.New(`missing "@"`)
	b.ReportAllocs()
	for b.Loop() {
		_ = werrors.Invalid("email", cause)
	}
}

func BenchmarkConflict(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = werrors.Conflict("user already exists")
	}
}

func BenchmarkWithDetailChainedTwice(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = werrors.NotFound("user", 42).WithDetail("id", 42).WithDetail("kind", "user")
	}
}

func BenchmarkErrorRendering(b *testing.B) {
	err := werrors.Unavailable("postgres", stderrors.New("connection refused"))
	b.ReportAllocs()
	for b.Loop() {
		_ = err.Error()
	}
}

// A field name that lists SEVERAL fields must not become a details key: a
// client mapping details onto form controls would render an error against a
// control that does not exist. Found by field-testing, 2026-08-02.
func TestInvalidDoesNotFabricateAMultiFieldDetailKey(t *testing.T) {
	t.Parallel()

	err := werrors.Invalid("customer, cents", stderrors.New("required"))
	if _, bogus := err.Details()["customer, cents"]; bogus {
		t.Errorf("a joined field list became a details key: %v", err.Details())
	}

	// The single-field case still carries its reason, which is the whole
	// point of recording it.
	single := werrors.Invalid("email", stderrors.New("must be an address"))
	if got := single.Details()["email"]; got != "must be an address" {
		t.Errorf("details[email] = %v, want the reason", got)
	}
}

// TestCodeOf — field test #6 asked for this, having written it themselves:
// there was werrors.Is(err, code) and no way to READ the code back, so getting
// one meant a linear scan over the vocabulary in user code. Two private
// copies already existed in the framework (transport/http's asWarrenError and
// broker's codeOf), which is the other half of the argument for exporting it.
func TestCodeOf(t *testing.T) {
	t.Parallel()

	t.Run("reads the code", func(t *testing.T) {
		t.Parallel()
		if got := werrors.CodeOf(werrors.Conflict("taken")); got != werrors.CodeConflict {
			t.Errorf("CodeOf = %q, want CONFLICT", got)
		}
	})

	t.Run("reads the OUTERMOST code, because wrapping is recategorisation", func(t *testing.T) {
		t.Parallel()
		// The asymmetry with Is is deliberate and already documented there:
		// an adapter maps the outermost code, Is asks whether a meaning
		// appears anywhere. CodeOf is the adapter's question.
		inner := werrors.NotFound("user", "u-1")
		outer := werrors.Invalid("body", inner)
		if got := werrors.CodeOf(outer); got != werrors.CodeInvalid {
			t.Errorf("CodeOf = %q, want INVALID — the outermost", got)
		}
		if !werrors.Is(outer, werrors.CodeNotFound) {
			t.Error("Is stopped finding the inner code; the two must stay different questions")
		}
	})

	t.Run("finds a Warren error wrapped by fmt.Errorf", func(t *testing.T) {
		t.Parallel()
		wrapped := fmt.Errorf("while saving: %w", werrors.Conflict("taken"))
		if got := werrors.CodeOf(wrapped); got != werrors.CodeConflict {
			t.Errorf("CodeOf = %q, want CONFLICT through %%w", got)
		}
	})

	t.Run("a foreign error is INTERNAL, the safe default for the unknown", func(t *testing.T) {
		t.Parallel()
		if got := werrors.CodeOf(stderrors.New("boom")); got != werrors.CodeInternal {
			t.Errorf("CodeOf = %q, want INTERNAL", got)
		}
	})

	t.Run("nil has no code", func(t *testing.T) {
		t.Parallel()
		// Not INTERNAL: a nil error is not a failure, and answering INTERNAL
		// would make `if CodeOf(err) == CodeInternal` fire on success.
		if got := werrors.CodeOf(nil); got != "" {
			t.Errorf("CodeOf(nil) = %q, want the empty code", got)
		}
	})

	t.Run("a typed-nil *Error never panics", func(t *testing.T) {
		t.Parallel()
		var e *werrors.Error
		if got := werrors.CodeOf(e); got != "" {
			t.Errorf("CodeOf(typed nil) = %q, want the empty code", got)
		}
	})
}
