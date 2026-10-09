package subscribe

import (
	"github.com/easykafka/easykafka-go/internal/subscribe/strategy"
	"github.com/rs/zerolog"
)

// ErrStrategyInUse is returned by Start when its retry strategy is held by
// another running subscriber. Give each subscriber its own NewRetryStrategy.
var ErrStrategyInUse = strategy.ErrStrategyInUse

// Re-export retry option types for public API
type RetryOption = strategy.RetryOption

// NewRetryStrategy retries failed messages with Kafka-based retry topics and a DLQ.
// RetryTopic and DLQTopic are required options.
func NewRetryStrategy(options ...RetryOption) (ErrorStrategy, error) {
	return strategy.NewRetryStrategy(options...)
}

// NewSkipStrategy logs errors and continues consumption, committing offsets.
func NewSkipStrategy(logger zerolog.Logger) ErrorStrategy {
	return strategy.NewSkipStrategy(logger)
}

// NewFailFastStrategy stops the consumer immediately on any handler error.
func NewFailFastStrategy() ErrorStrategy {
	return strategy.NewFailFastStrategy()
}

// Re-export retry option constructors
var (
	WithRetryTopic        = strategy.WithRetryTopic
	WithDLQTopic          = strategy.WithDLQTopic
	WithMaxAttempts       = strategy.WithMaxAttempts
	WithInitialDelay      = strategy.WithInitialDelay
	WithMaxDelay          = strategy.WithMaxDelay
	WithBackoffMultiplier = strategy.WithBackoffMultiplier
	WithCustomBackoff     = strategy.WithCustomBackoff
	WithDeliveryErrorFunc = strategy.WithDeliveryErrorFunc
)
