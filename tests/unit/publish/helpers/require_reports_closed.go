package helpers

import (
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
)

// RequireReportsClosed fails the test unless reports is closed within five
// seconds. Call it after the producer's Close, which promises to close
// Reports.
//
// Events still in flight when Close ran are read and discarded on the way:
// the driver forwards them one by one and closes Reports only after the
// last, and with nobody reading it would wait forever.
func RequireReportsClosed(t *testing.T, reports <-chan publishdriver.Event) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-reports: // comma-ok receive: open is false once closed
			if !open {
				return
			}
		case <-timeout:
			t.Fatal("Reports not closed within 5s of Close")
		}
	}
}
