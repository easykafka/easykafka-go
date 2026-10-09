package helpers

import (
	"context"
	"sync"
	"time"

	"github.com/easykafka/easykafka-go/internal/subscribe/subscribedriver"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// FakeRecordingConsumer records the order of the calls the subscriber makes, so a test can
// assert what has already happened by the time Start returns. Poll is counted
// rather than recorded — it fires on every loop iteration and would bury the
// sequence the tests care about.
type FakeRecordingConsumer struct {
	Messages []*types.Message
	// AutoCommit puts the client in interval mode, where MaybeCommitStored does
	// nothing and only the unconditional CommitStored calls are recorded.
	AutoCommit bool

	mu        sync.Mutex
	events    []string
	pollCount int
	pollIndex int
}

func (c *FakeRecordingConsumer) Connect(ctx context.Context) error { return nil }

func (c *FakeRecordingConsumer) SubscribeToTopic(ctx context.Context) error { return nil }

func (c *FakeRecordingConsumer) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	c.mu.Lock()
	c.pollCount++
	if c.pollIndex >= len(c.Messages) {
		c.mu.Unlock()
		time.Sleep(time.Duration(timeoutMs) * time.Millisecond)
		return nil, nil //nolint:nilnil // nil,nil is the fake poll contract for "no message"
	}
	msg := c.Messages[c.pollIndex]
	c.pollIndex++
	c.mu.Unlock()
	return msg, nil
}

func (c *FakeRecordingConsumer) StoreOffset(topic string, partition int32, offset int64) error {
	c.record("store")
	return nil
}

// MaybeCommitStored mirrors the driver: in interval mode librdkafka's
// background committer owns timing, so nothing is recorded here.
func (c *FakeRecordingConsumer) MaybeCommitStored() error {
	c.mu.Lock()
	auto := c.AutoCommit
	c.mu.Unlock()
	if auto {
		return nil
	}
	return c.CommitStored()
}

func (c *FakeRecordingConsumer) CommitStored() error {
	c.record("commit")
	return nil
}

func (c *FakeRecordingConsumer) SetOnRevoke(fn func()) {}

func (c *FakeRecordingConsumer) Close(ctx context.Context) error {
	c.record("close")
	return nil
}

func (c *FakeRecordingConsumer) record(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

// Calls returns the recorded events — "store", "commit", "close" — in order.
func (c *FakeRecordingConsumer) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// Polls returns how many times Poll has been called.
func (c *FakeRecordingConsumer) Polls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pollCount
}

// Factory returns a consumer factory that hands out this fake, for
// subscribe.WithConsumerFactory.
func (c *FakeRecordingConsumer) Factory() func(subscribedriver.Config) (subscribedriver.Consumer, error) {
	return func(subscribedriver.Config) (subscribedriver.Consumer, error) {
		return c, nil
	}
}
