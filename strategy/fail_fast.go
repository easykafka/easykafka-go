package strategy

import (
	"context"
	"fmt"

	"github.com/easykafka/easykafka-go/internal/types"
)

// FailFastStrategy stops the consumer immediately on any handler error.
type FailFastStrategy struct{}

// NewFailFastStrategy creates a new fail-fast error strategy.
func NewFailFastStrategy() *FailFastStrategy {
	return &FailFastStrategy{}
}

// HandleError returns the handler error, causing the consumer to stop.
func (f *FailFastStrategy) HandleError(_ context.Context, msgs []*types.Message, handlerErr error) error {
	if len(msgs) > 0 {
		return fmt.Errorf("fail-fast: handler error on topic=%s partition=%d offset=%d: %w",
			msgs[0].Topic, msgs[0].Partition, msgs[0].Offset, handlerErr)
	}
	return fmt.Errorf("fail-fast: handler error: %w", handlerErr)
}

// Name returns the strategy name.
func (f *FailFastStrategy) Name() string {
	return "fail-fast"
}
