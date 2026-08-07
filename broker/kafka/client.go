package kafka

import (
	"context"
	stderrors "errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
)

// client is the one franz-go client this module owns: producer and consumer
// both. One client means one group membership, one heartbeat, one fetch
// session and one connection set — a client per subscription would put
// several members of the same group in one process, splitting partitions
// against itself.
type client struct {
	cfg  config
	opts []kgo.Opt
	kc   *kgo.Client

	mu       sync.Mutex
	handlers map[string][]*subscription // topic → subscriptions, fan-out in process
	topics   []string
	started  bool

	loopOnce sync.Once
	stopLoop context.CancelFunc
	loopDone chan struct{}

	// held is the partitions this member currently owns, maintained by the
	// group callbacks. It exists to answer one question — "am I consuming
	// anything at all?" — which no other signal answers: an empty fetch
	// means no DATA, not no ASSIGNMENT.
	heldMu     sync.Mutex
	held       map[string][]int32
	idleWarned bool
}

var (
	_ broker.Publisher  = (*client)(nil)
	_ broker.Subscriber = (*client)(nil)
)

// newClient PARSES. Every configuration mistake fails here, at wiring, before
// any hook runs and with nothing to roll back — building the franz-go options
// does no I/O.
func newClient(cfg config, lc lifecycle.Lifecycle, reg health.Registry) (*client, error) {
	if len(cfg.brokers) == 0 {
		return nil, errNoBrokers()
	}
	if !cfg.balancer.valid() {
		return nil, errUnknownBalancer(cfg.balancer)
	}
	if cfg.mechanism.kind == mechPlain && cfg.tls == nil {
		return nil, errPlainWithoutTLS()
	}
	if cfg.mechanism.kind != mechNone && cfg.rawSASL != nil {
		return nil, errTwoMechanisms()
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.brokers...),
		kgo.DialTimeout(cfg.connectTimeout),
		kgo.ProduceRequestTimeout(cfg.produceTimeout),
		kgo.FetchMaxBytes(cfg.fetchMaxBytes),
		// The idempotent producer is franz-go's default and stays on: it
		// removes duplicates caused by producer RETRIES within a session,
		// which is the one duplicate source inside this driver's control.
		// It is not exactly-once and nothing here claims to be.
		kgo.RecordRetries(10),
		kgo.WithLogger(kgoLogger{}),
	}
	if cfg.autoCreate {
		opts = append(opts, kgo.AllowAutoTopicCreation())
	}
	if cfg.clientID != "" {
		opts = append(opts, kgo.ClientID(cfg.clientID))
	}
	if cfg.tls != nil {
		opts = append(opts, kgo.DialTLSConfig(cfg.tls))
	}
	if m := cfg.mechanism.resolve(); m != nil {
		opts = append(opts, kgo.SASL(m))
	}
	if cfg.rawSASL != nil {
		opts = append(opts, kgo.SASL(cfg.rawSASL))
	}
	if cfg.group != "" {
		opts = append(opts,
			kgo.ConsumerGroup(cfg.group),
			kgo.Balancers(cfg.balancer.resolve()),
			kgo.SessionTimeout(cfg.sessionTimeout),
			// The offset-commit strategy is a FIXED INVARIANT, not an
			// option. Auto-commit by time would advance offsets past
			// messages the consumer chain has not disposed of, silently
			// turning at-least-once into at-most-once. Marks are set only
			// after the chain returns.
			kgo.AutoCommitMarks(),
			kgo.AutoCommitInterval(cfg.commitInterval),
			// A partition is never revoked mid-flight: the loop calls
			// AllowRebalance only after it has disposed of the batch.
			kgo.BlockRebalanceOnPoll(),
		)
	}
	// Configure's options are applied LAST and win, which is the point and
	// also the hazard — overriding AutoCommitMarks or BlockRebalanceOnPoll
	// breaks the guarantees above. Said so in Configure's doc comment.
	opts = append(opts, cfg.configure...)

	c := &client{cfg: cfg, opts: opts, handlers: map[string][]*subscription{}}
	if cfg.group != "" {
		// Appended after the client exists, because the callbacks are its
		// methods. They only record assignment and schedule a check —
		// franz-go forbids Close or LeaveGroup from inside them, and neither
		// happens here.
		c.opts = append(c.opts,
			kgo.OnPartitionsAssigned(c.onAssigned),
			kgo.OnPartitionsRevoked(c.onRevoked),
			kgo.OnPartitionsLost(c.onRevoked),
		)
	}

	if err := reg.Register(health.NewCheck("kafka", c.probe), health.Timeout(cfg.healthTimeout)); err != nil {
		return nil, err
	}
	// Appended FIRST, so reverse-order teardown closes it LAST — shutdown
	// step 5. The subscription runner depends on this client, so its hook is
	// appended after and stops before, which is step 3. The client must stay
	// open in between: the outbox relay's step-4 flush publishes through it.
	lc.Append(lifecycle.Hook{Name: ModuleName, OnStart: c.open, OnStop: c.close})
	return c, nil
}

