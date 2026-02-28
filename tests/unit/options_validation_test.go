package unit

import (
	"context"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// T023 [US2] Unit tests for option validation
// =============================================================================

// =============================================================================
// Helpers
// =============================================================================

func noopHandler(ctx context.Context, payload []byte) error {
	return nil
}

func noopBatchHandler(ctx context.Context, payloads [][]byte) error {
	return nil
}

// TestNewRequiresTopicOption verifies that New() fails when WithTopic is missing.
func TestNewRequiresTopicOption(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "topic")
}

// TestNewRequiresBrokersOption verifies that New() fails when WithBrokers is missing.
func TestNewRequiresBrokersOption(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker")
}

// TestNewRequiresConsumerGroupOption verifies that New() fails when WithConsumerGroup is missing.
func TestNewRequiresConsumerGroupOption(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithHandler(noopHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer group")
}

// TestNewRequiresHandlerOption verifies that New() fails when neither handler is set.
func TestNewRequiresHandlerOption(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Handler")
}

// TestNewRejectsBothHandlers verifies that New() fails when both handler types are set.
func TestNewRejectsBothHandlers(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithBatchHandler(noopBatchHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one")
}

// TestWithTopicRejectsEmpty verifies that WithTopic rejects an empty topic.
func TestWithTopicRejectsEmpty(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic(""),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// TestWithBrokersRejectsEmpty verifies that WithBrokers rejects empty broker list.
func TestWithBrokersRejectsEmpty(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers(),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker")
}

// TestWithBrokersRejectsEmptyStrings verifies that WithBrokers rejects empty strings in broker list.
func TestWithBrokersRejectsEmptyStrings(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092", ""),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker")
}

// TestWithConsumerGroupRejectsEmpty verifies that WithConsumerGroup rejects an empty group ID.
func TestWithConsumerGroupRejectsEmpty(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup(""),
		easykafka.WithHandler(noopHandler),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// TestWithHandlerRejectsNil verifies that WithHandler rejects a nil handler.
func TestWithHandlerRejectsNil(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(nil),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// TestWithBatchHandlerRejectsNil verifies that WithBatchHandler rejects a nil handler.
func TestWithBatchHandlerRejectsNil(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithBatchHandler(nil),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// TestWithErrorStrategyRejectsNil verifies that WithErrorStrategy rejects nil strategy.
func TestWithErrorStrategyRejectsNil(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithErrorStrategy(nil),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// TestWithBatchSizeRejectsZero verifies that WithBatchSize rejects zero.
func TestWithBatchSizeRejectsZero(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithBatchHandler(noopBatchHandler),
		easykafka.WithBatchSize(0),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch size")
}

// TestWithBatchSizeRejectsNegative verifies that WithBatchSize rejects negative values.
func TestWithBatchSizeRejectsNegative(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithBatchHandler(noopBatchHandler),
		easykafka.WithBatchSize(-1),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch size")
}

// TestWithBatchTimeoutRejectsZero verifies that WithBatchTimeout rejects zero.
func TestWithBatchTimeoutRejectsZero(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithBatchHandler(noopBatchHandler),
		easykafka.WithBatchTimeout(0),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch timeout")
}

// TestWithPollTimeoutRejectsTooSmall verifies that WithPollTimeout rejects values under 10ms.
func TestWithPollTimeoutRejectsTooSmall(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithPollTimeout(1*time.Millisecond),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll timeout")
}

// TestWithShutdownTimeoutRejectsZero verifies that WithShutdownTimeout rejects zero.
func TestWithShutdownTimeoutRejectsZero(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithShutdownTimeout(0),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown timeout")
}

// TestWithKafkaConfigRejectsNil verifies that WithKafkaConfig rejects nil config.
func TestWithKafkaConfigRejectsNil(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithKafkaConfig(nil),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// TestWithKafkaConfigRejectsManagedKeys verifies that WithKafkaConfig rejects
// keys that are managed by the library (bootstrap.servers, group.id, enable.auto.commit).
func TestWithKafkaConfigRejectsManagedKeys(t *testing.T) {
	managedKeys := []struct {
		key  string
		desc string
	}{
		{"bootstrap.servers", "bootstrap.servers is managed by WithBrokers"},
		{"group.id", "group.id is managed by WithConsumerGroup"},
		{"enable.auto.commit", "enable.auto.commit is managed by the library"},
	}

	for _, tc := range managedKeys {
		t.Run(tc.key, func(t *testing.T) {
			_, err := easykafka.New(
				easykafka.WithTopic("test-topic"),
				easykafka.WithBrokers("localhost:9092"),
				easykafka.WithConsumerGroup("test-group"),
				easykafka.WithHandler(noopHandler),
				easykafka.WithKafkaConfig(map[string]any{
					tc.key: "override-value",
				}),
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "managed")
		})
	}
}

// =============================================================================
// Defaults tests
// =============================================================================

// TestNewAppliesPollTimeoutDefault verifies that PollTimeout defaults to 100ms.
func TestNewAppliesPollTimeoutDefault(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.NoError(t, err)
	require.NotNil(t, consumer)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, 100*time.Millisecond, cfg.PollTimeout)
}

// TestNewAppliesShutdownTimeoutDefault verifies that ShutdownTimeout defaults to 30s.
func TestNewAppliesShutdownTimeoutDefault(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.NoError(t, err)
	require.NotNil(t, consumer)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, 30*time.Second, cfg.ShutdownTimeout)
}

// TestNewAppliesDefaultErrorStrategy verifies that a default error strategy is set.
func TestNewAppliesDefaultErrorStrategy(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.NoError(t, err)
	require.NotNil(t, consumer)

	cfg := easykafka.GetConfig(consumer)
	require.NotNil(t, cfg.ErrorStrategy)
	assert.Equal(t, "skip", cfg.ErrorStrategy.Name())
}

// TestNewInitializesKafkaConfigMap verifies that KafkaConfig becomes a non-nil map.
func TestNewInitializesKafkaConfigMap(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.NoError(t, err)
	require.NotNil(t, consumer)

	cfg := easykafka.GetConfig(consumer)
	assert.NotNil(t, cfg.KafkaConfig)
}

// =============================================================================
// Custom option values tests
// =============================================================================

// TestWithPollTimeoutCustomValue verifies custom poll timeout is honored.
func TestWithPollTimeoutCustomValue(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithPollTimeout(250*time.Millisecond),
	)
	require.NoError(t, err)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, 250*time.Millisecond, cfg.PollTimeout)
}

// TestWithShutdownTimeoutCustomValue verifies custom shutdown timeout is honored.
func TestWithShutdownTimeoutCustomValue(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithShutdownTimeout(10*time.Second),
	)
	require.NoError(t, err)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, 10*time.Second, cfg.ShutdownTimeout)
}

// TestWithBatchHandlerSetsDefaults verifies that batch handler sets default batch size/timeout.
func TestWithBatchHandlerSetsDefaults(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithBatchHandler(noopBatchHandler),
	)
	require.NoError(t, err)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, easykafka.ModeBatch, cfg.Mode)
	assert.Equal(t, 100, cfg.BatchSize)
	assert.Equal(t, 5*time.Second, cfg.BatchTimeout)
}

