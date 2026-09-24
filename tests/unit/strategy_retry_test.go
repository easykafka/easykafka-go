package unit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// T029 [US3] Unit tests for retry headers/backoff and strategy behavior
// =============================================================================

// =============================================================================
// Header Encoding Tests
// =============================================================================

func TestBuildRetryHeadersSetsAllFields(t *testing.T) {
	msg := &types.Message{
		Topic:     "orders",
		Partition: 3,
		Offset:    12345,
		Headers:   map[string]string{"app-key": "app-value"},
		Payload:   []byte("test"),
	}
	retryTime := time.Date(2026, 2, 9, 10, 35, 0, 0, time.UTC)
	handlerErr := errors.New("connection timeout")

	headers := metadata.BuildRetryHeaders(msg, 2, retryTime, types.Failure{Err: handlerErr})

	assert.Equal(t, "2", headers[metadata.HeaderRetryAttempt])
	assert.Equal(t, "2026-02-09T10:35:00Z", headers[metadata.HeaderRetryTime])
	// A first delivery with no step reported has no step to carry, so none is
	// written; GetRetryStep still reads 0.
	assert.NotContains(t, headers, metadata.HeaderRetryStep)
	assert.Equal(t, "HANDLER_ERROR", headers[metadata.HeaderErrorCode])
	assert.Equal(t, "connection timeout", headers[metadata.HeaderErrorMessage])
	assert.Equal(t, "orders", headers[metadata.HeaderOriginalTopic])
	assert.Equal(t, "3", headers[metadata.HeaderOriginalPartition])
	assert.Equal(t, "12345", headers[metadata.HeaderOriginalOffset])
	assert.NotEmpty(t, headers[metadata.HeaderFailedAt])
	// Application headers should be preserved
	assert.Equal(t, "app-value", headers["app-key"])
}

func TestBuildRetryHeadersPreservesOriginalTopic(t *testing.T) {
	msg := &types.Message{
		Topic:     "orders.retry",
		Partition: 0,
		Offset:    99,
		Headers: map[string]string{
			metadata.HeaderOriginalTopic: "orders",
		},
		Payload: []byte("test"),
	}

	headers := metadata.BuildRetryHeaders(msg, 3, time.Now(), types.Failure{Err: errors.New("err")})
	assert.Equal(t, "orders", headers[metadata.HeaderOriginalTopic])
}

// TestBuildRetryHeadersWritesStepAndCode verifies the two values a handler owns
// reach the record.
func TestBuildRetryHeadersWritesStepAndCode(t *testing.T) {
	msg := &types.Message{Topic: "orders", Headers: map[string]string{}, Payload: []byte("test")}

	f := types.Failure{Err: errors.New("publish failed"), Step: 2, Code: "publish_failed"}
	headers := metadata.BuildRetryHeaders(msg, 1, time.Now(), f)

	assert.Equal(t, "2", headers[metadata.HeaderRetryStep])
	assert.Equal(t, "publish_failed", headers[metadata.HeaderErrorCode])
	assert.Equal(t, "publish failed", headers[metadata.HeaderErrorMessage])
}

// TestBuildHeadersStepCarriesForwardCodeDoesNot is the variant in the design:
// a failure with no step and no code keeps the step the record arrived with,
// and writes the library's fallback code rather than the inbound one.
func TestBuildHeadersStepCarriesForwardCodeDoesNot(t *testing.T) {
	msg := &types.Message{
		Topic: "orders.retry",
		Headers: map[string]string{
			metadata.HeaderRetryAttempt:  "2",
			metadata.HeaderRetryStep:     "2",
			metadata.HeaderErrorCode:     "publish_failed",
			metadata.HeaderOriginalTopic: "orders",
		},
		Payload: []byte("test"),
	}
	f := types.Failure{Err: errors.New("unclassified")}

	for name, headers := range map[string]map[string]string{
		"retry": metadata.BuildRetryHeaders(msg, 3, time.Now(), f),
		"dlq":   metadata.BuildDLQHeaders(msg, 3, f),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, "2", headers[metadata.HeaderRetryStep], "an unset step carries the inbound one forward")
			assert.Equal(t, "HANDLER_ERROR", headers[metadata.HeaderErrorCode], "an unset code must not repeat the inbound one")
		})
	}
}

