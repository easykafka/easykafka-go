package helpers

import (
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/strategy"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
	"github.com/stretchr/testify/require"
)

// NewRetryStrategyOnFake builds a retry strategy writing to "test.retry" and
// "test.dlq" through fake, with maxAttempts and then options applied, and
// initializes it as a consumer would. The strategy and the fake are closed when
// the test ends.
//
// maxAttempts is passed to strategy.WithMaxAttempts: how many attempts in
// total a message gets, the first included, before HandleError sends it to the
// DLQ instead of the retry topic. 1 sends every failure straight to the DLQ.
func NewRetryStrategyOnFake(
	t *testing.T, fake *sharedhelpers.FakeProducer, maxAttempts int, options ...strategy.RetryOption,
) *strategy.RetryStrategy {

	t.Helper()
	all := append([]strategy.RetryOption{
		strategy.WithRetryTopic("test.retry"),
		strategy.WithDLQTopic("test.dlq"),
		strategy.WithMaxAttempts(maxAttempts),
		strategy.WithInitialDelay(1 * time.Second),
		strategy.WithMaxDelay(30 * time.Second),
		strategy.WithBackoffMultiplier(2.0),
		strategy.WithProducerFactory(fake.Factory()),
	}, options...)
	s, err := strategy.NewRetryStrategy(all...)
	require.NoError(t, err)

	t.Cleanup(fake.Close) // runs after the strategy's Close below
	require.NoError(t, s.Initialize(types.InitConfig{Brokers: []string{sharedhelpers.PublishBroker}, Logger: sharedhelpers.TestLogger()}))
	t.Cleanup(func() { _ = s.Close() })
	return s
}
