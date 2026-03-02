package unit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/metadata"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/easykafka/easykafka-go/strategy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// T029 [US3] Unit tests for retry headers/backoff and strategy behavior
// =============================================================================

// =============================================================================
// Mock Producer for Testing
// =============================================================================

type producedMessage struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers map[string]string
}

type mockProducer struct {
	mu       sync.Mutex
	messages []producedMessage
	err      error
}

func (m *mockProducer) Produce(_ context.Context, msg *types.ProduceMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.messages = append(m.messages, producedMessage{
		Topic:   msg.Topic,
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: msg.Headers,
	})
	return nil
}

func (m *mockProducer) Flush(timeoutMs int) int { return 0 }
func (m *mockProducer) Close()                  {}

func (m *mockProducer) getMessages() []producedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]producedMessage, len(m.messages))
	copy(result, m.messages)
	return result
}

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

	headers := metadata.BuildRetryHeaders(msg, 2, retryTime, handlerErr)

	assert.Equal(t, "2", headers[metadata.HeaderRetryAttempt])
	assert.Equal(t, "2026-02-09T10:35:00Z", headers[metadata.HeaderRetryTime])
	assert.Equal(t, "0", headers[metadata.HeaderRetryStep])
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

	headers := metadata.BuildRetryHeaders(msg, 3, time.Now(), errors.New("err"))
	assert.Equal(t, "orders", headers[metadata.HeaderOriginalTopic])
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
	assert.Equal(t, 2.0, cfg.Multiplier)
	assert.Equal(t, types.PayloadEncodingJSON, cfg.PayloadEncoding)
}

func TestNewRetryStrategyCustomOptions(t *testing.T) {
	s, err := strategy.NewRetryStrategy(
		strategy.WithRetryTopic("my.retry"),
		strategy.WithDLQTopic("my.dlq"),
		strategy.WithMaxAttempts(5),
		strategy.WithInitialDelay(2*time.Second),
		strategy.WithMaxDelay(60*time.Second),
		strategy.WithBackoffMultiplier(3.0),
		strategy.WithFailedMessagePayloadEncoding(types.PayloadEncodingBase64),
	)
	require.NoError(t, err)

	cfg := s.Config()
	assert.Equal(t, "my.retry", cfg.RetryTopic)
	assert.Equal(t, "my.dlq", cfg.DLQTopic)
	assert.Equal(t, 5, cfg.MaxAttempts)
	assert.Equal(t, 2*time.Second, cfg.InitialDelay)
	assert.Equal(t, 60*time.Second, cfg.MaxDelay)
	assert.Equal(t, 3.0, cfg.Multiplier)
	assert.Equal(t, types.PayloadEncodingBase64, cfg.PayloadEncoding)
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

func newRetryStrategyWithMocks(maxAttempts int) (*strategy.RetryStrategy, *mockProducer, *mockProducer) {
	retryProd := &mockProducer{}
	dlqProd := &mockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:      "test.retry",
		DLQTopic:        "test.dlq",
		MaxAttempts:     maxAttempts,
		InitialDelay:    1 * time.Second,
		MaxDelay:        30 * time.Second,
		Multiplier:      2.0,
		PayloadEncoding: types.PayloadEncodingJSON,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())
	return s, retryProd, dlqProd
}

func TestRetryStrategySendsToRetryQueueOnFirstFailure(t *testing.T) {
	s, retryProd, dlqProd := newRetryStrategyWithMocks(3)
	ctx := context.Background()

	msg := &types.Message{
		Topic:     "orders",
		Partition: 0,
		Offset:    100,
		Headers:   make(map[string]string),
		Payload:   []byte(`{"orderId":"12345"}`),
	}

	err := s.HandleError(ctx, []*types.Message{msg}, errors.New("db connection failed"))
	require.NoError(t, err, "retry should return nil to continue consumption")

	// Should be sent to retry queue
	retryMsgs := retryProd.getMessages()
	require.Len(t, retryMsgs, 1)
	assert.Equal(t, "test.retry", retryMsgs[0].Topic)
	assert.Equal(t, []byte(`{"orderId":"12345"}`), retryMsgs[0].Value)

	// Verify headers
	assert.Equal(t, "1", retryMsgs[0].Headers[metadata.HeaderRetryAttempt])
	assert.NotEmpty(t, retryMsgs[0].Headers[metadata.HeaderRetryTime])
	assert.Equal(t, "orders", retryMsgs[0].Headers[metadata.HeaderOriginalTopic])
	assert.Equal(t, "db connection failed", retryMsgs[0].Headers[metadata.HeaderErrorMessage])

	// DLQ should be empty
	assert.Empty(t, dlqProd.getMessages())
}

