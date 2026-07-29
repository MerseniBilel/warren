package errors_test

import (
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/MerseniBilel/warren/errors"
)

// goldenPerm is the mode for a regenerated golden file. They are fixtures read
// by tests, never executed.
const goldenPerm = 0o600

// update rewrites the golden files instead of comparing against them. It backs
// `make golden-update`, which is why it has to be a package-level flag.
//
//nolint:gochecknoglobals // a test flag must be registered at package level for the flag package to parse it.
var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// TestDetailGolden pins the exact rendering of Detail, including its spacing.
// The missing-provider case is a v0.1 exit criterion: the message itself is the
// thing under test, so it is asserted as text rather than as a shape.
func TestDetailGolden(t *testing.T) {
	t.Parallel()

	tests := map[string]error{
		// The message docs/roadmap.md requires: the resolution chain, the
		// requesting file, and a copy-pasteable fix.
		// Kept identical to what warren/di actually emits, wrapped in the op
		// warren.Run will add: see di/SPEC.md §6.1.
		"di_missing_provider": errors.Invalid("no provider for *sql.DB").
			Op("di.Build").
			Op("warren.Run").
			Field("requested by", "internal/modules/orders/module.go:14").
			Field("chain", "*orders.Handler → *orders.Repository → *sql.DB").
			Fix("add di.Provide[*sql.DB](c, NewDB) to internal/modules/orders/module.go"),

		"message_only": errors.Internal("query failed"),

		"fields_only": errors.Invalid("validation failed").
			Op("orders.Create").
			Field("field", "email").
			Field("rule", "required"),

		"fix_only": errors.Conflict("order abc is already dispatched").
			Fix("cancel the dispatch first: POST /orders/abc/cancel"),

		"fields_across_a_chain": errors.Internal("boot failed").
			Op("warren.Run").
			Field("phase", "start").
			Wrapping(errors.Internal("dial tcp 127.0.0.1:5432: refused").
				Op("postgres.Open").
				Field("dsn", "postgres://localhost:5432/shop").
				Fix("start the database: docker compose up -d postgres")),
	}

	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := errors.Detail(err)
			path := filepath.Join("testdata", name+".golden")

			if *update {
				writeErr := os.WriteFile(path, []byte(got), goldenPerm)
				if writeErr != nil {
					t.Fatalf("writing golden file: %v", writeErr)
				}
			}

			want, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("reading golden file: %v (run: make golden-update)", readErr)
			}

			if got != string(want) {
				t.Errorf("Detail() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func TestDetailWithoutFieldsOrFixIsTheMessage(t *testing.T) {
	t.Parallel()

	tests := map[string]error{
		"a warren error":  errors.Internal("query failed").Op("orders.Get"),
		"a foreign error": stderrors.New("connection refused"),
	}

	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := errors.Detail(err); got != err.Error() {
				t.Errorf("Detail() = %q, want %q", got, err.Error())
			}
		})
	}
}

func TestDetailOfNil(t *testing.T) {
	t.Parallel()

	if got := errors.Detail(nil); got != "" {
		t.Errorf("Detail(nil) = %q, want empty", got)
	}
}
