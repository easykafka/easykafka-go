package publishdriver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// Producer is the part of librdkafka the publisher uses.
type Producer interface {
	// Produce enqueues one record. token comes back on its Report. It never
	// blocks: a full queue fails at once. An error is a *KafkaError, or
	// ErrClosed.
	Produce(record Record, token any) error
	// Reports yields one Report per produced record, plus client-level
	// events, until Close has run; then it is closed. Never nil.
	Reports() <-chan Event
	// Flush waits up to timeout for queued and in-flight records to be
	// delivered, and returns how many are left.
	Flush(timeout time.Duration) (remaining int)
	// Purge removes every queued and in-flight record, without waiting. Each
	// one is then reported as purged.
	Purge() error
	// Len is the number of records not yet reported. Meant for the
	// publisher's shutdown: it flushes until Len is 0 or its context ends,
	// and what Len still counts then is purged and reported as undelivered.
	Len() int
	// TopicPartitions returns the partition count of each requested topic
	// that exists; a missing topic is absent from the map. It makes one
	// request, bounded by ctx, even with no topics, so it also checks that a
	// broker answers.
	TopicPartitions(ctx context.Context, topics []string) (map[string]int, error)
	// Close releases the producer and closes Reports. Records still held are
	// dropped without a report. Idempotent.
	Close()
}

// producer is the confluent-kafka-go implementation of Producer.
type producer struct {
	kafkaProducer *kfk.Producer
	admin         *kfk.AdminClient
	reports       chan Event

	// handleMu stops a call from landing on a handle Close has destroyed.
	// confluent checks for a closed client itself, but its check and its cgo
	// call are not atomic, so a call that passed the check can still run after
	// rd_kafka_destroy.
	//
	// Every call into confluent (Produce, Flush, Purge, Len, TopicPartitions)
	// takes the read lock, held across its check of closed and the
	// call itself. Read locks do not exclude each other, so these calls run
	// concurrently. Close alone takes the write lock, which it can only get
	// once every call in progress has returned; it sets closed and releases
	// it before destroying the handle, so every later call sees closed and
	// returns ErrClosed instead of reaching the freed handle.
	handleMu sync.RWMutex
	closed   bool
}

// Compile-time check that *producer implements Producer; no runtime cost.
var _ Producer = (*producer)(nil)

// New creates a producer and starts translating its events onto Reports. It
// does not contact a broker; librdkafka connects in the background.
func New(config Config) (Producer, error) {
	configMap, err := ConfigMap(config)
	if err != nil {
		return nil, err
	}
	kafkaProducer, err := kfk.NewProducer(configMap)
	if err != nil {
		return nil, fmt.Errorf("creating kafka producer: %w", err)
	}
	admin, err := kfk.NewAdminClientFromProducer(kafkaProducer)
	if err != nil {
		kafkaProducer.Close()
		return nil, fmt.Errorf("creating the metadata client: %w", err)
	}
	created := &producer{
		kafkaProducer: kafkaProducer,
		admin:         admin,
		reports:       make(chan Event),
	}
	// Pass the events channel in rather than reading it from the producer inside
	// the goroutine: the goroutine then depends only on the channel, which
	// kafkaProducer.Close closes to end it, and never on the producer's state
	// during shutdown.
	go created.translate(kafkaProducer.Events())
	return created, nil
}

// translate forwards every event that matters onto reports, and closes reports
// once Close has closed the events channel.
func (p *producer) translate(events chan kfk.Event) {
	defer close(p.reports)
	for event := range events {
		if translated, ok := TranslateEvent(event); ok {
			p.reports <- translated
		}
	}
}

