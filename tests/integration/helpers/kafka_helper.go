// Package helpers starts Kafka brokers for the integration suite and provides
// the topic, producing and consuming calls the tests need.
package helpers

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/kafka"
)

const (
	defaultKafkaImage = "confluentinc/cp-kafka:7.5.0"
	adminTimeout      = 30 * time.Second

	// brokerStopTimeout is how long Docker waits after SIGTERM before killing
	// the broker. Kept short deliberately: this image does not shut down on
	// SIGTERM, so the timeout is really just "how long until SIGKILL", and a
	// generous value is paid in full on every outage — 30s here cost the two
	// restart tests 30s each. An abrupt stop is also the better model of the
	// failure being simulated, since a broker that dies does not shut down
	// gracefully either. The log survives it: Kafka recovers unflushed segments
	// when it starts again.
	brokerStopTimeout = 2 * time.Second

	// kafkaBrokerPort is the container port the Kafka module publishes.
	kafkaBrokerPort = "9093/tcp"
)

// kafkaImage returns the Kafka Docker image to use.
// In CI this is set to the GHCR mirror; locally it falls back to Docker Hub.
func kafkaImage() string {
	if img := os.Getenv("KAFKA_IMAGE"); img != "" {
		return img
	}
	return defaultKafkaImage
}

var (
	sharedCluster *KafkaTestCluster
	sharedOnce    sync.Once
	sharedErr     error
)

// KafkaTestCluster manages a Kafka container for integration tests.
type KafkaTestCluster struct {
	Container *kafka.KafkaContainer
	Brokers   []string
}

// SharedCluster starts one broker for the whole test binary and reuses it.
//
// Starting a container costs seconds, so tests share one rather than paying
// that per test. Each test must therefore use its own topic name — see
// UniqueTopicName — and its own consumer group, since the broker's state is
// shared.
//
// Nothing here terminates the container, deliberately. There is no test whose
// lifetime is the right one to tie it to: a t.Cleanup registered by whichever
// test happened to call first would tear the broker down while every other test
// is still using it. Ownership belongs to Ryuk, the testcontainers reaper, which
// holds an open connection to this process and deletes everything labelled with
// its session id once the binary exits — including on a panic, a -timeout kill
// or an interrupt, which a t.Cleanup would not cover.
func SharedCluster(t *testing.T) *KafkaTestCluster {
	t.Helper()

	sharedOnce.Do(func() {
		ctx := context.Background()

		container, err := kafka.Run(ctx, kafkaImage(), kafka.WithClusterID("test-cluster"))
		if err != nil {
			sharedErr = fmt.Errorf("starting kafka container: %w", err)

			return
		}

		brokers, err := container.Brokers(ctx)
		if err != nil {
			sharedErr = fmt.Errorf("resolving broker addresses: %w", err)

			return
		}

		sharedCluster = &KafkaTestCluster{Container: container, Brokers: brokers}
	})

	// Re-checked by every caller, not just the one that ran the Once: a failure
	// there would otherwise hand every later test a nil cluster to dereference.
	if sharedErr != nil {
		t.Fatalf("kafka cluster unavailable: %v", sharedErr)
	}

	return sharedCluster
}