// TestWithBatchHandlerCustomBatchSize verifies that custom batch size overrides default.
func TestWithBatchHandlerCustomBatchSize(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithBatchHandler(noopBatchHandler),
		easykafka.WithBatchSize(50),
	)
	require.NoError(t, err)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, 50, cfg.BatchSize)
}

// TestWithKafkaConfigPassthroughValues verifies that KafkaConfig values are stored.
func TestWithKafkaConfigPassthroughValues(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms":   30000,
			"max.poll.interval.ms": 300000,
			"auto.offset.reset":    "latest",
		}),
	)
	require.NoError(t, err)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, 30000, cfg.KafkaConfig["session.timeout.ms"])
	assert.Equal(t, 300000, cfg.KafkaConfig["max.poll.interval.ms"])
	assert.Equal(t, "latest", cfg.KafkaConfig["auto.offset.reset"])
}

// TestWithMultipleBrokers verifies that multiple brokers are accepted.
func TestWithMultipleBrokers(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("broker1:9092", "broker2:9092", "broker3:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.NoError(t, err)

	cfg := easykafka.GetConfig(consumer)
	assert.Equal(t, []string{"broker1:9092", "broker2:9092", "broker3:9092"}, cfg.Brokers)
}

// TestNewSuccessfulMinimalConfig verifies the minimum required configuration works.
func TestNewSuccessfulMinimalConfig(t *testing.T) {
	consumer, err := easykafka.New(
		easykafka.WithTopic("test-topic"),
		easykafka.WithBrokers("localhost:9092"),
		easykafka.WithConsumerGroup("test-group"),
		easykafka.WithHandler(noopHandler),
	)
	require.NoError(t, err)
	require.NotNil(t, consumer)
}

// TestOptionErrorWrapping verifies that option errors are wrapped with context.
func TestOptionErrorWrapping(t *testing.T) {
	_, err := easykafka.New(
		easykafka.WithTopic(""),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid option")
}
