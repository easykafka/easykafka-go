package unit

import (
	"context"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishPingAllTopicsPresent verifies that Ping asks for every bound
// topic once, in binding order, and returns nil when all exist.
func TestPublishPingAllTopicsPresent(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	fake.Partitions = map[string]int{"invoices": 3, "raw": 1}
	publisher.Bind(helpers.PublishInvoiceTopic())
	publisher.Bind(publish.Topic[string, []byte]{Name: "raw", EncodeKey: publish.StringKey, EncodeValue: publish.RawValue})
	// Binding "invoices" again returns a second, working writer, but does not
	// add the name to Ping's list again: Ping must ask for it only once.
	publisher.Bind(helpers.PublishInvoiceTopic())

	require.NoError(t, publisher.Ping(context.Background()))
	topics, _ := fake.PingTopics()
	assert.Equal(t, []string{"invoices", "raw"}, topics)
}

// TestPublishPingNamesEveryMissingTopic verifies that missing topics are
// ErrTopicNotFound, each named.
func TestPublishPingNamesEveryMissingTopic(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	fake.Partitions = map[string]int{"present": 1}
	for _, name := range []string{"present", "invoces", "ledger"} {
		publisher.Bind(publish.Topic[string, []byte]{Name: name, EncodeKey: publish.StringKey, EncodeValue: publish.RawValue})
	}

	err := publisher.Ping(context.Background())
	require.ErrorIs(t, err, publish.ErrTopicNotFound)
	assert.Contains(t, err.Error(), `"invoces"`)
	assert.Contains(t, err.Error(), `"ledger"`)
	assert.NotContains(t, err.Error(), `"present"`)
}

// TestPublishPingWithoutTopics verifies that Ping still asks the cluster when
// nothing is bound.
func TestPublishPingWithoutTopics(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	require.NoError(t, publisher.Ping(context.Background()))
	topics, hadDeadline := fake.PingTopics()
	assert.Empty(t, topics)
	assert.True(t, hadDeadline)
}

// TestPublishPingReportsClusterError verifies that a failed metadata request
// is returned, wrapped.
func TestPublishPingReportsClusterError(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	fake.PartitionsErr = context.DeadlineExceeded
	publisher.Bind(helpers.PublishInvoiceTopic())

	err := publisher.Ping(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, publish.ErrTopicNotFound)
	assert.Contains(t, err.Error(), "ping")
}

// TestPublishPingBoundsAContextWithoutDeadline verifies that Ping adds its own
// bound when the caller's context has none, and keeps the caller's otherwise.
func TestPublishPingBoundsAContextWithoutDeadline(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	require.NoError(t, publisher.Ping(context.Background()))
	_, hadDeadline := fake.PingTopics()
	assert.True(t, hadDeadline, "a context without a deadline gets the default bound")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, publisher.Ping(ctx))
	_, hadDeadline = fake.PingTopics()
	assert.True(t, hadDeadline)
}

// TestPublishPingRepeats verifies that Ping changes nothing, so a topic bound
// after one Ping is checked by the next.
func TestPublishPingRepeats(t *testing.T) {
	publisher, fake := helpers.NewFakePublisher(t)
	fake.Partitions = map[string]int{"invoices": 1}          // the fake cluster knows only "invoices"
	require.NoError(t, publisher.Ping(context.Background())) // nothing bound yet: nothing to check
	// Bind a topic the cluster does not have.
	publisher.Bind(publish.Topic[string, []byte]{Name: "late", EncodeKey: publish.StringKey, EncodeValue: publish.RawValue})

	err := publisher.Ping(context.Background()) // checks "late" now: missing
	require.ErrorIs(t, err, publish.ErrTopicNotFound)
	assert.Contains(t, err.Error(), `"late"`)
}
