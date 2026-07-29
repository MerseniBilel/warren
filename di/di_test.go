package di_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/di/internal/orders"
	"github.com/MerseniBilel/warren/errors"
)

// nodeA and nodeB depend on each other, which is the shortest cycle a graph can
// have that is not a self-reference.
type (
	nodeA struct{}
	nodeB struct{}
)

func newNodeA(*nodeB) *nodeA { return &nodeA{} }
func newNodeB(*nodeA) *nodeB { return &nodeB{} }

// selfish depends on itself.
type selfish struct{}

func newSelfish(*selfish) *selfish { return &selfish{} }

func newDB() *sql.DB { return new(sql.DB) }

// dsn is a plain value type, for the tests that need pointer-versus-value
// confusion without copying a lock the way *sql.DB would.
type dsn struct{ value string }

func newDSN() *dsn { return &dsn{value: localDSN} }

// Values repeated across the tests in this package, named because goconst counts
// occurrences rather than intent.
const (
	authTag  = "auth"
	localDSN = "postgres://localhost"
)

// mustBuild builds the container or fails the test, so that the tests below read
// as the behaviour they assert rather than as error handling.
func mustBuild(t *testing.T, c *di.Container) {
	t.Helper()

	err := c.Build(context.Background())
	if err != nil {
		t.Fatalf("Build() = %v, want nil", err)
	}
}

// mustResolve resolves T or fails the test.
func mustResolve[T any](t *testing.T, c *di.Container, target *T) {
	t.Helper()

	err := di.Resolve(c, target)
	if err != nil {
		t.Fatalf("Resolve() = %v, want nil", err)
	}
}

// mustGroup reads the group of T or fails the test.
func mustGroup[T any](t *testing.T, c *di.Container, target *[]T) {
	t.Helper()

	err := di.Group(c, target)
	if err != nil {
		t.Fatalf("Group() = %v, want nil", err)
	}
}

// TestValidateRejects covers every shape SPEC.md §5.3 refuses and every key
// conflict §6.2 and §6.8 describe. The message is asserted rather than the shape,
// because a DI error message is a product surface: see docs/architecture.md §6.
func TestValidateRejects(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		register func(*di.Container)
		want     string
	}{
		"a value where its constructor was meant": {
			register: func(c *di.Container) {
				di.Provide[*orders.Repository](c, orders.NewRepository(nil))
			},
			want: "di.Validate: the constructor for *orders.Repository is not a function",
		},
		"an untyped nil constructor": {
			register: func(c *di.Container) { di.Provide[*orders.Repository](c, nil) },
			want:     "di.Validate: the constructor for *orders.Repository is not a function",
		},
		"error as the provided type": {
			register: func(c *di.Container) { di.Provide[error](c, func() error { return nil }) },
			want:     "di.Validate: error cannot be provided",
		},
		"a constructor with no return values": {
			register: func(c *di.Container) { di.Provide[*orders.Repository](c, func() {}) },
			want: "di.Validate: the constructor for *orders.Repository must return " +
				"(*orders.Repository) or (*orders.Repository, error)",
		},
		"a second return that is not error": {
			register: func(c *di.Container) {
				di.Provide[*orders.Repository](c, func() (*orders.Repository, bool) { return nil, false })
			},
			want: "di.Validate: the constructor for *orders.Repository must return " +
				"(*orders.Repository) or (*orders.Repository, error)",
		},
		"three return values": {
			register: func(c *di.Container) {
				di.Provide[*orders.Repository](c, func() (*orders.Repository, int, error) { return nil, 0, nil })
			},
			want: "di.Validate: the constructor for *orders.Repository must return " +
				"(*orders.Repository) or (*orders.Repository, error)",
		},
		"a return type that does not satisfy the port": {
			register: func(c *di.Container) { di.Provide[orders.Port](c, orders.NewRepository) },
			want:     "di.Validate: func(*sql.DB) *orders.Repository does not provide orders.Port",
		},
		"a variadic constructor": {
			register: func(c *di.Container) {
				di.Provide[*orders.Repository](c, func(...*sql.DB) *orders.Repository { return nil })
			},
			want: "di.Validate: the constructor for *orders.Repository must not be variadic",
		},
		"two parameters of one type": {
			register: func(c *di.Container) {
				di.Provide[*orders.Repository](c, func(_, _ *sql.DB) *orders.Repository { return nil })
			},
			want: "di.Validate: the constructor for *orders.Repository takes two parameters of type *sql.DB",
		},
		"a nil supplied value": {
			register: func(c *di.Container) { di.Supply[*sql.DB](c, nil) },
			want:     "di.Validate: the value supplied for *sql.DB is nil",
		},
		"the same type provided twice": {
			register: func(c *di.Container) {
				di.Provide[*sql.DB](c, newDB)
				di.Provide[*sql.DB](c, func() *sql.DB { return nil })
			},
			want: "di.Validate: *sql.DB is provided twice",
		},
		"a type both provided and contributed": {
			register: func(c *di.Container) {
				di.Provide[*sql.DB](c, newDB)
				di.Contribute[*sql.DB](c, func() *sql.DB { return nil })
			},
			want: "di.Validate: *sql.DB is both provided and contributed to a group",
		},
		"a dependency nobody provides": {
			register: func(c *di.Container) {
				di.Provide[*orders.Handler](c, orders.NewHandler)
				di.Provide[*orders.Repository](c, orders.NewRepository)
			},
			want: "di.Validate: no provider for *sql.DB",
		},
		"a two-node cycle": {
			register: func(c *di.Container) {
				di.Provide[*nodeA](c, newNodeA)
				di.Provide[*nodeB](c, newNodeB)
			},
			want: "di.Validate: dependency cycle through *di_test.nodeA",
		},
		"a self-referencing provider": {
			register: func(c *di.Container) { di.Provide[*selfish](c, newSelfish) },
			want:     "di.Validate: dependency cycle through *di_test.selfish",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := di.New()
			test.register(c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want %q", test.want)
			}

			if err.Error() != test.want {
				t.Errorf("Validate() =\n  %q\nwant\n  %q", err.Error(), test.want)
			}
		})
	}
}

