package publish_test

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/publish/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishStringKey verifies that a string key becomes its bytes, and that
// an empty key stays empty and non-null rather than becoming a null key.
func TestPublishStringKey(t *testing.T) {
	encoded, err := publish.StringKey("player-42")
	require.NoError(t, err)
	assert.Equal(t, []byte("player-42"), encoded)

	empty, err := publish.StringKey("")
	require.NoError(t, err)
	assert.NotNil(t, empty, "an empty key must not become a null key")
	assert.Empty(t, empty)
}

// TestPublishBytesKey verifies that bytes pass through, and nil stays nil: a
// null key.
func TestPublishBytesKey(t *testing.T) {
	encoded, err := publish.BytesKey([]byte{0x00, 0xff})
	require.NoError(t, err)
	assert.Equal(t, []byte{0x00, 0xff}, encoded)

	null, err := publish.BytesKey(nil)
	require.NoError(t, err)
	assert.Nil(t, null)
}

// TestPublishInt64Key verifies that an int64 key is base-10 text that parses
// back to the same value, which is how easykafka-config-go's Int64Key decodes
// it (strconv.ParseInt, base 10).
func TestPublishInt64Key(t *testing.T) {
	for _, key := range []int64{0, 7, -7, 1234567890123, math.MaxInt64, math.MinInt64} {
		encoded, err := publish.Int64Key(key)
		require.NoError(t, err)
		assert.Equal(t, strconv.FormatInt(key, 10), string(encoded))

		decoded, err := strconv.ParseInt(string(encoded), 10, 64)
		require.NoError(t, err)
		assert.Equal(t, key, decoded)
	}
}

// TestPublishJSONValueMatchesEncodingJSON verifies that JSONValue produces
// exactly encoding/json's bytes, including the details where JSON encoders
// differ: a nil slice as null, sorted map keys and HTML escaping.
func TestPublishJSONValueMatchesEncodingJSON(t *testing.T) {
	invoice := helpers.NewPublishInvoice()

	encoded, err := publish.JSONValue(invoice)
	require.NoError(t, err)

	want, err := json.Marshal(invoice)
	require.NoError(t, err)
	assert.Equal(t, want, encoded)
	assert.Contains(t, string(encoded), `"lines":null`)
	assert.Contains(t, string(encoded), `"tags":{"a":"1","b":"2"}`)
	assert.Contains(t, string(encoded), "\\u003cb\\u003e\\u0026\\u003c/b\\u003e", "HTML is escaped, as encoding/json v1 does")
	assert.NotContains(t, string(encoded), "not encoded")
}

// TestPublishJSONValueAsTopicEncoder verifies that JSONValue, with its type
// argument spelled out, is assignable to Topic.EncodeValue.
func TestPublishJSONValueAsTopicEncoder(t *testing.T) {
	topic := publish.Topic[string, helpers.PublishInvoice]{
		Name:        "invoices",
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.JSONValue[helpers.PublishInvoice],
	}
	encoded, err := topic.EncodeValue(helpers.NewPublishInvoice())
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"id":"INV-1"`)
}

// TestPublishJSONValueReportsTheType verifies that an unencodable value fails
// with an error naming its type.
func TestPublishJSONValueReportsTheType(t *testing.T) {
	_, err := publish.JSONValue(make(chan int))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chan int")
}

// TestPublishRawValue verifies that bytes pass through, an empty value is
// allowed, and nil is refused because it would be a tombstone.
func TestPublishRawValue(t *testing.T) {
	raw := []byte{0x00, 0x01, 0xff, 0xfe} // arbitrary bytes, not even valid UTF-8
	encoded, err := publish.RawValue(raw)
	require.NoError(t, err)
	assert.Equal(t, raw, encoded)

	empty, err := publish.RawValue([]byte{})
	require.NoError(t, err)
	assert.NotNil(t, empty)

	_, err = publish.RawValue(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Delete")
}
