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
//
// For shutdown it behaves as librdkafka does, minus the network: Len and Flush
// count the records not yet reported, Flush waits for that count to reach zero,
// and Purge reports every pending record as purged, from a goroutine of its own.
// Close stops any purge reports not yet sent, as librdkafka drops reports that
// are not out when it closes.
type FakePublishProducer struct {
	// ProduceErr, if set, is returned by Produce, and nothing is recorded.
	ProduceErr error
	// Partitions is the fake cluster: topic → partition count. TopicPartitions
	// returns the requested topics found here.
	Partitions map[string]int
	// PartitionsErr, if set, is returned by TopicPartitions instead.
	PartitionsErr error
	// PurgeReportDelay, if set, delays each purge report Purge sends, so a test
	// can check that Close waits for the last one.
	PurgeReportDelay time.Duration
	// AutoAcknowledge, if set, makes Produce emit a success report for every
	// record it accepts, from a goroutine of its own, as a healthy broker
	// would. Set it before the first Produce.
	AutoAcknowledge bool

	// reports is the unbuffered channel Reports returns. The emit methods
	// write to it; Close closes it.
	reports chan publishdriver.Event
	// stop is closed by Close, so that emits still waiting give up.
	stop chan struct{}
	// emitting counts the emits in progress, so Close can close reports only
	// once none is left to send on it.
	emitting sync.WaitGroup

	// mu guards every field below.
	mu sync.Mutex
	// records is every record Produce accepted, in order.
	records []publishdriver.Record
	// tokens is the token passed with each record, at the same index, so a
	// test can emit a report for the n-th record.
	tokens []any
	// pending holds the token of every record not yet reported: what Len and
	// Flush count, and what Purge reports.
	pending map[any]bool
	// config is what the factory was given, so a test can check what the
	// options produced.
	config publishdriver.Config
	// pingTopics and pingDeadlined record the last TopicPartitions call: the
	// topics asked for, and whether its context had a deadline. pingCalls
	// counts the calls.
	pingTopics    []string
	pingDeadlined bool
	pingCalls     int
	// flushTimeouts is the timeout of every Flush call, in order.
	flushTimeouts []time.Duration
	// purgeCalls counts the Purge calls.
	purgeCalls int
	// closed makes Close idempotent, and stops new emits.
	closed bool
}

// NewFakePublishProducer returns a fake with nothing produced.
func NewFakePublishProducer() *FakePublishProducer {
	return &FakePublishProducer{
		reports: make(chan publishdriver.Event),
		stop:    make(chan struct{}),
		pending: map[any]bool{},
	}
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

// Produce records the record and its token, or returns ProduceErr. With
// AutoAcknowledge, it then reports the record as acknowledged, at the offset
// that is its index.
func (f *FakePublishProducer) Produce(record publishdriver.Record, token any) error {
	if f.ProduceErr != nil {
		return f.ProduceErr
	}
	f.mu.Lock()
	f.records = append(f.records, record)
	f.tokens = append(f.tokens, token)
	f.pending[token] = true
	offset := int64(len(f.records) - 1)
	f.mu.Unlock()

	if f.AutoAcknowledge {
		go f.Emit(publishdriver.Report{Token: token, Partition: 0, Offset: offset})
	}
	return nil
}

// Reports returns the channel the emit methods write to.
func (f *FakePublishProducer) Reports() <-chan publishdriver.Event {
	return f.reports
}

// Flush waits up to timeout for every pending record to be reported, and
// returns how many are left. It records the timeout.
func (f *FakePublishProducer) Flush(timeout time.Duration) int {
	f.mu.Lock()
	f.flushTimeouts = append(f.flushTimeouts, timeout)
	f.mu.Unlock()

	deadline := time.Now().Add(timeout)
	for {
		remaining := f.Len()
		if remaining == 0 || !time.Now().Before(deadline) {
			return remaining
		}
		time.Sleep(time.Millisecond)
	}
}

// Purge reports every pending record as purged, from a goroutine of its own,
// each after PurgeReportDelay. It returns at once, as a non-blocking purge does.
func (f *FakePublishProducer) Purge() error {
	f.mu.Lock()
	f.purgeCalls++
	var tokens []any
	for token := range f.pending {
		tokens = append(tokens, token)
	}
	f.mu.Unlock()

	go func() {
		for _, token := range tokens {
			select {
			case <-time.After(f.PurgeReportDelay):
			case <-f.stop:
				return
			}
			f.Emit(publishdriver.Report{Token: token, Partition: -1, Offset: -1, Err: &publishdriver.KafkaError{
				Code:     "Local: Purged in queue",
				Message:  "Local: Purged in queue",
				Sentinel: publishdriver.ErrNotDelivered,
			}})
		}
	}()
	return nil
}

// Len is the number of records not yet reported.
func (f *FakePublishProducer) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// TopicPartitions returns Partitions, filtered to the requested topics, or
// PartitionsErr. It records the call.
func (f *FakePublishProducer) TopicPartitions(ctx context.Context, topics []string) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingCalls++
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

// Close stops the emits still waiting, then closes Reports, which ends the
// publisher's report goroutine. Idempotent.
func (f *FakePublishProducer) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	close(f.stop)
	f.mu.Unlock()

	f.emitting.Wait()
	close(f.reports)
}

// Records returns what was produced, in order.
func (f *FakePublishProducer) Records() []publishdriver.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publishdriver.Record(nil), f.records...)
}

// RecordsTo returns what was produced to topic, in order.
func (f *FakePublishProducer) RecordsTo(topic string) []publishdriver.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var records []publishdriver.Record
	for _, record := range f.records {
		if record.Topic == topic {
			records = append(records, record)
		}
	}
	return records
}

// Produced is the number of records produced, reported or not.
func (f *FakePublishProducer) Produced() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
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

// PingCalls is the number of TopicPartitions calls.
func (f *FakePublishProducer) PingCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingCalls
}

// FlushTimeouts returns the timeout of every Flush call, in order.
func (f *FakePublishProducer) FlushTimeouts() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.flushTimeouts...)
}

// PurgeCalls is the number of Purge calls.
func (f *FakePublishProducer) PurgeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.purgeCalls
}

// Succeed emits a success report for the index-th record produced.
func (f *FakePublishProducer) Succeed(index int, partition int32, offset int64) {
	f.Emit(publishdriver.Report{Token: f.token(index), Partition: partition, Offset: offset})
}

// Fail emits a failure report for the index-th record produced.
func (f *FakePublishProducer) Fail(index int, partition int32, err *publishdriver.KafkaError) {
	f.Emit(publishdriver.Report{Token: f.token(index), Partition: partition, Offset: -1, Err: err})
}

// Emit sends one event to the publisher, returning once it has been taken. A
// report marks its record reported. After Close, it sends nothing.
func (f *FakePublishProducer) Emit(event publishdriver.Event) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.emitting.Add(1)
	f.mu.Unlock()
	defer f.emitting.Done()

	select {
	case f.reports <- event:
	case <-f.stop:
		return
	}
	if report, isReport := event.(publishdriver.Report); isReport {
		f.mu.Lock()
		delete(f.pending, report.Token)
		f.mu.Unlock()
	}
}

func (f *FakePublishProducer) token(index int) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[index]
}
