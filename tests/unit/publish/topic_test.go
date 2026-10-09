package publish_test

import (
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/publish/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishBindReturnsWriterForTopic verifies that a valid topic binds, with
// K and V inferred from the Topic.
func TestPublishBindReturnsWriterForTopic(t *testing.T) {
	writer := helpers.NewTestPublisher(t).Bind(helpers.PublishInvoiceTopic())
	require.NotNil(t, writer)
	assert.Equal(t, "invoices", writer.Topic())
}

// TestPublishBindSameTopicTwice verifies that a topic may be bound more than
// once, for example with different value types.
func TestPublishBindSameTopicTwice(t *testing.T) {
	publisher := helpers.NewTestPublisher(t)
	first := publisher.Bind(helpers.PublishInvoiceTopic())
	second := publisher.Bind(publish.Topic[string, []byte]{
		Name:        "invoices",
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.RawValue,
	})
	assert.Equal(t, first.Topic(), second.Topic())
}

// TestPublishBindPanicsOnInvalidTopic verifies that each missing field panics
// with an error naming the topic and the field.
func TestPublishBindPanicsOnInvalidTopic(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*publish.Topic[string, helpers.PublishInvoice])
		want   string
	}{
		{name: "no name", modify: func(topic *publish.Topic[string, helpers.PublishInvoice]) { topic.Name = "" }, want: "the Name field is required"},
		{name: "no key encoder", modify: func(topic *publish.Topic[string, helpers.PublishInvoice]) { topic.EncodeKey = nil }, want: "the EncodeKey field is required"},
		{name: "no value encoder", modify: func(topic *publish.Topic[string, helpers.PublishInvoice]) { topic.EncodeValue = nil }, want: "the EncodeValue field is required"},
	}
	publisher := helpers.NewTestPublisher(t)
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			topic := helpers.PublishInvoiceTopic()
			testCase.modify(&topic)

			recovered := helpers.RecoverPanic(func() { publisher.Bind(topic) })
			require.NotNil(t, recovered, "Bind must panic")
			err, isError := recovered.(error)
			require.True(t, isError, "the panic value must be an error, got %T", recovered)
			assert.Contains(t, err.Error(), testCase.want)
			assert.Contains(t, err.Error(), `invalid topic "`+topic.Name+`"`)
		})
	}
}

// TestPublishBindReportsEveryProblem verifies that one panic names every
// missing field at once.
func TestPublishBindReportsEveryProblem(t *testing.T) {
	recovered := helpers.RecoverPanic(func() {
		helpers.NewTestPublisher(t).Bind(publish.Topic[string, string]{Name: "bare"})
	})
	err, isError := recovered.(error)
	require.True(t, isError)
	assert.Contains(t, err.Error(), `"bare"`)
	assert.Contains(t, err.Error(), "the EncodeKey field is required")
	assert.Contains(t, err.Error(), "the EncodeValue field is required")
	assert.NotContains(t, err.Error(), "the Name field is required")
	assert.NotErrorIs(t, err, publish.ErrEncode, "a misconfigured topic is not an encoding failure")
}
