package publishdriver

import "errors"

// Sentinels for the ways a record can fail, which the publish package
// re-exports. They live here so the driver can attach the matching one to each
// error it returns (KafkaError.Sentinel, or ErrClosed itself), and publish only
// passes errors through. The publish package documents what each one means.
var (
	ErrClosed          = errors.New("publish: publisher is closed")
	ErrQueueFull       = errors.New("publish: local producer queue is full")
	ErrDeliveryTimeout = errors.New("publish: not acknowledged within the delivery timeout")
	ErrNotDelivered    = errors.New("publish: purged before delivery was confirmed")
	ErrFatal           = errors.New("publish: producer failed fatally")
)
