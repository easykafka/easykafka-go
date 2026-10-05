package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/stretchr/testify/require"
)

// NewFakePublisher returns a publisher on PublishBroker, with options applied,
// writing to the fake it also returns. The fake is closed when the test ends,
// which stops the publisher's report goroutine.
func NewFakePublisher(t *testing.T, options ...publish.Option) (*publish.Publisher, *FakePublishProducer) {
	t.Helper()
	fake := NewFakePublishProducer()
	t.Cleanup(fake.Close)
	all := append([]publish.Option{publish.WithBrokers(PublishBroker), publish.WithProducerFactory(fake.Factory())},
		options...)
	publisher, err := publish.New(all...)
	require.NoError(t, err)
	return publisher, fake
}
