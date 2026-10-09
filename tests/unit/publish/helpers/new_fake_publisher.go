package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
	"github.com/stretchr/testify/require"
)

// NewFakePublisher returns a publisher on sharedhelpers.PublishBroker, with options applied,
// writing to the fake it also returns. The fake is closed when the test ends,
// which stops the publisher's report goroutine.
func NewFakePublisher(t *testing.T, options ...publish.Option) (*publish.Publisher, *sharedhelpers.FakeProducer) {
	t.Helper()
	fake := sharedhelpers.NewFakeProducer()
	t.Cleanup(fake.Close)
	all := append([]publish.Option{publish.WithBrokers(sharedhelpers.PublishBroker), publish.WithProducerFactory(fake.Factory())},
		options...)
	publisher, err := publish.New(all...)
	require.NoError(t, err)
	return publisher, fake
}
