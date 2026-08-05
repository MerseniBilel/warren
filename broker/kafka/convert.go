package kafka

import (
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/MerseniBilel/warren/broker"
)

// headerKeyID carries broker.Message.ID, which Kafka has no native slot for.
// The inbox dedupes on it, so it must survive the round trip exactly.
const (
	headerKeyID   = "warren-message-id"
	headerKeyType = "warren-message-type"
)

// toRecord converts the driver-neutral envelope into a Kafka record.
//
// ID and Type get their own headers because Kafka models neither: a record
// has a key, a value, a timestamp and headers, and nothing else. Losing ID
// would disable inbox deduplication silently, which is why it round-trips
// under a reserved name rather than being folded into the payload.
func toRecord(topic string, m broker.Message) *kgo.Record {
	r := &kgo.Record{
		Topic: topic,
		Value: m.Payload,
	}
	// NIL, not empty, when there is no key — the two are a different
	// instruction to the partitioner. franz-go hashes "records with non-nil
	// keys" and distributes the rest, and []byte("") is NOT nil in Go, so
	// `Key: []byte(m.Key)` handed it a zero-length key to hash. murmur2 of
	// nothing is a constant, so EVERY keyless message in a service went to
	// ONE partition: measured 60 of 60 on partition 3 of 6.
	//
	// A one-partition topic hides it perfectly, which is why it survived a
	// contract suite. On a real topic it caps consumer parallelism at one
	// member however many partitions you provision, and makes one broker
	// carry the whole write load.
	if m.Key != "" {
		r.Key = []byte(m.Key)
	}
	if !m.OccurredAt.IsZero() {
		r.Timestamp = m.OccurredAt
	}
	r.Headers = make([]kgo.RecordHeader, 0, len(m.Headers)+2)
	if m.ID != "" {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: headerKeyID, Value: []byte(m.ID)})
	}
	if m.Type != "" {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: headerKeyType, Value: []byte(m.Type)})
	}
	for k, v := range m.Headers {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	return r
}

// fromRecord converts a Kafka record back.
//
// A duplicate header key keeps the FIRST occurrence: Kafka permits repeats
// and a map cannot hold them, and first-wins is at least deterministic —
// last-wins would make the value depend on producer iteration order.
func fromRecord(r *kgo.Record) broker.Message {
	m := broker.Message{
		Key:        string(r.Key),
		Payload:    r.Value,
		OccurredAt: r.Timestamp,
	}
	for _, h := range r.Headers {
		switch h.Key {
		case headerKeyID:
			m.ID = string(h.Value)
		case headerKeyType:
			m.Type = string(h.Value)
		default:
			if m.Headers == nil {
				m.Headers = make(map[string]string, len(r.Headers))
			}
			if _, seen := m.Headers[h.Key]; !seen {
				m.Headers[h.Key] = string(h.Value)
			}
		}
	}
	return m
}

var _ = time.Time{}
