package di_test

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/di/internal/orders"
	"github.com/MerseniBilel/warren/errors"
)

// goldenPerm is the mode for a regenerated golden file. They are fixtures read by
// tests, never executed.
const goldenPerm = 0o600

// update rewrites the golden files instead of comparing against them. It backs
// `make golden-update`, which is why it has to be a package-level flag.
//
//nolint:gochecknoglobals // a test flag must be registered at package level for the flag package to parse it.
var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// realSite matches a site this machine produced, so that a golden pins the shape
// of a constructor's location without pinning an absolute path or a line number
// that shifts whenever the file above it is edited.
//
// A site written by di.At is left alone: those are the ones a message is asserted
// on exactly, including the missing-provider case docs/roadmap.md names.
var realSite = regexp.MustCompile(`/\S*/warren/di/(\S+\.go):\d+`)

// moduleSite is the registration site a service would have. It is synthetic so
// that the exit-criterion message is byte-identical between machines.
func moduleSite() di.Site {
	return di.Site{File: "internal/modules/orders/module.go", Line: 14}
}

// platformSite is where a service's shared infrastructure is wired.
func platformSite() di.Site {
	return di.Site{File: "internal/platform/module.go", Line: 9}
}

// byValue takes a dsn by value, which is the near miss SPEC.md §6.12 exists for:
// the pointer is provided and the value was asked for.
type byValue struct{}

func newByValue(dsn) *byValue { return &byValue{} }

// TestErrorGolden pins every message in SPEC.md §6. A DI error message is a
// product surface — docs/architecture.md §6 calls a bad one the most common reason
// people abandon a framework — so the text is the thing under test.
func TestErrorGolden(t *testing.T) {
	t.Parallel()

	tests := map[string]func() error{
		// The message docs/roadmap.md requires as a v0.1 exit criterion: the
		// resolution chain, the requesting file, and a copy-pasteable fix.
		"no_provider": func() error {
			c := di.New()
			di.Provide[*orders.Handler](c, orders.NewHandler, di.At(moduleSite()))
			di.Provide[*orders.Repository](c, orders.NewRepository, di.At(moduleSite()))

			return c.Build(context.Background())
		},

		"no_provider_near_miss": func() error {
			c := di.New()
			di.Provide[*dsn](c, newDSN, di.At(platformSite()))
			di.Provide[*byValue](c, newByValue, di.At(moduleSite()))

			return c.Build(context.Background())
		},

		"provided_twice": func() error {
			c := di.New()
			di.Provide[*sql.DB](c, newDB, di.At(platformSite()))
			di.Provide[*sql.DB](c, newDB, di.At(moduleSite()))

			return c.Validate()
		},

		"cycle": func() error {
			c := di.New()
			di.Provide[*nodeA](c, newNodeA, di.At(moduleSite()))
			di.Provide[*nodeB](c, newNodeB, di.At(moduleSite()))

			return c.Validate()
		},

		"not_a_function": func() error {
			c := di.New()
			di.Provide[*orders.Repository](c, orders.NewRepository(nil), di.At(moduleSite()))

			return c.Validate()
		},

		"not_satisfied": func() error {
			c := di.New()
			di.Provide[orders.Port](c, orders.NewRepository, di.At(moduleSite()))

			return c.Validate()
		},

		"return_shape": func() error {
			c := di.New()
			di.Provide[*orders.Repository](c, func(*sql.DB) (*orders.Repository, bool) {
				return nil, false
			}, di.At(moduleSite()))

			return c.Validate()
		},

		"variadic": func() error {
			c := di.New()
			di.Provide[*router](c, func(...string) *router { return nil }, di.At(platformSite()))

			return c.Validate()
		},

		"duplicate_parameter": func() error {
			c := di.New()
			di.Provide[*orders.Repository](c, func(_, _ *sql.DB) *orders.Repository {
				return nil
			}, di.At(moduleSite()))

			return c.Validate()
		},

		"provides_error": func() error {
			c := di.New()
			di.Provide[error](c, func() error { return nil }, di.At(moduleSite()))

			return c.Validate()
		},

		"provided_and_contributed": func() error {
			c := di.New()
			di.Provide[string](c, func() string { return "a" }, di.At(platformSite()))
			di.Contribute[string](c, func() string { return "b" }, di.At(moduleSite()))

			return c.Validate()
		},

		"supplied_nil": func() error {
			c := di.New()
			di.Supply[*sql.DB](c, nil, di.At(platformSite()))

			return c.Validate()
		},

		"returned_nil": func() error {
			c := di.New()
			di.Provide[*sql.DB](c, func() *sql.DB { return nil }, di.At(platformSite()))

			return c.Build(context.Background())
		},

		"constructor_failed": func() error {
			c := di.New()
			di.Provide[*sql.DB](c, func() (*sql.DB, error) {
				return nil, errors.Internal("dial tcp 127.0.0.1:5432: connection refused")
			}, di.At(platformSite()))

			return c.Build(context.Background())
		},

		"panicked": func() error {
			c := di.New()
			di.Provide[*sql.DB](c, func() *sql.DB { panic("no dsn configured") }, di.At(platformSite()))

			return c.Build(context.Background())
		},

		"not_built": func() error {
			c := di.New()

			var db *sql.DB

			return di.Resolve(c, &db)
		},

		"resolve_of_a_group": func() error {
			c := di.New()
			di.Contribute[string](c, func() string { return "a" }, di.At(moduleSite()))

			err := c.Build(context.Background())
			if err != nil {
				return err
			}

			var one string

			return di.Resolve(c, &one)
		},

		"group_of_a_single_provider": func() error {
			c := di.New()
			di.Provide[*sql.DB](c, newDB, di.At(platformSite()))

			err := c.Build(context.Background())
			if err != nil {
				return err
			}

			var many []*sql.DB

			return di.Group(c, &many)
		},
	}

	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := build()
			if err == nil {
				t.Fatalf("%s produced no error", name)
			}

			got := realSite.ReplaceAllString(errors.Detail(err), "warren/di/$1:NN")
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

// TestEveryFailureCarriesAFix is the structural half of the same requirement:
// AGENT.md § Errors says a message must name the fix, and a test can only assert
// that when the fix is a field rather than prose inside a sentence.
func TestEveryFailureCarriesAFix(t *testing.T) {
	t.Parallel()

	failures := map[string]func() error{
		"a missing provider": func() error {
			c := di.New()
			di.Provide[*orders.Repository](c, orders.NewRepository)

			return c.Build(context.Background())
		},
		"a cycle": func() error {
			c := di.New()
			di.Provide[*selfish](c, newSelfish)

			return c.Validate()
		},
		"a constructor that panics": func() error {
			c := di.New()
			di.Provide[*sql.DB](c, func() *sql.DB { panic("boom") })

			return c.Build(context.Background())
		},
		"resolution before build": func() error {
			var db *sql.DB

			return di.Resolve(di.New(), &db)
		},
	}

	for name, build := range failures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := build()
			if err == nil {
				t.Fatal("expected a failure")
			}

			if errors.Fix(err) == "" {
				t.Errorf("%s carries no fix: %v", name, err)
			}
		})
	}
}
