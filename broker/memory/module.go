package memory

import (
	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/consumer"
	"github.com/MerseniBilel/warren/inbox"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/transport"
)

// ModuleName is the name of the module Module returns — the scope name that
// appears in a diagnostic when something it provides cannot be built.
const ModuleName = "warren/broker/memory"

// Module serves the event subscriptions registered with r.OnEvent, in process.
//
// It is the counterpart to transport/http's Server: that one serves
// Table.HTTP(), this one serves Table.Events(). Until it existed, r.OnEvent
// was a registration verb NOTHING consumed — a service registered
// subscriptions, boot refused them with a remedy naming a module that did not
// exist, and a test harness claimed the protocol on its own behalf and dropped
// every message in silence.
//
// It claims transport.ProtocolEvent, so a service that registers r.OnEvent and
// adds this module boots; one that registers r.OnEvent and adds no broker
// module still fails at boot, which is the point of the claim.
//
// It provides the in-process broker as both Publisher and Subscriber, so a
// modular monolith publishes and consumes through the port from day one and
// extraction later swaps the driver rather than rewriting call sites.
//
// IT IS NOT FOR PRODUCTION beyond a modular monolith that accepts the terms:
// an in-process broker loses every unacknowledged message when the process
// stops, and nothing is replayed. A service that needs durability uses
// warren/broker/kafka.
func Module(opts ...ModuleOption) warren.Module {
	cfg := moduleConfig{codec: transport.JSON()}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	return warren.NewModule(ModuleName,
		warren.Providers(
			func() *ports {
				b := New()
				return &ports{pub: b, sub: b}
			},
			func(p *ports) broker.Publisher { return broker.Correlating(p.pub) },
			func(p *ports) broker.Subscriber { return p.sub },
			func(tbl *transport.Table, sub broker.Subscriber, pub broker.Publisher, lc lifecycle.Lifecycle) *consumers {
				// The claim is what makes Table.Unserved() pass for events.
				// It happens here, at construction, exactly as
				// transport/http/server.go does for HTTP.
				tbl.Claim(transport.ProtocolEvent, ModuleName)

				store := cfg.inbox
				if store == nil {
					store = inbox.NewMemoryStore()
				}
				dlq := cfg.dlq
				if dlq == nil {
					// A dead letter is observable in the same process, which
					// is the only place an in-process broker could put it.
					dlq = pub
				}

				// The frozen route table, converted to the driver-neutral
				// shape broker.Serve takes. Bind runs ONCE per route, here at
				// boot — never per message.
				subs := make([]consumer.Subscription, 0, len(tbl.Events()))
				for _, route := range tbl.Events() {
					subs = append(subs, consumer.Subscription{
						Name:    route.Name,
						Topic:   route.Topic,
						Handler: route.Bind(cfg.codec),
						Options: route.Options,
					})
				}
				consumer.Serve(lc, ModuleName, subs, sub, store, dlq)
				return &consumers{}
			},
		),
		warren.Exports[broker.Publisher](),
		warren.Exports[broker.Subscriber](),
		// Nothing injects the consumers, so without Eager they would never be
		// built — and no subscription would ever be served.
		warren.Eager[*consumers](),
	)
}

// ports is one broker handed out as both halves. Two constructors would build
// two brokers, and the publisher's messages would never reach the subscriber.
type ports struct {
	pub broker.Publisher
	sub broker.Subscriber
}

// consumers exists to be built eagerly; it holds nothing.
type consumers struct{}

// ModuleOption configures Module.
type ModuleOption struct{ apply func(*moduleConfig) }

type moduleConfig struct {
	inbox inbox.Store
	dlq   broker.Publisher
	codec transport.Codec
}

// Inbox supplies the deduplication store the consumer chain requires. The
// default is inbox.NewMemoryStore().
func Inbox(s inbox.Store) ModuleOption {
	return ModuleOption{apply: func(c *moduleConfig) { c.inbox = s }}
}

// DeadLetters routes exhausted messages to p. The default is this module's own
// publisher, so a dead letter is observable in the same process.
func DeadLetters(p broker.Publisher) ModuleOption {
	return ModuleOption{apply: func(c *moduleConfig) { c.dlq = p }}
}

// Codec decodes message payloads into the handler's request type. The default
// is transport.JSON(), and it is LENIENT deliberately: this codec decodes
// events, a decode failure is INVALID, and §2.6 dead-letters INVALID without
// retrying — so a producer adding a field to a payload would take out 100% of
// a consumer's traffic under a strict codec.
func Codec(c transport.Codec) ModuleOption {
	return ModuleOption{apply: func(cfg *moduleConfig) { cfg.codec = c }}
}
