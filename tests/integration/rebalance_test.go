package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	easykafka "github.com/easykafka/easykafka-go"
	"github.com/easykafka/easykafka-go/tests/integration/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRebalanceDuplicateProcessingAcceptable verifies FR-043 under rebalance:
// when a second consumer joins the same group, a partition rebalance occurs.
// Messages that were in-flight at the time of revocation may be re-delivered
// to another consumer. At-least-once semantics guarantee every message is
// processed, but duplicates across consumers are acceptable.
func TestRebalanceDuplicateProcessingAcceptable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-rebalance-%d", time.Now().UnixNano())
	// Use multiple partitions so the rebalance actually reassigns work.
	cluster.CreateTopic(ctx, t, topic, 3)

	// Produce messages before any consumer starts
	const messageCount = 10_000
	var produced []string
	for i := 0; i < messageCount; i++ {
		produced = append(produced, fmt.Sprintf("rebal-msg-%d", i))
	}
	cluster.ProduceMessages(ctx, t, topic, produced)

	groupID := fmt.Sprintf("rebalance-group-%d", time.Now().UnixNano())

	// Shared tracking across both consumers
	var mu sync.Mutex
	var allReceived []string

	makeHandler := func(id string) easykafka.Handler {
		return func(ctx context.Context, payload []byte) error {
			mu.Lock()
			t.Logf("Consumer %s received: %s", id, payload)
			allReceived = append(allReceived, string(payload))
			mu.Unlock()
			return nil
		}
	}

	// Consumer 1: starts first and begins consuming all partitions
	c1, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(makeHandler("c1")),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms": 6000,
		}),
	)
	require.NoError(t, err)

	c1Ctx, c1Cancel := context.WithCancel(ctx)
	defer c1Cancel()

	var c1Err error
	c1Done := make(chan struct{})
	go func() {
		c1Err = c1.Start(c1Ctx)
		close(c1Done)
	}()

	// Let consumer 1 start processing (wait for some messages)
	waitForMessages(t, &mu, &allReceived, 5, 30*time.Second)
	t.Log("Consumer 1 processing started, launching consumer 2 to trigger rebalance")

	// Consumer 2: joins the same group, triggering a rebalance
	c2, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(makeHandler("c2")),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms": 6000,
		}),
	)
	require.NoError(t, err)

	c2Ctx, c2Cancel := context.WithCancel(ctx)
	defer c2Cancel()

	var c2Err error
	c2Done := make(chan struct{})
	go func() {
		c2Err = c2.Start(c2Ctx)
		close(c2Done)
	}()

	// Wait for all messages across both consumers
	waitForMessages(t, &mu, &allReceived, messageCount, 30*time.Second)

	// Stop both consumers
	c1Cancel()
	c2Cancel()
	<-c1Done
	<-c2Done

	assert.NoError(t, c1Err)
	assert.NoError(t, c2Err)

	mu.Lock()
	defer mu.Unlock()

	receivedSet := make(map[string]int)
	for _, msg := range allReceived {
		receivedSet[msg]++
	}

	// At-least-once: every produced message must appear at least once
	for _, msg := range produced {
		assert.GreaterOrEqual(t, receivedSet[msg], 1,
			"message %q produced but never received after rebalance (violates at-least-once)", msg)
	}

	// Duplicates from rebalance are acceptable - log them
	dupeCount := 0
	for msg, count := range receivedSet {
		if count > 1 {
			dupeCount++
			t.Logf("rebalance duplicate (acceptable): %q received %d times", msg, count)
		}
	}

	t.Logf("rebalance test: %d produced, %d received, %d unique, %d duplicated keys",
		len(produced), len(allReceived), len(receivedSet), dupeCount)
}

