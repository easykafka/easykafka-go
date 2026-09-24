package helpers

import (
	"time"

	"github.com/easykafka/easykafka-go/strategy"
	"github.com/rs/zerolog"
)

// NewRetryStrategyWithMocks builds an initialized retry strategy writing to
// "test.retry" and "test.dlq" through two MockProducers, and returns both so a
// test can inspect what was written where.
func NewRetryStrategyWithMocks(maxAttempts int) (*strategy.RetryStrategy, *MockProducer, *MockProducer) {
	retryProd := &MockProducer{}
	dlqProd := &MockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:   "test.retry",
		DLQTopic:     "test.dlq",
		MaxAttempts:  maxAttempts,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())
	return s, retryProd, dlqProd
}
