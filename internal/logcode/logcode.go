// Package logcode holds the codes the library puts on its notable log lines.
//
// A code goes in its own field, Field, next to a readable message:
//
//	logger.Error().
//	    Str(logcode.Field, logcode.ProducerRecordsDropped).
//	    Msg("retry/DLQ records not delivered before the producer closed; they are dropped")
//
// The message is for people and may be reworded; the code is for searches and
// alerts and does not change. A code is descriptive and starts with "EK_", so it
// is unique in logs shared with other libraries.
//
// Every code is listed in the README's "Log codes" section, with what it means
// and what to do about it. Add the row when adding a code here.
package logcode

// Field is the log field that carries the code.
const Field = "ek_code"

// Messages lost or written off.
const (
	// ProducerRecordsDropped: a retry or DLQ producer closed with records it
	// had not delivered within its flush timeout. Those records are dropped, and
	// their source offsets were already committed, so the messages are lost. The
	// "unflushed" field says how many. Error.
	ProducerRecordsDropped = "EK_PRODUCER_RECORDS_DROPPED"

	// ProducerDeliveryFailed: the broker never took a retry or DLQ write. Its
	// source offset was already committed, so the message is lost. One line per
	// record. Error.
	ProducerDeliveryFailed = "EK_PRODUCER_DELIVERY_FAILED"

	// RetryWriteFailed: a message could not even be queued for the retry topic
	// — a full local queue, say. The retry strategy returns the error and the
	// consumer stops. Error.
	RetryWriteFailed = "EK_RETRY_WRITE_FAILED"

	// DLQWriteFailed: as RetryWriteFailed, for the DLQ. Error.
	DLQWriteFailed = "EK_DLQ_WRITE_FAILED"

	// DLQMaxAttempts: a message failed its last allowed attempt and is sent to
	// the DLQ. Error.
	DLQMaxAttempts = "EK_DLQ_MAX_ATTEMPTS"

	// DLQPermanent: a message failed with ErrPermanent and is sent to the DLQ
	// without retrying. Error.
	DLQPermanent = "EK_DLQ_PERMANENT"

	// MessageSkipped: the skip strategy wrote off a failed message. Its offset
	// is committed and it is not processed again. Warning.
	MessageSkipped = "EK_MESSAGE_SKIPPED"
)

// The consumer stops.
const (
	// PollFatal: polling Kafka returned a fatal error, and the consumer stops.
	// Error.
	PollFatal = "EK_POLL_FATAL"

	// StoppedByStrategy: the error strategy returned an error, and the consumer
	// stops. That is how fail-fast stops a consumer — it logs its own line, with
	// the message's position, just before this one — and how the retry strategy
	// stops it when a retry or DLQ write fails. Error.
	StoppedByStrategy = "EK_STOPPED_BY_STRATEGY"

	// OffsetStoreFailed: an offset could not be stored for a reason other than
	// the partition being revoked, and the consumer stops rather than move past
	// the message. Error.
	OffsetStoreFailed = "EK_OFFSET_STORE_FAILED"
)

// Bugs in application code.
const (
	// HandlerPanic: a handler panicked. The panic is recovered and the message —
	// in batch mode, the whole batch — goes to the error strategy. Error.
	HandlerPanic = "EK_HANDLER_PANIC"

	// FailureWithoutError: a handler reported a Failure with no Err. It is
	// routed under ErrUnspecified anyway. Warning.
	FailureWithoutError = "EK_FAILURE_WITHOUT_ERROR"

	// DeliveryCallbackPanic: the WithDeliveryErrorFunc callback panicked. The
	// panic is recovered so the producer keeps draining. Error.
	DeliveryCallbackPanic = "EK_DELIVERY_CALLBACK_PANIC"
)

// Commits that failed: messages are replayed on restart, not lost.
const (
	// CommitFailed: committing stored offsets failed; they stay stored and a
	// later commit covers them. The "commit" field says which: per_message,
	// batch, final, revoke or auto. Warning.
	CommitFailed = "EK_COMMIT_FAILED"
)

// Broker and rebalance trouble.
const (
	// BrokerDisconnected: the connection to the brokers was lost. librdkafka
	// reconnects on its own. Warning.
	BrokerDisconnected = "EK_BROKER_DISCONNECTED"

	// BrokerReconnected: messages are arriving again after BrokerDisconnected.
	// Info.
	BrokerReconnected = "EK_BROKER_RECONNECTED"

	// KafkaError: the client reported a non-fatal error that is not a lost
	// connection. Consumption continues. Warning.
	KafkaError = "EK_KAFKA_ERROR"

	// RebalanceFailed: assigning or unassigning partitions during a rebalance
	// failed. Error.
	RebalanceFailed = "EK_REBALANCE_FAILED"

	// ConsumerCloseFailed: closing the Kafka consumer at shutdown failed. Error.
	ConsumerCloseFailed = "EK_CONSUMER_CLOSE_FAILED"
)

// The publish package.
const (
	// PublishDeliveryFailed: a record a publisher accepted was not
	// acknowledged. Its delivery resolves with the error, and the
	// WithDeliveryErrorFunc callback has been called. One line per record.
	// Error.
	PublishDeliveryFailed = "EK_PUBLISH_DELIVERY_FAILED"

	// PublishCallbackPanic: a publisher's WithDeliveryErrorFunc callback
	// panicked. The panic is recovered, the record's delivery still resolves,
	// and later reports are read as before. Error.
	PublishCallbackPanic = "EK_PUBLISH_CALLBACK_PANIC"

	// PublishUnmatchedReport: a delivery report matched no record waiting for
	// one: a second report for a record already resolved, or a report without
	// the publisher's token. It is dropped, and the record's first outcome
	// stands. It means librdkafka or the library broke the rule that every
	// record is reported exactly once. Error.
	PublishUnmatchedReport = "EK_PUBLISH_UNMATCHED_REPORT"

	// PublishBrokerDown: a publisher lost its connection to the brokers.
	// librdkafka reconnects on its own; records wait in the queue until their
	// delivery timeout. Logged once per outage. Warning.
	PublishBrokerDown = "EK_PUBLISH_BROKER_DOWN"

	// PublishBrokerRestored: a record was acknowledged again after
	// PublishBrokerDown. The "suppressed" field counts the connection errors
	// not logged in between. Info.
	PublishBrokerRestored = "EK_PUBLISH_BROKER_RESTORED"

	// PublishKafkaError: a publisher's client reported an error that is
	// neither about one record nor a lost connection. Publishing continues.
	// Warning.
	PublishKafkaError = "EK_PUBLISH_KAFKA_ERROR"
)
