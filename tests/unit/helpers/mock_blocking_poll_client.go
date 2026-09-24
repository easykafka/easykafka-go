package helpers

import (
	"context"
	"sync"
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
)

// BlockingPollClient returns FirstMessage from the first Poll; every later Poll
// blocks until the context is cancelled or the poll timeout passes.
type BlockingPollClient struct {
	FirstMessage *types.Message

	mu        sync.Mutex
	returned  bool
	connected bool
	closed    bool
}

func (c *BlockingPollClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *BlockingPollClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *BlockingPollClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.mu.Lock()
	if !c.returned {
		c.returned = true
		msg := c.FirstMessage
		c.mu.Unlock()
		return msg, nil
	}
	c.mu.Unlock()
	// Block until context is cancelled
	select {
	case <-ctx.Done():
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil, nil //nolint:nilnil // nil,nil is the mock poll contract for "no message"
	}
}

func (c *BlockingPollClient) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *BlockingPollClient) MaybeCommitStored() error { return c.CommitStored() }

func (c *BlockingPollClient) CommitStored() error {
	return nil
}

func (c *BlockingPollClient) SetOnRevoke(fn func()) {}

func (c *BlockingPollClient) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
