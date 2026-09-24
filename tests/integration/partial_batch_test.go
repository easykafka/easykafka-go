package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPartialBatchRetriesOnlyTheFailedMessage is the regression the gap
// describes: one poison message in a batch of fifty used to send all fifty to
// the retry topic under its error. Now only the poison message goes, on its
// first attempt; the other forty-nine are committed and never republished.
func TestPartialBatchRetriesOnlyTheFailedMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	source := helpers.UniqueTopicName(t, "partial-source")
	retry := helpers.UniqueTopicName(t, "partial-retry")
	dlq := helpers.UniqueTopicName(t, "partial-dlq")
	for _, topic := range []string{source, retry, dlq} {
		cluster.CreateTopic(ctx, t, topic, 1)
	}

	const total = 50
	payloads := make([]string, total)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("msg-%02d", i)
	}
	payloads[23] = "poison"
	cluster.ProduceMessages(ctx, t, source, payloads)

	var seen atomic.Int32
	consumer, err := easykafka.New(
		easykafka.WithTopic(source),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("partial-group-%d", time.Now().UnixNano())),
		easykafka.WithBatchHandler(func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
			for _, item := range batch.Items() {
				if string(item.Message().Payload) == "poison" {
					item.Fail(easykafka.Failure{Err: errors.New("cannot process poison")})
				}
			}
			seen.Add(int32(batch.Len()))
			return nil
		}),
		easykafka.WithBatchSize(total),
		easykafka.WithBatchTimeout(10*time.Second),
		easykafka.WithErrorStrategy(helpers.NewRetryStrategy(t, retry, dlq, 3)),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	stop := helpers.RunUntil(ctx, t, consumer)
	// >=, not ==: if a library bug dispatched the batch twice, the count would
	// jump past total and == would only time out. Stopping here lets the
	// retry-topic assertions below show the duplicates instead.
	require.Eventually(t, func() bool { return seen.Load() >= total },
		60*time.Second, 100*time.Millisecond, "the batch was never handled")
	require.NoError(t, stop())

	// Ask for every message the old behaviour would have written, and accept
	// only the one the new behaviour does.
	retried := cluster.ConsumeMessages(ctx, t, retry,
		fmt.Sprintf("verify-partial-%d", time.Now().UnixNano()), total, 10*time.Second)

	require.Len(t, retried, 1, "only the poison message may reach the retry topic")
	assert.Equal(t, "poison", string(retried[0].Value))
	assert.Equal(t, "1", helpers.GetHeader(retried[0], metadata.HeaderRetryAttempt))
	assert.Equal(t, "cannot process poison", helpers.GetHeader(retried[0], metadata.HeaderErrorMessage))
}

// TestPartialBatchFailFastLeavesOffsetUnmoved fails the eighth message of a
// twenty-message batch under fail-fast. The consumer stops, nothing is
// committed, and a restart receives the whole batch again.
func TestPartialBatchFailFastLeavesOffsetUnmoved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "partial-failfast")
	group := fmt.Sprintf("partial-failfast-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	const total = 20
	payloads := make([]string, total)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("msg-%02d", i)
	}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	eighthErr := errors.New("eighth message failed")
	first, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithBatchHandler(func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
			batch.Items()[7].Fail(easykafka.Failure{Err: eighthErr})
			return nil
		}),
		easykafka.WithBatchSize(total),
		easykafka.WithBatchTimeout(10*time.Minute), // the full batch is the only trigger
		easykafka.WithErrorStrategy(easykafka.NewFailFastStrategy()),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	firstCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	firstErr := first.Start(firstCtx)

	require.ErrorIs(t, firstErr, eighthErr, "the consumer should stop on the strategy's error")
	require.NoError(t, firstCtx.Err(), "the consumer should have stopped on its own")
	assert.Equal(t, kfk.OffsetInvalid, cluster.CommittedOffset(ctx, t, group, topic, 0),
		"a batch the strategy did not finish must commit nothing")

	var mu sync.Mutex
	var redelivered []string
	second, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithBatchHandler(func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
			mu.Lock()
			defer mu.Unlock()
			for _, item := range batch.Items() {
				redelivered = append(redelivered, string(item.Message().Payload))
			}
			return nil
		}),
		easykafka.WithBatchSize(total),
		easykafka.WithBatchTimeout(time.Second),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	stop := helpers.RunUntil(ctx, t, second)
	// >=, not ==: if a library bug delivered the batch twice, the count would
	// jump past total and == would only time out. Stopping here lets the exact
	// assert.Equal below show the duplicates instead.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(redelivered) >= total
	}, 60*time.Second, 100*time.Millisecond, "the batch was not redelivered")
	require.NoError(t, stop())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, payloads, redelivered, "every message of the batch comes back, in order")
}

