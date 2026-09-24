package helpers

import (
	"sync"
	"testing"
	"time"
)

// WaitForMessages polls until at least count payloads are received, failing the
// test if timeout passes first. payloads is read under mu.
func WaitForMessages(t *testing.T, mu *sync.Mutex, payloads *[]string, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		mu.Lock()
		n := len(*payloads)
		mu.Unlock()
		if n >= count {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d messages, only received %d", count, n)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
