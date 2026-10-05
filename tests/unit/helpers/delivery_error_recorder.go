package helpers

import (
	"sync"

	"github.com/easykafka/easykafka-go/publish"
)

// DeliveryErrorRecorder records what a publisher's WithDeliveryErrorFunc
// callback receives.
type DeliveryErrorRecorder struct {
	mu     sync.Mutex
	errors []publish.DeliveryError
}

// CallbackFunc is the callback to pass to publish.WithDeliveryErrorFunc.
func (r *DeliveryErrorRecorder) CallbackFunc(deliveryError publish.DeliveryError) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, deliveryError)
}

// Errors returns what the callback received, in order.
func (r *DeliveryErrorRecorder) Errors() []publish.DeliveryError {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]publish.DeliveryError(nil), r.errors...)
}
