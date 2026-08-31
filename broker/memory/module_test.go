package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/memory"
	warrentest "github.com/MerseniBilel/warren/testing"
	"github.com/MerseniBilel/warren/transport"
)

type ping struct {
	ID string `json:"id"`
}
type pong struct{}

type ctl struct {
	h     app.Handler[ping, pong]
	topic string
}

func (c *ctl) Register(r *transport.Registrar) { r.OnEvent(c.topic, c.h) }

func moduleThatConsumes(name, topic string, got chan<- string) warren.Module {
	return warren.NewModule(name,
		warren.Providers(func() app.Handler[ping, pong] {
			return app.HandlerFunc[ping, pong](func(_ context.Context, p ping) (pong, error) {
				got <- name + ":" + p.ID
				return pong{}, nil
			})
		}),
		warren.Consumers(func(h app.Handler[ping, pong]) *ctl {
			return &ctl{h: h, topic: topic}
		}),
	)
}

// TestOnEventIsServed is the whole point of this module.
//
// Before it existed, nothing in the repository read Table.Events(): a service
// registered subscriptions with r.OnEvent, production refused the boot with a
// remedy naming a module that did not exist, and the test harness claimed the
// protocol on its own behalf and dropped every message in silence. This is the
// consumer of that table.
//
// THE PUBLISH HAS NO SLEEP BEFORE IT, deliberately. Subscriber.Subscribe
// returns once the subscription is LIVE, so a message published the instant
// boot finishes must arrive. Wrapping Subscribe in a goroutine — the obvious
// shape — would make this flaky and a sleep would hide it.
func TestOnEventIsServed(t *testing.T) {
	t.Parallel()

	got := make(chan string, 4)
	a := warrentest.NewModuleTest(t,
		moduleThatConsumes("orders", "order.placed", got),
		warrentest.WithModules(memory.Module()),
	)

	pub := warrentest.ResolveIn[broker.Publisher](t, a, memory.ModuleName)
	publish(t, pub, "order.placed", "o-1")

	select {
	case v := <-got:
		if v != "orders:o-1" {
			t.Errorf("handler received %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the consumer never ran — the subscription was registered and nothing served it")
	}
}

// TestTwoFeaturesOnOneTopicBothReceive pins the reason the inbox keys on the
// SUBSCRIPTION and not on the topic: two features consuming one topic must not
// suppress each other's copies. Keying on the message id alone would deliver
// to whichever subscription won the race and silently drop the other.
func TestTwoFeaturesOnOneTopicBothReceive(t *testing.T) {
	t.Parallel()

	got := make(chan string, 8)
	a := warrentest.NewModuleTest(t,
		moduleThatConsumes("billing", "user.registered", got),
		warrentest.WithModules(
			moduleThatConsumes("notification", "user.registered", got),
			memory.Module(),
		),
	)

	publish(t, warrentest.ResolveIn[broker.Publisher](t, a, memory.ModuleName), "user.registered", "u-1")

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case v := <-got:
			seen[v] = true
		case <-deadline:
			t.Fatalf("only %v received; both subscriptions must get their own copy", seen)
		}
	}
}

// TestNoBrokerModuleStillFailsTheBoot is the other half. The claim must not be
// a blanket amnesty: a service that registers r.OnEvent and adds NO broker
// module has a consumer that will never run, and that is detectable at boot.
func TestNoBrokerModuleStillFailsTheBoot(t *testing.T) {
	t.Parallel()

	got := make(chan string, 1)
	a := warren.New(moduleThatConsumes("orders", "order.placed", got))
	err := a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("a subscription with no broker module booted — it would never run")
	}
	t.Logf("refused, as it must:\n%v", err)
}

func publish(t *testing.T, pub broker.Publisher, topic, id string) {
	t.Helper()
	if err := pub.Publish(context.Background(), topic, broker.Message{
		ID: "evt-" + id, Type: topic, Key: id,
		Payload: []byte(`{"id":"` + id + `"}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}
