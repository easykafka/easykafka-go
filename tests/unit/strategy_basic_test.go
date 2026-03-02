package unit

import (
	"context"
	"testing"

	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// T028 [US3] Unit tests for fail-fast and skip strategies
// =============================================================================

func newStrategyTestMessage(topic string, partition int32, offset int64, payload string) *types.Message {
	return &types.Message{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
		Headers:   make(map[string]string),
		Payload:   []byte(payload),
	}
}

// =============================================================================
// FailFast Strategy Tests
// =============================================================================

// TestFailFastStrategyReturnsError verifies that HandleError returns an error
// wrapping the handler error, which causes the consumer to stop.
func TestFailFastStrategyReturnsError(t *testing.T) {
	s := strategy.NewFailFastStrategy()

	msgs := []*types.Message{
		newStrategyTestMessage("test-topic", 0, 42, "payload"),
	}
	handlerErr := assert.AnError

	err := s.HandleError(context.Background(), msgs, handlerErr)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "fail-fast")
	assert.Contains(t, err.Error(), "test-topic")
	assert.Contains(t, err.Error(), "42") // offset
	assert.ErrorIs(t, err, handlerErr)
}

// TestFailFastStrategyName verifies the strategy name is "fail-fast".
func TestFailFastStrategyName(t *testing.T) {
	s := strategy.NewFailFastStrategy()
	assert.Equal(t, "fail-fast", s.Name())
}

// TestFailFastStrategyWithEmptyMessages verifies behavior with empty message slice.
func TestFailFastStrategyWithEmptyMessages(t *testing.T) {
	s := strategy.NewFailFastStrategy()
	handlerErr := assert.AnError

	err := s.HandleError(context.Background(), nil, handlerErr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fail-fast")
}

// TestFailFastStrategyAlwaysStopsConsumer verifies fail-fast never returns nil.
func TestFailFastStrategyAlwaysStopsConsumer(t *testing.T) {
	s := strategy.NewFailFastStrategy()

	// Call multiple times — always returns error
	for i := 0; i < 5; i++ {
		msgs := []*types.Message{
			newStrategyTestMessage("topic", 0, int64(i), "msg"),
		}
		err := s.HandleError(context.Background(), msgs, assert.AnError)
		require.Error(t, err, "fail-fast should always return error on attempt %d", i)
	}
}

// =============================================================================
// Skip Strategy Tests
// =============================================================================

// TestSkipStrategyReturnsNil verifies that HandleError returns nil,
// allowing the consumer to continue.
func TestSkipStrategyReturnsNil(t *testing.T) {
	s := strategy.NewSkipStrategy(zerolog.Nop())

	msgs := []*types.Message{
		newStrategyTestMessage("test-topic", 0, 42, "payload"),
	}

	err := s.HandleError(context.Background(), msgs, assert.AnError)
	require.NoError(t, err)
}

// TestSkipStrategyName verifies the strategy name is "skip".
func TestSkipStrategyName(t *testing.T) {
	s := strategy.NewSkipStrategy(zerolog.Nop())
	assert.Equal(t, "skip", s.Name())
}

// TestSkipStrategyAlwaysContinues verifies skip never returns an error.
func TestSkipStrategyAlwaysContinues(t *testing.T) {
	s := strategy.NewSkipStrategy(zerolog.Nop())

	for i := 0; i < 10; i++ {
		msgs := []*types.Message{
			newStrategyTestMessage("topic", 0, int64(i), "msg"),
		}
		err := s.HandleError(context.Background(), msgs, assert.AnError)
		require.NoError(t, err, "skip should always return nil on attempt %d", i)
	}
}

// TestSkipStrategyWithMultipleMessages verifies skip handles batch failures.
func TestSkipStrategyWithMultipleMessages(t *testing.T) {
	s := strategy.NewSkipStrategy(zerolog.Nop())

	msgs := []*types.Message{
		newStrategyTestMessage("topic", 0, 0, "msg-1"),
		newStrategyTestMessage("topic", 0, 1, "msg-2"),
		newStrategyTestMessage("topic", 0, 2, "msg-3"),
	}

	err := s.HandleError(context.Background(), msgs, assert.AnError)
	require.NoError(t, err)
}

// TestSkipNopStrategy verifies the nop-logger variant works.
func TestSkipNopStrategy(t *testing.T) {
	s := strategy.NewSkipStrategy(zerolog.Nop())

	msgs := []*types.Message{
		newStrategyTestMessage("topic", 0, 0, "msg"),
	}

	err := s.HandleError(context.Background(), msgs, assert.AnError)
	require.NoError(t, err)
	assert.Equal(t, "skip", s.Name())
}
