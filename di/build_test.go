package di_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/di/internal/orders"
	"github.com/MerseniBilel/warren/errors"
)

// reader and writer are two ports one implementation satisfies, which is how a
// constructor comes to be registered twice.
type (
	reader interface{ read() string }
	writer interface{ write() }
)

// tagged satisfies both, and carries a tag so that two instances can be told
// apart.
type tagged struct{ tag string }

func (t *tagged) read() string { return t.tag }
func (*tagged) write()         {}

func newTagged() *tagged { return &tagged{tag: "named"} }

// router consumes a group as a slice.
type router struct{ mw []string }

func newRouter(mw []string) *router { return &router{mw: mw} }

// TestOneConstructorUnderTwoPortsIsOneInstance is the case the prior-art audit
// in SPEC.md §9.1 caught: keyed by type this builds two repositories, and for
// anything holding a pool or a cache that is a bug.
func TestOneConstructorUnderTwoPortsIsOneInstance(t *testing.T) {
	t.Parallel()

	c := di.New()
	// Registered through a named function, so the two registrations share an
	// identity the container can recognise.
	di.Provide[reader](c, newTagged)
	di.Provide[writer](c, newTagged)

	mustBuild(t, c)

	var (
		asReader reader
		asWriter writer
	)

	mustResolve(t, c, &asReader)
	mustResolve(t, c, &asWriter)

	first, ok := asReader.(*tagged)
	if !ok {
		t.Fatalf("reader is %T, want *tagged", asReader)
	}

	second, ok := asWriter.(*tagged)
	if !ok {
		t.Fatalf("writer is %T, want *tagged", asWriter)
	}

	if first != second {
		t.Errorf("reader and writer are different instances, want one")
	}
}

// TestTwoClosuresAreTwoConstructors: reflect's documentation warns that a func
// value's pointer does not identify a function uniquely, because every closure
// from one source location shares a code pointer. Deduplicating those would hand
// one caller another's instance.
func TestTwoClosuresAreTwoConstructors(t *testing.T) {
	t.Parallel()

	taggedWith := func(tag string) func() *tagged {
		return func() *tagged { return &tagged{tag: tag} }
	}

	c := di.New()
	di.Provide[reader](c, taggedWith("first"))
	di.Provide[writer](c, taggedWith("second"))

	mustBuild(t, c)

	var asReader reader

	mustResolve(t, c, &asReader)

	if got := asReader.read(); got != "first" {
		t.Errorf("reader.read() = %q, want %q — the two closures were merged", got, "first")
	}
}

// TestConstructorRunsOncePerBuild covers a constructor with several dependents.
func TestConstructorRunsOncePerBuild(t *testing.T) {
	t.Parallel()

	calls := 0

	c := di.New()
	di.Provide[*sql.DB](c, func() *sql.DB {
		calls++

		return newDB()
	})
	di.Provide[*orders.Repository](c, orders.NewRepository)
	di.Provide[*orders.Handler](c, orders.NewHandler)
	di.Contribute[string](c, func(*sql.DB) string { return "a" })
	di.Contribute[string](c, func(*sql.DB) string { return "b" })

	mustBuild(t, c)

	if calls != 1 {
		t.Errorf("constructor ran %d times, want 1", calls)
	}
}

// TestGroupOrderIsRegistrationOrder pins the ordering a middleware chain depends
// on. A group whose order varies between runs is a bug that reproduces once a
// week, so it is asserted over many builds rather than once.
func TestGroupOrderIsRegistrationOrder(t *testing.T) {
	t.Parallel()

	const runs = 1000

	for range runs {
		c := di.New()
		di.Contribute[string](c, func() string { return "a" })
		di.Contribute[string](c, func() string { return "b" })
		di.Contribute[string](c, func() string { return "c" })

		mustBuild(t, c)

		var got []string
		mustGroup(t, c, &got)

		if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
			t.Fatalf("Group() = %v, want [a b c]", got)
		}
	}
}

// TestAConstructorConsumesAGroupAsASlice is the mechanism warren uses to collect
// routes and middleware from modules that know nothing about each other.
func TestAConstructorConsumesAGroupAsASlice(t *testing.T) {
	t.Parallel()

	c := di.New()
	di.Provide[*router](c, newRouter)
	di.Contribute[string](c, func() string { return authTag })
	di.Contribute[string](c, func() string { return "recover" })

	mustBuild(t, c)

	var got *router

	mustResolve(t, c, &got)

	if len(got.mw) != 2 || got.mw[0] != authTag || got.mw[1] != "recover" {
		t.Errorf("router.mw = %v, want [auth recover]", got.mw)
	}
}

