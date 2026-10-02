package publish

import (
	"errors"
	"fmt"
)

// Topic declares one Kafka topic and how its keys and values are encoded.
// Declare it once, and turn it into a typed Writer with Publisher.Bind.
type Topic[K, V any] struct {
	// Name is the topic name. Required.
	Name string
	// EncodeKey turns a key into the record key. Required. StringKey,
	// BytesKey and Int64Key cover the common cases.
	EncodeKey func(key K) ([]byte, error)
	// EncodeValue turns a value into the record value. Required. JSONValue and
	// RawValue cover the common cases. It must not return nil: a tombstone is
	// written with Delete or SendDelete.
	EncodeValue func(value V) ([]byte, error)
	// Headers are added to every record of the topic, before the headers given
	// to each call.
	Headers []Header
}

// Header is one record header. A slice of them keeps order and repeated keys,
// which a map would not.
type Header struct {
	Key   string
	Value []byte
}

// validate reports every problem with the topic at once, naming it.
func (t Topic[K, V]) validate() error {
	var problems []error
	if t.Name == "" {
		problems = append(problems, errors.New("the Name field is required"))
	}
	if t.EncodeKey == nil {
		problems = append(problems, errors.New("the EncodeKey field is required"))
	}
	if t.EncodeValue == nil {
		problems = append(problems, errors.New("the EncodeValue field is required"))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("publish: invalid topic %q: %w", t.Name, errors.Join(problems...))
}