// Produce enqueues one record. token is any value the caller wants back with
// the record's outcome: it is set as kfk.Message.Opaque, which confluent keeps
// client-side and never sends to Kafka, and returns as Report.Token on the
// record's delivery report. The publisher passes the record's *Delivery, so
// the report finds the delivery it resolves.
func (p *producer) Produce(record Record, token any) error {
	topic := record.Topic
	message := &kfk.Message{
		// PartitionAny lets librdkafka choose the partition with the configured
		// partitioner; an explicit partition would bypass it.
		TopicPartition: kfk.TopicPartition{Topic: &topic, Partition: kfk.PartitionAny},
		Key:            record.Key,
		Value:          record.Value,
		Opaque:         token,
	}
	if len(record.Headers) > 0 {
		message.Headers = make([]kfk.Header, len(record.Headers))
		for index, header := range record.Headers {
			message.Headers[index] = kfk.Header{Key: header.Key, Value: header.Value}
		}
	}

	p.handleMu.RLock()
	defer p.handleMu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	if err := p.kafkaProducer.Produce(message, nil); err != nil {
		return TranslateError(err)
	}
	return nil
}

func (p *producer) Reports() <-chan Event {
	return p.reports
}

func (p *producer) Flush(timeout time.Duration) int {
	p.handleMu.RLock()
	defer p.handleMu.RUnlock()
	if p.closed {
		return 0
	}
	return p.kafkaProducer.Flush(int(timeout.Milliseconds()))
}

func (p *producer) Purge() error {
	p.handleMu.RLock()
	defer p.handleMu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	if err := p.kafkaProducer.Purge(kfk.PurgeQueue | kfk.PurgeInFlight | kfk.PurgeNonBlocking); err != nil {
		return TranslateError(err)
	}
	return nil
}

func (p *producer) Len() int {
	p.handleMu.RLock()
	defer p.handleMu.RUnlock()
	if p.closed {
		return 0
	}
	return p.kafkaProducer.Len()
}

// TopicPartitions returns topic → partition count for each requested topic
// that exists. A missing topic is left out of the map, not an error; any
// other per-topic error, or a failed request, is. With no topics it only
// checks that a broker answers, and returns an empty map. It is what
// Publisher.Ping is built on: Ping reports every bound topic absent from the
// map as ErrTopicNotFound. The partition counts are not used today.
//
// It uses an admin client derived from the producer, which shares its
// connections, because DescribeTopics asks for exactly these topics in one
// request bounded by ctx; the producer's GetMetadata takes one topic or all.
func (p *producer) TopicPartitions(ctx context.Context, topics []string) (map[string]int, error) {
	p.handleMu.RLock()
	defer p.handleMu.RUnlock()
	if p.closed {
		return nil, ErrClosed
	}

	partitions := make(map[string]int, len(topics))
	if len(topics) == 0 {
		if _, err := p.admin.DescribeCluster(ctx); err != nil {
			return nil, fmt.Errorf("describing the cluster: %w", describeError(err))
		}
		return partitions, nil
	}

	result, err := p.admin.DescribeTopics(ctx, kfk.NewTopicCollectionOfTopicNames(topics))
	if err != nil {
		return nil, fmt.Errorf("describing topics: %w", describeError(err))
	}
	var failures []error
	for _, description := range result.TopicDescriptions {
		switch description.Error.Code() {
		case kfk.ErrNoError:
			partitions[description.Name] = len(description.Partitions)
		case kfk.ErrUnknownTopicOrPart:
			// Absent from the map: the caller decides what a missing topic means.
		default:
			failures = append(failures, fmt.Errorf("topic %q: %w", description.Name, TranslateError(description.Error)))
		}
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return partitions, nil
}

// describeError keeps a context error as it is, so errors.Is still finds it,
// and translates a Kafka one.
func describeError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return TranslateError(err)
}

func (p *producer) Close() {
	p.handleMu.Lock()
	if p.closed {
		p.handleMu.Unlock()
		return
	}
	p.closed = true
	p.handleMu.Unlock()

	// Not under the lock: nothing else can reach the handle now, and Close
	// waits for confluent's poller, which needs no lock of ours.
	p.admin.Close()
	p.kafkaProducer.Close()
}
