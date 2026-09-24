package helpers

import (
	"context"
	"sync"

	"github.com/easykafka/easykafka-go/internal/types"
)

// MockKafkaClient implements engine.KafkaClient for testing. It hands out
// Messages one per Poll, records every stored offset, and can be told to fail
// any call.
type MockKafkaClient struct {
	Messages     []*types.Message
	ConnectErr   error
	SubscribeErr error
	PollErr      error
	// StoreErr fails StoreOffset. Set it to types.ErrPartitionRevoked to simulate
	// a rebalance race, or to anything else to simulate a real store failure —
	// the engine treats the two very differently.
	StoreErr error
	// StoreErrByPartition fails StoreOffset for specific partitions only, so a
	// single batch can produce a mix of outcomes. Takes precedence over StoreErr.
	StoreErrByPartition map[int32]error
	CommitErr           error
	CloseErr            error
	// RevokeAtPoll fires the revoke hook once the poll index reaches it
	// (0 = never). The hook runs from inside Poll, mirroring where librdkafka
	// runs it.
	RevokeAtPoll int
	// AutoCommit puts the fake in interval mode, where MaybeCommitStored does
	// nothing — as librdkafka's background committer owns timing.
	AutoCommit bool

	mu            sync.Mutex
	connected     bool
	subscribed    bool
	closed        bool
	pollIndex     int
	storedOffsets []StoreRecord
	commitCount   int
	onRevoke      func()
	revokeFired   bool
}

// StoreRecord is one offset the engine stored through MockKafkaClient.
type StoreRecord struct {
	Topic     string
	Partition int32
	Offset    int64
}

func (m *MockKafkaClient) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ConnectErr != nil {
		return m.ConnectErr
	}
	m.connected = true
	return nil
}

func (m *MockKafkaClient) SubscribeToTopic(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SubscribeErr != nil {
		return m.SubscribeErr
	}
	m.subscribed = true
	return nil
}

func (m *MockKafkaClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	m.mu.Lock()
	if m.PollErr != nil {
		err := m.PollErr
		m.mu.Unlock()
		return nil, err
	}

	// Fire the revoke hook from inside Poll, on the caller's goroutine, because
	// that is where librdkafka runs the rebalance callback. Firing it from
	// anywhere else would be a race the real adapter cannot produce — and the
	// engine leaves the batch buffer unsynchronised on exactly this guarantee.
	if m.RevokeAtPoll > 0 && m.pollIndex >= m.RevokeAtPoll && !m.revokeFired {
		m.revokeFired = true
		fn := m.onRevoke
		m.mu.Unlock()
		if fn != nil {
			fn()
		}
		// A poll that delivered a rebalance yields no message, as in the adapter.
		return nil, nil //nolint:nilnil // "no message" sentinel in the mock poll contract
	}

	if m.pollIndex >= len(m.Messages) {
		m.mu.Unlock()
		return nil, nil //nolint:nilnil // "no message" sentinel in the mock poll contract
	}
	msg := m.Messages[m.pollIndex]
	m.pollIndex++
	m.mu.Unlock()
	return msg, nil
}

func (m *MockKafkaClient) StoreOffset(topic string, partition int32, offset int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.StoreErrByPartition[partition]; ok {
		return err
	}
	if m.StoreErr != nil {
		return m.StoreErr
	}
	m.storedOffsets = append(m.storedOffsets, StoreRecord{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
	})
	return nil
}

func (m *MockKafkaClient) CommitStored() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CommitErr != nil {
		return m.CommitErr
	}
	m.commitCount++
	return nil
}

// MaybeCommitStored mirrors the adapter: it skips the commit when the fake is in
// interval mode, and otherwise behaves exactly like CommitStored — including
// counting, so CommitCount keeps meaning "commits that reached the broker".
func (m *MockKafkaClient) MaybeCommitStored() error {
	m.mu.Lock()
	auto := m.AutoCommit
	m.mu.Unlock()
	if auto {
		return nil
	}
	return m.CommitStored()
}

func (m *MockKafkaClient) SetOnRevoke(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRevoke = fn
}

func (m *MockKafkaClient) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return m.CloseErr
}

// Connected reports whether Connect has succeeded.
func (m *MockKafkaClient) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Subscribed reports whether SubscribeToTopic has succeeded.
func (m *MockKafkaClient) Subscribed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subscribed
}

// Closed reports whether Close has been called.
func (m *MockKafkaClient) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// DidRevoke reports whether the revoke hook has fired yet.
func (m *MockKafkaClient) DidRevoke() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokeFired
}

// StoredOffsets returns the offsets the engine recorded as processed. These are
// raw message offsets: the +1 that turns one into a resume position lives in
// the real adapter, not here.
func (m *MockKafkaClient) StoredOffsets() []StoreRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]StoreRecord, len(m.storedOffsets))
	copy(result, m.storedOffsets)
	return result
}

// PolledCount reports how many of the canned messages Poll has handed over.
func (m *MockKafkaClient) PolledCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pollIndex
}

// CommitCount reports how many times the engine published the store.
func (m *MockKafkaClient) CommitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commitCount
}
