package helpers

import (
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/subscribe"
	"github.com/stretchr/testify/require"
)

// NewRetryStrategy builds a retry strategy over the given topics with a 10ms
// initial delay, so tests that walk the retry ladder do not wait on backoff.
func NewRetryStrategy(t *testing.T, retryTopic, dlqTopic string, maxAttempts int) subscribe.ErrorStrategy {
	t.Helper()
	s, err := subscribe.NewRetryStrategy(
		subscribe.WithRetryTopic(retryTopic),
		subscribe.WithDLQTopic(dlqTopic),
		subscribe.WithMaxAttempts(maxAttempts),
		subscribe.WithInitialDelay(10*time.Millisecond),
	)
	require.NoError(t, err)
	return s
}