// DedicatedCluster starts a broker for one test and terminates it afterwards.
//
// For tests that must disturb the broker itself — stopping it to watch a client
// reconnect — which the shared cluster cannot host, since every other test in
// the binary is using it. Costs a container start, so it is worth it only for
// that.
func DedicatedCluster(t *testing.T) *KafkaTestCluster {
	t.Helper()

	ctx := context.Background()

	// The host port is pinned rather than left to Docker, because a stop and
	// start would otherwise hand the broker a different one, and a client that
	// survived the outage would then be reconnecting to nothing. Pinning it is
	// what makes the outage look to the client like the broker it already knows
	// going away and coming back.
	port := freePort(t)

	broker, err := kafka.Run(ctx, kafkaImage(),
		kafka.WithClusterID("test-dedicated"),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				// Both families. The broker advertises "localhost", which
				// resolves to ::1 first, so an IPv4-only binding is refused by
				// every client that gets that far — the admin client falls back
				// to IPv4 and works, while the producer does not, which makes
				// the failure look like a broken broker rather than a binding.
				network.MustParsePort(kafkaBrokerPort): []network.PortBinding{
					{HostIP: netip.IPv4Unspecified(), HostPort: port},
					{HostIP: netip.IPv6Unspecified(), HostPort: port},
				},
			}
		}),
	)
	if err != nil {
		t.Fatalf("starting dedicated kafka container: %v", err)
	}

	t.Cleanup(func() {
		if err := broker.Terminate(context.Background()); err != nil {
			t.Logf("terminating dedicated kafka container: %v", err)
		}
	})

	brokers, err := broker.Brokers(ctx)
	if err != nil {
		t.Fatalf("failed to get broker addresses: %v", err)
	}

	t.Logf("dedicated kafka container started, brokers: %v", brokers)

	return &KafkaTestCluster{Container: broker, Brokers: brokers}
}

// freePort reserves a port by binding and releasing it, and returns it for the
// container to claim.
//
// Racy in principle — something else could take it in between — but the window
// is microseconds and the alternative is a hard-coded port that collides with
// whatever is already running.
func freePort(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a host port: %v", err)
	}
	defer func() { _ = l.Close() }()

	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("reading the reserved port: %v", err)
	}

	return port
}

// StopBroker stops the broker container, as an outage would.
//
// The container is stopped, not terminated: its log directory and host port
// mapping both survive, so the topics and messages written before the outage
// are still there when StartBroker brings it back, and the addresses handed to
// a client beforehand are still the right ones. Only use this on a
// DedicatedCluster.
func (k *KafkaTestCluster) StopBroker(ctx context.Context, t *testing.T) {
	t.Helper()

	timeout := brokerStopTimeout
	if err := k.Container.Stop(ctx, &timeout); err != nil {
		t.Fatalf("stopping broker: %v", err)
	}

	t.Log("Kafka container stopped (data and port preserved for restart)")
}

// StartBroker brings a stopped broker back at the same address.
func (k *KafkaTestCluster) StartBroker(ctx context.Context, t *testing.T) {
	t.Helper()

	if err := k.Container.Start(ctx); err != nil {
		t.Fatalf("restarting broker: %v", err)
	}

	// The addresses must not have moved, or a client that survived the outage
	// would be reconnecting to nothing and the test would prove the opposite of
	// what it claims.
	brokers, err := k.Container.Brokers(ctx)
	if err != nil {
		t.Fatalf("failed to get broker addresses after restart: %v", err)
	}
	if strings.Join(brokers, ",") != strings.Join(k.Brokers, ",") {
		t.Fatalf("broker moved across the restart: was %v, now %v — this test cannot say anything "+
			"about reconnection", k.Brokers, brokers)
	}

	t.Logf("Kafka container restarted, brokers: %v", brokers)
}

// CreateTopic creates a topic with the given name and partitions using an admin client.
func (k *KafkaTestCluster) CreateTopic(ctx context.Context, t *testing.T, topic string, partitions int) {
	t.Helper()

	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{
		"bootstrap.servers": k.Brokers[0],
	})
	if err != nil {
		t.Fatalf("failed to create admin client: %v", err)
	}
	defer admin.Close()

	results, err := admin.CreateTopics(ctx, []kfk.TopicSpecification{
		{
			Topic:             topic,
			NumPartitions:     partitions,
			ReplicationFactor: 1,
		},
	})
	if err != nil {
		t.Fatalf("failed to create topic: %v", err)
	}

	for _, result := range results {
		if result.Error.Code() != kfk.ErrNoError {
			t.Fatalf("failed to create topic %s: %v", result.Topic, result.Error)
		}
	}

	t.Logf("Created topic %s with %d partitions", topic, partitions)
}

