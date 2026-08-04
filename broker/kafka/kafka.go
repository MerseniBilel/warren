// Package kafka implements the warren/broker port over twmb/franz-go.
//
// It provides broker.Publisher and broker.Subscriber, joins one consumer
// group, and runs every subscription the application registered through the
// port's consumer chain. Swapping this block in main.go for another driver's
// is the only change a service makes to move off Kafka — no handler, no
// consumer, and no test touches a Kafka type.
//
// # It is at-least-once, and it says so
//
// The producer is idempotent, so a produce retry does not duplicate a record
// within a session. That is NOT exactly-once and nothing here claims to be:
// the outbox's ack is in the database and the publish is in Kafka, two
// systems no Kafka transaction spans. Duplicates are prevented downstream, by
// the inbox dedupe the consumer chain applies by default.
package kafka

import (
	"context"
	"crypto/tls"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
)

// ModuleName is the name of the module Broker returns — the scope name that
// appears in boot diagnostics.
const ModuleName = "warren/broker/kafka"

// Option configures the Kafka module.
type Option struct{ apply func(*config) }

type config struct {
	brokers        []string
	group          string
	clientID       string
	tls            *tls.Config
	mechanism      Mechanism
	rawSASL        sasl.Mechanism
	balancer       Balancer
	commitInterval time.Duration
	sessionTimeout time.Duration
	connectTimeout time.Duration
	produceTimeout time.Duration
	fetchMaxBytes  int32
	maxPollRecords int
	healthTimeout  time.Duration
	configure      []kgo.Opt
	raw            []func(context.Context, *kgo.Client) error
}

func defaults() config {
	return config{
		balancer:       BalancerCooperative,
		commitInterval: 5 * time.Second,
		sessionTimeout: 45 * time.Second,
		connectTimeout: 10 * time.Second,
		produceTimeout: 30 * time.Second,
		fetchMaxBytes:  50 << 20,
		maxPollRecords: 500,
		healthTimeout:  2 * time.Second,
	}
}

// Broker returns the warren.Module providing broker.Publisher and
// broker.Subscriber over Kafka.
//
// The constructor PARSES: missing brokers, an unknown balancer, and a
// malformed SASL setting all fail at wiring, before any hook runs. The
// lifecycle hook CONNECTS: seeds are dialled and metadata fetched in OnStart
// under ConnectTimeout, so an unreachable cluster rolls the boot back instead
// of leaving a live client behind a boot that failed later for something else.
//
// Two hooks are appended, and the DEPENDENCY between them fixes the shutdown
// order warren.md §2.3 requires. The subscription runner depends on the
// client, so it is built second, appended second, and — teardown being
// reverse-order — stops FIRST: consumers stop fetching and in-flight messages
// ack at step 3. The client stays open through the outbox relay's step-4
// flush and closes at step 5.
func Broker(opts ...Option) warren.Module {
	cfg := defaults()
	for _, o := range opts {
		o.apply(&cfg)
	}
	return warren.NewModule(ModuleName,
		warren.Providers(
			func(lc lifecycle.Lifecycle, reg health.Registry) (*client, error) {
				return newClient(cfg, lc, reg)
			},
			// Correlating, so a direct publish carries the correlation ID of
			// the request that made it and the consumer's own log lines
			// belong to the same causal chain. The outbox path is already
			// stamped at Append — outbox.Sink runs inside the request, and
			// the relay publishes long after it is gone — and Correlating
			// never overwrites a header that is already there.
			func(c *client) broker.Publisher { return broker.Correlating(c) },
			func(c *client) broker.Subscriber { return c },
		),
		warren.Exports[broker.Publisher](),
		warren.Exports[broker.Subscriber](),
		warren.Eager[*client](),
	)
}

// Brokers sets the seed broker addresses. Required: omitting it fails the
// boot naming the fix.
func Brokers(addrs ...string) Option {
	return Option{apply: func(c *config) { c.brokers = addrs }}
}

// ConsumerGroup sets the group this service joins. It is required before any
// subscription can start; a service that only publishes may omit it.
func ConsumerGroup(name string) Option {
	return Option{apply: func(c *config) { c.group = name }}
}

