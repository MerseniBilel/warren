package kafka

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"

	"github.com/MerseniBilel/warren/broker"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
)

var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("does not match golden file %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// --- the error table the outbox depends on --------------------------------

// outbox.Relay switches on the CODE this returns to decide whether a record
// waits for the next drain or is parked forever. Classify a broker outage as
// INVALID and one outage parks the entire outbox; classify a
// too-large record as UNAVAILABLE and the relay retries it until the end of
// time. This table is the whole reason Publish maps errors at all.
func TestPublishErrorsCarryTheCodeTheOutboxSwitchesOn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want werrors.Code
	}{
		// Retryable: the cluster changes on its own, so the record waits.
		{"not leader", kerr.NotLeaderForPartition, werrors.CodeUnavailable},
		{"not enough replicas", kerr.NotEnoughReplicas, werrors.CodeUnavailable},
		{"request timed out", kerr.RequestTimedOut, werrors.CodeUnavailable},
		{"coordinator not available", kerr.CoordinatorNotAvailable, werrors.CodeUnavailable},
		{"leader not available", kerr.LeaderNotAvailable, werrors.CodeUnavailable},
		// Permanent: retrying cannot help, so the record is parked.
		{"message too large", kerr.MessageTooLarge, werrors.CodeInvalid},
		{"record list too large", kerr.RecordListTooLarge, werrors.CodeInvalid},
		{"invalid record", kerr.InvalidRecord, werrors.CodeInvalid},
		{"unknown topic", kerr.UnknownTopicOrPartition, werrors.CodeInvalid},
		{"topic authorization failed", kerr.TopicAuthorizationFailed, werrors.CodeInvalid},
		// Transport-level, not broker-reported: retryable.
		{"deadline exceeded", context.DeadlineExceeded, werrors.CodeUnavailable},
		{"context cancelled", context.Canceled, werrors.CodeUnavailable},
		{"dial failure", errNotStarted(), werrors.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			if !werrors.Is(got, tc.want) {
				t.Errorf("classify(%v) = %v, want %s", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyPassesNilThrough(t *testing.T) {
	t.Parallel()
	if got := classify(nil); got != nil {
		t.Errorf("classify(nil) = %v", got)
	}
}

// --- the envelope round trip ----------------------------------------------

// broker.Message.ID is what the inbox dedupes on, and Kafka has no native
// slot for it. Losing it in translation would disable deduplication silently.
func TestMessageRoundTripsThroughARecord(t *testing.T) {
	t.Parallel()

	want := broker.Message{
		ID:         "evt-1",
		Type:       "user.registered",
		Key:        "u-42",
		Payload:    []byte(`{"email":"bob@example.com"}`),
		Headers:    map[string]string{"traceparent": "00-abc-def-01", "tenant": "acme"},
		OccurredAt: time.Unix(1700000000, 0).UTC(),
	}

	got := fromRecord(toRecord("user.events", want))

	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q — the inbox dedupes on this", got.ID, want.ID)
	}
	if got.Type != want.Type || got.Key != want.Key {
		t.Errorf("Type/Key = %q/%q", got.Type, got.Key)
	}
	if string(got.Payload) != string(want.Payload) {
		t.Errorf("payload = %s", got.Payload)
	}
	if !got.OccurredAt.Equal(want.OccurredAt) {
		t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, want.OccurredAt)
	}
	for k, v := range want.Headers {
		if got.Headers[k] != v {
			t.Errorf("header %q = %q, want %q", k, got.Headers[k], v)
		}
	}
	// The reserved keys must NOT reappear as ordinary headers, or a republish
	// would duplicate them and they would accumulate.
	for _, reserved := range []string{headerKeyID, headerKeyType} {
		if _, leaked := got.Headers[reserved]; leaked {
			t.Errorf("reserved header %q leaked into Headers", reserved)
		}
	}
}

func TestEmptyMessageRoundTrips(t *testing.T) {
	t.Parallel()

	got := fromRecord(toRecord("t", broker.Message{}))
	if got.ID != "" || got.Type != "" || got.Key != "" {
		t.Errorf("empty message gained fields: %+v", got)
	}
	if len(got.Headers) != 0 {
		t.Errorf("empty message gained headers: %v", got.Headers)
	}
}

// Kafka permits duplicate header keys and a map cannot hold them. First-wins
// is at least deterministic; last-wins would make the value depend on
// producer iteration order.
func TestDuplicateHeaderKeysKeepTheFirst(t *testing.T) {
	t.Parallel()

	r := &kgo.Record{Headers: []kgo.RecordHeader{
		{Key: "x", Value: []byte("first")},
		{Key: "x", Value: []byte("second")},
	}}
	if got := fromRecord(r).Headers["x"]; got != "first" {
		t.Errorf("header x = %q, want the first occurrence", got)
	}
}

// --- wiring, with no cluster ----------------------------------------------

func TestMissingBrokersFailsAtWiring(t *testing.T) {
	t.Parallel()

	if _, err := newClient(defaults(), nil, nil); err == nil {
		t.Fatal("a module with no brokers must fail")
	} else if !strings.Contains(err.Error(), "kafka.Brokers") {
		t.Errorf("diagnostic must name the fix:\n%s", err)
	}
}

// PLAIN sends the password in the clear. This is refused rather than left to
// whoever reads the config file later.
func TestPlainWithoutTLSIsRefused(t *testing.T) {
	t.Parallel()

	cfg := defaults()
	cfg.brokers = []string{"localhost:9092"}
	cfg.mechanism = Plain("user", "hunter2")

	_, err := newClient(cfg, nil, nil)
	if err == nil {
		t.Fatal("SASL/PLAIN without TLS was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the diagnostic leaked the password:\n%s", err)
	}
	if !strings.Contains(err.Error(), "SCRAM512") {
		t.Errorf("diagnostic should offer the alternative:\n%s", err)
	}
}

func TestPlainWithTLSIsAccepted(t *testing.T) {
	t.Parallel()

	cfg := defaults()
	cfg.brokers = []string{"localhost:9092"}
	cfg.mechanism = Plain("user", "hunter2")
	cfg.tls = &tls.Config{MinVersion: tls.VersionTLS12}

	if _, err := newClient(cfg, nopLifecycle{}, nopRegistry{}); err != nil {
		t.Errorf("PLAIN over TLS was refused: %v", err)
	}
}

func TestTwoMechanismsIsRefused(t *testing.T) {
	t.Parallel()

	cfg := defaults()
	cfg.brokers = []string{"localhost:9092"}
	cfg.mechanism = SCRAM512("u", "p")
	cfg.rawSASL = fakeMechanism{}

	if _, err := newClient(cfg, nil, nil); err == nil {
		t.Fatal("two SASL mechanisms were accepted; which one wins is not argument order's to decide")
	}
}

func TestUnknownBalancerIsRefused(t *testing.T) {
	t.Parallel()

	cfg := defaults()
	cfg.brokers = []string{"localhost:9092"}
	cfg.balancer = "nonesuch"

	_, err := newClient(cfg, nil, nil)
	if err == nil {
		t.Fatal("an unknown balancer was accepted")
	}
	if !strings.Contains(err.Error(), "cooperative") {
		t.Errorf("diagnostic must list what is available:\n%s", err)
	}
}

func TestEveryBalancerResolves(t *testing.T) {
	t.Parallel()

	for _, b := range []Balancer{BalancerCooperative, BalancerSticky, BalancerRoundRobin, BalancerRange} {
		if !b.valid() {
			t.Errorf("%s is not valid", b)
		}
		if b.resolve() == nil {
			t.Errorf("%s resolved to nil", b)
		}
	}
}

// A publish before OnStart is a constructor reaching the network, which is
// the mistake the parse/acquire split exists to prevent.
func TestPublishBeforeStartIsUnavailable(t *testing.T) {
	t.Parallel()

	c := &client{cfg: defaults()}
	err := c.Publish(context.Background(), "t", broker.Message{ID: "1"})
	if !werrors.Is(err, werrors.CodeUnavailable) {
		t.Errorf("err = %v, want UNAVAILABLE", err)
	}
}

func TestPublishOfNothingIsNotAnError(t *testing.T) {
	t.Parallel()

	c := &client{cfg: defaults()}
	if err := c.Publish(context.Background(), "t"); err != nil {
		t.Errorf("publishing zero messages: %v", err)
	}
}

func TestSubscribeWithoutAGroupIsRefused(t *testing.T) {
	t.Parallel()

	c := &client{cfg: defaults(), handlers: map[string][]*subscription{}}
	err := c.Subscribe(context.Background(), "t", func(context.Context, broker.Message) error { return nil })
	if err == nil {
		t.Fatal("subscribing with no consumer group was accepted")
	}
	if !strings.Contains(err.Error(), "ConsumerGroup") {
		t.Errorf("diagnostic must name the fix:\n%s", err)
	}
}

// --- diagnostics ----------------------------------------------------------

func TestDiagnosticsAreGolden(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	write := func(label string, err error) {
		b.WriteString("── " + label + "\n")
		b.WriteString(err.Error())
		b.WriteString("\n\n")
	}
	write("no brokers", errNoBrokers())
	write("no consumer group", errNoConsumerGroup("user.registered"))
	write("unknown balancer", errUnknownBalancer("nonesuch"))
	write("plain without tls", errPlainWithoutTLS())
	write("two mechanisms", errTwoMechanisms())
	write("cannot connect", errCannotConnect([]string{"a:9092", "b:9092"}, context.DeadlineExceeded))
	write("raw failed", errRawFailed(context.DeadlineExceeded))
	write("not started", errNotStarted())

	assertGolden(t, "diagnostics", strings.TrimRight(b.String(), "\n")+"\n")
}

// --- fakes ----------------------------------------------------------------

// nopRegistry and nopLifecycle let the wiring tests run with no cluster and
// no boot: the constructor's job is to PARSE, and parsing needs neither.
type nopRegistry struct{}

func (nopRegistry) Register(health.Check, ...health.RegisterOption) error { return nil }
func (nopRegistry) Live(context.Context) health.Report                    { return health.Report{} }
func (nopRegistry) Ready(context.Context) health.Report                   { return health.Report{} }

type nopLifecycle struct{}

func (nopLifecycle) Append(lifecycle.Hook)       {}
func (nopLifecycle) Start(context.Context) error { return nil }
func (nopLifecycle) Stop(context.Context) error  { return nil }
func (nopLifecycle) Ready() bool                 { return true }

type fakeMechanism struct{}

func (fakeMechanism) Name() string { return "FAKE" }
func (fakeMechanism) Authenticate(context.Context, string) (sasl.Session, []byte, error) {
	return nil, nil, nil
}

// TestUnknownTopicIsExplained pins the diagnostic, not just the code.
//
// The code was always right — a missing topic is permanent, so INVALID parks
// the outbox record instead of retrying it forever. What a developer saw was
// the wire protocol's sentence about hosting a topic-partition, which does not
// tell them that provisioning or AutoCreateTopics is the fix.
func TestUnknownTopicIsExplained(t *testing.T) {
	err := classifyProduce(kerr.UnknownTopicOrPartition, "orders")
	if code := werrors.CodeOf(err); code != werrors.CodeInvalid {
		t.Errorf("code = %v, want INVALID", code)
	}
	for _, want := range []string{`"orders"`, "AutoCreateTopics", "--partitions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic does not mention %s:\n%s", want, err)
		}
	}
	// A different broker error must not be rewritten into this one.
	other := classifyProduce(kerr.NotEnoughReplicas, "orders")
	if strings.Contains(other.Error(), "AutoCreateTopics") {
		t.Errorf("unrelated error got the unknown-topic diagnostic:\n%s", other)
	}
}
