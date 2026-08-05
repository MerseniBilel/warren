//go:build integration

// The exported broker contract, run against a REAL Kafka.
//
// It lives here, behind the build tag, because AGENT.md's unit-test rule is
// no Docker and no network — and because the claim this file exists to
// support cannot be made by a test that never talks to a broker.
//
// Until this existed, brokertest.Run had exactly ONE caller: broker/memory.
// So "the same exported contract suite that broker/kafka passes" — the
// sentence broker/rabbitmq's spec used to justify deferring itself, and the
// sentence the whole community-adapter strategy rests on — was never true of
// Kafka. An in-process broker is the easiest possible subject: "the
// subscription is live" is a map insert, delivery is a channel send, and
// ordering is whatever order the sender used. Every one of those is a
// network round trip here.
//
// Run it:
//
//	docker run --rm -d --name warren-kafka -p 9092:9092 apache/kafka:3.9.0 …
//	WARREN_TEST_KAFKA_BROKERS=localhost:9092 go test -tags integration ./...
package kafka_test

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/brokertest"
	"github.com/MerseniBilel/warren/broker/kafka"
)

// brokers returns the seed addresses, or skips.
func brokers(t *testing.T) string {
	t.Helper()
	v := os.Getenv("WARREN_TEST_KAFKA_BROKERS")
	if v == "" {
		t.Skip("WARREN_TEST_KAFKA_BROKERS is not set. To run these:\n" +
			"  docker run --rm -d --name warren-kafka -p 9092:9092 apache/kafka:3.9.0\n" +
			"  export WARREN_TEST_KAFKA_BROKERS=localhost:9092")
	}
	return v
}

// group makes each subtest its own consumer group.
//
// brokertest's suite assumes newBroker gives it a clean subject — the same
// requirement RunVersionedContract states outright, and the one that made
// that suite pass on memory and fail on Postgres. Sharing a group across
// subtests would split partitions between them and deliver each message to
// exactly one, which reads as loss rather than as the harness's fault.
var groupSeq int

func newKafka(t *testing.T) (broker.Publisher, broker.Subscriber) {
	t.Helper()
	addr := brokers(t)
	groupSeq++
	group := "warren-contract-" + t.Name() + "-" + strconv.Itoa(groupSeq)

	var (
		pub broker.Publisher
		sub broker.Subscriber
	)
	km := kafka.Broker(kafka.Brokers(addr), kafka.ConsumerGroup(group), kafka.AutoCreateTopics())
	probe := warren.NewModule("probe",
		warren.Imports(km),
		warren.Providers(func(p broker.Publisher, s broker.Subscriber) *captured {
			pub, sub = p, s
			return &captured{}
		}),
		warren.Eager[*captured](),
	)

	a := warren.New(km, probe)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	// Kafka topics OUTLIVE a subtest; an in-process broker's do not. Every
	// subtest asks for topic "orders", so without this each one consumes the
	// PREVIOUS subtests' records: envelope round-trip read "evt-0" from the
	// at-least-once test and reported the envelope as corrupt. A fresh
	// consumer group does not save you — it makes it certain, because a group
	// with no committed offset resets to the START of the topic.
	//
	// Renaming topics is the whole of the isolation. Nothing else about the
	// driver is wrapped, so what the suite exercises is still the real one.
	p := "-" + strconv.Itoa(groupSeq)
	return prefixPub{pub, p}, prefixSub{sub, p}
}

// prefixPub and prefixSub give a subtest its own topic namespace.
type prefixPub struct {
	pub    broker.Publisher
	suffix string
}

func (p prefixPub) Publish(ctx context.Context, topic string, msgs ...broker.Message) error {
	return p.pub.Publish(ctx, topic+p.suffix, msgs...)
}

type prefixSub struct {
	sub    broker.Subscriber
	suffix string
}

func (p prefixSub) Subscribe(ctx context.Context, topic string, h broker.MessageHandler) error {
	return p.sub.Subscribe(ctx, topic+p.suffix, h)
}

type captured struct{}

// TestKafkaContract is the whole point of the file: the exported suite,
// unmodified, against a real broker.
//
// It certifies the properties a community adapter is held to — envelope
// round-trip, at-least-once delivery, handler errors reaching the driver,
// batch publish, topic isolation, fan-out, per-key ordering, drain on
// cancellation, and that Subscribe RETURNS ONCE LIVE. That last one is the
// contract changed on 2026-08-05, and it had only ever been proven where
// "live" is a map insert; on Kafka it is a group join.
func TestKafkaContract(t *testing.T) {
	brokertest.Run(t, newKafka)
}
