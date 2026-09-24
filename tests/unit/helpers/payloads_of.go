package helpers

import "github.com/easykafka/easykafka-go/internal/types"

// PayloadsOf returns the messages' payloads as strings, in order.
func PayloadsOf(msgs []*types.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m.Payload)
	}
	return out
}
