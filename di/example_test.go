package di_test

import (
	"context"
	"fmt"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/errors"
)

// Config is a value a service loads before the container exists.
type Config struct{ DSN string }

// Database stands in for a driver handle.
type Database struct{ DSN string }

// UserRepository is the port a use case depends on.
type UserRepository interface {
	Count() int
}

// sqlUsers is the implementation registered against that port.
type sqlUsers struct{ db *Database }

func (s *sqlUsers) Count() int { return len(s.db.DSN) }

// Middleware is a group: several modules contribute, and one consumer reads them
// all in registration order.
type Middleware string

type middlewareChain struct{ mw []Middleware }

func newDatabase(cfg *Config) *Database         { return &Database{DSN: cfg.DSN} }
func newSQLUsers(db *Database) *sqlUsers        { return &sqlUsers{db: db} }
func newAuth() Middleware                       { return authTag }
func newRecovery() Middleware                   { return "recovery" }
func newChain(mw []Middleware) *middlewareChain { return &middlewareChain{mw: mw} }

// ExampleProvide registers a constructor against the port it satisfies, which is
// the pattern the dependency rule in docs/architecture.md §4 is built on: the port
// is declared in domain, the implementation in infrastructure, and only this line
// sees both.
func ExampleProvide() {
	c := di.New()
	di.Supply(c, &Config{DSN: localDSN})
	di.Provide[*Database](c, newDatabase)
	di.Provide[UserRepository](c, newSQLUsers)

	err := c.Build(context.Background())
	if err != nil {
		fmt.Println(err)

		return
	}

	var users UserRepository

	err = di.Resolve(c, &users)
	if err != nil {
		fmt.Println(err)

		return
	}

	fmt.Println(users.Count())
	// Output: 20
}

// ExampleSupply registers a value that was built before the container, which is
// how configuration reaches the graph.
func ExampleSupply() {
	c := di.New()
	di.Supply(c, &Config{DSN: "postgres://shop"})

	err := c.Build(context.Background())
	if err != nil {
		fmt.Println(err)

		return
	}

	var cfg *Config

	err = di.Resolve(c, &cfg)
	if err != nil {
		fmt.Println(err)

		return
	}

	fmt.Println(cfg.DSN)
	// Output: postgres://shop
}

// ExampleContribute collects one type from several modules. The order is the order
// the registrations ran, which is what a middleware chain depends on.
func ExampleContribute() {
	c := di.New()
	di.Contribute[Middleware](c, newAuth)
	di.Contribute[Middleware](c, newRecovery)
	di.Provide[*middlewareChain](c, newChain)

	err := c.Build(context.Background())
	if err != nil {
		fmt.Println(err)

		return
	}

	var chain *middlewareChain

	err = di.Resolve(c, &chain)
	if err != nil {
		fmt.Println(err)

		return
	}

	fmt.Println(chain.mw)
	// Output: [auth recovery]
}

// ExampleGroup reads every contributed member directly, rather than through a
// constructor that consumes the slice.
func ExampleGroup() {
	c := di.New()
	di.Contribute[Middleware](c, newAuth)
	di.Contribute[Middleware](c, newRecovery)

	err := c.Build(context.Background())
	if err != nil {
		fmt.Println(err)

		return
	}

	var mw []Middleware

	err = di.Group(c, &mw)
	if err != nil {
		fmt.Println(err)

		return
	}

	fmt.Println(len(mw), mw[0])
	// Output: 2 auth
}

// ExampleResolve reports a type nobody provided, rather than returning a zero
// value a caller would then dereference.
func ExampleResolve() {
	c := di.New()

	err := c.Build(context.Background())
	if err != nil {
		fmt.Println(err)

		return
	}

	var cfg *Config

	fmt.Println(di.Resolve(c, &cfg))
	// Output: di.Resolve: no provider for *di_test.Config
}

// ExampleContainer_Validate shows the message a missing provider produces. It is
// the v0.1 exit criterion in docs/roadmap.md: the resolution chain, the file that
// asked, and a fix that can be pasted.
func ExampleContainer_Validate() {
	site := di.Site{File: "internal/modules/users/module.go", Line: 12}

	c := di.New()
	di.Provide[UserRepository](c, newSQLUsers, di.At(site))
	di.Provide[*Database](c, newDatabase, di.At(site))

	fmt.Println(errors.Detail(c.Validate()))
	// Output:
	// di.Validate: no provider for *di_test.Config
	//
	//   requested by  internal/modules/users/module.go:12
	//   chain         di_test.UserRepository → *di_test.Database → *di_test.Config
	//
	//   fix: add di.Provide[*di_test.Config](c, NewConfig) to internal/modules/users/module.go
}

// ExampleContainer_Build constructs every singleton in dependency order, which is
// also the order warren/lifecycle stops them in reverse.
func ExampleContainer_Build() {
	c := di.New()
	di.Supply(c, &Config{DSN: localDSN})
	di.Provide[*Database](c, newDatabase)

	fmt.Println(c.Build(context.Background()))
	// Output: <nil>
}

// ExampleContainer_Graph returns the graph as data, which is what warren graph di
// and warren explain di read.
func ExampleContainer_Graph() {
	c := di.New()
	di.Supply(c, &Config{DSN: localDSN})
	di.Provide[*Database](c, newDatabase)
	di.Contribute[Middleware](c, newAuth)

	nodes := c.Graph().Nodes

	for i := range nodes {
		fmt.Printf("%-20s %-12s %d dep(s)\n", nodes[i].Type, nodes[i].Kind, len(nodes[i].Deps))
	}
	// Output:
	// *di_test.Config      Supplied     0 dep(s)
	// *di_test.Database    Provided     1 dep(s)
	// di_test.Middleware   Contributed  0 dep(s)
}
