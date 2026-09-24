package integration

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRebalanceDoesNotCommitUnprocessedBatch is the regression test for the
// batch-mode message loss this offset work exists to fix.
//
// The bug: enable.auto.offset.store defaulted to true, so librdkafka recorded the
// offset of every message the moment Poll handed it over — including messages
// sitting in the batch buffer that no handler had seen. The commit in the revoke
// callback then published those offsets, telling the group that work nobody had
// done was finished. The new owner started after them and they were never
// processed by anyone.
//
// The test pins the group in exactly that state — a full buffer, no dispatch, a
// rebalance — and asserts the committed offset has not moved. It asserts on the
// committed offset rather than on what handlers received because the committed
// offset is what the next owner resumes from, which is where the loss shows up.
func TestRebalanceDoesNotCommitUnprocessedBatch(t *testing.T) {
	t.Log("TestRebalanceDoesNotCommitUnprocessedBatch started")
	defer t.Log("TestRebalanceDoesNotCommitUnprocessedBatch finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "unprocessed-batch")
	group := fmt.Sprintf("unprocessed-batch-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	const messageCount = 10
	payloads := make([]string, messageCount)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("msg-%02d", i)
	}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	// Both consumers use a batch far larger than the message count and a timeout
	// long enough never to fire. They therefore poll every message into their
	// buffers and dispatch none of them, which is the state the bug needs.
	var dispatched atomic.Int32
	newBufferingConsumer := func() easykafka.Consumer {
		c, err := easykafka.New(
			easykafka.WithTopic(topic),
			easykafka.WithBrokers(cluster.Brokers...),
			easykafka.WithConsumerGroup(group),
			easykafka.WithBatchHandler(func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
				dispatched.Add(int32(batch.Len()))
				return nil
			}),
			easykafka.WithBatchSize(1000),
			easykafka.WithBatchTimeout(10*time.Minute),
			easykafka.WithPollTimeout(100*time.Millisecond),
		)
		require.NoError(t, err)
		return c
	}

	consumerA := newBufferingConsumer()
	ctxA, cancelA := context.WithCancel(ctx)
	doneA := make(chan error, 1)
	go func() { doneA <- consumerA.Start(ctxA) }()

	// Let A join the group, take the partition, and buffer every message.
	time.Sleep(10 * time.Second)
	require.Zero(t, dispatched.Load(), "A must still be buffering, not dispatching")

	// A second consumer joining the same group revokes A's partition. This is the
	// moment the bug describes.
	consumerB := newBufferingConsumer()
	ctxB, cancelB := context.WithCancel(ctx)
	doneB := make(chan error, 1)
	go func() { doneB <- consumerB.Start(ctxB) }()

	// Let the rebalance complete.
	time.Sleep(10 * time.Second)

	committed := cluster.CommittedOffset(ctx, t, group, topic, 0)
	t.Logf("committed offset after rebalance: %v (dispatched=%d)", committed, dispatched.Load())

	// Nothing may have been dispatched, or the premise is gone and the assertion
	// below would be meaningless.
	require.Zero(t, dispatched.Load(), "no batch may have been dispatched during the test")

	// The assertion. No handler has seen a message, so the group must not claim
	// any message is done. Before the fix this reported 10.
	assert.True(t, committed == kfk.OffsetInvalid || int64(committed) == 0,
		"rebalance committed offset %v for %d messages no handler has seen — they would never be redelivered",
		committed, messageCount)

	cancelA()
	cancelB()
	require.NoError(t, <-doneA)
	require.NoError(t, <-doneB)
}

// TestRedeliveryResumesAtExactOffset guards the +1 in Adapter.StoreOffset.
//
// Kafka's committed offset is a resume position — the next message to read — not
// a high-water mark of what has been handled, so a processed offset is stored as
// offset+1. Getting that wrong is silent in both directions and asymmetric: one
// too low redelivers a message that was already handled, which is a duplicate and
// harmless under at-least-once; one too high marks a message done that nobody
// processed, which is permanent loss.
//
// Asserting only "nothing was lost" would pass on a too-low store, and asserting
// only "no duplicates" would pass on a too-high one. So this asserts the exact
// boundary: the second consumer's first message must be the one after the last
// message the first consumer handled, with nothing skipped and nothing repeated.
func TestRedeliveryResumesAtExactOffset(t *testing.T) {
	t.Log("TestRedeliveryResumesAtExactOffset started")
	defer t.Log("TestRedeliveryResumesAtExactOffset finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "exact-boundary")
	group := fmt.Sprintf("exact-boundary-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	const (
		messageCount = 10
		stopAfter    = 5
	)
	payloads := make([]string, messageCount)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("msg-%02d", i)
	}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	// First consumer: handle exactly stopAfter messages, then stop.
	//
	// Start blocks, so the poll loop runs on this goroutine and the handler is
	// called inline from it. firstRun and secondRun below are therefore only ever
	// touched by one goroutine and need no synchronisation.
	var firstRun []string

	ctx1, cancel1 := context.WithTimeout(ctx, 60*time.Second)
	defer cancel1()

	consumer1, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithHandler(func(_ context.Context, payload []byte) *easykafka.Failure {
			firstRun = append(firstRun, string(payload))
			handled := len(firstRun)

			// Cancelling from inside the handler is deliberate. The engine stores
			// and commits this message's offset after the handler returns, and
			// only then sees the cancelled context, so the committed position
			// lands on exactly stopAfter messages — no race over the boundary.
			if handled == stopAfter {
				cancel1()
			}
			return nil
		}),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)
	require.NoError(t, consumer1.Start(ctx1))

	require.Equal(t, payloads[:stopAfter], firstRun,
		"first consumer should have handled exactly the first %d messages", stopAfter)

	// Second consumer, same group: it must pick up precisely where the first left
	// off, reading from the committed offset.
	var secondRun []string

	ctx2, cancel2 := context.WithTimeout(ctx, 60*time.Second)
	defer cancel2()

	consumer2, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithHandler(func(_ context.Context, payload []byte) *easykafka.Failure {
			secondRun = append(secondRun, string(payload))
			handled := len(secondRun)

			if handled == messageCount-stopAfter {
				cancel2()
			}
			return nil
		}),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)
	require.NoError(t, consumer2.Start(ctx2))

	// The exact boundary: no message skipped (loss) and none repeated (a store
	// that was one too low).
	assert.Equal(t, payloads[stopAfter:], secondRun,
		"second consumer must resume at exactly the first unhandled message")
}
