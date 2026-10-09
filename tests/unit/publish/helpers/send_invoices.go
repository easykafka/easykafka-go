package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/stretchr/testify/require"
)

// SendInvoices sends count invoices through writer and returns their
// deliveries, in order.
func SendInvoices(t *testing.T, writer *publish.Writer[string, PublishInvoice], count int) []*publish.Delivery {
	t.Helper()
	deliveries := make([]*publish.Delivery, count)
	for index := range deliveries {
		delivery, err := writer.Send("k", NewPublishInvoice())
		require.NoError(t, err)
		deliveries[index] = delivery
	}
	return deliveries
}
