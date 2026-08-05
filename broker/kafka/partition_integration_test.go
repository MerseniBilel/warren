//go:build integration

// Partitioning, which only a multi-partition topic can test.
//
// Every earlier run of the contract suite used auto-created topics, and an
// auto-created topic has ONE partition. On one partition every partitioning
// question answers itself: keys cannot spread wrongly, and per-key ordering
// is whatever order the producer used. Both properties were certified by a
// topic that could not fail them.
package kafka_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/broker/kafka"
)

const probeParts = 6

// TestKeylessMessagesUseEveryPartition is the regression test for a defect a
// one-partition topic hid completely.
//
// toRecord set Key: []byte(m.Key), and []byte("") is NOT nil in Go. franz-go
// hashes "records with non-nil keys", so an unkeyed message was handed a
// zero-length key to hash — and murmur2 of nothing is a constant. Every
// keyless message a service published went to ONE partition: measured 300 of
// 300 on partition 3 of 6, which caps consumer parallelism at a single member
// however many partitions you provision.
//
// The volume matters. A first attempt at this measurement used 60 tiny
// messages and saw 60 on one partition WITH the fix too, because franz-go's
// default partitioner deliberately sticks to one partition until a 64 KiB
// batch fills. Anything under that threshold cannot tell a stuck partitioner
// from a working one.
func TestKeylessMessagesUseEveryPartition(t *testing.T) {
	addr := brokers(t)
	topic := freshTopic(t, addr, "warren-keyless")

	pub, _ := newProbeBroker(t, addr)
	payload := make([]byte, 2048)
	for i := range payload {
		payload[i] = 'x'
	}
	for i := range 300 {
		if err := pub.Publish(context.Background(), topic,
			broker.Message{ID: fmt.Sprintf("m%d", i), Type: "probe", Payload: payload}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	counts := endOffsets(t, addr, topic)
	used := 0
	total := int64(0)
	for _, n := range counts {
		total += n
		if n > 0 {
			used++
		}
	}
	if total != 300 {
		t.Fatalf("partition totals sum to %d, want 300: %v", total, counts)
	}
	if used < 2 {
		t.Errorf("300 keyless messages used %d partition(s) of %d: %v\n"+
			"every keyless message is pinned to one partition, so only one consumer ever works",
			used, probeParts, counts)
	}
}

// TestOneKeyOnePartition is the other half, and the one per-key ordering
// rests on: a key must always choose the same partition, or two messages for
// one aggregate can be consumed in either order by different members.
func TestOneKeyOnePartition(t *testing.T) {
	addr := brokers(t)
	topic := freshTopic(t, addr, "warren-keyed")

	pub, _ := newProbeBroker(t, addr)
	const keys, per = 8, 40
	payload := make([]byte, 2048) // past the sticky-batch threshold again
	for k := range keys {
		for i := range per {
			err := pub.Publish(context.Background(), topic, broker.Message{
				ID: fmt.Sprintf("k%d-%d", k, i), Type: "probe",
				Key: fmt.Sprintf("aggregate-%d", k), Payload: payload,
			})
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
	}

	// Every key produced the same number of records, so if keys were spread
	// the partition totals could not be multiples of `per` — but the real
	// assertion is stronger and simpler: read the records back and check no
	// key appears on two partitions.
	where := keyPartitions(t, addr, topic)
	for key, parts := range where {
		if len(parts) != 1 {
			t.Errorf("key %q landed on %d partitions %v; per-key ordering cannot hold", key, len(parts), parts)
		}
	}
	if len(where) != keys {
		t.Errorf("saw %d keys, want %d", len(where), keys)
	}
}

// freshTopic creates a NEW topic with probeParts partitions.
func freshTopic(t *testing.T, addr, prefix string) string {
	t.Helper()
	// Unique per RUN, not per process: Kafka deletes topics ASYNCHRONOUSLY,
	// so delete-then-create with a fixed name races, and the reader then
	// counts the previous run's records as this one's. That cost a
	// misreading — a "saw 7 keys, want 8" failure that looked like the
	// defect under test and was leftover data.
	topic := fmt.Sprintf("%s-%s-%d", prefix, strconv.FormatInt(time.Now().UnixNano(), 36), topicSeq())

	admin, err := kgo.NewClient(kgo.SeedBrokers(addr))
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	t.Cleanup(admin.Close)

	req := kmsg.NewPtrCreateTopicsRequest()
	rt := kmsg.NewCreateTopicsRequestTopic()
	rt.Topic = topic
	rt.NumPartitions = probeParts
	rt.ReplicationFactor = 1
	req.Topics = append(req.Topics, rt)
	req.TimeoutMillis = 10000
	resp, err := req.RequestWith(context.Background(), admin)
	if err != nil {
		t.Fatalf("CreateTopics: %v", err)
	}
	for _, rtp := range resp.Topics {
		// 36 is TOPIC_ALREADY_EXISTS, which a rerun may still race.
		if rtp.ErrorCode != 0 && rtp.ErrorCode != 36 {
			t.Fatalf("CreateTopics %s: error code %d", rtp.Topic, rtp.ErrorCode)
		}
	}
	return topic
}

// freshPartitions creates one already-named topic with probeParts
// partitions. It is what gives the CONTRACT suite real partitions instead of
// auto-creation's one.
func freshPartitions(t *testing.T, addr, topic string) {
	t.Helper()
	admin, err := kgo.NewClient(kgo.SeedBrokers(addr))
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	defer admin.Close()

	req := kmsg.NewPtrCreateTopicsRequest()
	rt := kmsg.NewCreateTopicsRequestTopic()
	rt.Topic = topic
	rt.NumPartitions = probeParts
	rt.ReplicationFactor = 1
	req.Topics = append(req.Topics, rt)
	req.TimeoutMillis = 10000
	resp, err := req.RequestWith(context.Background(), admin)
	if err != nil {
		t.Fatalf("CreateTopics %s: %v", topic, err)
	}
	for _, rtp := range resp.Topics {
		if rtp.ErrorCode != 0 && rtp.ErrorCode != 36 {
			t.Fatalf("CreateTopics %s: error code %d", rtp.Topic, rtp.ErrorCode)
		}
	}
	awaitTopic(t, admin, topic)
}

// awaitTopic blocks until the topic is visible in metadata with all its
// partitions.
//
// CreateTopics returning is not the same as the cluster having told anyone.
// Without this the consumer subscribes before the topic exists to it, and the
// first delivery waits for a metadata refresh — about five seconds, which is
// exactly brokertest's deadline. The envelope subtest passed alone at 5.10s
// and failed in the full suite, the signature of a race rather than a defect.
func awaitTopic(t *testing.T, admin *kgo.Client, topic string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		req := kmsg.NewPtrMetadataRequest()
		rt := kmsg.NewMetadataRequestTopic()
		rt.Topic = &topic
		req.Topics = append(req.Topics, rt)
		resp, err := req.RequestWith(context.Background(), admin)
		if err == nil {
			for _, mt := range resp.Topics {
				if mt.ErrorCode == 0 && len(mt.Partitions) == probeParts {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %s never became visible with %d partitions", topic, probeParts)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var topicN int

func topicSeq() int { topicN++; return topicN }

// endOffsets returns the record count per partition.
func endOffsets(t *testing.T, addr, topic string) map[int32]int64 {
	t.Helper()
	admin, err := kgo.NewClient(kgo.SeedBrokers(addr))
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	defer admin.Close()

	req := kmsg.NewPtrListOffsetsRequest()
	rt := kmsg.NewListOffsetsRequestTopic()
	rt.Topic = topic
	for p := range int32(probeParts) {
		rp := kmsg.NewListOffsetsRequestTopicPartition()
		rp.Partition = p
		rp.Timestamp = -1 // latest
		rt.Partitions = append(rt.Partitions, rp)
	}
	req.Topics = append(req.Topics, rt)
	req.ReplicaID = -1

	resp, err := req.RequestWith(context.Background(), admin)
	if err != nil {
		t.Fatalf("ListOffsets: %v", err)
	}
	out := map[int32]int64{}
	for _, rtp := range resp.Topics {
		for _, p := range rtp.Partitions {
			if p.ErrorCode != 0 {
				t.Fatalf("ListOffsets partition %d: error code %d", p.Partition, p.ErrorCode)
			}
			out[p.Partition] = p.Offset
		}
	}
	return out
}

// keyPartitions reads the topic and reports which partitions each key used.
func keyPartitions(t *testing.T, addr, topic string) map[string]map[int32]bool {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(addr),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer cl.Close()

	out := map[string]map[int32]bool{}
	seen := 0
	want := 8 * 40
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for seen < want {
		fs := cl.PollRecords(ctx, 500)
		if fs.IsClientClosed() || ctx.Err() != nil {
			break
		}
		fs.EachRecord(func(r *kgo.Record) {
			seen++
			k := string(r.Key)
			if out[k] == nil {
				out[k] = map[int32]bool{}
			}
			out[k][r.Partition] = true
		})
	}
	if seen < want {
		t.Fatalf("read %d records, want %d", seen, want)
	}
	return out
}

// newProbeBroker boots the module and hands back its ports.
func newProbeBroker(t *testing.T, addr string) (broker.Publisher, broker.Subscriber) {
	t.Helper()
	var (
		pub broker.Publisher
		sub broker.Subscriber
	)
	km := kafka.Broker(kafka.Brokers(addr), kafka.ConsumerGroup("warren-partition-probe"))
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
	return pub, sub
}
