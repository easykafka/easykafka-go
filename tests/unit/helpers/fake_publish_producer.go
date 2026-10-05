package helpers

import (
	"context"
	"sync"
	"time"

	"github.com/easykafka/easykafka-go/internal/publishdriver"
)

// FakePublishProducer is a scripted publishdriver.Producer. It records what is
// produced, and emits only the reports and events a test asks for, so a test
// decides their order, their timing and their outcome.
//
// Reports is unbuffered: an emit returns once the publisher's report goroutine
// has taken the event, which then settles it.
type FakePublishProducer struct {
	// ProduceErr, if set, is returned by Produce, and nothing is recorded.
	ProduceErr error
	// Partitions is what TopicPartitions returns for the topics it holds;
	// PartitionsErr, if set, is returned instead.
	Partitions    map[string]int
	PartitionsErr error

	reports chan publishdriver.Event

	mu            sync.Mutex
	records       []publishdriver.Record
	tokens        []any
	config        publishdriver.Config
	pingTopics    []string
	pingDeadlined bool
	closed        bool
}

// NewFakePublishProducer returns a fake with nothing produced.
func NewFakePublishProducer() *FakePublishProducer {
	return &FakePublishProducer{reports: make(chan publishdriver.Event)}
}

// Factory returns a producer factory that hands out this fake, recording the
// configuration it was given.
func (f *FakePublishProducer) Factory() func(publishdriver.Config) (publishdriver.Producer, error) {
	return func(config publishdriver.Config) (publishdriver.Producer, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.config = config
		return f, nil
	}
}

// Produce records the record and its token, or returns ProduceErr.
func (f *FakePublishProducer) Produce(record publishdriver.Record, token any) error {
	if f.ProduceErr != nil {
		return f.ProduceErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, record)
	f.tokens = append(f.tokens, token)
	return nil
}

// Reports returns the channel the emit methods write to.
func (f *FakePublishProducer) Reports() <-chan publishdriver.Event {
	return f.reports
}

// Flush reports nothing left.
func (f *FakePublishProducer) Flush(time.Duration) int { return 0 }

// Purge does nothing.
func (f *FakePublishProducer) Purge() error { return nil }

// Len is the number of records produced.
func (f *FakePublishProducer) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// TopicPartitions returns Partitions, filtered to the requested topics, or
// PartitionsErr. It records the topics and whether ctx had a deadline.
func (f *FakePublishProducer) TopicPartitions(ctx context.Context, topics []string) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, f.pingDeadlined = ctx.Deadline()
	f.pingTopics = append([]string(nil), topics...)
	if f.PartitionsErr != nil {
		return nil, f.PartitionsErr
	}
	found := map[string]int{}
	for _, topic := range topics {
		if count, ok := f.Partitions[topic]; ok {
			found[topic] = count
		}
	}
	return found, nil
}

// Close closes Reports, which ends the publisher's report goroutine.
// Idempotent.
func (f *FakePublishProducer) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.reports)
	}
}

// Records returns what was produced, in order.
func (f *FakePublishProducer) Records() []publishdriver.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publishdriver.Record(nil), f.records...)
}

// Config returns the configuration the factory was given.
func (f *FakePublishProducer) Config() publishdriver.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.config
}

// PingTopics returns the topics of the last TopicPartitions call, and whether
// its context had a deadline.
func (f *FakePublishProducer) PingTopics() (topics []string, hadDeadline bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingTopics, f.pingDeadlined
}

// Succeed emits a success report for the index-th record produced.
func (f *FakePublishProducer) Succeed(index int, partition int32, offset int64) {
	f.Emit(publishdriver.Report{Token: f.token(index), Partition: partition, Offset: offset})
}

// Fail emits a failure report for the index-th record produced.
func (f *FakePublishProducer) Fail(index int, partition int32, err *publishdriver.KafkaError) {
	f.Emit(publishdriver.Report{Token: f.token(index), Partition: partition, Offset: -1, Err: err})
}

// Emit sends one event to the publisher, returning once it has been taken.
func (f *FakePublishProducer) Emit(event publishdriver.Event) {
	f.reports <- event
}

func (f *FakePublishProducer) token(index int) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[index]
}
