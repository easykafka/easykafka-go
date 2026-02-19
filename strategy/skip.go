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

// HandleError logs the error and returns nil to continue.
func (s *SkipStrategy) HandleError(ctx context.Context, msgs []*types.Message, handlerErr error) error {
	s.logger.Warn().Int("count", len(msgs)).Err(handlerErr).Msg("skipping failed message")
	return nil // Continue consumption
}

// Name returns the strategy name.
func (s *SkipStrategy) Name() string {
	return "skip"
}
