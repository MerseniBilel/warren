package di_test

import (
	"database/sql"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/di/internal/orders"
	"github.com/MerseniBilel/warren/errors"
)

// TestResolveRoundTrip is the whole point of the package in four lines.
func TestResolveRoundTrip(t *testing.T) {
	t.Parallel()

	c := di.New()
	di.Provide[*sql.DB](c, newDB)
	di.Provide[*orders.Repository](c, orders.NewRepository)

	mustBuild(t, c)

	var repo *orders.Repository

	mustResolve(t, c, &repo)

	if repo == nil || repo.DB == nil {
		t.Fatalf("Resolve() gave %+v, want a repository holding the provided *sql.DB", repo)
	}
}

// TestSupplyIsResolvable covers a value built before the container existed,
// which is how configuration reaches the graph.
func TestSupplyIsResolvable(t *testing.T) {
	t.Parallel()

	db := newDB()

	c := di.New()
	di.Supply(c, db)
	di.Provide[*orders.Repository](c, orders.NewRepository)

	mustBuild(t, c)

	var repo *orders.Repository

	mustResolve(t, c, &repo)

	if repo.DB != db {
		t.Error("the supplied value did not reach the constructor")
	}
}

// TestResolveFailures covers every reason a resolution is refused.
func TestResolveFailures(t *testing.T) {
	t.Parallel()

	t.Run("before the container is built", func(t *testing.T) {
		t.Parallel()

		c := di.New()
		di.Provide[*sql.DB](c, newDB)

		var db *sql.DB

		err := di.Resolve(c, &db)
		if err == nil {
			t.Fatal("Resolve() = nil, want an error")
		}

		if want := "di.Resolve: the container is not built"; err.Error() != want {
			t.Errorf("Resolve() = %q, want %q", err, want)
		}
	})

	t.Run("a type nobody provided", func(t *testing.T) {
		t.Parallel()

		c := di.New()

		mustBuild(t, c)

		var db *sql.DB

		err := di.Resolve(c, &db)
		if err == nil {
			t.Fatal("Resolve() = nil, want an error")
		}

		if want := "di.Resolve: no provider for *sql.DB"; err.Error() != want {
			t.Errorf("Resolve() = %q, want %q", err, want)
		}
	})

	t.Run("a type that is a group", func(t *testing.T) {
		t.Parallel()

		c := di.New()
		di.Contribute[string](c, func() string { return "a" })

		mustBuild(t, c)

		var one string

		err := di.Resolve(c, &one)
		if err == nil {
			t.Fatal("Resolve() = nil, want an error")
		}

		if want := "di.Resolve: string is contributed to a group, not provided singly"; err.Error() != want {
			t.Errorf("Resolve() = %q, want %q", err, want)
		}
	})

	t.Run("a group that is a single provider", func(t *testing.T) {
		t.Parallel()

		c := di.New()
		di.Provide[*sql.DB](c, newDB)

		mustBuild(t, c)

		var many []*sql.DB

		err := di.Group(c, &many)
		if err == nil {
			t.Fatal("Group() = nil, want an error")
		}

		if want := "di.Group: *sql.DB is provided singly, not contributed to a group"; err.Error() != want {
			t.Errorf("Group() = %q, want %q", err, want)
		}
	})
}

// TestGroupOfNothingIsEmpty: no contributors is a legitimate answer, not a
// failure. A service with no middleware still boots.
func TestGroupOfNothingIsEmpty(t *testing.T) {
	t.Parallel()

	c := di.New()

	mustBuild(t, c)

	var got []string
	mustGroup(t, c, &got)

	if len(got) != 0 {
		t.Errorf("Group() = %v, want empty", got)
	}
}

// TestNearMiss covers SPEC.md §6.12: the type is not provided, but something one
// pointer or one interface away is. dig has four test groups for these
// confusions, which is why they earn their own message.
func TestNearMiss(t *testing.T) {
	t.Parallel()

	t.Run("the pointer is provided, the value was asked for", func(t *testing.T) {
		t.Parallel()

		c := di.New()
		di.Provide[*dsn](c, newDSN)

		mustBuild(t, c)

		var value dsn

		err := di.Resolve(c, &value)
		if err == nil {
			t.Fatal("Resolve() = nil, want an error")
		}

		if !contains(err.Error(), "no provider for di_test.dsn") {
			t.Errorf("Resolve() = %q, want it to name the missing value type", err)
		}

		if !contains(errors.Fix(err), "*di_test.dsn") {
			t.Errorf("Fix() = %q, want it to point at the pointer that is provided", errors.Fix(err))
		}
	})

	t.Run("an implementation is provided, an interface was asked for", func(t *testing.T) {
		t.Parallel()

		c := di.New()
		di.Provide[*tagged](c, newTagged)

		mustBuild(t, c)

		var port reader

		err := di.Resolve(c, &port)
		if err == nil {
			t.Fatal("Resolve() = nil, want an error")
		}

		if !contains(err.Error(), "no provider for di_test.reader") {
			t.Errorf("Resolve() = %q, want it to name the missing interface", err)
		}
	})
}

// TestResolveIsConcurrencySafe: after Build the instance map is never written
// again, which is what lets resolution run without a lock.
func TestResolveIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	c := di.New()
	di.Provide[*sql.DB](c, newDB)
	di.Provide[*orders.Repository](c, orders.NewRepository)
	di.Contribute[string](c, func() string { return "a" })

	mustBuild(t, c)

	const goroutines = 100

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			var (
				repo *orders.Repository
				tags []string
			)

			resolveErr := di.Resolve(c, &repo)
			if resolveErr != nil {
				t.Errorf("Resolve() = %v, want nil", resolveErr)
			}

			groupErr := di.Group(c, &tags)
			if groupErr != nil {
				t.Errorf("Group() = %v, want nil", groupErr)
			}
		}()
	}

	wg.Wait()
}

// TestGraph is the data warren graph di and warren explain di read.
func TestGraph(t *testing.T) {
	t.Parallel()

	c := di.New()
	di.Provide[*orders.Repository](c, orders.NewRepository)
	di.Supply(c, newDB())
	di.Contribute[string](c, func() string { return "a" })

	nodes := c.Graph().Nodes
	if len(nodes) != 3 {
		t.Fatalf("Graph() has %d nodes, want 3", len(nodes))
	}

	// Sorted by type name, so a rendering is stable between runs.
	if got := nodes[0].Type.String(); got != "*orders.Repository" {
		t.Errorf("first node is %s, want *orders.Repository", got)
	}

	if got := nodes[0].Kind; got != di.KindProvided {
		t.Errorf("Kind = %v, want Provided", got)
	}

	if got := len(nodes[0].Deps); got != 1 {
		t.Errorf("*orders.Repository has %d deps, want 1", got)
	}

	if got := nodes[1].Kind; got != di.KindSupplied {
		t.Errorf("*sql.DB Kind = %v, want Supplied", got)
	}

	if got := nodes[2].Kind; got != di.KindContributed {
		t.Errorf("string Kind = %v, want Contributed", got)
	}
}

// TestKindString keeps the enum's rendering honest, including a value outside the
// set.
func TestKindString(t *testing.T) {
	t.Parallel()

	tests := map[di.Kind]string{
		di.KindProvided:    "Provided",
		di.KindContributed: "Contributed",
		di.KindSupplied:    "Supplied",
		di.Kind(7):         "Kind(7)",
	}

	for kind, want := range tests {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}

	return false
}