// ProduceMessages produces messages to a topic and waits for delivery.
func (k *KafkaTestCluster) ProduceMessages(ctx context.Context, t *testing.T, topic string, messages []string) {
	t.Helper()

	producer, err := kfk.NewProducer(&kfk.ConfigMap{
		"bootstrap.servers": k.Brokers[0],
	})
	if err != nil {
		t.Fatalf("failed to create producer: %v", err)
	}
	defer producer.Close()

	deliveryChan := make(chan kfk.Event, len(messages))

	for i, msg := range messages {
		err := producer.Produce(&kfk.Message{
			TopicPartition: kfk.TopicPartition{
				Topic:     &topic,
				Partition: kfk.PartitionAny,
			},
			Value: []byte(msg),
		}, deliveryChan)
		if err != nil {
			t.Fatalf("failed to produce message %d: %v", i, err)
		}
	}

	// Wait for deliveries
	for i := range messages {
		ev := <-deliveryChan
		m := ev.(*kfk.Message)
		if m.TopicPartition.Error != nil {
			t.Fatalf("delivery failed for message %d: %v", i, m.TopicPartition.Error)
		}
	}

	t.Logf("Produced %d messages to topic %s", len(messages), topic)
}

// Record is a message to produce with ProduceRecords. A nil Partition lets the
// producer choose.
type Record struct {
	Key       []byte
	Value     []byte
	Partition *int32
}

// ProduceRecords produces records — with keys, raw bytes and, optionally, a
// fixed partition — and waits for delivery, in order.
func (k *KafkaTestCluster) ProduceRecords(ctx context.Context, t *testing.T, topic string, records []Record) {
	t.Helper()

	producer, err := kfk.NewProducer(&kfk.ConfigMap{
		"bootstrap.servers": k.Brokers[0],
	})
	if err != nil {
		t.Fatalf("failed to create producer: %v", err)
	}
	defer producer.Close()

	deliveryChan := make(chan kfk.Event, 1)

	// One at a time, so the records land in the order given.
	for i, r := range records {
		partition := kfk.PartitionAny
		if r.Partition != nil {
			partition = *r.Partition
		}
		err := producer.Produce(&kfk.Message{
			TopicPartition: kfk.TopicPartition{Topic: &topic, Partition: partition},
			Key:            r.Key,
			Value:          r.Value,
		}, deliveryChan)
		if err != nil {
			t.Fatalf("failed to produce record %d: %v", i, err)
		}
		m := (<-deliveryChan).(*kfk.Message)
		if m.TopicPartition.Error != nil {
			t.Fatalf("delivery failed for record %d: %v", i, m.TopicPartition.Error)
		}
	}

	t.Logf("Produced %d records to topic %s", len(records), topic)
}

// ConsumeMessages reads up to expectedCount messages from a topic within a timeout.
// Returns the raw Kafka messages read from the topic.
func (k *KafkaTestCluster) ConsumeMessages(ctx context.Context, t *testing.T, topic, group string, expectedCount int, timeout time.Duration) []*kfk.Message {
	t.Helper()

	consumer, err := kfk.NewConsumer(&kfk.ConfigMap{
		"bootstrap.servers":  k.Brokers[0],
		"group.id":           group,
		"auto.offset.reset":  "earliest",
		"enable.auto.commit": "false",
	})
	if err != nil {
		t.Fatalf("failed to create consumer for topic %s: %v", topic, err)
	}
	defer consumer.Close()

	if err := consumer.Subscribe(topic, nil); err != nil {
		t.Fatalf("failed to subscribe to topic %s: %v", topic, err)
	}

	var messages []*kfk.Message
	deadline := time.After(timeout)

	for len(messages) < expectedCount {
		select {
		case <-deadline:
			t.Logf("ConsumeMessages: timed out after %v, got %d of %d messages from %s", timeout, len(messages), expectedCount, topic)
			return messages
		default:
			ev := consumer.Poll(200)
			if ev == nil {
				continue
			}
			switch m := ev.(type) {
			case *kfk.Message:
				messages = append(messages, m)
				t.Logf("ConsumeMessages: received message %d from %s [%d] @ %v",
					len(messages), topic, m.TopicPartition.Partition, m.TopicPartition.Offset)
			case kfk.Error:
				t.Logf("ConsumeMessages: kafka error: %v", m)
			}
		}
	}

	return messages
}

