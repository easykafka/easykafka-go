package helpers

import (
	"context"
	"sync"

	"github.com/easykafka/easykafka-go/internal/types"
)

// MockStrategy implements types.ErrorStrategy for testing. It records every
// call and returns ReturnErr from each.
type MockStrategy struct {
	ReturnErr error

	mu          sync.Mutex
	handleCalls []HandleCall
}

// HandleCall is one call the engine made to a fake strategy.
type HandleCall struct {
	Msgs       []*types.Message
	HandlerErr error // Failure.Err, kept separately for brevity in assertions
	Failure    types.Failure
}

func (s *MockStrategy) HandleError(ctx context.Context, msgs []*types.Message, f types.Failure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handleCalls = append(s.handleCalls, HandleCall{Msgs: msgs, HandlerErr: f.Err, Failure: f})
	return s.ReturnErr
}

func (s *MockStrategy) Name() string { return "mock" }

// HandleCalls returns a copy of the calls recorded so far.
func (s *MockStrategy) HandleCalls() []HandleCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]HandleCall, len(s.handleCalls))
	copy(result, s.handleCalls)
	return result
}