// TestBuildHeadersReportedStepReplacesInbound verifies a step the handler reports
// wins over the one the record arrived with.
func TestBuildHeadersReportedStepReplacesInbound(t *testing.T) {
	msg := &types.Message{
		Topic:   "orders.retry",
		Headers: map[string]string{metadata.HeaderRetryStep: "1"},
		Payload: []byte("test"),
	}

	headers := metadata.BuildRetryHeaders(msg, 2, time.Now(), types.Failure{Err: errors.New("x"), Step: 2})
	assert.Equal(t, "2", headers[metadata.HeaderRetryStep])
}

// TestBuildHeadersPassApplicationHeadersThrough verifies headers that are not
// the library's reach the retry and DLQ record unchanged, while inbound library
// headers are replaced rather than copied.
func TestBuildHeadersPassApplicationHeadersThrough(t *testing.T) {
	msg := &types.Message{
		Topic: "orders.retry",
		Headers: map[string]string{
			"trace-id":                       "abc-123",
			"tenant":                         "hr",
			metadata.HeaderRetryAttempt:      "1",
			metadata.HeaderErrorMessage:      "the previous failure",
			metadata.HeaderRetryTime:         "2026-01-01T00:00:00Z",
			metadata.HeaderOriginalTopic:     "orders",
			metadata.HeaderOriginalOffset:    "7",
			metadata.HeaderOriginalPartition: "1",
		},
		Payload: []byte("test"),
	}
	f := types.Failure{Err: errors.New("this failure")}

	for name, headers := range map[string]map[string]string{
		"retry": metadata.BuildRetryHeaders(msg, 2, time.Now(), f),
		"dlq":   metadata.BuildDLQHeaders(msg, 2, f),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, "abc-123", headers["trace-id"])
			assert.Equal(t, "hr", headers["tenant"])
			assert.Equal(t, "2", headers[metadata.HeaderRetryAttempt])
			assert.Equal(t, "this failure", headers[metadata.HeaderErrorMessage])
		})
	}
}

func TestGetRetryAttemptFromHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		expected int
	}{
		{"no headers", nil, 0},
		{"empty headers", map[string]string{}, 0},
		{"attempt 1", map[string]string{metadata.HeaderRetryAttempt: "1"}, 1},
		{"attempt 3", map[string]string{metadata.HeaderRetryAttempt: "3"}, 3},
		{"invalid", map[string]string{metadata.HeaderRetryAttempt: "abc"}, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := &types.Message{Headers: tc.headers}
			assert.Equal(t, tc.expected, metadata.GetRetryAttempt(msg))
		})
	}
}

func TestGetRetryTimeFromHeaders(t *testing.T) {
	msg := &types.Message{
		Headers: map[string]string{
			metadata.HeaderRetryTime: "2026-02-09T10:35:00Z",
		},
	}

	rt := metadata.GetRetryTime(msg)
	assert.Equal(t, 2026, rt.Year())
	assert.Equal(t, time.Month(2), rt.Month())
	assert.Equal(t, 9, rt.Day())
}

func TestGetRetryTimeNilMessage(t *testing.T) {
	assert.True(t, metadata.GetRetryTime(nil).IsZero())
}

func TestGetRetryStepFromHeaders(t *testing.T) {
	msg := &types.Message{
		Headers: map[string]string{
			metadata.HeaderRetryStep: "2",
		},
	}
	assert.Equal(t, int32(2), metadata.GetRetryStep(msg))
}

// =============================================================================
// Retry Strategy Configuration Tests
// =============================================================================

func TestNewRetryStrategyRequiresRetryTopic(t *testing.T) {
	_, err := strategy.NewRetryStrategy(
		strategy.WithDLQTopic("orders.dlq"),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry topic")
}

func TestNewRetryStrategyRequiresDLQTopic(t *testing.T) {
	_, err := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("orders.retry"),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DLQ topic")
}

func TestNewRetryStrategyDefaults(t *testing.T) {
	s, err := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("orders.retry"),
		strategy.WithDLQTopic("orders.dlq"),
	)
	require.NoError(t, err)
	assert.Equal(t, "retry", s.Name())

	cfg := s.Config()
	assert.Equal(t, 3, cfg.MaxAttempts)
	assert.Equal(t, time.Second, cfg.InitialDelay)
	assert.Equal(t, 30*time.Second, cfg.MaxDelay)
	assert.InEpsilon(t, 2.0, cfg.Multiplier, 1e-9)
}

