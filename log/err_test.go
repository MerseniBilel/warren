package log_test

import (
	"bytes"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// goldenPerm is the mode for a regenerated golden file. They are fixtures read
// by tests, never executed.
const goldenPerm = 0o600

// update rewrites the golden files instead of comparing against them. It backs
// `make golden-update`, which is why it has to be a package-level flag.
//
//nolint:gochecknoglobals // a test flag must be registered at package level for the flag package to parse it.
var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// TestErrGolden pins the JSON a logged error produces. The structure is the
// contract: a query for error.code or error.order_id has to keep working, and a
// change to any of these files is a change to what operators can search on.
func TestErrGolden(t *testing.T) {
	t.Parallel()

	tests := map[string]error{
		"full": errors.NotFound("no provider for *sql.DB").
			Op("di.Resolve").
			Op("warren.Run").
			Field("requested_by", "internal/modules/orders/module.go:14").
			Field("attempt", 2).
			Fix("add warren.Provide(NewDB) to internal/platform/module.go"),

		"message_and_code_only": errors.Conflict("order abc is already dispatched"),

		"foreign_error": stderrors.New("dial tcp 127.0.0.1:5432: connection refused"),

		"across_a_chain": errors.Internal("boot failed").
			Op("warren.Run").
			Field("phase", "start").
			Wrapping(errors.Internal("refused").
				Op("postgres.Open").
				Field("port", 5432).
				Fix("start the database: docker compose up -d postgres")),

		// A field named like one of Err's own keys is emitted anyway: dropping
		// a caller's context silently is worse than a duplicate key.
		"colliding_field_key": errors.Invalid("validation failed").
			Field("code", "EMAIL_REQUIRED"),
	}

	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			ctx := log.Into(t.Context(), newLogger(&buf))
			log.From(ctx).ErrorContext(ctx, "request failed", log.Err(err))

			got := buf.String()
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
				t.Errorf("record mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
			}
		})
	}
}

func TestErrOfNilEmitsNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	ctx := log.Into(t.Context(), newLogger(&buf))
	log.From(ctx).ErrorContext(ctx, "request failed", log.Err(nil))

	const want = `{"level":"ERROR","msg":"request failed"}` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("record = %s, want %s", got, want)
	}
}
