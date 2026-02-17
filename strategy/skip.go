package strategy

import (
	"context"

	"github.com/easykafka/easykafka-go/internal/types"
)

// SkipStrategy logs errors and commits offsets, continuing consumption.
type SkipStrategy struct {
	logger types.Logger
}

// NewSkipStrategy creates a skip strategy that logs and continues.
func NewSkipStrategy(logger types.Logger) *SkipStrategy {
	return &SkipStrategy{logger: logger}
}

// HandleError logs the error and returns nil to continue.
func (s *SkipStrategy) HandleError(ctx context.Context, msgs []*types.Message, handlerErr error) error {
	if s.logger != nil {
		s.logger.Warn("skipping failed message", "count", len(msgs), "error", handlerErr.Error())
	}
	return nil // Continue consumption
}

// Name returns the strategy name.
func (s *SkipStrategy) Name() string {
	return "skip"
}