// CommittedOffset returns the offset a consumer group has committed for a
// topic-partition, or kfk.OffsetInvalid when the group has committed nothing.
//
// This reads the group's state from the broker without joining the group, so it
// can be called while consumers are running without provoking a rebalance.
func (k *KafkaTestCluster) CommittedOffset(
	ctx context.Context, t *testing.T, group, topic string, partition int32,
) kfk.Offset {

	t.Helper()

	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{
		"bootstrap.servers": k.Brokers[0],
	})
	if err != nil {
		t.Fatalf("failed to create admin client: %v", err)
	}
	defer admin.Close()

	result, err := admin.ListConsumerGroupOffsets(ctx, []kfk.ConsumerGroupTopicPartitions{{
		Group:      group,
		Partitions: []kfk.TopicPartition{{Topic: &topic, Partition: partition}},
	}})
	if err != nil {
		t.Fatalf("failed to list committed offsets for group %s: %v", group, err)
	}

	// One group with one partition was requested, so at most one entry comes back.
	for _, groupOffsets := range result.ConsumerGroupsTopicPartitions {
		if len(groupOffsets.Partitions) == 0 {
			continue
		}
		tp := groupOffsets.Partitions[0]
		if tp.Error != nil {
			t.Fatalf("committed offset lookup failed for %s[%d]: %v", topic, partition, tp.Error)
		}
		return tp.Offset
	}

	return kfk.OffsetInvalid
}

// GetHeader extracts a header value from a Kafka message by key.
func GetHeader(msg *kfk.Message, key string) string {
	for _, h := range msg.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// WaitForBrokerReady waits until the Kafka broker is ready to accept connections.
//
// Starting a container is not the same as the broker inside it being ready, and
// testcontainers does not re-apply its wait strategy to a restart — so without
// this, the first call after StartBroker races Kafka's startup.
func (k *KafkaTestCluster) WaitForBrokerReady(ctx context.Context, t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	attempt := 0
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for kafka broker to be ready after %d attempts", attempt)
		default:
		}

		attempt++
		admin, err := kfk.NewAdminClient(&kfk.ConfigMap{
			"bootstrap.servers": k.Brokers[0],
			"socket.timeout.ms": 2000,
		})
		if err != nil {
			t.Logf("WaitForBrokerReady: attempt %d - admin client create failed: %v", attempt, err)
			time.Sleep(1 * time.Second)
			continue
		}

		// Try to get cluster metadata as a readiness check
		md, err := admin.GetMetadata(nil, true, 3000)
		admin.Close()
		if err != nil {
			t.Logf("WaitForBrokerReady: attempt %d - GetMetadata failed: %v", attempt, err)
			time.Sleep(1 * time.Second)
			continue
		}
		if len(md.Brokers) > 0 {
			t.Logf("WaitForBrokerReady: broker ready after %d attempts, brokers: %v", attempt, md.Brokers)
			return
		}

		t.Logf("WaitForBrokerReady: attempt %d - no brokers in metadata", attempt)
		time.Sleep(1 * time.Second)
	}
}

// UniqueTopicName returns a topic name unique to this test, so tests sharing the
// broker cannot interfere with one another.
func UniqueTopicName(t *testing.T, prefix string) string {
	t.Helper()

	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), sanitise(t.Name()))
}

// sanitise reduces a test name to characters Kafka accepts in a topic name.
func sanitise(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}

	return string(out)
}
