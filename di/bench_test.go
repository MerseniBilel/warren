package di_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/di/internal/orders"
)

// sizes are the provider counts the startup budget is measured against.
// docs/roadmap.md allows 50 ms for the whole of boot, so the container's share of
// it has to be a rounding error.
func sizes() []int { return []int{10, 100, 1000} }

// registerN builds a container with a realistic chain plus n group members.
//
// The members are how a provider count is scaled without generating n Go types:
// what is being measured is the cost of the walk over providers, which does not
// depend on how many distinct types they carry.
func registerN(n int) *di.Container {
	c := di.New()
	di.Provide[*sql.DB](c, newDB)
	di.Provide[*orders.Repository](c, orders.NewRepository)
	di.Provide[*orders.Handler](c, orders.NewHandler)

	for i := range n {
		tag := strconv.Itoa(i)
		di.Contribute[string](c, func() string { return tag })
	}

	return c
}

func BenchmarkValidate(b *testing.B) {
	for _, n := range sizes() {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			c := registerN(n)

			b.ReportAllocs()

			for b.Loop() {
				err := c.Validate()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBuild(b *testing.B) {
	ctx := context.Background()

	for _, n := range sizes() {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				// A container is built once, so the registration is inside the
				// measurement: a service pays for both, once.
				err := registerN(n).Build(ctx)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkResolve(b *testing.B) {
	c := registerN(10)

	buildErr := c.Build(context.Background())
	if buildErr != nil {
		b.Fatal(buildErr)
	}

	var repo *orders.Repository

	b.ReportAllocs()

	for b.Loop() {
		err := di.Resolve(c, &repo)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGroup(b *testing.B) {
	c := registerN(10)

	buildErr := c.Build(context.Background())
	if buildErr != nil {
		b.Fatal(buildErr)
	}

	var tags []string

	b.ReportAllocs()

	for b.Loop() {
		err := di.Group(c, &tags)
		if err != nil {
			b.Fatal(err)
		}
	}
}
