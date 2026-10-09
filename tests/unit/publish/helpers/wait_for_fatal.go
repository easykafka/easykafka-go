package helpers

import (
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/stretchr/testify/require"
)

// WaitForFatal waits until the publisher has recorded a fatal error, failing
// the test if that takes more than two seconds. The report goroutine records
// it once it has taken the fatal event, which an emit does not wait for.
func WaitForFatal(t *testing.T, publisher *publish.Publisher) {
	t.Helper()
	require.Eventually(t, func() bool { return publisher.Err() != nil }, 2*time.Second, time.Millisecond,
		"no fatal error recorded")
}
