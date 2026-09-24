package metadata

import (
	"strconv"
	"time"

	"github.com/easykafka/easykafka-go/internal/types"
)

// Retry header keys used in Kafka message headers for retry tracking.
const (
	HeaderRetryAttempt      = "easykafka.retry.attempt"
	HeaderRetryTime         = "easykafka.retry.time"
	HeaderRetryStep         = "easykafka.retry.step"
	HeaderErrorCode         = "easykafka.error.code"
	HeaderErrorMessage      = "easykafka.error.message"
	HeaderOriginalTopic     = "easykafka.original.topic"
	HeaderOriginalPartition = "easykafka.original.partition"
	HeaderOriginalOffset    = "easykafka.original.offset"
	HeaderFailedAt          = "easykafka.failed.at"
)

// GetRetryAttempt reads the retry attempt count from message headers.
// Returns 0 if not present (first failure).
func GetRetryAttempt(msg *types.Message) int {
	if msg == nil || msg.Headers == nil {
		return 0
	}
	val, ok := msg.Headers[HeaderRetryAttempt]
	if !ok {
		return 0
	}
	attempt, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	return attempt
}

// GetRetryTime reads the scheduled retry time from message headers.
// Returns zero time if not present.
func GetRetryTime(msg *types.Message) time.Time {
	if msg == nil || msg.Headers == nil {
		return time.Time{}
	}
	val, ok := msg.Headers[HeaderRetryTime]
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, val)
	if err != nil {
		return time.Time{}
	}
	return t
}

// GetRetryStep reads the retry step number from message headers.
// Returns 0 if not present (start from beginning).
func GetRetryStep(msg *types.Message) int32 {
	if msg == nil || msg.Headers == nil {
		return 0
	}
	val, ok := msg.Headers[HeaderRetryStep]
	if !ok {
		return 0
	}
	step, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	// TODO(lint): gosec G109 — validate that the parsed step fits in int32
	// instead of truncating silently.
	return int32(step) //nolint:gosec // see TODO above
}

// GetErrorCode reads the error code of the failure that republished this
// message. Returns the empty string if not present (first delivery).
func GetErrorCode(msg *types.Message) string {
	if msg == nil || msg.Headers == nil {
		return ""
	}
	return msg.Headers[HeaderErrorCode]
}

// GetOriginalTopic reads the original topic from message headers.
func GetOriginalTopic(msg *types.Message) string {
	if msg == nil || msg.Headers == nil {
		return ""
	}
	return msg.Headers[HeaderOriginalTopic]
}

// BuildRetryHeaders creates retry metadata headers for a message being sent to the retry queue.
func BuildRetryHeaders(msg *types.Message, attempt int, retryTime time.Time, f types.Failure) map[string]string {
	headers := buildFailureHeaders(msg, attempt, f)
	headers[HeaderRetryTime] = retryTime.Format(time.RFC3339)
	return headers
}

// BuildDLQHeaders creates retry metadata headers for a message being sent to the DLQ.
// Same as retry headers, minus the retry time: the record is not due again.
func BuildDLQHeaders(msg *types.Message, attempt int, f types.Failure) map[string]string {
	return buildFailureHeaders(msg, attempt, f)
}

// buildFailureHeaders creates the headers a retry and a DLQ record share.
//
// Headers the record arrived with that are not the library's pass through
// unchanged. Of the library's own, only the step and the code are the
// handler's to set, through the Failure; everything else is written here.
func buildFailureHeaders(msg *types.Message, attempt int, f types.Failure) map[string]string {
	headers := make(map[string]string)

	// Copy existing application-level headers (skip easykafka. prefixed ones)
	for k, v := range msg.Headers {
		if !isRetryHeader(k) {
			headers[k] = v
		}
	}

	headers[HeaderRetryAttempt] = strconv.Itoa(attempt)
	if step := resolveStep(msg, f); step != 0 {
		headers[HeaderRetryStep] = strconv.Itoa(int(step))
	}
	headers[HeaderErrorCode] = resolveErrorCode(f)
	headers[HeaderErrorMessage] = errorMessage(f.Err)
	headers[HeaderOriginalTopic] = resolveOriginalTopic(msg)
	headers[HeaderOriginalPartition] = resolveOriginalHeader(msg, HeaderOriginalPartition,
		strconv.Itoa(int(msg.Partition)))
	headers[HeaderOriginalOffset] = resolveOriginalHeader(msg, HeaderOriginalOffset,
		strconv.FormatInt(msg.Offset, 10))
	headers[HeaderFailedAt] = time.Now().Format(time.RFC3339)

	return headers
}

// isRetryHeader returns true if the header key is an easykafka retry header.
func isRetryHeader(key string) bool {
	switch key {
	case HeaderRetryAttempt, HeaderRetryTime, HeaderRetryStep,
		HeaderErrorCode, HeaderErrorMessage,
		HeaderOriginalTopic, HeaderOriginalPartition, HeaderOriginalOffset,
		HeaderFailedAt:
		return true
	}
	return false
}

// resolveStep returns the step the handler reported, or the step the record
// arrived with when it reported none. A resume point must never be silently
// lost: losing it repeats work that already succeeded. 0 means there is none.
func resolveStep(msg *types.Message, f types.Failure) int32 {
	if f.Step != 0 {
		return f.Step
	}
	return GetRetryStep(msg)
}

// resolveErrorCode returns the code the handler reported, or the library's
// fallback when it reported none. Unlike the step, the inbound code is never
// carried forward: it describes the previous failure, not this one.
func resolveErrorCode(f types.Failure) string {
	if f.Code != "" {
		return f.Code
	}
	return classifyError(f.Err)
}

// resolveOriginalTopic returns the original topic, preserving it through retries.
func resolveOriginalTopic(msg *types.Message) string {
	return resolveOriginalHeader(msg, HeaderOriginalTopic, msg.Topic)
}

// resolveOriginalHeader returns the inbound value of an easykafka.original.*
// header, or current when the record carries none — which is the case only on
// the first hop, where the current record is the original. Taking the current
// value on a later hop would name a position on the retry topic instead.
func resolveOriginalHeader(msg *types.Message, key, current string) string {
	if orig := msg.Headers[key]; orig != "" {
		return orig
	}
	return current
}

// errorMessage returns the text of err, or the empty string for nil. The engine
// never passes a nil error, but a strategy can be driven directly.
func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// classifyError returns a simple error code based on the error type.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	// Could be extended with more specific error classification
	return "HANDLER_ERROR"
}
