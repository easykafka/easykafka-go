package publish_test

import (
	"strings"
	"testing"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/publish/helpers"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishConnectionLossIsLoggedOncePerOutage verifies that a lost
// connection is logged once however often it repeats, and its recovery once,
// with the count of errors suppressed in between.
func TestPublishConnectionLossIsLoggedOncePerOutage(t *testing.T) {
	logs := &sharedhelpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithLogger(zerolog.New(logs).Level(zerolog.InfoLevel)))
	delivery, err := publisher.Bind(helpers.PublishInvoiceTopic()).Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)

	down := publishdriver.ClientError{Err: &publishdriver.KafkaError{Code: "Local: All broker connections are down", Disconnected: true}}
	fake.Emit(down)
	fake.Emit(down)
	fake.Emit(down)
	fake.Succeed(0, 0, 1)
	_, err = helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)

	output := logs.String()
	assert.Equal(t, 1, strings.Count(output, "EK_PUBLISH_BROKER_DOWN"))
	assert.Equal(t, 1, strings.Count(output, "EK_PUBLISH_BROKER_RESTORED"))
	assert.Contains(t, output, `"suppressed":2`)
}

// TestPublishSuccessWithoutOutageLogsNothing verifies that an acknowledged
// record logs nothing when the connection was never lost.
func TestPublishSuccessWithoutOutageLogsNothing(t *testing.T) {
	logs := &sharedhelpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithLogger(zerolog.New(logs).Level(zerolog.InfoLevel)))
	delivery, err := publisher.Bind(helpers.PublishInvoiceTopic()).Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)
	fake.Succeed(0, 0, 1)
	_, err = helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)
	assert.Empty(t, logs.String())
}

// TestPublishOtherClientErrorsAreLogged verifies that a client error that is
// neither a lost connection nor fatal is logged as EK_PUBLISH_KAFKA_ERROR, and
// a fatal one as EK_PUBLISH_FATAL at error level.
func TestPublishOtherClientErrorsAreLogged(t *testing.T) {
	logs := &sharedhelpers.SyncBuffer{}
	publisher, fake := helpers.NewFakePublisher(t, publish.WithLogger(zerolog.New(logs)))
	delivery, err := publisher.Bind(helpers.PublishInvoiceTopic()).Send("k", helpers.NewPublishInvoice())
	require.NoError(t, err)

	fake.Emit(publishdriver.ClientError{Err: &publishdriver.KafkaError{Code: "Local: SSL error"}})
	fake.Emit(helpers.FatalClientError())
	// A barrier before reading the logs. Emit returns once the report goroutine
	// has taken an event, not once it has logged it. That goroutine handles
	// events one at a time, in order, so once this later report's delivery
	// has resolved, both client errors above have been handled and logged.
	fake.Succeed(0, 0, 1)
	_, err = helpers.WaitForDelivery(t, delivery)
	require.NoError(t, err)

	output := logs.String()
	assert.Equal(t, 1, strings.Count(output, "EK_PUBLISH_KAFKA_ERROR"))
	assert.Contains(t, output, `{"level":"warn","ek_code":"EK_PUBLISH_KAFKA_ERROR"`)
	assert.Equal(t, 1, strings.Count(output, "EK_PUBLISH_FATAL"))
	assert.Contains(t, output, `{"level":"error","ek_code":"EK_PUBLISH_FATAL"`)
	assert.NotContains(t, output, "EK_PUBLISH_BROKER_DOWN")
}
