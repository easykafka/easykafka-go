package easykafka

import (
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/rs/zerolog"
)

// Handler is a re-export from internal/types
type Handler = types.Handler

// BatchHandler is a re-export from internal/types
type BatchHandler = types.BatchHandler

// ErrorStrategy is a re-export from internal/types
type ErrorStrategy = types.ErrorStrategy

// Message is a re-export from internal/types
type Message = types.Message

// PayloadEncoding is a re-export from internal/types
type PayloadEncoding = types.PayloadEncoding

const (
	PayloadEncodingJSON   = types.PayloadEncodingJSON
	PayloadEncodingBase64 = types.PayloadEncodingBase64
)

// Re-export retry option types for public API
type RetryOption = strategy.RetryOption
type CircuitBreakerOption = strategy.CircuitBreakerOption

// ============================================================================
// PUBLIC ERROR STRATEGY CONSTRUCTORS
// ============================================================================

// NewRetryStrategy retries failed messages with Kafka-based retry topics and a DLQ.
// RetryTopic and DLQTopic are required options.
func NewRetryStrategy(options ...RetryOption) (ErrorStrategy, error) {
	return strategy.NewRetryStrategy(options...)
}

// NewCircuitBreakerStrategy pauses consumption after consecutive failures.
// Only supported in single-message mode.
func NewCircuitBreakerStrategy(options ...CircuitBreakerOption) (ErrorStrategy, error) {
	return strategy.NewCircuitBreakerStrategy(options...)
}

// NewSkipStrategy logs errors and continues consumption, committing offsets.
func NewSkipStrategy(logger zerolog.Logger) ErrorStrategy {
	return strategy.NewSkipStrategy(logger)
}

// NewFailFastStrategy stops the consumer immediately on any handler error.
func NewFailFastStrategy() ErrorStrategy {
	return strategy.NewFailFastStrategy()
}

// Re-export retry/circuit breaker option constructors
var (
	WithRetryTopic                   = strategy.WithRetryTopic
	WithDLQTopic                     = strategy.WithDLQTopic
	WithMaxAttempts                  = strategy.WithMaxAttempts
	WithInitialDelay                 = strategy.WithInitialDelay
	WithMaxDelay                     = strategy.WithMaxDelay
	WithBackoffMultiplier            = strategy.WithBackoffMultiplier
	WithCustomBackoff                = strategy.WithCustomBackoff
	WithFailedMessagePayloadEncoding = strategy.WithFailedMessagePayloadEncoding
	WithFailureThreshold             = strategy.WithFailureThreshold
	WithCooldownPeriod               = strategy.WithCooldownPeriod
	WithHalfOpenAttempts             = strategy.WithHalfOpenAttempts
	WithRetryOptions                 = strategy.WithRetryOptions
)
