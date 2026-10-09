package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
)

// NewPublishWithFake calls publish.New with options, building the producer from
// a sharedhelpers.FakeProducer rather than librdkafka. The fake is closed when the
// test ends, which stops the publisher's report goroutine.
func NewPublishWithFake(t *testing.T, options ...publish.Option) (*publish.Publisher, error) {
	t.Helper()
	fake := sharedhelpers.NewFakeProducer()
	t.Cleanup(fake.Close)
	return publish.New(append([]publish.Option{publish.WithProducerFactory(fake.Factory())}, options...)...)
}
