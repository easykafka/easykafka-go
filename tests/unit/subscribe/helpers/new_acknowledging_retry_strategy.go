package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/internal/subscribe/strategy"
	"github.com/easykafka/easykafka-go/tests/unit/sharedhelpers"
)

// NewAcknowledgingRetryStrategy is NewRetryStrategyOnFake on a fake that
// acknowledges every record, as a healthy broker would. It returns the fake
// too, so a test can inspect what was written where.
//
// maxAttempts is passed to strategy.WithMaxAttempts: how many attempts in
// total a message gets, the first included, before HandleError sends it to the
// DLQ instead of the retry topic. 1 sends every failure straight to the DLQ.
func NewAcknowledgingRetryStrategy(
	t *testing.T, maxAttempts int, options ...strategy.RetryOption,
) (*strategy.RetryStrategy, *sharedhelpers.FakeProducer) {

	t.Helper()
	fake := sharedhelpers.NewFakeProducer()
	fake.AutoAcknowledge = true
	return NewRetryStrategyOnFake(t, fake, maxAttempts, options...), fake
}
