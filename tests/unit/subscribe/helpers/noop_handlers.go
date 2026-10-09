package helpers

import (
	"context"

	"github.com/easykafka/easykafka-go/subscribe"
)

// NoopHandler is a single-message handler that succeeds without doing anything.
func NoopHandler(ctx context.Context, payload []byte) *subscribe.Failure {
	return nil
}

// NoopBatchHandler is a batch handler that succeeds without doing anything.
func NoopBatchHandler(ctx context.Context, batch *subscribe.Batch) *subscribe.Failure {
	return nil
}
