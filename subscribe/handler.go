package subscribe

import (
	"github.com/easykafka/easykafka-go/internal/subscribe/types"
)

// Handler is a re-export from internal/subscribe/types
type Handler = types.Handler

// BatchHandler is a re-export from internal/subscribe/types
type BatchHandler = types.BatchHandler

// ErrorStrategy is a re-export from internal/subscribe/types
type ErrorStrategy = types.ErrorStrategy

// Message is a re-export from internal/subscribe/types
type Message = types.Message

// Failure is how a handler reports that a message failed: the error, and
// optionally a resume point and a domain error code. Re-export from
// internal/subscribe/types.
type Failure = types.Failure

// Batch is what a BatchHandler is given: the polled messages, each paired with
// the verdict the handler records for it. Re-export from internal/subscribe/types.
type Batch = types.Batch

// BatchItem is one message of a Batch and its verdict. Re-export from
// internal/subscribe/types.
type BatchItem = types.BatchItem

// NewBatch builds a Batch over msgs, so a batch handler can be tested without a
// broker: run the handler over it, then check which items came back failed.
func NewBatch(msgs []Message) *Batch {
	return types.NewBatch(msgs)
}

var (
	// ErrPermanent marks a failure that reprocessing cannot fix. The retry
	// strategy sends it straight to the DLQ instead of walking the retry
	// ladder. Wrap it with %w, not %v, or the marker is lost:
	//
	//	item.Fail(subscribe.Failure{Err: fmt.Errorf("%w: unmarshal: %v", subscribe.ErrPermanent, err)})
	ErrPermanent = types.ErrPermanent

	// ErrUnspecified stands in for a failure a handler recorded without an
	// error. The message is still routed, under this error.
	ErrUnspecified = types.ErrUnspecified
)