func TestNewRetryStrategyCustomOptions(t *testing.T) {
	s, err := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("my.retry"),
		strategy.WithDLQTopic("my.dlq"),
		strategy.WithMaxAttempts(5),
		strategy.WithInitialDelay(2*time.Second),
		strategy.WithMaxDelay(60*time.Second),
		strategy.WithBackoffMultiplier(3.0),
	)
	require.NoError(t, err)

	cfg := s.Config()
	assert.Equal(t, "my.retry", cfg.RetryTopic)
	assert.Equal(t, "my.dlq", cfg.DLQTopic)
	assert.Equal(t, 5, cfg.MaxAttempts)
	assert.Equal(t, 2*time.Second, cfg.InitialDelay)
	assert.Equal(t, 60*time.Second, cfg.MaxDelay)
	assert.InEpsilon(t, 3.0, cfg.Multiplier, 1e-9)
}

func TestRetryOptionValidation(t *testing.T) {
	tests := []struct {
		name        string
		opt         strategy.RetryOption
		errContains string
	}{
		{"empty retry topic", strategy.WithRetryTopic(""), "retry topic cannot be empty"},
		{"empty DLQ topic", strategy.WithDLQTopic(""), "DLQ topic cannot be empty"},
		{"zero max attempts", strategy.WithMaxAttempts(0), "max attempts must be positive"},
		{"negative max attempts", strategy.WithMaxAttempts(-1), "max attempts must be positive"},
		{"zero initial delay", strategy.WithInitialDelay(0), "initial delay must be positive"},
		{"zero max delay", strategy.WithMaxDelay(0), "max delay must be positive"},
		{"multiplier < 1", strategy.WithBackoffMultiplier(0.5), "backoff multiplier must be >= 1.0"},
		{"nil custom backoff", strategy.WithCustomBackoff(nil), "custom backoff function cannot be nil"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := strategy.NewRetryStrategy(
				strategy.WithRetryTopic("t.retry"),
				strategy.WithDLQTopic("t.dlq"),
				tc.opt,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errContains)
		})
	}
}

// =============================================================================
// Retry Strategy HandleError Tests (with mock producers)
// =============================================================================

func TestRetryStrategySendsToRetryQueueOnFirstFailure(t *testing.T) {
	s, retryProd, dlqProd := helpers.NewRetryStrategyWithMocks(3)
	ctx := context.Background()

	msg := &types.Message{
		Topic:     "orders",
		Partition: 0,
		Offset:    100,
		Headers:   make(map[string]string),
		Payload:   []byte(`{"orderId":"12345"}`),
	}

	err := s.HandleError(ctx, []*types.Message{msg}, types.Failure{Err: errors.New("db connection failed")})
	require.NoError(t, err, "retry should return nil to continue consumption")

	// Should be sent to retry queue
	retryMsgs := retryProd.Messages()
	require.Len(t, retryMsgs, 1)
	assert.Equal(t, "test.retry", retryMsgs[0].Topic)
	assert.JSONEq(t, `{"orderId":"12345"}`, string(retryMsgs[0].Value))

	// Verify headers
	assert.Equal(t, "1", retryMsgs[0].Headers[metadata.HeaderRetryAttempt])
	assert.NotEmpty(t, retryMsgs[0].Headers[metadata.HeaderRetryTime])
	assert.Equal(t, "orders", retryMsgs[0].Headers[metadata.HeaderOriginalTopic])
	assert.Equal(t, "db connection failed", retryMsgs[0].Headers[metadata.HeaderErrorMessage])

	// DLQ should be empty
	assert.Empty(t, dlqProd.Messages())
}

