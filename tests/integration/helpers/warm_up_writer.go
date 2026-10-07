package helpers

import (
	"context"
	"strconv"
	"testing"

	"github.com/easykafka/easykafka-go/publish"
)

// WarmUpWriter sends records through writer as fast as it accepts them, and
// waits until every one is acknowledged.
func WarmUpWriter(tb testing.TB, writer *publish.Writer[string, PublishInvoice], records int) {
	tb.Helper()

	deliveries := make([]*publish.Delivery, 0, records)
	for index := range records {
		delivery, err := writer.Send("player-"+strconv.Itoa(index%1000), NewPublishInvoice("warm-up"))
		if err != nil {
			tb.Fatalf("warming up: %v", err)
		}
		deliveries = append(deliveries, delivery)
	}
	if err := publish.WaitAll(context.Background(), deliveries...); err != nil {
		tb.Fatalf("warming up: %v", err)
	}
}
