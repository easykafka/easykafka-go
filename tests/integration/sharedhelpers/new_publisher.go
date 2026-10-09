package sharedhelpers

import (
	"context"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
)

// NewPublisher creates a publisher for brokers with the given options, and
// closes it when the test ends, unless the test has closed it already.
func NewPublisher(tb testing.TB, brokers []string, options ...publish.Option) *publish.Publisher {
	tb.Helper()

	publisher, err := publish.New(append([]publish.Option{publish.WithBrokers(brokers...)}, options...)...)
	if err != nil {
		tb.Fatalf("creating publisher: %v", err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// A second Close returns the first one's error, which the test has
		// already checked.
		_ = publisher.Close(ctx)
	})
	return publisher
}
