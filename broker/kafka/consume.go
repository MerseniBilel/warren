package kafka

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/MerseniBilel/warren/broker"
)

// Subscribe registers a handler for a topic.
//
// It does NOT start a consumer of its own. franz-go assigns consume topics
// client-wide rather than per subscription, so every subscription shares one
// client, one group membership and one poll loop, and this call only adds the
// topic and the handler to the demultiplexer.
//
// TWO SUBSCRIPTIONS ON THE SAME TOPIC FAN OUT IN PROCESS. Two real group
// members would SPLIT the partitions between them, not deliver to both — so
// the second handler would silently see half the traffic. Fanning out here is
// what makes "two consumers of one topic" mean what a user expects, and it is
// safe because the chain's dedupe key is scoped by subscription name: a
// sibling that already succeeded acks from its own inbox entry without
// re-running.
//
// It RETURNS ONCE THE SUBSCRIPTION IS LIVE: the handler is registered and the
// topic added to the poll set before this returns, so a publish ordered after
// it cannot miss the subscription. Cancelling ctx deregisters the handler;
// the poll loop itself is bound to the CLIENT's lifetime and is stopped by
// stopConsuming at shutdown step 3.
func (c *client) Subscribe(ctx context.Context, topic string, h broker.MessageHandler) error {
	if h == nil {
		panic("kafka: Subscribe with a nil handler for topic " + topic)
	}
	if c.cfg.group == "" {
		return errNoConsumerGroup(topic)
	}

	sub := &subscription{h: h}
	sub.live.Store(true)

	c.mu.Lock()
	first := len(c.handlers[topic]) == 0
	c.handlers[topic] = append(c.handlers[topic], sub)
	if first {
		c.topics = append(c.topics, topic)
	}
	started := c.started
	c.mu.Unlock()

	if first && started && c.kc != nil {
		c.kc.AddConsumeTopics(topic)
	}
	// One poll loop for the whole client, started by whichever subscription
	// gets here first. It is bound to the CLIENT's lifetime, not this
	// caller's context: a second subscription cancelling must not stop
	// fetching for the first, and the loop ends when the client closes.
	c.startLoop()

	// Deregister when this subscription's context ends. The loop is shared,
	// so it must keep running for the other subscriptions; what has to stop
	// is delivery to THIS handler.
	//
	// The flag is cleared BEFORE the slice edit, and dispatch reads it once
	// per record. Removing from the slice alone is not enough: dispatch
	// snapshots the subscription set once per partition and a fetched batch
	// can be thousands of records long, so a cancel landing mid-batch would
	// otherwise go unseen until the next poll. Against a real broker that
	// gap delivered 121 messages after cancel.
	go func() {
		<-ctx.Done()
		sub.live.Store(false)
		c.mu.Lock()
		hs := c.handlers[topic]
		// Identity is the subscription POINTER, not the handler's code
		// pointer: func values are not comparable, and two subscriptions
		// sharing one handler function — which fan-out makes ordinary —
		// compare equal by code pointer and deregister each other.
		for i, other := range hs {
			if other == sub {
				c.handlers[topic] = append(hs[:i:i], hs[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
	}()
	return nil
}

// subscription is one Subscribe call: a handler plus whether it is still live.
//
// The flag exists because deregistration has to take effect in the MIDDLE of
// a dispatched batch, not merely by the next poll.
type subscription struct {
	h    broker.MessageHandler
	live atomic.Bool
}

// startLoop starts the single poll loop, once.
func (c *client) startLoop() {
	c.loopOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		c.stopLoop = cancel
		c.loopDone = make(chan struct{})
		go func() {
			defer close(c.loopDone)
			for ctx.Err() == nil {
				c.poll(ctx)
			}
		}()
		// Bound by the same ctx, so it ends with the loop it belongs to.
		go c.watchIdle(ctx)
	})
}

// stopConsuming ends the poll loop and waits for the in-flight batch, then
// commits what the chain disposed of. It is shutdown step 3: consumers stop
// fetching and in-flight messages ack, BEFORE any pool closes.
func (c *client) stopConsuming(ctx context.Context) error {
	if c.stopLoop == nil {
		return nil
	}
	c.stopLoop()
	<-c.loopDone
	if c.kc != nil {
		return c.kc.CommitMarkedOffsets(ctx)
	}
	return nil
}

// poll runs one fetch-and-dispatch cycle. It is separated from the loop so a
// test can drive exactly one.
func (c *client) poll(ctx context.Context) {
	fetches := c.kc.PollRecords(ctx, c.cfg.maxPollRecords)

	// BlockRebalanceOnPoll means the client will not rebalance — and cannot
	// finish leaving the group — until this is called. Deferring it covers
	// the early return below, which used to skip it: the LAST poll of a
	// cancelled loop left the rebalance blocked, and Stop then waited
	// THIRTY SECONDS for a coordinator it had not released. Kubernetes'
	// default grace period is thirty seconds, so a rolling deploy killed
	// every consumer mid-commit.
	//
	// Deferring does not weaken the guarantee it replaces: a deferred call
	// still runs after wg.Wait() below, so a partition is no more revocable
	// mid-flight than before.
	defer c.kc.AllowRebalance()

	if fetches.IsClientClosed() || ctx.Err() != nil {
		return
	}
	fetches.EachError(func(t string, p int32, err error) {
		slog.Warn("kafka fetch error", "module", ModuleName, "topic", t, "partition", p, "error", err)
	})

	// One goroutine per partition: ordered WITHIN a partition, parallel
	// across them. Order within a partition is the guarantee Kafka gives and
	// the one a per-key aggregate depends on.
	var wg sync.WaitGroup
	fetches.EachPartition(func(fp kgo.FetchTopicPartition) {
		if len(fp.Records) == 0 {
			return
		}
		wg.Add(1)
		go func(fp kgo.FetchTopicPartition) {
			defer wg.Done()
			c.dispatch(ctx, fp)
		}(fp)
	})
	wg.Wait()
}

// dispatch runs one partition's records through the chain and decides what to
// mark.
//
// MARKING IS PREFIX-ONLY. A Kafka offset is a high-water mark, so marking
// record k+1 would commit past a failed record k and lose it. The longest
// contiguous run of successes is marked; the first failure seeks the
// partition back to its own offset and discards the rest of the batch, so it
// is redelivered. Head-of-line blocking on that partition is not a bug — it
// is what per-key ordering costs, and Retry has already exhausted its policy
// before the driver ever sees the error.
func (c *client) dispatch(ctx context.Context, fp kgo.FetchTopicPartition) {
	c.mu.Lock()
	subs := append([]*subscription(nil), c.handlers[fp.Topic]...)
	c.mu.Unlock()
	if len(subs) == 0 {
		return
	}

	marked := 0
	for _, rec := range fp.Records {
		msg := fromRecord(rec)
		ok := true
		for _, s := range subs {
			// Cancelled between records: stop delivering to THIS handler
			// immediately, and do not let its absence fail the record for
			// the others. A cancelled subscription is not a failed one.
			if !s.live.Load() {
				continue
			}
			h := s.h
			// Every handler must dispose of the record before its offset can
			// advance: with fan-out the offset is shared, so one failure
			// redelivers to all of them. The chain's per-subscription dedupe
			// is what keeps that from re-running the ones that succeeded.
			if err := h(ctx, msg); err != nil {
				ok = false
				break
			}
		}
		if !ok {
			break
		}
		marked++
	}

	if marked > 0 {
		c.kc.MarkCommitRecords(fp.Records[:marked]...)
	}
	if marked < len(fp.Records) {
		failed := fp.Records[marked]
		c.kc.SetOffsets(map[string]map[int32]kgo.EpochOffset{
			fp.Topic: {fp.Partition: {Epoch: failed.LeaderEpoch, Offset: failed.Offset}},
		})
	}
}

// idleWarnAfter is how long a member may hold no partitions before the
// warning fires. A cooperative rebalance settles in seconds; this is long
// enough that a routine one never trips it, and short enough that a
// misconfigured deployment is told before anyone goes looking. It is a
// variable so a test need not wait.
var idleWarnAfter = 30 * time.Second

// onAssigned and onRevoked maintain the set of partitions this member holds.
//
// franz-go calls them serially with each other, so the map needs no ordering
// beyond its own mutex.
func (c *client) onAssigned(_ context.Context, _ *kgo.Client, m map[string][]int32) {
	c.heldMu.Lock()
	if c.held == nil {
		c.held = map[string][]int32{}
	}
	for t, ps := range m {
		c.held[t] = append(c.held[t], ps...)
	}
	c.heldMu.Unlock()
}

func (c *client) onRevoked(_ context.Context, _ *kgo.Client, m map[string][]int32) {
	c.heldMu.Lock()
	for t, ps := range m {
		kept := c.held[t][:0]
		for _, have := range c.held[t] {
			if !slices.Contains(ps, have) {
				kept = append(kept, have)
			}
		}
		if len(kept) == 0 {
			delete(c.held, t)
		} else {
			c.held[t] = kept
		}
	}
	c.heldMu.Unlock()
}

// watchIdle warns when this member is subscribed but holds no partitions.
//
// IT IS A TICKER, NOT A CALLBACK, and that is the whole design. franz-go
// calls OnPartitionsAssigned with what was ADDED, so a member assigned
// NOTHING is never called at all — the exact case this warns about would
// never fire. An earlier version hung the check off the callbacks and its
// test passed anyway, by catching a member whose partition was briefly
// revoked mid-rebalance; the real two-replica app stayed silent.
//
// It cannot live in the poll loop either: PollRecords blocks until a record
// arrives, and a member holding nothing never gets one.
//
// The streak has to EXCEED idleWarnAfter, so a cooperative rebalance — where
// a member does briefly hold nothing — passes through without a word.
func (c *client) watchIdle(ctx context.Context) {
	tick := time.NewTicker(idleWarnAfter / 3)
	defer tick.Stop()
	var emptySince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		c.mu.Lock()
		topics := append([]string(nil), c.topics...)
		c.mu.Unlock()
		if len(topics) == 0 {
			continue
		}

		c.heldMu.Lock()
		total := 0
		for _, ps := range c.held {
			total += len(ps)
		}
		if total > 0 {
			c.idleWarned = false
			c.heldMu.Unlock()
			emptySince = time.Time{}
			continue
		}
		warned := c.idleWarned
		c.heldMu.Unlock()

		if emptySince.IsZero() {
			emptySince = time.Now()
			continue
		}
		if warned || time.Since(emptySince) < idleWarnAfter {
			continue
		}
		c.heldMu.Lock()
		c.idleWarned = true
		c.heldMu.Unlock()

		slog.Warn("kafka consumer holds no partitions and is processing nothing",
			"module", ModuleName,
			"group", c.cfg.group,
			"topics", topics,
			"cause", "a consumer group divides each topic's partitions between its members, so members beyond the partition count are assigned nothing and idle — an AUTO-CREATED topic gets the broker's default, which is usually ONE partition, so the second replica of a service that auto-created its topics always idles",
			"fix", "give the topic at least as many partitions as the replicas you run: kafka-topics.sh --alter --topic <topic> --partitions <n>. Partitions can be added and never removed, and adding them changes which partition a key lands on — so per-key ordering holds only from that point on",
			"safe_if", "this replica is deliberate standby capacity")
	}
}
