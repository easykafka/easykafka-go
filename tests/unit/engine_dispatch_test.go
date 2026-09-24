package unit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/internal/engine"
	"github.com/easykafka/easykafka-go/internal/types"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockKafkaClient implements engine.KafkaClient for testing.
type mockKafkaClient struct {
	mu            sync.Mutex
	connected     bool
	subscribed    bool
	closed        bool
	messages      []*types.Message
	pollIndex     int
	storedOffsets []storeRecord
	commitCount   int
	onRevoke      func()
	connectErr    error
	subscribeErr  error
	pollErr       error
	// storeErr fails StoreOffset. Set it to types.ErrPartitionRevoked to simulate
	// a rebalance race, or to anything else to simulate a real store failure —
	// the engine treats the two very differently.
	storeErr error
	// storeErrByPartition fails StoreOffset for specific partitions only, so a
	// single batch can produce a mix of outcomes. Takes precedence over storeErr.
	storeErrByPartition map[int32]error
	commitErr           error
	closeErr            error
	// revokeAtPoll fires the revoke hook once pollIndex reaches it (0 = never).
	// The hook runs from inside Poll, mirroring where librdkafka runs it.
	revokeAtPoll int
	revokeFired  bool
	// autoCommit puts the fake in interval mode, where MaybeCommitStored does
	// nothing — as librdkafka's background committer owns timing.
	autoCommit bool
}

type storeRecord struct {
	Topic     string
	Partition int32
	Offset    int64
}

func (m *mockKafkaClient) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connectErr != nil {
		return m.connectErr
	}
	m.connected = true
	return nil
}

func (m *mockKafkaClient) SubscribeToTopic(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subscribeErr != nil {
		return m.subscribeErr
	}
	m.subscribed = true
	return nil
}

func (m *mockKafkaClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	m.mu.Lock()
	if m.pollErr != nil {
		err := m.pollErr
		m.mu.Unlock()
		return nil, err
	}

	// Fire the revoke hook from inside Poll, on the caller's goroutine, because
	// that is where librdkafka runs the rebalance callback. Firing it from
	// anywhere else would be a race the real adapter cannot produce — and the
	// engine leaves the batch buffer unsynchronised on exactly this guarantee.
	if m.revokeAtPoll > 0 && m.pollIndex >= m.revokeAtPoll && !m.revokeFired {
		m.revokeFired = true
		fn := m.onRevoke
		m.mu.Unlock()
		if fn != nil {
			fn()
		}
		// A poll that delivered a rebalance yields no message, as in the adapter.
		return nil, nil //nolint:nilnil // "no message" sentinel in the mock poll contract
	}

	if m.pollIndex >= len(m.messages) {
		m.mu.Unlock()
		return nil, nil //nolint:nilnil // "no message" sentinel in the mock poll contract
	}
	msg := m.messages[m.pollIndex]
	m.pollIndex++
	m.mu.Unlock()
	return msg, nil
}

func (m *mockKafkaClient) StoreOffset(topic string, partition int32, offset int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.storeErrByPartition[partition]; ok {
		return err
	}
	if m.storeErr != nil {
		return m.storeErr
	}
	m.storedOffsets = append(m.storedOffsets, storeRecord{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
	})
	return nil
}

func (m *mockKafkaClient) CommitStored() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.commitErr != nil {
		return m.commitErr
	}
	m.commitCount++
	return nil
}

// MaybeCommitStored mirrors the adapter: it skips the commit when the fake is in
// interval mode, and otherwise behaves exactly like CommitStored — including
// counting, so getCommitCount keeps meaning "commits that reached the broker".
func (m *mockKafkaClient) MaybeCommitStored() error {
	m.mu.Lock()
	auto := m.autoCommit
	m.mu.Unlock()
	if auto {
		return nil
	}
	return m.CommitStored()
}

func (m *mockKafkaClient) SetOnRevoke(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRevoke = fn
}

// didRevoke reports whether the revoke hook has fired yet.
func (m *mockKafkaClient) didRevoke() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokeFired
}

func (m *mockKafkaClient) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return m.closeErr
}

// getStoredOffsets returns the offsets the engine recorded as processed. These
// are raw message offsets: the +1 that turns one into a resume position lives in
// the real adapter, not here.
func (m *mockKafkaClient) getStoredOffsets() []storeRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]storeRecord, len(m.storedOffsets))
	copy(result, m.storedOffsets)
	return result
}

// getPolledCount reports how many of the canned messages Poll has handed over.
func (m *mockKafkaClient) getPolledCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pollIndex
}

// getCommitCount reports how many times the engine published the store.
func (m *mockKafkaClient) getCommitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commitCount
}

// mockStrategy implements types.ErrorStrategy for testing.
type mockStrategy struct {
	mu          sync.Mutex
	handleCalls []handleCall
	returnErr   error
	name        string
}

