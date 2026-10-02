package helpers

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/stretchr/testify/require"
)

// NewTestPublisher returns a publisher with only the required option set.
func NewTestPublisher(t *testing.T) *publish.Publisher {
	t.Helper()
	publisher, err := publish.New(publish.WithBrokers(PublishBroker))
	require.NoError(t, err)
	return publisher
}
