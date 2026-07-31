package lifecycle_test

import (
	"context"
	"fmt"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/lifecycle"
)

// ExampleHooks_Append registers a component from its constructor, which is what
// makes the order correct: constructors run in dependency order, so hooks arrive
// in dependency order.
func ExampleHooks_Append() {
	hooks := lifecycle.New()

	hooks.Append(lifecycle.Hook{
		Name:    "postgres.pool",
		OnStart: func(context.Context) error { fmt.Println("pool: dialling"); return nil },
		OnStop:  func(context.Context) error { fmt.Println("pool: closed"); return nil },
	})

	ctx := context.Background()

	err := hooks.Start(ctx)
	if err != nil {
		fmt.Println("start:", err)

		return
	}

	err = hooks.Stop(ctx)
	if err != nil {
		fmt.Println("stop:", err)
	}

	// Output:
	// pool: dialling
	// pool: closed
}

// ExampleClose registers a component that has nothing to start and a closer that
// cannot fail.
func ExampleClose() {
	hooks := lifecycle.New()

	hooks.Append(lifecycle.Close("broker.memory", func() {
		fmt.Println("broker: closed")
	}))

	err := hooks.Stop(mustStart(hooks))
	if err != nil {
		fmt.Println("stop:", err)
	}

	// Output:
	// broker: closed
}

// ExampleCloser registers a component whose closer returns an error, which is
// the shape sql.DB, os.File, and net.Listener all have.
func ExampleCloser() {
	hooks := lifecycle.New()

	hooks.Append(lifecycle.Closer("postgres.pool", func() error {
		fmt.Println("pool: closed")

		return nil
	}))

	err := hooks.Stop(mustStart(hooks))
	if err != nil {
		fmt.Println("stop:", err)
	}

	// Output:
	// pool: closed
}

// ExampleHooks_Start starts components in registration order, and unwinds what
// it started when one of them fails.
func ExampleHooks_Start() {
	hooks := lifecycle.New()

	hooks.Append(lifecycle.Hook{
		Name:    "postgres.pool",
		OnStart: func(context.Context) error { fmt.Println("pool: open"); return nil },
		OnStop:  func(context.Context) error { fmt.Println("pool: closed"); return nil },
	})
	hooks.Append(lifecycle.Hook{
		Name: "http.server",
		OnStart: func(context.Context) error {
			return errors.Internal("bind: address already in use")
		},
	})

	err := hooks.Start(context.Background())
	if err != nil {
		fmt.Println("boot failed, and the pool was closed on the way out")
	}

	// Output:
	// pool: open
	// pool: closed
	// boot failed, and the pool was closed on the way out
}

// ExampleHooks_Stop stops components in reverse order, so the listener drains
// before the pool it depends on closes.
func ExampleHooks_Stop() {
	hooks := lifecycle.New()

	hooks.Append(lifecycle.Close("postgres.pool", func() { fmt.Println("pool: closed") }))
	hooks.Append(lifecycle.Close("http.server", func() { fmt.Println("server: drained") }))

	err := hooks.Stop(mustStart(hooks))
	if err != nil {
		fmt.Println("stop:", err)
	}

	// Output:
	// server: drained
	// pool: closed
}

// ExampleHooks_State reports readiness. Ready is the only value that means
// ready, which is what a health endpoint renders.
func ExampleHooks_State() {
	hooks := lifecycle.New()
	hooks.Append(lifecycle.Close("postgres.pool", func() {}))

	fmt.Println("before:", hooks.State())

	ctx := mustStart(hooks)

	fmt.Println("serving:", hooks.State())

	err := hooks.Stop(ctx)
	if err != nil {
		fmt.Println("stop:", err)
	}

	fmt.Println("after:", hooks.State())

	// Output:
	// before: New
	// serving: Ready
	// after: Stopped
}

// mustStart starts hooks and returns the context to stop them with, so that an
// example about stopping is not half made of start-error handling.
func mustStart(hooks *lifecycle.Hooks) context.Context {
	ctx := context.Background()

	err := hooks.Start(ctx)
	if err != nil {
		fmt.Println("start:", err)
	}

	return ctx
}
