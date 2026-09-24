package helpers

import (
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/stretchr/testify/require"
)

// NewRetryStrategy builds a retry strategy over the given topics with a 10ms
// initial delay, so tests that walk the retry ladder do not wait on backoff.
func NewRetryStrategy(t *testing.T, retryTopic, dlqTopic string, maxAttempts int) easykafka.ErrorStrategy {
	t.Helper()
	s, err := easykafka.NewRetryStrategy(
		easykafka.WithRetryTopic(retryTopic),
		easykafka.WithDLQTopic(dlqTopic),
		easykafka.WithMaxAttempts(maxAttempts),
		easykafka.WithInitialDelay(10*time.Millisecond),
	)
	require.NoError(t, err)
	return s
}
