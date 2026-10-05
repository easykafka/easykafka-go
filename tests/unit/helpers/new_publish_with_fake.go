package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
)

// NewPublishWithFake calls publish.New with options, building the producer from
// a FakePublishProducer rather than librdkafka. The fake is closed when the
// test ends, which stops the publisher's report goroutine.
func NewPublishWithFake(t *testing.T, options ...publish.Option) (*publish.Publisher, error) {
	t.Helper()
	fake := NewFakePublishProducer()
	t.Cleanup(fake.Close)
	return publish.New(append([]publish.Option{publish.WithProducerFactory(fake.Factory())}, options...)...)
}
