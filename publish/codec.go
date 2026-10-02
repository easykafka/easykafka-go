package publish

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Topic.EncodeKey and Topic.EncodeValue are plain function fields, so a topic
// can always supply its own encoding. The functions below cover the common
// cases.

// StringKey encodes a string key as its bytes. An empty string stays an empty,
// non-null key; it never becomes a null key. It never fails.
func StringKey(key string) ([]byte, error) {
	if key == "" {
		return []byte{}, nil
	}
	return []byte(key), nil
}

// BytesKey passes a key through unchanged. A nil key is a null key, which
// librdkafka's key-hashing partitioners place on a random partition. It never
// fails.
func BytesKey(key []byte) ([]byte, error) {
	return key, nil
}

// Int64Key encodes an int64 key as base-10 text, which easykafka-config-go's
// Int64Key decodes back. It never fails.
func Int64Key(key int64) ([]byte, error) {
	return strconv.AppendInt(nil, key, 10), nil //nolint:mnd // base 10
}

// JSONValue encodes a value as JSON.
//
// It is generic because V differs per topic, but it is an ordinary function
// assignable to Topic.EncodeValue:
//
//	EncodeValue: publish.JSONValue[Invoice]
//
// Spell out the type argument. On go1.27.0 an uninstantiated generic function
// from another package, used as a composite-literal field value, crashes the
// compiler ("internal compiler error: ... is not assignable to ...").
//
// This uses encoding/json (v1) deliberately, so records are byte-for-byte what
// json.Marshal produces: v2 differs in details such as nil slices and HTML
// escaping. A topic that wants v2, or a generated encoder, supplies its own
// function.
func JSONValue[V any](value V) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding %T as JSON: %w", value, err)
	}
	return encoded, nil
}

// RawValue passes already-encoded bytes through unchanged. A nil value is an
// error: on Kafka a nil value is a tombstone, and a tombstone is written only on
// purpose, with Delete or SendDelete. An empty, non-nil value is allowed.
func RawValue(value []byte) ([]byte, error) {
	if value == nil {
		return nil, errors.New("nil value: a nil value is a tombstone, write it with Delete or SendDelete")
	}
	return value, nil
}
