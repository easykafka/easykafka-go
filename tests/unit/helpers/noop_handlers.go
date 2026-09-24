package helpers

import (
	"context"

	easykafka "github.com/easykafka/easykafka-go"
)

// NoopHandler is a single-message handler that succeeds without doing anything.
func NoopHandler(ctx context.Context, payload []byte) *easykafka.Failure {
	return nil
}

// NoopBatchHandler is a batch handler that succeeds without doing anything.
func NoopBatchHandler(ctx context.Context, batch *easykafka.Batch) *easykafka.Failure {
	return nil
}
