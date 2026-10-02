package publish

import (
	"errors"
	"slices"
)

// Publisher writes records to Kafka through one librdkafka producer, and reads
// the delivery report of every record it accepts. Safe for concurrent use.
type Publisher struct {
	config config
}

// New validates the options and returns a publisher.
//
// Creating the librdkafka producer and reading its delivery reports is not
// implemented yet; for now New only builds and checks the configuration.
func New(options ...Option) (*Publisher, error) {
	built := defaultConfig()
	for _, option := range options {
		if option == nil {
			return nil, errors.New("option cannot be nil")
		}
		if err := option(&built); err != nil {
			return nil, err
		}
	}
	if err := built.validate(); err != nil {
		return nil, err
	}
	return &Publisher{config: built}, nil
}

// Bind returns the typed writer for a topic.
//
// It panics on an invalid Topic (an empty Name, or a nil encoder), because a
// running service cannot act on that. Binding the same topic more than once is
// allowed. The topic's Headers are copied, so changing the caller's slice later
// does not change what the writer sends.
func (p *Publisher) Bind[K, V any](topic Topic[K, V]) *Writer[K, V] {
	if err := topic.validate(); err != nil {
		panic(err)
	}
	topic.Headers = slices.Clone(topic.Headers)
	return &Writer[K, V]{publisher: p, topic: topic}
}
