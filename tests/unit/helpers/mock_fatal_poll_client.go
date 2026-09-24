package helpers

import (
	"context"
	"sync"

	"github.com/easykafka/easykafka-go/internal/types"
)

// FatalPollClient simulates a Kafka client that hands out Messages, then fails
// every further Poll with PollError.
type FatalPollClient struct {
	Messages  []*types.Message
	PollError error

	mu        sync.Mutex
	pollIndex int
	closed    bool
}

func (f *FatalPollClient) Connect(ctx context.Context) error          { return nil }
func (f *FatalPollClient) SubscribeToTopic(ctx context.Context) error { return nil }

func (f *FatalPollClient) Poll(ctx context.Context, timeoutMs int) (*types.Message, error) {
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

func (f *FatalPollClient) StoreOffset(topic string, partition int32, offset int64) error {
	return nil
}

func (f *FatalPollClient) MaybeCommitStored() error { return f.CommitStored() }

func (f *FatalPollClient) CommitStored() error {
	return nil
}

func (f *FatalPollClient) SetOnRevoke(fn func()) {}

func (f *FatalPollClient) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
