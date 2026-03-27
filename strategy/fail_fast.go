package strategy

import (
	"context"
	"fmt"

	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// FailFastStrategy stops the consumer immediately on any handler error.
type FailFastStrategy struct {
	logger zerolog.Logger
}

// NewFailFastStrategy creates a new fail-fast error strategy.
func NewFailFastStrategy() *FailFastStrategy {
	return &FailFastStrategy{logger: zerolog.Nop()}
}

// SetLogger sets the logger for the fail-fast strategy.
func (f *FailFastStrategy) SetLogger(logger zerolog.Logger) {
	f.logger = logger
}

// HandleError returns the handler error, causing the consumer to stop.
func (f *FailFastStrategy) HandleError(_ context.Context, msgs []*types.Message, handlerErr error) error {
	if len(msgs) > 0 {
		f.logger.Error().
			Str("topic", msgs[0].Topic).
			Int32("partition", msgs[0].Partition).
			Int64("offset", msgs[0].Offset).
			Err(handlerErr).
			Msg("fail-fast: stopping consumer due to handler error")
		return fmt.Errorf("fail-fast: handler error on topic=%s partition=%d offset=%d: %w",
			msgs[0].Topic, msgs[0].Partition, msgs[0].Offset, handlerErr)
	}
	f.logger.Error().Err(handlerErr).Msg("fail-fast: stopping consumer due to handler error")
	return fmt.Errorf("fail-fast: handler error: %w", handlerErr)
}

// Name returns the strategy name.
func (f *FailFastStrategy) Name() string {
	return "fail-fast"
}
