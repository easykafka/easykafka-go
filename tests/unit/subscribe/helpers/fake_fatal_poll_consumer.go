package helpers

import (
	"context"
	"sync"

	"github.com/easykafka/easykafka-go/internal/subscribe/subscribedriver"
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// FakeFatalPollConsumer simulates a Kafka client that hands out Messages, then fails
// every further Poll with PollError.
type FakeFatalPollConsumer struct {
	Messages  []*types.Message
	PollError error

	mu        sync.Mutex
	pollIndex int
	closed    bool
}

func (f *FakeFatalPollConsumer) Connect(ctx context.Context) error          { return nil }
func (f *FakeFatalPollConsumer) SubscribeToTopic(ctx context.Context) error { return nil }

func (f *FakeFatalPollConsumer) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.pollIndex < len(f.Messages) {
		msg := f.Messages[f.pollIndex]
		f.pollIndex++
		return msg, nil
	}
	// Return fatal error after all messages
	return nil, f.PollError
}

func (f *FakeFatalPollConsumer) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (f *FakeFatalPollConsumer) MaybeCommitStored() error { return f.CommitStored() }

func (f *FakeFatalPollConsumer) CommitStored() error {
	return nil
}

func (f *FakeFatalPollConsumer) SetOnRevoke(fn func()) {}

func (f *FakeFatalPollConsumer) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Factory returns a consumer factory that hands out this fake, for
// subscribe.WithConsumerFactory.
func (f *FakeFatalPollConsumer) Factory() func(subscribedriver.Config) (subscribedriver.Consumer, error) {
	return func(subscribedriver.Config) (subscribedriver.Consumer, error) {
		return f, nil
	}
}
