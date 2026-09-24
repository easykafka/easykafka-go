package helpers

import (
	"bytes"
	"sync"
)

// SyncBuffer is an io.Writer safe for the logger to use from the engine
// goroutine while the test reads it.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