type handleCall struct {
	Msgs       []*types.Message
	HandlerErr error // Failure.Err, kept separately for brevity in assertions
	Failure    types.Failure
}

func (s *mockStrategy) HandleError(ctx context.Context, msgs []*types.Message, f types.Failure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handleCalls = append(s.handleCalls, handleCall{Msgs: msgs, HandlerErr: f.Err, Failure: f})
	return s.returnErr
}

func (s *mockStrategy) Name() string {
	if s.name != "" {
		return s.name
	}
	return "mock"
}

func (s *mockStrategy) getHandleCalls() []handleCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]handleCall, len(s.handleCalls))
	copy(result, s.handleCalls)
	return result
}

func newTestMessage(topic string, partition int32, offset int64, payload string) *types.Message {
	return &types.Message{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
		Timestamp: time.Now(),
		Headers:   make(map[string]string),
		Payload:   []byte(payload),
	}
}

func testLogger() zerolog.Logger {
	return zerolog.Nop()
}

// TestEngineDispatchSuccess verifies the handler is invoked and offset committed on success.
func TestEngineDispatchSuccess(t *testing.T) {
	var receivedPayloads []string
	var mu sync.Mutex

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		mu.Lock()
		defer mu.Unlock()
		receivedPayloads = append(receivedPayloads, string(payload))
		return nil
	}

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "msg-1"),
		newTestMessage("test-topic", 0, 1, "msg-2"),
		newTestMessage("test-topic", 0, 2, "msg-3"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	// Cancel after all messages are consumed (mock returns nil after messages)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Verify all messages were dispatched to handler
	mu.Lock()
	assert.Equal(t, []string{"msg-1", "msg-2", "msg-3"}, receivedPayloads)
	mu.Unlock()

	// Verify all offsets were committed
	commits := client.getStoredOffsets()
	require.Len(t, commits, 3)
	assert.Equal(t, int64(0), commits[0].Offset)
	assert.Equal(t, int64(1), commits[1].Offset)
	assert.Equal(t, int64(2), commits[2].Offset)

	// Verify strategy was never called (no errors)
	assert.Empty(t, strat.getHandleCalls())

	// Verify adapter lifecycle
	assert.True(t, client.connected)
	assert.True(t, client.subscribed)
	assert.True(t, client.closed)
}

// TestEngineDispatchHandlerError verifies the error strategy is called on handler failure
// and offset is committed even for failed message.
func TestEngineDispatchHandlerError(t *testing.T) {
	handlerErr := errors.New("processing failed")

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		if string(payload) == "bad-msg" {
			return &types.Failure{Err: handlerErr}
		}
		return nil
	}

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "good-msg"),
		newTestMessage("test-topic", 0, 1, "bad-msg"),
		newTestMessage("test-topic", 0, 2, "good-msg-2"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{} // Returns nil = continue consumption

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Verify only good messages had their offsets committed
	commits := client.getStoredOffsets()
	require.Len(t, commits, 3)
	assert.Equal(t, int64(0), commits[0].Offset) // good-msg
	assert.Equal(t, int64(1), commits[1].Offset) // bad-msg
	assert.Equal(t, int64(2), commits[2].Offset) // good-msg-2

	// Verify strategy was called once with the bad message
	calls := strat.getHandleCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, handlerErr, calls[0].HandlerErr)
	require.Len(t, calls[0].Msgs, 1)
	assert.Equal(t, int64(1), calls[0].Msgs[0].Offset)
}

// TestEngineDispatchStrategyFatal verifies the engine stops when strategy returns an error.
func TestEngineDispatchStrategyFatal(t *testing.T) {
	strategyErr := errors.New("fatal: must stop")

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		return &types.Failure{Err: errors.New("handler error")}
	}

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "msg-1"),
		newTestMessage("test-topic", 0, 1, "msg-2"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{returnErr: strategyErr}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)

	// Engine should return error from strategy
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error strategy")

	// Strategy should have been called only once (engine stops after first fatal)
	calls := strat.getHandleCalls()
	assert.Len(t, calls, 1)

	// No offsets should be committed
	assert.Empty(t, client.getStoredOffsets())
}

// TestEngineDispatchPanicRecovery verifies that handler panics are recovered
// and treated as errors.
func TestEngineDispatchPanicRecovery(t *testing.T) {
	handler := func(ctx context.Context, payload []byte) *types.Failure {
		if string(payload) == "panic-msg" {
			panic("unexpected crash!")
		}
		return nil
	}

	messages := []*types.Message{
		newTestMessage("test-topic", 0, 0, "good-msg"),
		newTestMessage("test-topic", 0, 1, "panic-msg"),
		newTestMessage("test-topic", 0, 2, "good-msg-2"),
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{} // Returns nil = continue

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Verify strategy was called with the panic error
	calls := strat.getHandleCalls()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].HandlerErr.Error(), "handler panic")
	assert.Contains(t, calls[0].HandlerErr.Error(), "unexpected crash!")

	// Verify all messages were committed (even failed ones since error strategy does not commit by itself!)
	commits := client.getStoredOffsets()
	require.Len(t, commits, 3)
	assert.Equal(t, int64(0), commits[0].Offset)
	assert.Equal(t, int64(1), commits[1].Offset)
	assert.Equal(t, int64(2), commits[2].Offset)
}

