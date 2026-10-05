package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
)

// NewTestPublisher returns a publisher with only the required option set,
// writing to a fake producer.
func NewTestPublisher(t *testing.T) *publish.Publisher {
	t.Helper()
	publisher, _ := NewFakePublisher(t)
	return publisher
}