// open ACQUIRES: dial the seeds and fetch metadata, so an unreachable cluster
// rolls the boot back through the lifecycle rather than failing on request 1.
func (c *client) open(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.connectTimeout)
	defer cancel()

	kc, err := kgo.NewClient(c.opts...)
	if err != nil {
		return errCannotConnect(c.cfg.brokers, err)
	}
	// Ping proves the seeds are reachable AND that authentication works,
	// which a lazy client would not discover until the first produce.
	if err := kc.Ping(ctx); err != nil {
		kc.Close()
		return errCannotConnect(c.cfg.brokers, err)
	}
	c.kc = kc

	for _, fn := range c.cfg.raw {
		if err := fn(ctx, kc); err != nil {
			kc.Close()
			c.kc = nil
			return errRawFailed(err)
		}
	}
	c.mu.Lock()
	c.started = true
	topics := append([]string(nil), c.topics...)
	c.mu.Unlock()
	if len(topics) > 0 {
		kc.AddConsumeTopics(topics...)
	}
	slog.Info("kafka connected",
		"module", ModuleName, "brokers", strings.Join(c.cfg.brokers, ","),
		"group", c.cfg.group, "balancer", string(c.cfg.balancer))
	return nil
}

// close is shutdown step 5. Consuming has already stopped at step 3 — see
// stopConsuming — and the outbox relay's step-4 flush published through this
// client in between, which is why it is still open here.
func (c *client) close(ctx context.Context) error {
	if err := c.stopConsuming(ctx); err != nil {
		slog.Warn("kafka: committing marked offsets", "module", ModuleName, "error", err)
	}
	if c.kc != nil {
		c.kc.Close()
	}
	return nil
}

func (c *client) probe(ctx context.Context) error {
	if c.kc == nil {
		return errors.Unavailable("kafka", errNotStarted())
	}
	if err := c.kc.Ping(ctx); err != nil {
		return errors.Unavailable("kafka", err)
	}
	return nil
}

// Publish produces the messages and waits for the brokers to acknowledge
// them.
//
// The error it returns carries a warren/errors CODE, and that is
// load-bearing rather than tidy: outbox.Relay switches on it to decide
// whether a record is left for the next drain or parked forever. Classify a
// broker outage as INVALID and a single outage parks the entire outbox.
func (c *client) Publish(ctx context.Context, topic string, msgs ...broker.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if c.kc == nil {
		return errors.Unavailable("kafka", errNotStarted())
	}
	// A publisher adapter's first act: what makes a span survive into the
	// consumer, and the other half of the chain's TraceExtract stage. Before
	// the timeout context, so the headers describe the caller's span rather
	// than this function's derived one.
	broker.InjectTrace(ctx, msgs)

	ctx, cancel := context.WithTimeout(ctx, c.cfg.produceTimeout)
	defer cancel()

	records := make([]*kgo.Record, 0, len(msgs))
	for _, m := range msgs {
		records = append(records, toRecord(topic, m))
	}
	if err := c.kc.ProduceSync(ctx, records...).FirstErr(); err != nil {
		return classifyProduce(err, topic)
	}
	return nil
}

// classifyProduce is classify with the topic in hand, so the one failure a
// developer meets on their first publish — the topic does not exist — can say
// so in Warren's voice instead of the wire protocol's.
func classifyProduce(err error, topic string) error {
	var kerrErr *kerr.Error
	if stderrors.As(err, &kerrErr) && kerrErr.Code == kerr.UnknownTopicOrPartition.Code {
		return errors.Invalid("record", errUnknownTopic(topic))
	}
	return classify(err)
}

// classify maps a produce failure onto the seven-code vocabulary.
//
// The split that matters: RETRYABLE failures — no leader, not enough
// replicas, a dial that did not connect — are UNAVAILABLE, which leaves the
// outbox record for the next drain. PERMANENT ones — a record too large for
// the broker's limit, a topic that does not exist and will not be
// auto-created — are INVALID, which parks the record for a human. Anything
// unrecognised is INTERNAL, the safe default for the unknown.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled) {
		return errors.Unavailable("kafka", err)
	}
	var kerrErr *kerr.Error
	if !stderrors.As(err, &kerrErr) {
		// Not a broker-reported error: a dial failure, a closed client, a TLS
		// handshake. Transport-level, therefore retryable.
		return errors.Unavailable("kafka", err)
	}
	switch kerrErr.Code {
	case kerr.RecordListTooLarge.Code,
		kerr.MessageTooLarge.Code,
		kerr.InvalidRecord.Code,
		kerr.InvalidRequiredAcks.Code,
		kerr.UnknownTopicOrPartition.Code,
		kerr.TopicAuthorizationFailed.Code:
		return errors.Invalid("record", err)
	default:
		// Everything else — NotLeaderForPartition, NotEnoughReplicas,
		// RequestTimedOut, CoordinatorNotAvailable — is a cluster condition
		// that changes on its own.
		return errors.Unavailable("kafka", err)
	}
}

// kgoLogger routes franz-go's internal logging into this service's own, so a
// broker problem appears in the same stream as everything else rather than on
// stderr in a different shape.
type kgoLogger struct{}

func (kgoLogger) Level() kgo.LogLevel { return kgo.LogLevelWarn }

func (kgoLogger) Log(_ kgo.LogLevel, msg string, keyvals ...any) {
	slog.Warn(msg, append([]any{"module", ModuleName}, keyvals...)...)
}
