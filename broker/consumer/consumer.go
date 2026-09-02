package consumer

import (
	"context"
	"fmt"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/lifecycle"
)

// Package consumer is what every event adapter shares: the loop that turns a
// frozen route table's subscriptions into a running consumer, and the two
// rules a driver must not get wrong.
//
// It is a SEPARATE PACKAGE from warren/broker because broker is a CONTRACTS
// package — interfaces, types and pure functions — and registering lifecycle
// hooks is an implementation. The package's own ring test caught that
// distinction when this code briefly lived there, which is the guard doing
// its job.

// Subscription is one thing to serve: a handler, the topic it consumes, and
// the identity that scopes its deduplication.
//
// It carries NO transport type, deliberately. warren/transport imports this
// package, so a helper here that took transport.EventRoute would close an
// import cycle. Each adapter converts the frozen route table into these — five
// lines — and everything after that conversion is identical for every driver,
// which is the part worth having in one place.
type Subscription struct {
	// Name is the subscription identity, "<module>.<handler>". It scopes
	// deduplication: two features consuming one topic must not suppress each
	// other's copies, so this and not the topic is what the inbox keys on.
	Name string

	// Topic is what the driver subscribes to.
	Topic string

	// Handler is the decoded, bound handler — built ONCE at boot, never per
	// message.
	Handler broker.MessageHandler

	// Options are the per-subscription options, forwarded to Pipeline.
	Options []broker.SubscribeOption
}

// Serve wires subscriptions onto sub and registers the lifecycle hooks that
// start and drain them. Every event adapter calls it, so the two rules below
// are written down once instead of being rediscovered per driver.
//
// THE LOOP'S CONTEXT KEEPS THE BOOT CONTEXT'S VALUES AND DROPS ITS
// CANCELLATION. OnStart's context dies when boot finishes, so a subscription
// holding it would stop consuming the moment the application became ready.
// context.Background() is the obvious fix and the wrong one: the boot context
// carries app.Telemetry, so severing it silently ends trace continuation for
// every message. context.WithCancel(context.WithoutCancel(ctx)) keeps the
// values and takes a cancel this package owns.
//
// SUBSCRIBE IS NOT WRAPPED IN A GOROUTINE. broker.Subscriber.Subscribe returns once
// the subscription is LIVE; running it in a goroutine would let OnStart report
// "started" before it existed, and anything published in that gap is discarded
// by a Publish that reports success. That race is invisible in a test with a
// sleep in it and fatal in one without.
func Serve(lc lifecycle.Lifecycle, name string, subs []Subscription, sub broker.Subscriber, store inbox.Store, dlq broker.Publisher) {
	if len(subs) == 0 {
		return
	}

	var (
		cancel func()
		waits  []func(context.Context) error
	)

	start := func(ctx context.Context) error {
		loopCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		cancel = stop
		for _, s := range subs {
			pipeline, wait := broker.Pipeline(s.Name, s.Topic, s.Handler, store, dlq, s.Options...)
			waits = append(waits, wait)
			if err := sub.Subscribe(loopCtx, s.Topic, pipeline); err != nil {
				stop()
				return fmt.Errorf("subscribing %s to %q: %w", s.Name, s.Topic, err)
			}
		}
		return nil
	}

	// Consumers stop LAST — after readiness closes and after the servers stop
	// accepting (§1.3). Hooks unwind in reverse registration order, so this
	// registering after the HTTP server's is what puts it first on the way
	// down. Cancel, then wait: cancelling alone returns before in-flight
	// messages finish, which is how a consumer loses the message it was
	// holding.
	stopHook := func(ctx context.Context) error {
		if cancel != nil {
			cancel()
		}
		for _, wait := range waits {
			if err := wait(ctx); err != nil {
				return err
			}
		}
		return nil
	}

	lc.Append(lifecycle.Hook{Name: name, OnStart: start, OnStop: stopHook})
}