func TestRetryStrategySendsToDLQAfterMaxAttempts(t *testing.T) {
	s, retryProd, dlqProd := helpers.NewRetryStrategyWithMocks(3)
	ctx := context.Background()

	// Simulate a message that has already been retried twice (attempt=2). Its
	// position on the retry topic differs from the source's in both partition
	// and offset, so the assertions below can tell which one was written.
	msg := &types.Message{
		Topic:     "test.retry",
		Partition: 1,
		Offset:    50,
		Headers: map[string]string{
			metadata.HeaderRetryAttempt:      "2",
			metadata.HeaderOriginalTopic:     "orders",
			metadata.HeaderOriginalPartition: "0",
			metadata.HeaderOriginalOffset:    "100",
		},
		Payload: []byte(`{"orderId":"12345"}`),
	}

	err := s.HandleError(ctx, []*types.Message{msg}, types.Failure{Err: errors.New("still failing")})
	require.NoError(t, err, "should continue after sending to DLQ")

	// Retry queue should be empty (max attempts reached)
	assert.Empty(t, retryProd.Messages())

	// DLQ should have the message
	dlqMsgs := dlqProd.Messages()
	require.Len(t, dlqMsgs, 1)
	assert.Equal(t, "test.dlq", dlqMsgs[0].Topic)

	// The DLQ record is the consumed message; the failure is in headers.
	assert.Equal(t, msg.Payload, dlqMsgs[0].Value)
	headers := dlqMsgs[0].Headers
	assert.Equal(t, "orders", headers[metadata.HeaderOriginalTopic])
	assert.Equal(t, "0", headers[metadata.HeaderOriginalPartition], "must name the source partition, not the retry topic's")
	assert.Equal(t, "100", headers[metadata.HeaderOriginalOffset], "must name the source offset, not the retry topic's")
	assert.Equal(t, "3", headers[metadata.HeaderRetryAttempt])
	assert.Equal(t, "still failing", headers[metadata.HeaderErrorMessage])
}

func TestRetryStrategyHandlesMultipleMessages(t *testing.T) {
	s, retryProd, _ := helpers.NewRetryStrategyWithMocks(3)
	ctx := context.Background()

	msgs := []*types.Message{
		{Topic: "orders", Partition: 0, Offset: 100, Headers: make(map[string]string), Payload: []byte("msg-1")},
		{Topic: "orders", Partition: 0, Offset: 101, Headers: make(map[string]string), Payload: []byte("msg-2")},
		{Topic: "orders", Partition: 0, Offset: 102, Headers: make(map[string]string), Payload: []byte("msg-3")},
	}

	err := s.HandleError(ctx, msgs, types.Failure{Err: errors.New("batch failed")})
	require.NoError(t, err)

	// Each message should be sent individually to retry queue
	retryMsgs := retryProd.Messages()
	require.Len(t, retryMsgs, 3)
	assert.Equal(t, []byte("msg-1"), retryMsgs[0].Value)
	assert.Equal(t, []byte("msg-2"), retryMsgs[1].Value)
	assert.Equal(t, []byte("msg-3"), retryMsgs[2].Value)
}

func TestRetryStrategyReturnsFatalOnProducerError(t *testing.T) {
	retryProd := &helpers.MockProducer{Err: errors.New("kafka unavailable")}
	dlqProd := &helpers.MockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:   "test.retry",
		DLQTopic:     "test.dlq",
		MaxAttempts:  3,
		InitialDelay: time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("handler err")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry queue write failed")
}

func TestRetryStrategyDLQProducerError(t *testing.T) {
	retryProd := &helpers.MockProducer{}
	dlqProd := &helpers.MockProducer{Err: errors.New("dlq unavailable")}
	cfg := strategy.RetryConfig{
		RetryTopic:   "test.retry",
		DLQTopic:     "test.dlq",
		MaxAttempts:  1, // Immediately to DLQ
		InitialDelay: time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("handler err")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DLQ write failed")
}

// =============================================================================
// Retry Strategy Interface Compliance
// =============================================================================

func TestRetryImplementsErrorStrategy(t *testing.T) {
	s, _ := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("t.retry"),
		strategy.WithDLQTopic("t.dlq"),
	)
	var _ types.ErrorStrategy = s
}

func TestRetryImplementsInitializable(t *testing.T) {
	s, _ := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("t.retry"),
		strategy.WithDLQTopic("t.dlq"),
	)
	var _ types.Initializable = s
}

func TestRetryStrategyNotInitializedError(t *testing.T) {
	s, _ := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("t.retry"),
		strategy.WithDLQTopic("t.dlq"),
	)

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("err")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not initialized")
}

// =============================================================================
// Backoff Calculation Tests
// =============================================================================

