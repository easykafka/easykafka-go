package helpers

import (
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/publish/publishdriver"
	"github.com/stretchr/testify/require"
)

// NextPublishReport returns the next Report on reports, skipping client
// errors, and fails the test if none arrives within five seconds.
func NextPublishReport(t *testing.T, reports <-chan publishdriver.Event) publishdriver.Report {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		// Go's comma-ok receive: open is false once the channel is closed and
		// drained, and event is then nil. Here it means Close has run, so fail
		// at once rather than spin on nils until the timeout.
		case event, open := <-reports:
			require.True(t, open, "reports closed before a report arrived")
			if report, isReport := event.(publishdriver.Report); isReport {
				return report
			}
		case <-timeout:
			require.FailNow(t, "no report within 5s")
		}
	}
}
