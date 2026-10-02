package unit

import (
	"errors"
	"fmt"
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishDeliveryErrorMessage verifies that the message names the topic,
// the partition and the cause.
func TestPublishDeliveryErrorMessage(t *testing.T) {
	deliveryError := &publish.DeliveryError{
		Topic:     "invoices",
		Partition: 3,
		Code:      "Local: Message timed out",
		Err:       fmt.Errorf("%w: Local: Message timed out", publish.ErrDeliveryTimeout),
	}
	message := deliveryError.Error()
	assert.Contains(t, message, "invoices")
	assert.Contains(t, message, "partition 3")
	assert.Contains(t, message, "not acknowledged within the delivery timeout")
}

// TestPublishDeliveryErrorWithoutPartition verifies the wording when no
// partition was ever chosen.
func TestPublishDeliveryErrorWithoutPartition(t *testing.T) {
	deliveryError := &publish.DeliveryError{Topic: "invoices", Partition: -1, Code: "Local: Unknown topic"}
	assert.Contains(t, deliveryError.Error(), "no partition")
	assert.Contains(t, deliveryError.Error(), "Local: Unknown topic", "the code stands in for a missing cause")
}

// TestPublishDeliveryErrorUnwraps verifies that errors.Is reaches the sentinel
// and errors.As the DeliveryError, through further wrapping.
func TestPublishDeliveryErrorUnwraps(t *testing.T) {
	cause := errors.New("broker said no")
	wrapped := fmt.Errorf("publishing invoice: %w", &publish.DeliveryError{
		Topic:     "invoices",
		Partition: 0,
		Err:       fmt.Errorf("%w: %w", publish.ErrNotDelivered, cause),
	})

	require.ErrorIs(t, wrapped, publish.ErrNotDelivered)
	require.ErrorIs(t, wrapped, cause)
	require.NotErrorIs(t, wrapped, publish.ErrDeliveryTimeout)

	var deliveryError *publish.DeliveryError
	require.ErrorAs(t, wrapped, &deliveryError)
	assert.Equal(t, "invoices", deliveryError.Topic)
}

// TestPublishSentinelsAreDistinct verifies that no two sentinels match each
// other, so errors.Is tells them apart.
func TestPublishSentinelsAreDistinct(t *testing.T) {
	sentinels := []error{
		publish.ErrClosed,
		publish.ErrQueueFull,
		publish.ErrDeliveryTimeout,
		publish.ErrNotDelivered,
		publish.ErrFatal,
		publish.ErrEncode,
		publish.ErrTopicNotFound,
	}
	for firstIndex, first := range sentinels {
		assert.Contains(t, first.Error(), "publish: ")
		for secondIndex, second := range sentinels {
			if firstIndex != secondIndex {
				assert.NotErrorIs(t, first, second)
			}
		}
	}
}
