package sharedhelpers

import (
	"sync"

	"github.com/easykafka/easykafka-go/publish"
)

// PublishDeliveryErrorRecorder records what a publisher's
// WithDeliveryErrorFunc callback receives.
type PublishDeliveryErrorRecorder struct {
	mu     sync.Mutex
	errors []publish.DeliveryError
}

// CallbackFunc is the callback to pass to publish.WithDeliveryErrorFunc.
func (r *PublishDeliveryErrorRecorder) CallbackFunc(deliveryError publish.DeliveryError) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, deliveryError)
}

// Errors returns what the callback received, in order.
func (r *PublishDeliveryErrorRecorder) Errors() []publish.DeliveryError {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]publish.DeliveryError(nil), r.errors...)
}