func TestRetryStrategySendsToDLQAfterMaxAttempts(t *testing.T) {
	s, retryProd, dlqProd := newRetryStrategyWithMocks(3)
	ctx := context.Background()

	// Simulate a message that has already been retried twice (attempt=2)
	msg := &types.Message{
		Topic:     "test.retry",
		Partition: 0,
		Offset:    50,
		Headers: map[string]string{
			metadata.HeaderRetryAttempt:      "2",
			metadata.HeaderOriginalTopic:     "orders",
			metadata.HeaderOriginalPartition: "0",
			metadata.HeaderOriginalOffset:    "100",
		},
		Payload: []byte(`{"orderId":"12345"}`),
	}

	err := s.HandleError(ctx, []*types.Message{msg}, errors.New("still failing"))
	require.NoError(t, err, "should continue after sending to DLQ")

	// Retry queue should be empty (max attempts reached)
	assert.Empty(t, retryProd.getMessages())

	// DLQ should have the message
	dlqMsgs := dlqProd.getMessages()
	require.Len(t, dlqMsgs, 1)
	assert.Equal(t, "test.dlq", dlqMsgs[0].Topic)

	// DLQ payload should be a JSON envelope
	assert.Contains(t, string(dlqMsgs[0].Value), `"originalTopic":"orders"`)
	assert.Contains(t, string(dlqMsgs[0].Value), `"attemptCount":3`)
	assert.Contains(t, string(dlqMsgs[0].Value), `"error":"still failing"`)
}

func TestRetryStrategyHandlesMultipleMessages(t *testing.T) {
	s, retryProd, _ := newRetryStrategyWithMocks(3)
	ctx := context.Background()

	msgs := []*types.Message{
		{Topic: "orders", Partition: 0, Offset: 100, Headers: make(map[string]string), Payload: []byte("msg-1")},
		{Topic: "orders", Partition: 0, Offset: 101, Headers: make(map[string]string), Payload: []byte("msg-2")},
		{Topic: "orders", Partition: 0, Offset: 102, Headers: make(map[string]string), Payload: []byte("msg-3")},
	}

	err := s.HandleError(ctx, msgs, errors.New("batch failed"))
	require.NoError(t, err)

	// Each message should be sent individually to retry queue
	retryMsgs := retryProd.getMessages()
	require.Len(t, retryMsgs, 3)
	assert.Equal(t, []byte("msg-1"), retryMsgs[0].Value)
	assert.Equal(t, []byte("msg-2"), retryMsgs[1].Value)
	assert.Equal(t, []byte("msg-3"), retryMsgs[2].Value)
}

func TestRetryStrategyReturnsFatalOnProducerError(t *testing.T) {
	retryProd := &mockProducer{err: errors.New("kafka unavailable")}
	dlqProd := &mockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:      "test.retry",
		DLQTopic:        "test.dlq",
		MaxAttempts:     3,
		InitialDelay:    time.Second,
		MaxDelay:        30 * time.Second,
		Multiplier:      2.0,
		PayloadEncoding: types.PayloadEncodingJSON,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, errors.New("handler err"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry queue write failed")
}

func TestRetryStrategyDLQProducerError(t *testing.T) {
	retryProd := &mockProducer{}
	dlqProd := &mockProducer{err: errors.New("dlq unavailable")}
	cfg := strategy.RetryConfig{
		RetryTopic:      "test.retry",
		DLQTopic:        "test.dlq",
		MaxAttempts:     1, // Immediately to DLQ
		InitialDelay:    time.Second,
		MaxDelay:        30 * time.Second,
		Multiplier:      2.0,
		PayloadEncoding: types.PayloadEncodingJSON,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, errors.New("handler err"))
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

	err := s.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not initialized")
}

// =============================================================================
// Backoff Calculation Tests
// =============================================================================

func TestExponentialBackoff(t *testing.T) {
	s, retryProd, _ := newRetryStrategyWithMocks(10)
	ctx := context.Background()

	// First failure -> attempt 1 -> delay = 1s * 2^0 = 1s
	msg1 := &types.Message{Topic: "t", Headers: make(map[string]string), Payload: []byte("m")}
	_ = s.HandleError(ctx, []*types.Message{msg1}, errors.New("err"))

	retryMsgs := retryProd.getMessages()
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
	_ = s.HandleError(ctx, []*types.Message{msg2}, errors.New("err"))

	retryMsgs = retryProd.getMessages()
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
	retryProd := &mockProducer{}
	dlqProd := &mockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:      "test.retry",
		DLQTopic:        "test.dlq",
		MaxAttempts:     20,
		InitialDelay:    1 * time.Second,
		MaxDelay:        5 * time.Second, // Low cap
		Multiplier:      10.0,            // Aggressive multiplier
		PayloadEncoding: types.PayloadEncodingJSON,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	// High attempt should be capped at MaxDelay
	msg := &types.Message{
		Topic:   "t.retry",
		Headers: map[string]string{metadata.HeaderRetryAttempt: "5"},
		Payload: []byte("m"),
	}
	_ = s.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))

	retryMsgs := retryProd.getMessages()
	require.Len(t, retryMsgs, 1)
	rt := retryMsgs[0].Headers[metadata.HeaderRetryTime]
	parsedTime, err := time.Parse(time.RFC3339, rt)
	require.NoError(t, err)

	// Should be now + maxDelay (5s), not some huge value
	maxExpected := time.Now().Add(6 * time.Second)
	assert.True(t, parsedTime.Before(maxExpected), "retry time should be capped at MaxDelay")
}

