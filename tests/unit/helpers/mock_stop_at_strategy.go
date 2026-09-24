package helpers

import (
	"context"
	"errors"
	"sync"

	"github.com/easykafka/easykafka-go/internal/types"
)

// StopAtStrategy records every call like MockStrategy, and returns an error on
// the StopAt-th call (1-based; 0 = never), as the retry strategy does when a
// retry or DLQ write fails synchronously.
type StopAtStrategy struct {
	StopAt int

	mu    sync.Mutex
	calls []HandleCall
}

func (s *StopAtStrategy) HandleError(_ context.Context, msgs []*types.Message, f types.Failure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, HandleCall{Msgs: msgs, HandlerErr: f.Err, Failure: f})
	if s.StopAt > 0 && len(s.calls) == s.StopAt {
		return errors.New("retry queue write failed")
	}
	return nil
}

func (s *StopAtStrategy) Name() string { return "stop-at" }

// Calls returns a copy of the calls recorded so far.
func (s *StopAtStrategy) Calls() []HandleCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HandleCall(nil), s.calls...)
}
