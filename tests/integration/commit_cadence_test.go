package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WithAutoCommitEvery hands commit timing to librdkafka. These tests pin the
// bargain that makes: a wider duplicate window while running, closed again by
// the commit at shutdown, and never any loss.

// TestAutoCommitEveryWidensTheDuplicateWindow makes the window observable.
//
// The consumer handles every message but the broker is told nothing, because the
// interval is longer than the test. That gap *is* the duplicate window: a process
// dying at this point replays all of it. It cannot be shown by actually killing
// the consumer — every route out of Start commits — so the test asserts on the
// committed offset instead, which is what a restart would resume from.
func TestAutoCommitEveryWidensTheDuplicateWindow(t *testing.T) {
	t.Log("TestAutoCommitEveryWidensTheDuplicateWindow started")
	defer t.Log("TestAutoCommitEveryWidensTheDuplicateWindow finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "autocommit-window")
	group := fmt.Sprintf("autocommit-window-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	payloads := []string{"msg-1", "msg-2", "msg-3", "msg-4", "msg-5"}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	var handled atomic.Int32

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithHandler(func(context.Context, []byte) *easykafka.Failure {
			handled.Add(1)
			return nil
		}),
		easykafka.WithPollTimeout(100*time.Millisecond),
		// Longer than the test will run, so the background committer never fires
		// and the only commit that can happen is the one at shutdown.
		easykafka.WithAutoCommitEvery(10*time.Minute),
	)
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Start(runCtx) }()

	require.Eventually(t, func() bool { return handled.Load() == int32(len(payloads)) },
		60*time.Second, 200*time.Millisecond, "consumer did not handle every message")

	// Every message is done, and the group still does not know. In the default
	// cadence this would already read 5.
	committed := cluster.CommittedOffset(ctx, t, group, topic, 0)
	assert.Equal(t, kfk.OffsetInvalid, committed,
		"with an interval longer than the test, nothing should have been committed yet")

	cancel()
	require.NoError(t, <-done)
}

// TestAutoCommitEveryCleanShutdownLeavesNothingToReplay is the other half: the
// window above is closed by the unconditional commit on the way out, so a clean
// stop costs nothing. A fresh consumer in the same group must find no work.
func TestAutoCommitEveryCleanShutdownLeavesNothingToReplay(t *testing.T) {
	t.Log("TestAutoCommitEveryCleanShutdownLeavesNothingToReplay started")
	defer t.Log("TestAutoCommitEveryCleanShutdownLeavesNothingToReplay finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "autocommit-clean-stop")
	group := fmt.Sprintf("autocommit-clean-stop-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	payloads := []string{"msg-1", "msg-2", "msg-3"}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	newConsumer := func(handled *atomic.Int32) easykafka.Consumer {
		c, err := easykafka.New(
			easykafka.WithTopic(topic),
			easykafka.WithBrokers(cluster.Brokers...),
			easykafka.WithConsumerGroup(group),
			easykafka.WithHandler(func(context.Context, []byte) *easykafka.Failure {
				handled.Add(1)
				return nil
			}),
			easykafka.WithPollTimeout(100*time.Millisecond),
			easykafka.WithAutoCommitEvery(10*time.Minute),
		)
		require.NoError(t, err)
		return c
	}

	var firstHandled atomic.Int32
	first := newConsumer(&firstHandled)

	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Start(firstCtx) }()

	require.Eventually(t, func() bool { return firstHandled.Load() == int32(len(payloads)) },
		60*time.Second, 200*time.Millisecond, "first consumer did not handle every message")

	cancelFirst()
	require.NoError(t, <-firstDone)

	// The shutdown commit ran, so the group resumes past everything handled.
	assert.Equal(t, kfk.Offset(len(payloads)), cluster.CommittedOffset(ctx, t, group, topic, 0),
		"a clean shutdown must publish the tail the interval had not reached")

	var secondHandled atomic.Int32
	second := newConsumer(&secondHandled)

	secondCtx, cancelSecond := context.WithCancel(ctx)
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Start(secondCtx) }()

	// Give it long enough to join the group and read anything that was left.
	time.Sleep(15 * time.Second)

	cancelSecond()
	require.NoError(t, <-secondDone)

	assert.Zero(t, secondHandled.Load(),
		"a clean shutdown under an interval must leave nothing to replay")
}

