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
	return int32(step)
}

// GetOriginalTopic reads the original topic from message headers.
func GetOriginalTopic(msg *types.Message) string {
	if msg == nil || msg.Headers == nil {
		return ""
	}
	return msg.Headers[HeaderOriginalTopic]
}

// BuildRetryHeaders creates retry metadata headers for a message being sent to the retry queue.
func BuildRetryHeaders(msg *types.Message, attempt int, retryTime time.Time, handlerErr error) map[string]string {
	headers := make(map[string]string)

	// Copy existing application-level headers (skip easykafka. prefixed ones)
	if msg.Headers != nil {
		for k, v := range msg.Headers {
			if !isRetryHeader(k) {
				headers[k] = v
			}
		}
	}

	// Set retry metadata
	headers[HeaderRetryAttempt] = strconv.Itoa(attempt)
	headers[HeaderRetryTime] = retryTime.Format(time.RFC3339)
	headers[HeaderRetryStep] = strconv.Itoa(int(GetRetryStep(msg)))
	headers[HeaderErrorCode] = classifyError(handlerErr)
	headers[HeaderErrorMessage] = handlerErr.Error()
	headers[HeaderOriginalTopic] = resolveOriginalTopic(msg)
	headers[HeaderOriginalPartition] = strconv.Itoa(int(msg.Partition))
	headers[HeaderOriginalOffset] = strconv.FormatInt(msg.Offset, 10)
	headers[HeaderFailedAt] = time.Now().Format(time.RFC3339)

	return headers
}

// BuildDLQHeaders creates retry metadata headers for a message being sent to the DLQ.
// Same as retry headers but preserves final attempt count.
func BuildDLQHeaders(msg *types.Message, attempt int, handlerErr error) map[string]string {
	headers := make(map[string]string)

	// Copy existing application-level headers
	if msg.Headers != nil {
		for k, v := range msg.Headers {
			if !isRetryHeader(k) {
				headers[k] = v
			}
		}
	}

	headers[HeaderRetryAttempt] = strconv.Itoa(attempt)
	headers[HeaderErrorCode] = classifyError(handlerErr)
	headers[HeaderErrorMessage] = handlerErr.Error()
	headers[HeaderOriginalTopic] = resolveOriginalTopic(msg)
	headers[HeaderOriginalPartition] = strconv.Itoa(int(msg.Partition))
	headers[HeaderOriginalOffset] = strconv.FormatInt(msg.Offset, 10)
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

// resolveOriginalTopic returns the original topic, preserving it through retries.
func resolveOriginalTopic(msg *types.Message) string {
	if orig := msg.Headers[HeaderOriginalTopic]; orig != "" {
		return orig
	}
	return msg.Topic
}

// classifyError returns a simple error code based on the error type.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	// Could be extended with more specific error classification
	return "HANDLER_ERROR"
}
