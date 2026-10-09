package helpers

import (
	"context"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/stretchr/testify/require"
)

// WaitForDelivery waits for a delivery's outcome, failing the test if it is not
// resolved within two seconds.
func WaitForDelivery(t *testing.T, delivery *publish.Delivery) (publish.Result, error) {
	t.Helper()
	select {
	case <-delivery.Done():
	case <-time.After(2 * time.Second):
		require.FailNow(t, "delivery not resolved within 2s")
	}
	return delivery.Wait(context.Background())
}