// TestAutoCommitFailureIsLogged covers R3: under an interval the commit happens
// on librdkafka's background thread, so a failure is reported as an
// OffsetsCommitted event rather than returned. Without the adapter handling that
// event, a coordinator rejecting every commit would be completely silent while
// the duplicate window grew.
//
// The provocation is the same as TestCommitFailureDuringGroupLossIsTolerated:
// stall the handler past max.poll.interval.ms so the broker ejects the consumer,
// leaving the background committer with no group to commit to.
//
// One message is produced but the test waits for two deliveries, which looks
// unreachable until the ejection is followed through. The sequence:
//
//  1. Poll returns msg-1 and the handler stalls for 20s. The poll loop is
//     blocked inside it, so nothing polls.
//  2. At ~10s the broker hits max.poll.interval.ms and ejects the consumer.
//  3. Nothing was committable in the meantime: the offset is stored only after
//     the handler returns, so the store is empty for this partition throughout.
//  4. At 20s the handler returns, the engine stores the offset, and the
//     background committer tries to publish it with no group to publish to.
//     That failure is the log line asserted at the end.
//  5. Polling resumes, the consumer rejoins, and the fetch position resets to
//     the committed offset — still nothing, because the commit failed.
//  6. msg-1 is redelivered. That second delivery is the signal to stop waiting,
//     and doubles as proof that a failed commit recorded nothing as done.
//
// So deliveries counts deliveries, not distinct messages: the same message
// arrives twice.
func TestAutoCommitFailureIsLogged(t *testing.T) {
	t.Log("TestAutoCommitFailureIsLogged started")
	defer t.Log("TestAutoCommitFailureIsLogged finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "autocommit-failure")
	group := fmt.Sprintf("autocommit-failure-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)
	cluster.ProduceMessages(ctx, t, topic, []string{"msg-1"})

	logs := &helpers.SyncBuffer{}
	logger := zerolog.New(logs).Level(zerolog.DebugLevel)

	const stallFor = 20 * time.Second

	var mu sync.Mutex
	var deliveries int

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithLogger(logger),
		easykafka.WithPollTimeout(100*time.Millisecond),
		// Short enough to fire repeatedly while the handler stalls, so the
		// background committer keeps trying across the ejection.
		easykafka.WithAutoCommitEvery(time.Second),
		easykafka.WithKafkaConfig(map[string]any{
			"max.poll.interval.ms":  10000,
			"session.timeout.ms":    6000,
			"heartbeat.interval.ms": 2000,
		}),
		easykafka.WithHandler(func(handlerCtx context.Context, _ []byte) *easykafka.Failure {
			mu.Lock()
			deliveries++
			first := deliveries == 1
			mu.Unlock()

			if !first {
				return nil
			}
			select {
			case <-time.After(stallFor):
			case <-handlerCtx.Done():
			}
			return nil
		}),
	)
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Start(runCtx) }()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return deliveries >= 2
	}, 90*time.Second, 500*time.Millisecond,
		"the one message should be delivered a second time after the consumer rejoins")

	cancel()
	require.NoError(t, <-done, "an auto-commit failure must not stop the consumer")

	captured := logs.String()

	// The failing OffsetsCommitted event reached the adapter and was logged. Drop
	// the R3 handler and this event falls to Poll's default branch, leaving the
	// whole ejection silent.
	assert.Contains(t, captured, "auto-commit failed, offsets remain stored",
		"a failed background commit must be logged, not dropped in Poll's default branch; logs were:\n%s",
		captured)
	assert.NotContains(t, captured, "stopping consumer",
		"a commit failure must never be treated as fatal")
}
