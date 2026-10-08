package helpers

import "github.com/easykafka/easykafka-go/internal/publishdriver"

// HeadersOf returns a record's headers as a map, for looking one up by key. A
// repeated key keeps its last value, as the consumer's Message.Headers does.
func HeadersOf(record publishdriver.Record) map[string]string {
	headers := make(map[string]string, len(record.Headers))
	for _, header := range record.Headers {
		headers[header.Key] = string(header.Value)
	}
	return headers
}
