package helpers

import (
	"context"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/types"
	"github.com/easykafka/easykafka-go/subscribe"
	"github.com/rs/zerolog"
)

// RunBatch drives a subscriber in batch mode over the fake's messages as a
// single batch and returns Start's result. The subscriber runs for 500ms,
// which is ample for a fake consumer that hands its messages over without
// delay.
func RunBatch(
	t *testing.T,
	fake *FakeConsumer,
	handler types.BatchHandler,
	strat types.ErrorStrategy,
	logger zerolog.Logger,
) error {

	t.Helper()
	subscriber := NewSubscriberOnFake(t, fake,
		subscribe.WithBatchHandler(handler),
		subscribe.WithErrorStrategy(strat),
		subscribe.WithLogger(logger),
		subscribe.WithPollTimeout(10*time.Millisecond),
		subscribe.WithBatchSize(len(fake.Messages)),
		subscribe.WithBatchTimeout(5*time.Second),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return subscriber.Start(ctx)
}