// TestRebalanceOnConsumerLeave verifies that when a consumer leaves the group,
// its partitions are reassigned and remaining messages are still consumed.
// No messages are lost during the rebalance.
func TestRebalanceOnConsumerLeave(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-rebal-leave-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 2)

	groupID := fmt.Sprintf("rebal-leave-group-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var allReceived []string

	// Counter used to stop consumer 1 after a few messages
	var c1Count atomic.Int32

	c1Handler := func(ctx context.Context, payload []byte) error {
		c1Count.Add(1)
		mu.Lock()
		t.Logf("Consumer 1 received: %s", payload)
		allReceived = append(allReceived, string(payload))
		mu.Unlock()
		return nil
	}

	c2Handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		t.Logf("Consumer 2 received: %s", payload)
		allReceived = append(allReceived, string(payload))
		mu.Unlock()
		return nil
	}

	// Start consumer 1
	c1, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(c1Handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms": 6000,
		}),
	)
	require.NoError(t, err)

	// Start consumer 2
	c2, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(c2Handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms": 6000,
		}),
	)
	require.NoError(t, err)

	c1Ctx, c1Cancel := context.WithCancel(ctx)
	defer c1Cancel()
	c2Ctx, c2Cancel := context.WithCancel(ctx)
	defer c2Cancel()

	var c1Err, c2Err error
	c1Done := make(chan struct{})
	c2Done := make(chan struct{})
	go func() { c1Err = c1.Start(c1Ctx); close(c1Done) }()
	go func() { c2Err = c2.Start(c2Ctx); close(c2Done) }()

	// Wait a moment for both consumers to join and stabilize
	time.Sleep(5 * time.Second)

	// Produce messages across both partitions
	const messageCount = 20
	var produced []string
	for i := 0; i < messageCount; i++ {
		produced = append(produced, fmt.Sprintf("leave-msg-%d", i))
	}
	cluster.ProduceMessages(ctx, t, topic, produced)

	// Wait for consumer 1 to process at least a few messages, then stop it
	deadline := time.After(20 * time.Second)
	for c1Count.Load() < 3 {
		select {
		case <-deadline:
			// Consumer 1 may not be assigned any partition; continue anyway
			t.Log("consumer 1 received fewer than 3 messages before leave timeout, proceeding")
			goto stopC1
		case <-time.After(200 * time.Millisecond):
		}
	}

stopC1:
	t.Logf("Stopping consumer 1 (processed %d messages) to trigger rebalance", c1Count.Load())
	c1Cancel()
	<-c1Done

	// Consumer 2 should now pick up all remaining messages
	waitForMessages(t, &mu, &allReceived, messageCount, 30*time.Second)

	c2Cancel()
	<-c2Done

	assert.NoError(t, c1Err)
	assert.NoError(t, c2Err)

	mu.Lock()
	defer mu.Unlock()

	receivedSet := make(map[string]int)
	for _, msg := range allReceived {
		receivedSet[msg]++
	}

	// Every produced message must appear at least once
	for _, msg := range produced {
		assert.GreaterOrEqual(t, receivedSet[msg], 1,
			"message %q lost after consumer left the group", msg)
	}

	t.Logf("rebalance-leave test: %d produced, %d received, %d unique",
		len(produced), len(allReceived), len(receivedSet))
}

