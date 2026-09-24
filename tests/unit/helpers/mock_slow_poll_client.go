package helpers

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
)

// SlowPollClient counts every Poll in PollCount and hands out Messages; once
// they run out, each Poll sleeps for its full timeout and returns nothing.
type SlowPollClient struct {
	Messages  []*types.Message
	PollCount *atomic.Int32

	mu        sync.Mutex
	pollIndex int
	connected bool
	closed    bool
}

func (c *SlowPollClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *SlowPollClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *SlowPollClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.PollCount.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pollIndex >= len(c.Messages) {
		// Simulate blocking poll behavior
		time.Sleep(time.Duration(timeoutMs) * time.Millisecond)
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	}
	msg := c.Messages[c.pollIndex]
	c.pollIndex++
	return msg, nil
}

func (c *SlowPollClient) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *SlowPollClient) MaybeCommitStored() error { return c.CommitStored() }

func (c *SlowPollClient) CommitStored() error {
	return nil
}

func (c *SlowPollClient) SetOnRevoke(fn func()) {}

func (c *SlowPollClient) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