// =============================================================================
// DLQ Payload Encoding Tests
// =============================================================================

func TestDLQPayloadEncodingJSON(t *testing.T) {
	retryProd := &mockProducer{}
	dlqProd := &mockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:      "test.retry",
		DLQTopic:        "test.dlq",
		MaxAttempts:     1, // Send immediately to DLQ
		InitialDelay:    time.Second,
		MaxDelay:        30 * time.Second,
		Multiplier:      2.0,
		PayloadEncoding: types.PayloadEncodingJSON,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	msg := &types.Message{
		Topic:     "orders",
		Partition: 2,
		Offset:    999,
		Headers:   make(map[string]string),
		Payload:   []byte(`{"orderId":"ABC"}`),
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, errors.New("processing failed"))
	require.NoError(t, err)

	dlqMsgs := dlqProd.getMessages()
	require.Len(t, dlqMsgs, 1)
	payload := string(dlqMsgs[0].Value)

	assert.Contains(t, payload, `"payloadEncoding":"json"`)
	assert.Contains(t, payload, `"payload":"{\"orderId\":\"ABC\"}"`)
	assert.Contains(t, payload, `"originalTopic":"orders"`)
	assert.Contains(t, payload, `"originalPartition":2`)
	assert.Contains(t, payload, `"originalOffset":999`)
}

func TestDLQPayloadEncodingBase64(t *testing.T) {
	retryProd := &mockProducer{}
	dlqProd := &mockProducer{}
	cfg := strategy.RetryConfig{
		RetryTopic:      "test.retry",
		DLQTopic:        "test.dlq",
		MaxAttempts:     1,
		InitialDelay:    time.Second,
		MaxDelay:        30 * time.Second,
		Multiplier:      2.0,
		PayloadEncoding: types.PayloadEncodingBase64,
	}
	s := strategy.NewRetryStrategyWithProducers(cfg, retryProd, dlqProd, zerolog.Nop())

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte{0x00, 0xFF, 0xFE}, // Binary payload
	}

	err := s.HandleError(context.Background(), []*types.Message{msg}, errors.New("binary error"))
	require.NoError(t, err)

	dlqMsgs := dlqProd.getMessages()
	require.Len(t, dlqMsgs, 1)
	payload := string(dlqMsgs[0].Value)

	assert.Contains(t, payload, `"payloadEncoding":"base64"`)
	assert.Contains(t, payload, `"payload":"AP/+"`) // base64 of 0x00 0xFF 0xFE
}

// =============================================================================
// Circuit Breaker Tests
// =============================================================================

func TestCircuitBreakerDefaults(t *testing.T) {
	cb, err := strategy.NewCircuitBreakerStrategy()
	require.NoError(t, err)
	assert.Equal(t, "circuit-breaker", cb.Name())
	assert.Equal(t, strategy.CircuitClosed, cb.State())
}

func TestCircuitBreakerCustomOptions(t *testing.T) {
	_, err := strategy.NewCircuitBreakerStrategy(
		strategy.WithFailureThreshold(5),
		strategy.WithCooldownPeriod(30*time.Second),
		strategy.WithHalfOpenAttempts(2),
	)
	require.NoError(t, err)
}

func TestCircuitBreakerOptionValidation(t *testing.T) {
	tests := []struct {
		name        string
		opt         strategy.CircuitBreakerOption
		errContains string
	}{
		{"zero threshold", strategy.WithFailureThreshold(0), "failure threshold must be positive"},
		{"negative threshold", strategy.WithFailureThreshold(-1), "positive"},
		{"zero cooldown", strategy.WithCooldownPeriod(0), "cooldown period must be positive"},
		{"zero half-open", strategy.WithHalfOpenAttempts(0), "half-open attempts must be positive"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := strategy.NewCircuitBreakerStrategy(tc.opt)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errContains)
		})
	}
}

func TestCircuitBreakerTripsAfterThreshold(t *testing.T) {
	cb, err := strategy.NewCircuitBreakerStrategy(
		strategy.WithFailureThreshold(3),
		strategy.WithCooldownPeriod(60*time.Second),
	)
	require.NoError(t, err)

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	// First two failures: circuit stays closed
	for i := 0; i < 2; i++ {
		err := cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
		require.NoError(t, err, "failure %d should not open circuit", i+1)
		assert.Equal(t, strategy.CircuitClosed, cb.State())
	}

	// Third failure: circuit opens
	err = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker opened")
	assert.Equal(t, strategy.CircuitOpen, cb.State())
}

