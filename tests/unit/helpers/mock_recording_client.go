package helpers

import (
	"context"
	"sync"
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
)

// RecordingClient records the order of the calls the engine makes, so a test can
// assert what has already happened by the time Start returns. Poll is counted
// rather than recorded — it fires on every loop iteration and would bury the
// sequence the tests care about.
type RecordingClient struct {
	Messages []*types.Message
	// AutoCommit puts the client in interval mode, where MaybeCommitStored does
	// nothing and only the unconditional CommitStored calls are recorded.
	AutoCommit bool

	mu        sync.Mutex
	events    []string
	pollCount int
	pollIndex int
}

func (c *RecordingClient) Connect(ctx context.Context) error { return nil }

func (c *RecordingClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *RecordingClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.mu.Lock()
	c.pollCount++
	if c.pollIndex >= len(c.Messages) {
		c.mu.Unlock()
		time.Sleep(time.Duration(timeoutMs) * time.Millisecond)
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	}
	msg := c.Messages[c.pollIndex]
	c.pollIndex++
	c.mu.Unlock()
	return msg, nil
}

func (c *RecordingClient) StoreOffset(topic string, partition int32, offset int64) error {
	c.record("store")
	return nil
}

// MaybeCommitStored mirrors the adapter: in interval mode librdkafka's
// background committer owns timing, so nothing is recorded here.
func (c *RecordingClient) MaybeCommitStored() error {
	c.mu.Lock()
	auto := c.AutoCommit
	c.mu.Unlock()
	if auto {
		return nil
	}
	return c.CommitStored()
}

func (c *RecordingClient) CommitStored() error {
	c.record("commit")
	return nil
}

func (c *RecordingClient) SetOnRevoke(fn func()) {}

func (c *RecordingClient) Close(ctx context.Context) error {
	c.record("close")
	return nil
}

func (c *RecordingClient) record(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

// Calls returns the recorded events — "store", "commit", "close" — in order.
func (c *RecordingClient) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// Polls returns how many times Poll has been called.
func (c *RecordingClient) Polls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pollCount
}