func TestExponentialBackoff(t *testing.T) {
	s, retryProd, _ := helpers.NewRetryStrategyWithMocks(10)
	ctx := context.Background()

	// First failure -> attempt 1 -> delay = 1s * 2^0 = 1s
	msg1 := &types.Message{Topic: "t", Headers: make(map[string]string), Payload: []byte("m")}
	_ = s.HandleError(ctx, []*types.Message{msg1}, types.Failure{Err: errors.New("err")})

	retryMsgs := retryProd.Messages()
	require.Len(t, retryMsgs, 1)
	rt1 := retryMsgs[0].Headers[metadata.HeaderRetryTime]
	parsedTime1, err := time.Parse(time.RFC3339, rt1)
	require.NoError(t, err)

	// Retry time should be in the future (within ~5s window to account for slow CI)
	assert.True(t, parsedTime1.After(time.Now().Add(-1*time.Second)), "retry time should be roughly in the future")
	assert.True(t, parsedTime1.Before(time.Now().Add(5*time.Second)), "retry time should be within 5s of now")

	// Second failure (attempt 2) -> delay = 1s * 2^1 = 2s
	msg2 := &types.Message{
		Topic:   "t.retry",
		Headers: map[string]string{metadata.HeaderRetryAttempt: "1"},
		Payload: []byte("m"),
	}
	_ = s.HandleError(ctx, []*types.Message{msg2}, types.Failure{Err: errors.New("err")})

	retryMsgs = retryProd.Messages()
	require.Len(t, retryMsgs, 2)
	rt2 := retryMsgs[1].Headers[metadata.HeaderRetryTime]
	parsedTime2, err := time.Parse(time.RFC3339, rt2)
	require.NoError(t, err)

	// Second retry should be further out than the first (exponential growth)
	assert.True(t, parsedTime2.After(parsedTime1), "second retry should be later than first")
}

func TestCustomBackoff(t *testing.T) {
	customFn := func(attempt int) time.Duration {
		return time.Duration(attempt) * 10 * time.Second
	}

	s, err := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("t.retry"),
		strategy.WithDLQTopic("t.dlq"),
		strategy.WithCustomBackoff(customFn),
	)
	require.NoError(t, err)

	cfg := s.Config()
	assert.NotNil(t, cfg.CustomBackoff)
	// Custom backoff: attempt 1 = 10s, attempt 2 = 20s, etc.
	assert.Equal(t, 10*time.Second, cfg.CustomBackoff(1))
	assert.Equal(t, 20*time.Second, cfg.CustomBackoff(2))
}

func TestMaxDelayCappedBackoff(t *testing.T) {
	retryProd := &helpers.MockProducer{}
	dlqProd := &helpers.MockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:   "test.retry",
		DLQTopic:     "test.dlq",
		MaxAttempts:  20,
		InitialDelay: 1 * time.Second,
		MaxDelay:     5 * time.Second, // Low cap
		Multiplier:   10.0,            // Aggressive multiplier
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	// High attempt should be capped at MaxDelay
	msg := &types.Message{
		Topic:   "t.retry",
		Headers: map[string]string{metadata.HeaderRetryAttempt: "5"},
		Payload: []byte("m"),
	}
	_ = s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("err")})

	retryMsgs := retryProd.Messages()
	require.Len(t, retryMsgs, 1)
	rt := retryMsgs[0].Headers[metadata.HeaderRetryTime]
	parsedTime, err := time.Parse(time.RFC3339, rt)
	require.NoError(t, err)

	// Should be now + maxDelay (5s), not some huge value
	maxExpected := time.Now().Add(6 * time.Second)
	assert.True(t, parsedTime.Before(maxExpected), "retry time should be capped at MaxDelay")
}

// =============================================================================
// DLQ Record Tests
// =============================================================================