// TestResumePointSurvivesTheRetryTopic runs the three-attempt example from the
// design end to end. Step 1 writes, step 2 publishes:
//
//   - attempt 1, from the source: the write fails → step 1, write_failed
//   - attempt 2, from the retry topic: resumes at step 1, the write succeeds and
//     the publish fails → step 2, publish_failed
//   - attempt 3: resumes at step 2, skipping the write, and fails without a step
//     or a code → step 2 carries forward, the code becomes HANDLER_ERROR
//   - attempt 4: still resumes at step 2, and succeeds
func TestResumePointSurvivesTheRetryTopic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	source := helpers.UniqueTopicName(t, "resume-source")
	retry := helpers.UniqueTopicName(t, "resume-retry")
	dlq := helpers.UniqueTopicName(t, "resume-dlq")
	for _, topic := range []string{source, retry, dlq} {
		cluster.CreateTopic(ctx, t, topic, 1)
	}
	cluster.ProduceMessages(ctx, t, source, []string{"slip-1"})

	type observation struct {
		attempt int
		step    int32
		code    string
		wrote   bool
	}
	var (
		mu           sync.Mutex
		observations []observation
		finishOnce   sync.Once
		finished     = make(chan struct{})
	)

	// One handler serves both topics, as a service consuming its own retry topic
	// would. It keys its behaviour off the attempt, and its resume point off the
	// step, exactly as the design describes.
	handler := func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
		for _, item := range batch.Items() {
			msg := item.Message()
			attempt := easykafka.GetRetryAttempt(&msg)
			step := easykafka.GetRetryStep(&msg)
			obs := observation{attempt: attempt, step: step, code: easykafka.GetErrorCode(&msg)}

			if step <= 1 { // step 1 has not succeeded yet
				if attempt == 0 {
					item.Fail(easykafka.Failure{Err: errors.New("write failed"), Step: 1, Code: "write_failed"})
					mu.Lock()
					observations = append(observations, obs)
					mu.Unlock()
					continue
				}
				obs.wrote = true
			}

			switch attempt {
			case 1:
				item.Fail(easykafka.Failure{Err: errors.New("publish failed"), Step: 2, Code: "publish_failed"})
			case 2:
				item.Fail(easykafka.Failure{Err: errors.New("unclassified")})
			default:
				finishOnce.Do(func() { close(finished) })
			}
			mu.Lock()
			observations = append(observations, obs)
			mu.Unlock()
		}
		return nil
	}

	newConsumer := func(topic string) easykafka.Consumer {
		c, err := easykafka.New(
			easykafka.WithTopic(topic),
			easykafka.WithBrokers(cluster.Brokers...),
			easykafka.WithConsumerGroup(fmt.Sprintf("resume-group-%s-%d", topic, time.Now().UnixNano())),
			easykafka.WithBatchHandler(handler),
			easykafka.WithBatchSize(1),
			easykafka.WithBatchTimeout(100*time.Millisecond),
			easykafka.WithErrorStrategy(helpers.NewRetryStrategy(t, retry, dlq, 10)),
			easykafka.WithPollTimeout(100*time.Millisecond),
		)
		require.NoError(t, err)
		return c
	}

	stopSource := helpers.RunUntil(ctx, t, newConsumer(source))
	stopRetry := helpers.RunUntil(ctx, t, newConsumer(retry))

	select {
	case <-finished:
	case <-time.After(90 * time.Second):
		t.Fatal("the message never completed its fourth attempt")
	}
	require.NoError(t, stopSource())
	require.NoError(t, stopRetry())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []observation{
		{attempt: 0, step: 0, code: "", wrote: false},
		{attempt: 1, step: 1, code: "write_failed", wrote: true},
		{attempt: 2, step: 2, code: "publish_failed", wrote: false},
		{attempt: 3, step: 2, code: "HANDLER_ERROR", wrote: false},
	}, observations)
}

