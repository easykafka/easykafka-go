package helpers

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/subscribedriver"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// FakeSlowPollConsumer counts every Poll in PollCount and hands out Messages; once
// they run out, each Poll sleeps for its full timeout and returns nothing.
type FakeSlowPollConsumer struct {
	Messages  []*types.Message
	PollCount *atomic.Int32

	mu        sync.Mutex
	pollIndex int
	connected bool
	closed    bool
}

func (c *FakeSlowPollConsumer) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	return nil
}

func (c *FakeSlowPollConsumer) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *FakeSlowPollConsumer) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.PollCount.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pollIndex >= len(c.Messages) {
		// Simulate blocking poll behavior
		time.Sleep(time.Duration(timeoutMs) * time.Millisecond)
		return nil, nil //nolint:nilnil // nil,nil is the fake poll contract for "no message"
	}
	msg := c.Messages[c.pollIndex]
	c.pollIndex++
	return msg, nil
}

func (c *FakeSlowPollConsumer) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (c *FakeSlowPollConsumer) MaybeCommitStored() error { return c.CommitStored() }

func (c *FakeSlowPollConsumer) CommitStored() error {
	return nil
}

func (c *FakeSlowPollConsumer) SetOnRevoke(fn func()) {}

func (c *FakeSlowPollConsumer) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// Factory returns a consumer factory that hands out this fake, for
// subscribe.WithConsumerFactory.
func (c *FakeSlowPollConsumer) Factory() func(subscribedriver.Config) (subscribedriver.Consumer, error) {
	return func(subscribedriver.Config) (subscribedriver.Consumer, error) {
		return c, nil
	}
}