// TestDLQRecordIsTheConsumedMessage verifies a DLQ record carries the consumed
// payload byte for byte — binary included — and the consumed key, with the
// original position, the error, the attempt count and the failure time in
// headers.
func TestDLQRecordIsTheConsumedMessage(t *testing.T) {
	s, _, dlqProd := helpers.NewRetryStrategyWithMocks(1) // straight to the DLQ

	payload := []byte{0x00, 0xFF, 0xFE, '{', 0x80} // not valid UTF-8
	msg := &types.Message{
		Topic:     "orders",
		Partition: 2,
		Offset:    999,
		Key:       []byte("order-42"),
		Headers:   make(map[string]string),
		Payload:   payload,
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("processing failed")})
	require.NoError(t, err)

	dlqMsgs := dlqProd.Messages()
	require.Len(t, dlqMsgs, 1)
	assert.Equal(t, payload, dlqMsgs[0].Value)
	assert.Equal(t, []byte("order-42"), dlqMsgs[0].Key)

	headers := dlqMsgs[0].Headers
	assert.Equal(t, "orders", headers[metadata.HeaderOriginalTopic])
	assert.Equal(t, "2", headers[metadata.HeaderOriginalPartition])
	assert.Equal(t, "999", headers[metadata.HeaderOriginalOffset])
	assert.Equal(t, "processing failed", headers[metadata.HeaderErrorMessage])
	assert.Equal(t, "1", headers[metadata.HeaderRetryAttempt])
	assert.NotEmpty(t, headers[metadata.HeaderFailedAt])
}

// TestRetryRecordKeepsTheKey verifies a retry record is keyed like its source.
func TestRetryRecordKeepsTheKey(t *testing.T) {
	s, retryProd, _ := helpers.NewRetryStrategyWithMocks(3)

	msg := &types.Message{Topic: "orders", Key: []byte("order-42"), Headers: map[string]string{}, Payload: []byte("m")}
	require.NoError(t, s.HandleError(context.Background(), []*types.Message{msg}, types.Failure{Err: errors.New("x")}))

	retryMsgs := retryProd.Messages()
	require.Len(t, retryMsgs, 1)
	assert.Equal(t, []byte("order-42"), retryMsgs[0].Key)
}

// =============================================================================
// Permanent Failures, Steps and Codes
// =============================================================================

// TestPermanentFailureGoesStraightToDLQ verifies ErrPermanent skips the retry
// ladder on the very first attempt.
func TestPermanentFailureGoesStraightToDLQ(t *testing.T) {
	s, retryProd, dlqProd := helpers.NewRetryStrategyWithMocks(10)

	msg := &types.Message{Topic: "orders", Headers: map[string]string{}, Payload: []byte("not json")}
	f := types.Failure{Err: fmt.Errorf("%w: unmarshal: bad input", types.ErrPermanent)}

	require.NoError(t, s.HandleError(context.Background(), []*types.Message{msg}, f))

	assert.Empty(t, retryProd.Messages(), "a permanent failure must not be retried")
	dlqMsgs := dlqProd.Messages()
	require.Len(t, dlqMsgs, 1)
	assert.Equal(t, "1", dlqMsgs[0].Headers[metadata.HeaderRetryAttempt], "the attempt count is left as it is")
}

// TestPermanentMarkerLostToPercentV pins the trap the design warns about: %v
// drops the marker, so the message walks the retry ladder after all.
func TestPermanentMarkerLostToPercentV(t *testing.T) {
	s, retryProd, dlqProd := helpers.NewRetryStrategyWithMocks(10)

	msg := &types.Message{Topic: "orders", Headers: map[string]string{}, Payload: []byte("not json")}
	f := types.Failure{Err: fmt.Errorf("%v: unmarshal: bad input", types.ErrPermanent)}

	require.NoError(t, s.HandleError(context.Background(), []*types.Message{msg}, f))

	assert.Len(t, retryProd.Messages(), 1)
	assert.Empty(t, dlqProd.Messages())
}

// TestStepAndCodeReachRetryAndDLQRecords verifies Failure.Step and Failure.Code
// appear on both record kinds and read back through the public accessors.
func TestStepAndCodeReachRetryAndDLQRecords(t *testing.T) {
	f := types.Failure{Err: errors.New("publish failed"), Step: 2, Code: "publish_failed"}

	cases := []struct {
		name    string
		inbound string // attempt the record arrived with
		toDLQ   bool
	}{
		{"retry", "0", false},
		{"dlq", "2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, retryProd, dlqProd := helpers.NewRetryStrategyWithMocks(3)
			msg := &types.Message{
				Topic:   "orders",
				Headers: map[string]string{metadata.HeaderRetryAttempt: tc.inbound},
				Payload: []byte("m"),
			}
			require.NoError(t, s.HandleError(context.Background(), []*types.Message{msg}, f))

			produced := retryProd.Messages()
			if tc.toDLQ {
				assert.Empty(t, produced)
				produced = dlqProd.Messages()
			}
			require.Len(t, produced, 1)

			republished := &types.Message{Headers: produced[0].Headers}
			assert.Equal(t, int32(2), easykafka.GetRetryStep(republished))
			assert.Equal(t, "publish_failed", easykafka.GetErrorCode(republished))
		})
	}
}

