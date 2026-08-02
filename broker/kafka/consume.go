package kafka

import (
	"context"
	"log/slog"
	"sync"

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
// It returns when ctx is done, which is how the runner's OnStop stops it.
func (c *client) Subscribe(ctx context.Context, topic string, h broker.MessageHandler) error {
	if h == nil {
		panic("kafka: Subscribe with a nil handler for topic " + topic)
	}
	if c.cfg.group == "" {
		return errNoConsumerGroup(topic)
	}

	c.mu.Lock()
	first := len(c.handlers[topic]) == 0
	c.handlers[topic] = append(c.handlers[topic], h)
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

	<-ctx.Done()
	return nil
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

	// Only now: a partition is never revoked mid-flight.
	c.kc.AllowRebalance()
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
	handlers := append([]broker.MessageHandler(nil), c.handlers[fp.Topic]...)
	c.mu.Unlock()
	if len(handlers) == 0 {
		return
	}

	marked := 0
	for _, rec := range fp.Records {
		msg := fromRecord(rec)
		ok := true
		for _, h := range handlers {
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
