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
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/brokertest"
	"github.com/MerseniBilel/warren/broker/kafka"

	"github.com/twmb/franz-go/pkg/kgo"
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

// runToken makes every topic and group name unique to this process.
var runToken = strconv.FormatInt(time.Now().UnixNano(), 36)

func newKafka(t *testing.T) (broker.Publisher, broker.Subscriber) {
	t.Helper()
	addr := brokers(t)
	groupSeq++
	group := "warren-contract-" + runToken + "-" + t.Name() + "-" + strconv.Itoa(groupSeq)

	var (
		pub broker.Publisher
		sub broker.Subscriber
	)
	// NO AutoCreateTopics: the harness creates every topic itself, with six
	// partitions, and leaving auto-creation on would silently paper over a
	// creation that did not happen — which is exactly how the first version
	// of this ran the whole suite on ONE partition while claiming six.
	km := kafka.Broker(kafka.Brokers(addr), kafka.ConsumerGroup(group),
		// HARNESS TUNING, not a product default. franz-go throttles metadata
		// refreshes, so a topic created after the client started takes a
		// refresh cycle to appear — about five seconds, which is exactly
		// brokertest's deadline, and the envelope subtest failed one run in
		// three on it. A real deployment provisions topics before it boots
		// and never meets this.
		kafka.Configure(kgo.MetadataMinAge(250*time.Millisecond)),
	)
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
	//
	// The topics are created with SIX partitions, not left to
	// auto-creation's one. On one partition every partitioning question
	// answers itself — per-key ordering is whatever order the producer used,
	// and a key cannot spread wrongly — so the suite was certifying
	// properties the subject could not fail. Six is enough that a broken
	// partitioner shows up and small enough to stay fast.
	// runToken, not just the sequence: a topic that already exists keeps the
	// partition count it was created with, and CreateTopics answers
	// TOPIC_ALREADY_EXISTS. Reusing "orders-1" across runs meant the suite
	// inherited yesterday's one-partition topic.
	p := "-" + runToken + "-" + strconv.Itoa(groupSeq)
	ensure := func(topic string) {
		contractTopics.Do(topic, func() { freshPartitions(t, addr, topic) })
	}
	return prefixPub{pub, p, ensure}, prefixSub{sub, p, ensure}
}

// prefixPub and prefixSub give a subtest its own topic namespace.
type prefixPub struct {
	pub    broker.Publisher
	suffix string
	ensure func(string)
}

func (p prefixPub) Publish(ctx context.Context, topic string, msgs ...broker.Message) error {
	p.ensure(topic + p.suffix)
	return p.pub.Publish(ctx, topic+p.suffix, msgs...)
}

type prefixSub struct {
	sub    broker.Subscriber
	suffix string
	ensure func(string)
}

func (p prefixSub) Subscribe(ctx context.Context, topic string, h broker.MessageHandler) error {
	p.ensure(topic + p.suffix)
	return p.sub.Subscribe(ctx, topic+p.suffix, h)
}

// contractTopics makes topic creation once-per-name across the suite.
var contractTopics = onceByName{done: map[string]bool{}}

type onceByName struct {
	mu   sync.Mutex
	done map[string]bool
}

func (o *onceByName) Do(name string, fn func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done[name] {
		return
	}
	o.done[name] = true
	fn()
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
