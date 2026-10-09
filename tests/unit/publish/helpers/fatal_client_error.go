package helpers

import "github.com/easykafka/easykafka-go/internal/publish/publishdriver"

// FatalClientError is a fatal client error, as the driver translates one, with
// librdkafka's generic code for a producer that has failed fatally.
func FatalClientError() publishdriver.ClientError {
	return publishdriver.ClientError{Err: &publishdriver.KafkaError{
		Code: "Local: Fatal error", Message: "Local: Fatal error", Fatal: true,
		Sentinel: publishdriver.ErrFatal,
	}}
}