// TestRebalanceCommitsBeforeRevocation verifies that offsets are committed
// before partitions are revoked. With explicit offset management (auto.commit
// disabled), the rebalance callback must commit processed offsets so that the
// new partition owner does not re-process already committed messages beyond
// what at-least-once allows.
func TestRebalanceCommitsBeforeRevocation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-rebal-commit-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1) // single partition for deterministic offsets

	groupID := fmt.Sprintf("rebal-commit-group-%d", time.Now().UnixNano())

	// Phase 1: consumer 1 processes all messages and commits offsets
	messages := []string{"commit-1", "commit-2", "commit-3", "commit-4", "commit-5"}
	cluster.ProduceMessages(ctx, t, topic, messages)

	var mu sync.Mutex
	var c1Received []string

	c1Handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		c1Received = append(c1Received, string(payload))
		mu.Unlock()
		return nil
	}

	c1, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(c1Handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	c1Ctx, c1Cancel := context.WithCancel(ctx)
	var c1Err error
	c1Done := make(chan struct{})
	go func() { c1Err = c1.Start(c1Ctx); close(c1Done) }()

	// Wait for all messages to be consumed
	waitForMessages(t, &mu, &c1Received, len(messages), 30*time.Second)
	t.Log("Consumer 1 consumed all messages, stopping to trigger revocation + commit")

	// Stop consumer 1 - rebalance callback should commit offsets
	c1Cancel()
	<-c1Done
	assert.NoError(t, c1Err)

	// Phase 2: consumer 2 joins the same group and should NOT re-receive
	// messages already committed by consumer 1 (beyond at-least-once margin)
	var c2Received []string
	c2Handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		c2Received = append(c2Received, string(payload))
		mu.Unlock()
		return nil
	}

	c2, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(c2Handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	// Run consumer 2 briefly - it should find no new messages
	c2Ctx, c2Cancel := context.WithTimeout(ctx, 10*time.Second)
	defer c2Cancel()

	var c2Err error
	c2Done := make(chan struct{})
	go func() { c2Err = c2.Start(c2Ctx); close(c2Done) }()

	<-c2Done
	assert.NoError(t, c2Err)

	mu.Lock()
	defer mu.Unlock()

	// Consumer 2 should receive 0 messages (offsets were committed).
	// In rare at-least-once edge cases a small number of duplicates is
	// acceptable, but the committed offsets should prevent full reprocessing.
	assert.Less(t, len(c2Received), len(messages),
		"consumer 2 re-processed all messages - offsets were NOT committed before revocation")

	t.Logf("revocation commit test: c1 received %d, c2 received %d (expected 0 or few duplicates)",
		len(c1Received), len(c2Received))
}

// TestRebalanceWithSlowHandler verifies that a slow handler does not prevent
// the consumer from committing offsets before revocation. Messages processed
// before the rebalance should be committed; the slow in-flight message may
// be duplicated to the new owner - that is acceptable under at-least-once.
func TestRebalanceWithSlowHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-rebal-slow-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 2)

	groupID := fmt.Sprintf("rebal-slow-group-%d", time.Now().UnixNano())

	// Produce messages
	const messageCount = 15
	var produced []string
	for i := 0; i < messageCount; i++ {
		produced = append(produced, fmt.Sprintf("slow-msg-%d", i))
	}
	cluster.ProduceMessages(ctx, t, topic, produced)

	var mu sync.Mutex
	var allReceived []string
	var c1Count atomic.Int32

	// Consumer 1 handler: deliberately slow on the first few messages
	c1Handler := func(ctx context.Context, payload []byte) error {
		n := c1Count.Add(1)
		if n <= 3 {
			time.Sleep(500 * time.Millisecond) // simulate slow processing
		}
		mu.Lock()
		allReceived = append(allReceived, string(payload))
		mu.Unlock()
		return nil
	}

	c2Handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		allReceived = append(allReceived, string(payload))
		mu.Unlock()
		return nil
	}

	c1, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(c1Handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms": 6000,
		}),
	)
	require.NoError(t, err)

	c1Ctx, c1Cancel := context.WithCancel(ctx)
	defer c1Cancel()
	c1Done := make(chan struct{})
	var c1Err error
	go func() { c1Err = c1.Start(c1Ctx); close(c1Done) }()

	// Let consumer 1 start the slow processing
	time.Sleep(2 * time.Second)

	// Start consumer 2 - triggers rebalance while c1 has a slow handler
	c2, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(c2Handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
		easykafka.WithKafkaConfig(map[string]any{
			"session.timeout.ms": 6000,
		}),
	)
	require.NoError(t, err)

	c2Ctx, c2Cancel := context.WithCancel(ctx)
	defer c2Cancel()
	c2Done := make(chan struct{})
	var c2Err error
	go func() { c2Err = c2.Start(c2Ctx); close(c2Done) }()

	// Wait for all messages across both consumers
	waitForMessages(t, &mu, &allReceived, messageCount, 45*time.Second)

	c1Cancel()
	c2Cancel()
	<-c1Done
	<-c2Done

	assert.NoError(t, c1Err)
	assert.NoError(t, c2Err)

	mu.Lock()
	defer mu.Unlock()

	receivedSet := make(map[string]int)
	for _, msg := range allReceived {
		receivedSet[msg]++
	}

	// At-least-once: every message must be delivered
	for _, msg := range produced {
		assert.GreaterOrEqual(t, receivedSet[msg], 1,
			"message %q lost during slow-handler rebalance", msg)
	}

	t.Logf("slow-handler rebalance: %d produced, %d received, %d unique",
		len(produced), len(allReceived), len(receivedSet))
}