func TestCircuitBreakerSuccessResetsCounter(t *testing.T) {
	cb, err := strategy.NewCircuitBreakerStrategy(
		strategy.WithFailureThreshold(3),
	)
	require.NoError(t, err)

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	// 2 failures
	for i := 0; i < 2; i++ {
		_ = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	}

	assert.Equal(t, strategy.CircuitClosed, cb.State())

	// A success resets the counter
	cb.OnSuccess()

	// Now need 3 more failures to trip
	for i := 0; i < 2; i++ {
		err := cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
		require.NoError(t, err)
		assert.Equal(t, strategy.CircuitClosed, cb.State())
	}

	// Third failure now trips
	err = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	require.Error(t, err)
	assert.Equal(t, strategy.CircuitOpen, cb.State())
}

func TestCircuitBreakerCooldownTransitionsToHalfOpen(t *testing.T) {
	cb, err := strategy.NewCircuitBreakerStrategy(
		strategy.WithFailureThreshold(1),
		strategy.WithCooldownPeriod(5*time.Second),
	)
	require.NoError(t, err)

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	// Trip the circuit
	_ = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	assert.Equal(t, strategy.CircuitOpen, cb.State())

	// Mock time to simulate cooldown elapsed
	now := time.Now()
	cb.SetNowFunc(func() time.Time {
		return now.Add(6 * time.Second)
	})

	// Next HandleError should transition to HalfOpen and return nil
	err = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	require.NoError(t, err) // Allowed through for testing
	assert.Equal(t, strategy.CircuitHalfOpen, cb.State())
}

func TestCircuitBreakerHalfOpenToClosedOnSuccess(t *testing.T) {
	cb, err := strategy.NewCircuitBreakerStrategy(
		strategy.WithFailureThreshold(1),
		strategy.WithCooldownPeriod(1*time.Second),
		strategy.WithHalfOpenAttempts(2),
	)
	require.NoError(t, err)

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	// Trip the circuit
	_ = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	assert.Equal(t, strategy.CircuitOpen, cb.State())

	// Advance time past cooldown
	now := time.Now()
	cb.SetNowFunc(func() time.Time {
		return now.Add(2 * time.Second)
	})

	// Trigger transition to HalfOpen
	_ = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	assert.Equal(t, strategy.CircuitHalfOpen, cb.State())

	// Two successes should close the circuit
	cb.OnSuccess()
	assert.Equal(t, strategy.CircuitHalfOpen, cb.State())

	cb.OnSuccess()
	assert.Equal(t, strategy.CircuitClosed, cb.State())
}

func TestCircuitBreakerHalfOpenToOpenOnFailure(t *testing.T) {
	cb, err := strategy.NewCircuitBreakerStrategy(
		strategy.WithFailureThreshold(1),
		strategy.WithCooldownPeriod(1*time.Second),
		strategy.WithHalfOpenAttempts(3),
	)
	require.NoError(t, err)

	msg := &types.Message{
		Topic:   "orders",
		Headers: make(map[string]string),
		Payload: []byte("msg"),
	}

	// Trip the circuit
	_ = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))

	// Advance past cooldown
	now := time.Now()
	cb.SetNowFunc(func() time.Time {
		return now.Add(2 * time.Second)
	})

	// Transition to HalfOpen
	_ = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("err"))
	assert.Equal(t, strategy.CircuitHalfOpen, cb.State())

	// One success then a failure -> should reopen
	cb.OnSuccess()
	assert.Equal(t, strategy.CircuitHalfOpen, cb.State())

	// Advance time again for the HalfOpen -> Open transition
	// todo: is this step needed? when in CircuitHalfOpen HandleError does not really need to advance the time
	now2 := time.Now()
	cb.SetNowFunc(func() time.Time {
		return now2.Add(3 * time.Second)
	})

	err = cb.HandleError(context.Background(), []*types.Message{msg}, errors.New("still failing"))
	// The state should reflect that the circuit was reopened
	assert.Equal(t, strategy.CircuitOpen, cb.State())
}

// TestCircuitBreakerImplementsErrorStrategy verifies interface compliance.
func TestCircuitBreakerImplementsErrorStrategy(t *testing.T) {
	cb, _ := strategy.NewCircuitBreakerStrategy()
	var _ types.ErrorStrategy = cb
}

// TestCircuitBreakerImplementsInitializable verifies interface compliance.
func TestCircuitBreakerImplementsInitializable(t *testing.T) {
	cb, _ := strategy.NewCircuitBreakerStrategy()
	var _ types.Initializable = cb
}