// TestEngineContextCancellation verifies the engine exits cleanly on context cancellation.
func TestEngineContextCancellation(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		mu.Lock()
		callCount++
		count := callCount
		mu.Unlock()

		if count >= 2 {
			cancel() // Cancel after processing 2 messages
		}
		return nil
	}

	// Provide many messages but expect only ~2 to be processed
	messages := make([]*types.Message, 10)
	for i := range messages {
		messages[i] = newTestMessage("test-topic", 0, int64(i), fmt.Sprintf("msg-%d", i))
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	err := eng.Start(ctx)
	require.NoError(t, err)

	// Should have processed at least 2 messages and then stopped
	mu.Lock()
	assert.GreaterOrEqual(t, callCount, 2)
	mu.Unlock()

	assert.True(t, client.closed)
}

// TestEngineConnectError verifies error handling when Kafka connection fails.
func TestEngineConnectError(t *testing.T) {
	client := &mockKafkaClient{connectErr: errors.New("connection refused")}
	strat := &mockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	err := eng.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect")
}

// TestEngineSubscribeError verifies error handling when subscription fails.
func TestEngineSubscribeError(t *testing.T) {
	client := &mockKafkaClient{subscribeErr: errors.New("subscription failed")}
	strat := &mockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	err := eng.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to subscribe")
	assert.True(t, client.closed)
}

// fatalPollClient simulates a Kafka client that fails on the second poll.
type fatalPollClient struct {
	mu        sync.Mutex
	messages  []*types.Message
	pollIndex int
	pollError error
	closed    bool
}

func (f *fatalPollClient) Connect(ctx context.Context) error          { return nil }
func (f *fatalPollClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (f *fatalPollClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.pollIndex < len(f.messages) {
		msg := f.messages[f.pollIndex]
		f.pollIndex++
		return msg, nil
	}
	// Return fatal error after all messages
	return nil, f.pollError
}

func (f *fatalPollClient) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (f *fatalPollClient) MaybeCommitStored() error { return f.CommitStored() }

func (f *fatalPollClient) CommitStored() error {
	return nil
}

func (f *fatalPollClient) SetOnRevoke(fn func()) {}

func (f *fatalPollClient) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// TestEnginePollError verifies the engine stops on fatal poll errors.
func TestEnginePollError(t *testing.T) {
	strat := &mockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	// Use a client that returns an error on second poll
	fatalClient := &fatalPollClient{
		messages:  []*types.Message{newTestMessage("test-topic", 0, 0, "msg-1")},
		pollError: errors.New("kafka connection lost"),
	}

	eng := engine.NewEngine(fatalClient, handler, strat, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "polling error")
}

// TestEngineMessageContext verifies that message metadata is accessible via context.
func TestEngineMessageContext(t *testing.T) {
	var capturedMsg *types.Message

	handler := func(ctx context.Context, payload []byte) *types.Failure {
		// Deliberately the public accessor, not internal/metadata: this test is
		// the guard that the re-export in handler.go stays.
		msg, ok := easykafka.MessageFromContext(ctx)
		if ok {
			capturedMsg = msg
		}
		return nil
	}

	messages := []*types.Message{
		{
			Topic:     "ctx-topic",
			Partition: 3,
			Offset:    42,
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Headers:   map[string]string{"key": "value"},
			Payload:   []byte("test-payload"),
		},
	}

	client := &mockKafkaClient{messages: messages}
	strat := &mockStrategy{}

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := eng.Start(ctx)
	require.NoError(t, err)

	require.NotNil(t, capturedMsg)
	assert.Equal(t, "ctx-topic", capturedMsg.Topic)
	assert.Equal(t, int32(3), capturedMsg.Partition)
	assert.Equal(t, int64(42), capturedMsg.Offset)
	assert.Equal(t, "value", capturedMsg.Headers["key"])
}

// TestEngineDoubleStartError verifies engine prevents double-start.
func TestEngineDoubleStartError(t *testing.T) {
	client := &mockKafkaClient{}
	strat := &mockStrategy{}

	handler := func(ctx context.Context, payload []byte) *types.Failure { return nil }

	eng := engine.NewEngine(client, handler, strat, testLogger(), 100)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// First start
	_ = eng.Start(ctx)

	// Second start should fail
	err := eng.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already started")
}
