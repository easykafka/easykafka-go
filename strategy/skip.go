package strategy

import (
	"context"

	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
)

// SkipStrategy logs errors and commits offsets, continuing consumption.
type SkipStrategy struct {
	logger zerolog.Logger
}

// NewSkipStrategy creates a skip strategy that logs and continues.
func NewSkipStrategy(logger zerolog.Logger) *SkipStrategy {
	return &SkipStrategy{logger: logger}
}

// HandleError logs the error for each message and returns nil to continue.
func (s *SkipStrategy) HandleError(ctx context.Context, msgs []*types.Message, handlerErr error) error {
	for _, msg := range msgs {
		s.logger.Warn().
			Str("topic", msg.Topic).
			Int32("partition", msg.Partition).
			Int64("offset", msg.Offset).
			Err(handlerErr).
			Msg("skipping failed message")
	}
	return nil // Continue consumption
}

// SetLogger sets the logger for the skip strategy.
func (s *SkipStrategy) SetLogger(logger zerolog.Logger) {
	s.logger = logger
}

// Name returns the strategy name.
func (s *SkipStrategy) Name() string {
	return "skip"
}
