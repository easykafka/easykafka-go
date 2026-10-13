package subscribe_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/easykafka/easykafka-go/subscribe"
	"github.com/easykafka/easykafka-go/tests/integration/sharedhelpers"
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
	cluster := sharedhelpers.SharedCluster(t)

	topic := sharedhelpers.UniqueTopicName(t, "autocommit-window")
	group := fmt.Sprintf("autocommit-window-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	payloads := []string{"msg-1", "msg-2", "msg-3", "msg-4", "msg-5"}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	var handled atomic.Int32

	consumer, err := subscribe.New(
		subscribe.WithTopic(topic),
		subscribe.WithBrokers(cluster.Brokers...),
		subscribe.WithConsumerGroup(group),
		subscribe.WithHandler(func(context.Context, []byte) *subscribe.Failure {
			handled.Add(1)
			return nil
		}),
		subscribe.WithPollTimeout(100*time.Millisecond),
		// Longer than the test will run, so the background committer never fires
		// and the only commit that can happen is the one at shutdown.
		subscribe.WithAutoCommitEvery(10*time.Minute),
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
	cluster := sharedhelpers.SharedCluster(t)

	topic := sharedhelpers.UniqueTopicName(t, "autocommit-clean-stop")
	group := fmt.Sprintf("autocommit-clean-stop-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	payloads := []string{"msg-1", "msg-2", "msg-3"}
	cluster.ProduceMessages(ctx, t, topic, payloads)

	newConsumer := func(handled *atomic.Int32) *subscribe.Subscriber {
		c, err := subscribe.New(
			subscribe.WithTopic(topic),
			subscribe.WithBrokers(cluster.Brokers...),
			subscribe.WithConsumerGroup(group),
			subscribe.WithHandler(func(context.Context, []byte) *subscribe.Failure {
				handled.Add(1)
				return nil
			}),
			subscribe.WithPollTimeout(100*time.Millisecond),
			subscribe.WithAutoCommitEvery(10*time.Minute),
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

// TestCommitFailureOnEjectionIsLogged verifies that a consumer ejected from its
// group under an auto-commit interval logs the commit that fails because of it,
// keeps running, and gets the message again: a failed commit is neither silent,
// nor fatal, nor a record of work as done.
//
// The provocation is the same as TestCommitFailureDuringGroupLossIsTolerated:
// stall the handler past max.poll.interval.ms so the broker ejects the consumer.
//
// One message is produced but the test waits for two deliveries, which looks
// unreachable until the ejection is followed through. The sequence:
//
//  1. Poll returns msg-1 and the handler stalls for 20s. The poll loop is
//     blocked inside it, so nothing polls.
//  2. At ~10s the broker hits max.poll.interval.ms and ejects the consumer.
//  3. Nothing was committable in the meantime: the offset is stored only after
//     the handler returns, so the store is empty for this partition throughout.
//  4. At 20s the handler returns, the subscriber stores the offset, and the
//     next commit fails, since the consumer no longer holds the partition. That
//     failure is the EK_COMMIT_FAILED line asserted at the end.
//  5. Polling resumes, the consumer rejoins, and the fetch position resets to
//     the committed offset — still nothing, because the commit failed.
//  6. msg-1 is redelivered. That second delivery is the signal to stop waiting,
//     and doubles as proof that a failed commit recorded nothing as done.
//
// Which commit fails in step 4 depends on librdkafka. Up to 2.15, the background
// committer still tried to commit the lost partition, and the driver logged the
// failed OffsetsCommitted event ("auto-commit failed", commit=auto). From 2.16
// librdkafka skips commits for an assignment it has lost (librdkafka #5585), and
// it is the subscriber's own commit on revocation that fails (commit=revoke).
// Either is logged under EK_COMMIT_FAILED, which is what the test asserts. Since
// 2.16 this test no longer exercises the code that logs a failed background
// commit (subscribedriver's consumer.Poll, the `case kfk.OffsetsCommitted:`
// branch with e.Error != nil, which logs "auto-commit failed, offsets remain
// stored" with commit=auto): librdkafka no longer produces a failing
// OffsetsCommitted event in this scenario, so that branch has no test at all.
//
// So deliveries counts deliveries, not distinct messages: the same message
// arrives twice.
func TestCommitFailureOnEjectionIsLogged(t *testing.T) {
	t.Log("TestCommitFailureOnEjectionIsLogged started")
	defer t.Log("TestCommitFailureOnEjectionIsLogged finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()
	cluster := sharedhelpers.SharedCluster(t)

	topic := sharedhelpers.UniqueTopicName(t, "autocommit-failure")
	group := fmt.Sprintf("autocommit-failure-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)
	cluster.ProduceMessages(ctx, t, topic, []string{"msg-1"})

	logs := &sharedhelpers.SyncBuffer{}
	logger := zerolog.New(logs).Level(zerolog.DebugLevel)

	const stallFor = 20 * time.Second

	var mu sync.Mutex
	var deliveries int

	consumer, err := subscribe.New(
		subscribe.WithTopic(topic),
		subscribe.WithBrokers(cluster.Brokers...),
		subscribe.WithConsumerGroup(group),
		subscribe.WithLogger(logger),
		subscribe.WithPollTimeout(100*time.Millisecond),
		// Short enough to fire repeatedly while the handler stalls, so the
		// background committer keeps trying across the ejection.
		subscribe.WithAutoCommitEvery(time.Second),
		subscribe.WithKafkaConfig(map[string]any{
			"max.poll.interval.ms":  10000,
			"session.timeout.ms":    6000,
			"heartbeat.interval.ms": 2000,
		}),
		subscribe.WithHandler(func(handlerCtx context.Context, _ []byte) *subscribe.Failure {
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

	// The commit that failed because of the ejection was logged under its code,
	// whichever commit it was (see step 4 above).
	assert.Contains(t, captured, `"ek_code":"EK_COMMIT_FAILED"`,
		"a commit failing after the ejection must be logged, not silent; logs were:\n%s", captured)
	assert.NotContains(t, captured, "stopping consumer",
		"a commit failure must never be treated as fatal")
}
