package helpers

import (
	"context"
	"sync"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/subscribedriver"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// FakeBlockingPollConsumer returns FirstMessage from the first Poll; every later Poll
// blocks until the context is cancelled or the poll timeout passes.
type FakeBlockingPollConsumer struct {
	FirstMessage *types.Message

	mu        sync.Mutex
	returned  bool
	connected bool
	closed    bool
}

func (c *FakeBlockingPollConsumer) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *FakeBlockingPollConsumer) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *FakeBlockingPollConsumer) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
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
		return nil, nil //nolint:nilnil // nil,nil is the fake poll contract for "no message"
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil, nil //nolint:nilnil // nil,nil is the fake poll contract for "no message"
	}
}

func (c *FakeBlockingPollConsumer) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *FakeBlockingPollConsumer) MaybeCommitStored() error { return c.CommitStored() }

func (c *FakeBlockingPollConsumer) CommitStored() error {
	return nil
}

func (c *FakeBlockingPollConsumer) SetOnRevoke(fn func()) {}

func (c *FakeBlockingPollConsumer) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// Factory returns a consumer factory that hands out this fake, for
// subscribe.WithConsumerFactory.
func (c *FakeBlockingPollConsumer) Factory() func(subscribedriver.Config) (subscribedriver.Consumer, error) {
	return func(subscribedriver.Config) (subscribedriver.Consumer, error) {
		return c, nil
	}
}
