package integration

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishPingFindsBoundTopics verifies Ping against a real broker: nil
// once every bound topic exists.
func TestPublishPingFindsBoundTopics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)
	first := helpers.UniqueTopicName(t, "publish-ping-first")
	second := helpers.UniqueTopicName(t, "publish-ping-second")
	cluster.CreateTopic(ctx, t, first, 1)
	cluster.CreateTopic(ctx, t, second, 3)

	publisher := helpers.NewPublisher(t, cluster.Brokers)
	publisher.Bind(helpers.PublishInvoiceTopic(first))
	publisher.Bind(helpers.PublishInvoiceTopic(second))

	require.NoError(t, publisher.Ping(ctx))
}

// TestPublishPingNamesMissingTopic verifies that Ping reports a misspelt
// topic as ErrTopicNotFound, naming it and none of the topics that exist.
func TestPublishPingNamesMissingTopic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	cluster := helpers.SharedCluster(t)
	existing := helpers.UniqueTopicName(t, "publish-ping-existing")
	cluster.CreateTopic(ctx, t, existing, 1)
	misspelt := existing + "-misspelt"

	publisher := helpers.NewPublisher(t, cluster.Brokers)
	publisher.Bind(helpers.PublishInvoiceTopic(existing))
	publisher.Bind(helpers.PublishInvoiceTopic(misspelt))

	err := publisher.Ping(ctx)
	require.ErrorIs(t, err, publish.ErrTopicNotFound)
	assert.Contains(t, err.Error(), strconv.Quote(misspelt))
	assert.NotContains(t, err.Error(), strconv.Quote(existing))
}

// TestPublishPingWithoutTopicsChecksTheCluster verifies that Ping with no
// topic bound still asks the cluster, and succeeds once a broker answers.
func TestPublishPingWithoutTopicsChecksTheCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	cluster := helpers.SharedCluster(t)
	publisher := helpers.NewPublisher(t, cluster.Brokers)

	require.NoError(t, publisher.Ping(context.Background()))
}

// TestPublishPingUnreachableBrokerEndsWithinBound verifies that Ping against
// no broker fails within its bound: the context's deadline when it has one,
// and 5 s when it has none.
func TestPublishPingUnreachableBrokerEndsWithinBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	cases := []struct {
		name  string
		ctx   func() (context.Context, context.CancelFunc)
		bound time.Duration
	}{
		{
			name: "context deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 2*time.Second)
			},
			bound: 2 * time.Second,
		},
		{
			name: "no deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			bound: 5 * time.Second,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			publisher := helpers.NewPublisher(t, []string{helpers.UnreachableBroker})
			publisher.Bind(helpers.PublishInvoiceTopic("never-reached"))

			ctx, cancel := testCase.ctx()
			defer cancel()
			started := time.Now()
			err := publisher.Ping(ctx)
			elapsed := time.Since(started)
			t.Logf("Ping returned after %s: %v", elapsed.Round(time.Millisecond), err)

			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, publish.ErrTopicNotFound, "an unreachable cluster is not a missing topic")
			assert.Less(t, elapsed, testCase.bound+time.Second)
		})
	}
}