// TestKeyAndBytesSurviveToRetryAndDLQ verifies the consumed key reaches the
// handler and is carried onto both the retry and the DLQ record, that
// ErrPermanent skips the retry topic, and that a payload which is not valid
// UTF-8 reaches the DLQ unchanged.
func TestKeyAndBytesSurviveToRetryAndDLQ(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	source := helpers.UniqueTopicName(t, "key-source")
	retry := helpers.UniqueTopicName(t, "key-retry")
	dlq := helpers.UniqueTopicName(t, "key-dlq")
	for _, topic := range []string{source, retry, dlq} {
		cluster.CreateTopic(ctx, t, topic, 1)
	}

	binary := []byte{0x00, 0xFF, 0xFE, '{', 0x80, 0xC3, 0x28}
	cluster.ProduceRecords(ctx, t, source, []helpers.Record{
		{Key: []byte("slip-transient"), Value: []byte(`{"slipId":"transient"}`)},
		{Key: []byte("slip-malformed"), Value: binary},
	})

	var (
		mu       sync.Mutex
		seenKeys []string
	)
	consumer, err := easykafka.New(
		easykafka.WithTopic(source),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(fmt.Sprintf("key-group-%d", time.Now().UnixNano())),
		easykafka.WithBatchHandler(func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
			for _, item := range batch.Items() {
				msg := item.Message()
				mu.Lock()
				seenKeys = append(seenKeys, string(msg.Key))
				mu.Unlock()
				if string(msg.Key) == "slip-malformed" {
					item.Fail(easykafka.Failure{Err: fmt.Errorf("%w: unmarshal: invalid json", easykafka.ErrPermanent)})
				} else {
					item.Fail(easykafka.Failure{Err: errors.New("downstream timeout")})
				}
			}
			return nil
		}),
		easykafka.WithBatchSize(2),
		easykafka.WithBatchTimeout(5*time.Second),
		easykafka.WithErrorStrategy(helpers.NewRetryStrategy(t, retry, dlq, 10)),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	stop := helpers.RunUntil(ctx, t, consumer)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seenKeys) >= 2
	}, 60*time.Second, 100*time.Millisecond, "the batch was never handled")
	require.NoError(t, stop())

	mu.Lock()
	assert.Equal(t, []string{"slip-transient", "slip-malformed"}, seenKeys, "the handler sees each key")
	mu.Unlock()

	retried := cluster.ConsumeMessages(ctx, t, retry, fmt.Sprintf("verify-key-retry-%d", time.Now().UnixNano()), 2, 10*time.Second)
	require.Len(t, retried, 1, "only the transient failure is retried")
	assert.Equal(t, "slip-transient", string(retried[0].Key))

	dead := cluster.ConsumeMessages(ctx, t, dlq, fmt.Sprintf("verify-key-dlq-%d", time.Now().UnixNano()), 2, 10*time.Second)
	require.Len(t, dead, 1, "the permanent failure goes straight to the DLQ")
	assert.Equal(t, "slip-malformed", string(dead[0].Key))
	assert.Equal(t, binary, dead[0].Value, "the DLQ body must be the consumed bytes, unchanged")
	assert.Equal(t, "1", helpers.GetHeader(dead[0], metadata.HeaderRetryAttempt), "no attempt was burned")
	assert.Equal(t, source, helpers.GetHeader(dead[0], metadata.HeaderOriginalTopic))
	assert.Equal(t, "1", helpers.GetHeader(dead[0], metadata.HeaderOriginalOffset))
}

// TestPartialBatchFailuresOnOnePartitionCommitBoth runs a batch spanning two
// partitions in which every failure is on one of them. Once the strategy has
// resolved the failures, both partitions commit past their last message.
func TestPartialBatchFailuresOnOnePartitionCommitBoth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "partial-partitions")
	group := fmt.Sprintf("partial-partitions-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 2)

	const perPartition = 5
	var records []helpers.Record
	for i := range perPartition {
		for _, p := range []int32{0, 1} {
			partition := p
			records = append(records, helpers.Record{
				Value:     fmt.Appendf(nil, "p%d-%d", partition, i),
				Partition: &partition,
			})
		}
	}
	// records now holds 10 records alternating between the partitions —
	// p0-0, p1-0, p0-1, p1-1, … p0-4, p1-4 — so each partition gets offsets 0-4.
	cluster.ProduceRecords(ctx, t, topic, records)

	var seen atomic.Int32
	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithBatchHandler(func(_ context.Context, batch *easykafka.Batch) *easykafka.Failure {
			for _, item := range batch.Items() {
				if item.Message().Partition == 1 {
					item.Fail(easykafka.Failure{Err: errors.New("partition 1 is unhappy")})
				}
			}
			seen.Add(int32(batch.Len()))
			return nil
		}),
		easykafka.WithBatchSize(len(records)),
		easykafka.WithBatchTimeout(time.Second),
		easykafka.WithPollTimeout(100*time.Millisecond),
		// The default skip strategy resolves every failure.
	)
	require.NoError(t, err)

	stop := helpers.RunUntil(ctx, t, consumer)
	require.Eventually(t, func() bool { return seen.Load() >= int32(len(records)) },
		60*time.Second, 100*time.Millisecond, "not every record was handled")
	require.NoError(t, stop())

	// The committed offset of both partitions (0 and 1) is 5, one past the last
	// message (offset 4): the offset the next consumer in the group starts from.
	for _, p := range []int32{0, 1} {
		assert.Equal(t, kfk.Offset(perPartition), cluster.CommittedOffset(ctx, t, group, topic, p),
			"partition %d should commit past its last message", p)
	}
}
