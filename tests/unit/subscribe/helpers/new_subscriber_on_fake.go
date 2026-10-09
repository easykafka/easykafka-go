package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/internal/subscribe/subscribedriver"
	"github.com/easykafka/easykafka-go/subscribe"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
	"github.com/stretchr/testify/require"
)

// consumerFake is any of the fake consumers: each hands itself out through
// Factory.
type consumerFake interface {
	Factory() func(subscribedriver.Config) (subscribedriver.Consumer, error)
}

// NewSubscriberOnFake builds a Subscriber over fake, as NewRetryStrategyOnFake
// builds a retry strategy over a FakeProducer. It supplies what New requires
// but the poll loop's tests do not care about — a topic, a broker and a
// consumer group — and a logger that discards everything, then applies options:
// the handler or batch handler, the strategy, the poll timeout, the batch
// settings, and a logger of the test's own if it wants one.
func NewSubscriberOnFake(t *testing.T, fake consumerFake, options ...subscribe.Option) *subscribe.Subscriber {
	t.Helper()
	all := append([]subscribe.Option{
		subscribe.WithTopic("test-topic"),
		subscribe.WithBrokers(sharedhelpers.PublishBroker),
		subscribe.WithConsumerGroup("test-group"),
		subscribe.WithLogger(sharedhelpers.TestLogger()),
		subscribe.WithConsumerFactory(fake.Factory()),
	}, options...)
	subscriber, err := subscribe.New(all...)
	require.NoError(t, err)
	return subscriber
}
