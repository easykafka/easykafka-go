package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers what happens when a consumer loses its place in the group
// while a handler is still running — the offset work's two rules, observed
// against a real broker rather than a fake adapter.
//
// A failed store is fatal, because the next message on that partition would
// store a higher offset whose commit seals the gap. A failed commit is not,
// because the offset stays in the store and a later commit picks it up. The unit
// tests drive both from a fake; this one provokes the real thing.
//
// It needs machinery the other tests do not have, hence its own file: a handler
// that stalls past max.poll.interval.ms so the broker ejects the consumer
// mid-message, and a captured logger to show which branch ran.
//
// Note on what is NOT covered here. The engine also tolerates StoreOffset
// returning ErrPartitionRevoked, and that branch could not be provoked. Even with
// the consumer already ejected from the group, librdkafka accepted the store —
// its offset store is local and does not consult group state, and the engine
// stores before the Poll that processes the revocation. The branch appears to be
// unreachable in practice and remains defensive; the unit tests cover the engine's
// reaction to it.

// TestCommitFailureDuringGroupLossIsTolerated stalls a handler until the broker
// ejects the consumer, so the commit that follows has no group to commit to.
//
// Everything then hinges on the store being the source of truth: the commit fails
// harmlessly, the consumer keeps running, the offset waits in the store, and the
// message is redelivered because nothing ever recorded it as done.
func TestCommitFailureDuringGroupLossIsTolerated(t *testing.T) {
	t.Log("TestCommitFailureDuringGroupLossIsTolerated started")
	defer t.Log("TestCommitFailureDuringGroupLossIsTolerated finished")

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)

	topic := helpers.UniqueTopicName(t, "group-loss")
	group := fmt.Sprintf("group-loss-group-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)
	cluster.ProduceMessages(ctx, t, topic, []string{"msg-00"})

	// Debug level, so both the benign and fatal branches would be visible.
	logs := &helpers.SyncBuffer{}
	logger := zerolog.New(logs).Level(zerolog.DebugLevel)

	const stallFor = 20 * time.Second

	var mu sync.Mutex
	var handledCount int

	consumer, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithLogger(logger),
		easykafka.WithPollTimeout(100*time.Millisecond),
		// The ejection mechanism: going longer than max.poll.interval.ms without
		// polling makes the broker treat this consumer as gone and take its
		// partition away. The handler below stalls well past that.
		easykafka.WithKafkaConfig(map[string]any{
			"max.poll.interval.ms":  10000,
			"session.timeout.ms":    6000,
			"heartbeat.interval.ms": 2000,
		}),
		easykafka.WithHandler(func(handlerCtx context.Context, _ []byte) *easykafka.Failure {
			mu.Lock()
			handledCount++
			first := handledCount == 1
			mu.Unlock()

			if !first {
				// Redelivery after the rejoin. Return promptly so the test ends.
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

	// The message coming back is the observable proof that nothing recorded it as
	// done while the group was lost.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return handledCount >= 2
	}, 90*time.Second, 500*time.Millisecond,
		"the message should be redelivered after the consumer rejoins the group")

	cancel()
	startErr := <-done

	captured := logs.String()
	t.Logf("handled %d times", handledCount)

	// The consumer survived. A commit failure that stopped the engine would
	// surface here.
	require.NoError(t, startErr, "a failed commit must not stop the consumer")

	// It took the benign branch, and said so.
	assert.Contains(t, captured, "commit failed, offset remains stored",
		"expected the commit to fail while the group was lost; logs were:\n%s", captured)
	assert.NotContains(t, captured, "stopping consumer",
		"a commit failure must never be treated as fatal")

	// The offset was not lost with the failed commit: a later commit published it.
	// This is the whole reason a failed commit is survivable.
	assert.Contains(t, captured, "stored offsets committed",
		"the offset should have been committed by a later commit; logs were:\n%s", captured)
}