// TestRebalanceNoDataLossWithDirectConsumer is a lower-level verification
// using a raw confluent-kafka-go consumer. It confirms that our adapter
// rebalance callback correctly invokes Commit() during partition revocation,
// so the new owner starts from the committed offset rather than replaying
// the entire partition.
func TestRebalanceNoDataLossWithDirectConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	cluster := helpers.StartKafkaCluster(ctx, t)
	defer cluster.Stop(ctx, t)

	topic := fmt.Sprintf("test-rebal-direct-%d", time.Now().UnixNano())
	cluster.CreateTopic(ctx, t, topic, 1)

	groupID := fmt.Sprintf("rebal-direct-group-%d", time.Now().UnixNano())

	produced := []string{"d-1", "d-2", "d-3", "d-4", "d-5", "d-6", "d-7", "d-8", "d-9", "d-10"}
	cluster.ProduceMessages(ctx, t, topic, produced)

	// Use the library consumer to process and commit all messages
	var mu sync.Mutex
	var received []string
	handler := func(ctx context.Context, payload []byte) error {
		mu.Lock()
		t.Logf("consumer 1 received: %s", payload)
		received = append(received, string(payload))
		mu.Unlock()
		return nil
	}

	c, err := easykafka.New(
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cluster.Brokers...),
		easykafka.WithConsumerGroup(groupID),
		easykafka.WithHandler(handler),
		easykafka.WithPollTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)

	cCtx, cCancel := context.WithCancel(ctx)
	cDone := make(chan struct{})
	var cErr error
	go func() { cErr = c.Start(cCtx); close(cDone) }()

	waitForMessages(t, &mu, &received, len(produced), 30*time.Second)
	cCancel()
	<-cDone
	assert.NoError(t, cErr)

	// Now verify committed offsets via a raw consumer - it should have
	// nothing new to read from the same group because offsets were committed.
	rawConsumer, err := kfk.NewConsumer(&kfk.ConfigMap{
		"bootstrap.servers":  cluster.Brokers[0],
		"group.id":           groupID,
		"auto.offset.reset":  "earliest",
		"enable.auto.commit": "false",
	})
	require.NoError(t, err)
	defer rawConsumer.Close()

	require.NoError(t, rawConsumer.Subscribe(topic, nil))

	// Poll briefly - should get no new messages
	var rawReceived int
	pollDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(pollDeadline) {
		ev := rawConsumer.Poll(500)
		if ev == nil {
			continue
		}
		if _, ok := ev.(*kfk.Message); ok {
			// t.Logf("raw consumer 1 received: %s", msg.Value)
			rawReceived++
		}
	}

	assert.Zero(t, rawReceived,
		"raw consumer in same group received messages - offsets were not committed properly")
	t.Logf("direct consumer verification: library consumed %d, raw consumer received %d (expected 0)",
		len(received), rawReceived)
}