// TestValidateAcceptsAWellFormedGraph is the diamond that must not be mistaken
// for a cycle: two providers sharing one dependency.
func TestValidateAcceptsAWellFormedGraph(t *testing.T) {
	t.Parallel()

	c := di.New()
	di.Provide[*sql.DB](c, newDB)
	di.Provide[*orders.Repository](c, orders.NewRepository)
	di.Provide[*orders.Handler](c, orders.NewHandler)
	di.Contribute[string](c, func(*sql.DB) string { return "a" })

	err := c.Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// TestValidateReportsEveryProblem is SPEC.md §5.2: a service with four mistakes
// learns all four rather than the first.
func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()

	c := di.New()
	di.Provide[*orders.Repository](c, nil)
	di.Provide[error](c, func() error { return nil })
	di.Provide[*orders.Handler](c, func() {})
	di.Supply[*sql.DB](c, nil)

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want four problems")
	}

	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Fatalf("Validate() = %v, want a joined error", err)
	}

	if got := len(joined.Unwrap()); got != 4 {
		t.Errorf("Validate() reported %d problems, want 4", got)
	}
}

// TestValidateIsSilentAboutConsequences: a rejected registration is not in the
// registry, so reporting every type it would have provided as missing would bury
// the real mistake under its own fallout.
func TestValidateIsSilentAboutConsequences(t *testing.T) {
	t.Parallel()

	c := di.New()
	di.Provide[*sql.DB](c, nil) // rejected
	di.Provide[*orders.Repository](c, orders.NewRepository)

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want the rejected registration")
	}

	if want := "di.Validate: the constructor for *sql.DB is not a function"; err.Error() != want {
		t.Errorf("Validate() =\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// TestValidateDoesNotConstruct is what makes a failed boot safe: the graph is
// checked before a single connection is opened.
func TestValidateDoesNotConstruct(t *testing.T) {
	t.Parallel()

	calls := 0

	c := di.New()
	di.Provide[*sql.DB](c, func() *sql.DB {
		calls++

		return newDB()
	})
	di.Provide[*orders.Repository](c, orders.NewRepository)

	err := c.Validate()
	if err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}

	if calls != 0 {
		t.Errorf("Validate() called %d constructors, want 0", calls)
	}
}

// TestRegistrationSiteSurvivesAWrapper is SPEC.md §7. Without At, a
// warren.Provide that delegates here would make every message name a file inside
// the framework instead of the caller's module.
func TestRegistrationSiteSurvivesAWrapper(t *testing.T) {
	t.Parallel()

	// provideVia stands in for warren.Provide: one hop above di.Provide.
	provideVia := func(c *di.Container, ctor any) {
		di.Provide[*orders.Repository](c, ctor, di.At(di.Caller(1)))
	}

	c := di.New()
	provideVia(c, orders.NewRepository)

	nodes := c.Graph().Nodes
	if len(nodes) != 1 {
		t.Fatalf("Graph() has %d nodes, want 1", len(nodes))
	}

	// The wrapper is declared in this file, so the recorded site must be here and
	// not in di.go.
	if got := nodes[0].Registered.File; !hasSuffix(got, "di_test.go") {
		t.Errorf("Registered.File = %q, want this test file", got)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestBootContextIsInjected: a driver whose constructor needs a context gets
// Build's, and it is cancelled when boot finishes rather than when the process
// stops — which is why a constructor must not retain it.
func TestBootContextIsInjected(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	// Compared inside the constructor rather than captured: a context assigned to
	// a variable that outlives the call is the mistake this package's doc comment
	// warns about, and the linter is right to flag it.
	injected := false

	c := di.New()
	di.Provide[*sql.DB](c, func(got context.Context) *sql.DB {
		injected = got == ctx

		return newDB()
	})

	// Built with ctx rather than through mustBuild, which supplies its own.
	err := c.Build(ctx)
	if err != nil {
		t.Fatalf("Build() = %v, want nil", err)
	}

	if !injected {
		t.Error("the constructor did not receive Build's context")
	}

	cancel()

	if ctx.Err() == nil {
		t.Error("the boot context did not observe cancellation")
	}
}