// TestEverythingRegisteredIsConstructed: a provider nobody depends on is still
// built, so a connection opens at boot rather than on the first request.
func TestEverythingRegisteredIsConstructed(t *testing.T) {
	t.Parallel()

	built := false

	c := di.New()
	di.Provide[*sql.DB](c, func() *sql.DB {
		built = true

		return newDB()
	})

	mustBuild(t, c)

	if !built {
		t.Error("an unused provider was not constructed")
	}
}

// TestBuildFailures covers each way construction itself can fail, as opposed to
// the graph being wrong.
func TestBuildFailures(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		register func(*di.Container)
		want     string
		code     errors.Code
	}{
		"a constructor returning an error": {
			register: func(c *di.Container) {
				di.Provide[*sql.DB](c, func() (*sql.DB, error) {
					return nil, errors.Internal("dial tcp 127.0.0.1:5432: connection refused")
				})
			},
			want: "di.Build: constructing *sql.DB failed: dial tcp 127.0.0.1:5432: connection refused",
			code: errors.CodeInternal,
		},
		"a constructor returning nil": {
			register: func(c *di.Container) {
				di.Provide[*sql.DB](c, func() *sql.DB { return nil })
			},
			want: "di.Build: the constructor for *sql.DB returned nil",
			code: errors.CodeInvalid,
		},
		"a constructor that panics": {
			register: func(c *di.Container) {
				di.Provide[*sql.DB](c, func() *sql.DB { panic("no dsn configured") })
			},
			want: "di.Build: the constructor for *sql.DB panicked: no dsn configured",
			code: errors.CodeInternal,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := di.New()
			test.register(c)

			err := c.Build(context.Background())
			if err == nil {
				t.Fatalf("Build() = nil, want %q", test.want)
			}

			if err.Error() != test.want {
				t.Errorf("Build() =\n  %q\nwant\n  %q", err.Error(), test.want)
			}

			if got := errors.CodeOf(err); got != test.code {
				t.Errorf("CodeOf() = %v, want %v", got, test.code)
			}
		})
	}
}

// TestConstructorErrorIsReachable: a driver's own error must survive being
// wrapped, or a caller cannot branch on it.
func TestConstructorErrorIsReachable(t *testing.T) {
	t.Parallel()

	cause := errors.Conflict("already open")

	c := di.New()
	di.Provide[*sql.DB](c, func() (*sql.DB, error) { return nil, cause })

	err := c.Build(context.Background())
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause) = false, want true")
	}
}

// TestBuildIsIdempotent: a second call returns the first call's error and
// constructs nothing.
func TestBuildIsIdempotent(t *testing.T) {
	t.Parallel()

	calls := 0

	c := di.New()
	di.Provide[*sql.DB](c, func() *sql.DB {
		calls++

		return newDB()
	})

	ctx := context.Background()

	first := c.Build(ctx)
	if first != nil {
		t.Fatalf("Build() = %v, want nil", first)
	}

	second := c.Build(ctx)
	if second != nil {
		t.Fatalf("second Build() = %v, want nil", second)
	}

	if calls != 1 {
		t.Errorf("constructor ran %d times across two builds, want 1", calls)
	}
}

// TestFailedBuildIsNotRetried is the other half of idempotency: a container that
// failed stays failed, so a test that builds it twice does not construct twice.
func TestFailedBuildIsNotRetried(t *testing.T) {
	t.Parallel()

	calls := 0

	c := di.New()
	di.Provide[*sql.DB](c, func() (*sql.DB, error) {
		calls++

		return nil, errors.Internal("refused")
	})

	ctx := context.Background()

	first := c.Build(ctx)
	second := c.Build(ctx)

	if first == nil || second == nil {
		t.Fatal("Build() = nil twice, want the same failure")
	}

	if first.Error() != second.Error() {
		t.Errorf("second Build() = %q, want the first error %q", second, first)
	}

	if calls != 1 {
		t.Errorf("constructor ran %d times, want 1", calls)
	}
}