// TestBatchLevelFailureKeepsEachRecordsStep verifies that a failure shared by
// several messages keeps each record's own inbound step and writes the
// fallback code for all of them.
func TestBatchLevelFailureKeepsEachRecordsStep(t *testing.T) {
	s, retryProd, _ := helpers.NewRetryStrategyWithMocks(10)

	msgs := []*types.Message{
		{Topic: "orders", Headers: map[string]string{}, Payload: []byte("fresh")},
		{Topic: "orders", Headers: map[string]string{
			metadata.HeaderRetryAttempt: "1", metadata.HeaderRetryStep: "2", metadata.HeaderErrorCode: "publish_failed",
		}, Payload: []byte("resumed")},
	}
	require.NoError(t, s.HandleError(context.Background(), msgs, types.Failure{Err: errors.New("db down")}))

	retryMsgs := retryProd.Messages()
	require.Len(t, retryMsgs, 2)
	assert.NotContains(t, retryMsgs[0].Headers, metadata.HeaderRetryStep)
	assert.Equal(t, "2", retryMsgs[1].Headers[metadata.HeaderRetryStep])
	for _, m := range retryMsgs {
		assert.Equal(t, "HANDLER_ERROR", m.Headers[metadata.HeaderErrorCode])
	}
}

// TestAttemptIsAlwaysInboundPlusOne verifies nothing a handler reports moves
// the attempt count.
func TestAttemptIsAlwaysInboundPlusOne(t *testing.T) {
	s, retryProd, _ := helpers.NewRetryStrategyWithMocks(10)

	msg := &types.Message{
		Topic:   "orders.retry",
		Headers: map[string]string{metadata.HeaderRetryAttempt: "4"},
		Payload: []byte("m"),
	}
	f := types.Failure{Err: errors.New("x"), Step: 9, Code: "whatever"}
	require.NoError(t, s.HandleError(context.Background(), []*types.Message{msg}, f))

	retryMsgs := retryProd.Messages()
	require.Len(t, retryMsgs, 1)
	assert.Equal(t, "5", retryMsgs[0].Headers[metadata.HeaderRetryAttempt])
}

// TestOriginalPositionSurvivesTwoHops runs a record source → retry → retry → DLQ
// and checks every republished record still names the source record.
func TestOriginalPositionSurvivesTwoHops(t *testing.T) {
	s, retryProd, dlqProd := helpers.NewRetryStrategyWithMocks(3)
	f := types.Failure{Err: errors.New("x")}

	// Hop 1: consumed from the source topic.
	source := &types.Message{Topic: "orders", Partition: 4, Offset: 100, Headers: map[string]string{}, Payload: []byte("m")}
	require.NoError(t, s.HandleError(context.Background(), []*types.Message{source}, f))

	// Hop 2: consumed from the retry topic, at a position of its own.
	first := retryProd.Messages()[0]
	hop2 := &types.Message{Topic: "test.retry", Partition: 0, Offset: 7, Headers: first.Headers, Payload: first.Value}
	require.NoError(t, s.HandleError(context.Background(), []*types.Message{hop2}, f))

	// Hop 3: consumed from the retry topic again; attempts are exhausted.
	second := retryProd.Messages()[1]
	hop3 := &types.Message{Topic: "test.retry", Partition: 1, Offset: 3, Headers: second.Headers, Payload: second.Value}
	require.NoError(t, s.HandleError(context.Background(), []*types.Message{hop3}, f))

	dlqMsgs := dlqProd.Messages()
	require.Len(t, dlqMsgs, 1)

	for name, headers := range map[string]map[string]string{
		"second retry record": second.Headers,
		"dlq record":          dlqMsgs[0].Headers,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, "orders", headers[metadata.HeaderOriginalTopic])
			assert.Equal(t, "4", headers[metadata.HeaderOriginalPartition])
			assert.Equal(t, "100", headers[metadata.HeaderOriginalOffset])
		})
	}
}