// ClientID sets the client id the brokers report in their logs and quotas.
func ClientID(id string) Option {
	return Option{apply: func(c *config) { c.clientID = id }}
}

// TLS configures transport security. crypto/tls is the standard library, not
// a driver type, so this is the ordinary path and not an escape hatch.
func TLS(cfg *tls.Config) Option {
	return Option{apply: func(c *config) { c.tls = cfg }}
}

// PartitionAssignment selects the consumer group's balancer. The default is
// BalancerCooperative.
func PartitionAssignment(b Balancer) Option {
	return Option{apply: func(c *config) { c.balancer = b }}
}

// CommitInterval is how often marked offsets are flushed to the group
// coordinator. The default is 5s.
//
// It is a DUPLICATE-WINDOW knob, never a correctness one: an offset is marked
// only after the consumer chain has disposed of the message, marked offsets
// are committed when a partition is revoked, and again at shutdown. A longer
// interval costs fewer coordinator round trips and redelivers more after an
// unclean exit; it can never ack a message a handler did not finish.
func CommitInterval(d time.Duration) Option {
	return Option{apply: func(c *config) { c.commitInterval = d }}
}

// SessionTimeout is how long the coordinator waits for a heartbeat before
// declaring this member dead and rebalancing. The default is 45s.
//
// It bounds how long a partition is stalled by a crashed instance, and it
// must EXCEED the drain budget — otherwise a slow shutdown triggers the
// rebalance it was draining to get ahead of.
func SessionTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.sessionTimeout = d }}
}

// ConnectTimeout bounds the dial and the metadata fetch in OnStart. The
// default is 10s: a boot that hangs on an unreachable cluster is worse than a
// boot that fails.
func ConnectTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.connectTimeout = d }}
}

// ProduceTimeout bounds one Publish including its retries. The default is
// 30s. On expiry Publish returns UNAVAILABLE, which leaves an outbox record
// for the next drain rather than parking it.
func ProduceTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.produceTimeout = d }}
}

// FetchMaxBytes caps one fetch response per broker. The default is 50 MiB.
func FetchMaxBytes(n int32) Option {
	return Option{apply: func(c *config) { c.fetchMaxBytes = n }}
}

// MaxPollRecords caps how many records one poll hands to the chain. The
// default is 500.
//
// Records are dispatched one goroutine per partition — ordered within a
// partition, parallel across them — so this bounds the BATCH, not the
// concurrency. Use broker.WithConcurrency for that.
func MaxPollRecords(n int) Option {
	return Option{apply: func(c *config) { c.maxPollRecords = n }}
}

// HealthTimeout bounds one readiness probe. The default is 2s.
func HealthTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.healthTimeout = d }}
}

// Configure appends franz-go options to the ones this package builds, BEFORE
// the client is created — the only moment a hook, a custom partitioner or a
// dial function can still be set. It is the first of this package's two named
// escape hatches (AGENT.md invariant 3), and it exists because instrumentation
// cannot be wired any other way: the hook seam is a franz-go interface, and
// warren/observability may not import this module (invariant 4).
//
//	kafka.Configure(kgo.WithHooks(kotel.NewKotel().Hooks()...))
//
// Options passed here are applied LAST and win over this package's own,
// including the ones that keep the consumer correct. Overriding
// AutoCommitMarks or BlockRebalanceOnPoll will ack messages the consumer
// chain has not finished with.
func Configure(opts ...kgo.Opt) Option {
	return Option{apply: func(c *config) { c.configure = append(c.configure, opts...) }}
}

// Raw runs fn against the live client during OnStart, after the metadata
// fetch succeeded and before any dependent hook starts. An error fails the
// boot.
//
// It is the second named escape hatch: create a topic, inspect broker
// metadata, reach a franz-go API this package does not model. It CANNOT
// install a hook — hooks are read when the client is constructed, which has
// already happened. Use Configure for that.
//
// The client is deliberately NOT provided into the container. A *kgo.Client
// any constructor could inject is not an escape hatch, it is a second default
// path — and a consumer that reaches kgo.Record has left the driver-agnostic
// chain this package exists to keep it in.
func Raw(fn func(context.Context, *kgo.Client) error) Option {
	return Option{apply: func(c *config) { c.raw = append(c.raw, fn) }}
}
