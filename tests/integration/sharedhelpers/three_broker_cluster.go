package sharedhelpers

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// threeBrokerCount is the number of brokers, each a KRaft controller too,
	// so the controller quorum survives one of them being down.
	threeBrokerCount = 3

	// threeBrokerClientPort is the container port clients connect to; it is
	// published on a pinned host port, see NewThreeBrokerCluster.
	threeBrokerClientPort = "9092/tcp"

	// threeBrokerClusterID is the KRaft cluster id every node is formatted
	// with: any 16 bytes, base64url-encoded.
	threeBrokerClusterID = "ZWFzeWthZmthLXRocmVlYg"

	// gracefulStopTimeout is how long a stopped broker gets for its controlled
	// shutdown before Docker kills it. A real one takes a few seconds; unlike
	// the single-broker module's image, these containers run Kafka itself as
	// the entrypoint, so SIGTERM reaches it.
	gracefulStopTimeout = 60 * time.Second

	// brokerReadyTimeout bounds the wait for a started broker to answer.
	brokerReadyTimeout = 90 * time.Second
)

// ThreeBrokerCluster is three Kafka nodes in KRaft combined mode (each a broker
// and a controller), configured like production: replication factor 3 and
// min.insync.replicas=2 by default, automatic topic creation off. It is for
// tests that stop or kill brokers, which the single-broker clusters cannot
// show: a write stays acknowledged only while enough replicas are in sync.
type ThreeBrokerCluster struct {
	// Brokers holds one address per node, in node order, for clients.
	Brokers []string

	nodes []testcontainers.Container

	// down marks the nodes stopped or killed and not started again, for Heal.
	// Only the test's own goroutine changes the cluster, so it needs no lock.
	down []bool
}

// NewThreeBrokerCluster starts the cluster and waits until every broker
// answers. The containers are terminated at the end of the test or benchmark.
//
// Two findings from easykafka-config-go's outage tests are built in:
//
//   - Host ports are pinned. A container stopped and started again would
//     otherwise get new mapped ports, and a client that survived the outage
//     would reconnect to nothing.
//   - Each port is bound for both address families. The brokers advertise
//     "localhost", which resolves to ::1 first, and librdkafka's producer does
//     not fall back to IPv4 when that is refused.
func NewThreeBrokerCluster(tb testing.TB) *ThreeBrokerCluster {
	tb.Helper()

	ctx := context.Background()

	clusterNetwork, err := tcnetwork.New(ctx)
	if err != nil {
		tb.Fatalf("creating the cluster network: %v", err)
	}
	tb.Cleanup(func() {
		if err := clusterNetwork.Remove(context.Background()); err != nil {
			tb.Logf("removing the cluster network: %v", err)
		}
	})

	hostPorts := make([]string, threeBrokerCount)
	voters := make([]string, threeBrokerCount)
	cluster := &ThreeBrokerCluster{
		Brokers: make([]string, threeBrokerCount),
		nodes:   make([]testcontainers.Container, threeBrokerCount),
		down:    make([]bool, threeBrokerCount),
	}
	for index := range threeBrokerCount {
		hostPorts[index] = freePort(tb)
		voters[index] = fmt.Sprintf("%d@%s:29093", index+1, nodeAlias(index))
		cluster.Brokers[index] = "localhost:" + hostPorts[index]
	}

	// All three start at once: a node finishes starting only once the
	// controller quorum has formed, which needs two of them up.
	var wg sync.WaitGroup
	errs := make([]error, threeBrokerCount)
	for index := range threeBrokerCount {
		wg.Go(func() {
			cluster.nodes[index], errs[index] = testcontainers.GenericContainer(ctx,
				testcontainers.GenericContainerRequest{
					ContainerRequest: nodeRequest(index, hostPorts[index], strings.Join(voters, ","),
						clusterNetwork.Name),
					Started: true,
				})
		})
	}
	wg.Wait()

	// Registered before any failure is reported, so that the nodes which did
	// start are terminated too.
	tb.Cleanup(func() {
		for _, node := range cluster.nodes {
			if node == nil {
				continue
			}
			if err := node.Terminate(context.Background()); err != nil {
				tb.Logf("terminating a kafka node: %v", err)
			}
		}
	})
	for index, err := range errs {
		if err != nil {
			tb.Fatalf("starting kafka node %d: %v", index+1, err)
		}
	}

	cluster.waitForBrokers(tb, threeBrokerCount)
	tb.Logf("three-broker cluster started, brokers: %v", cluster.Brokers)

	return cluster
}

// nodeAlias is the node's host name on the cluster network.
func nodeAlias(index int) string {
	return fmt.Sprintf("kafka%d", index+1)
}

// nodeRequest describes one node. Clients reach it on hostPort through the
// EXTERNAL listener; the nodes reach each other on the cluster network through
// INTERNAL and CONTROLLER.
func nodeRequest(index int, hostPort, voters, networkName string) testcontainers.ContainerRequest {
	alias := nodeAlias(index)
	return testcontainers.ContainerRequest{
		Image:          kafkaImage(),
		ExposedPorts:   []string{threeBrokerClientPort},
		Networks:       []string{networkName},
		NetworkAliases: map[string][]string{networkName: {alias}},
		Env: map[string]string{
			"CLUSTER_ID":                           threeBrokerClusterID,
			"KAFKA_NODE_ID":                        fmt.Sprint(index + 1),
			"KAFKA_PROCESS_ROLES":                  "broker,controller",
			"KAFKA_CONTROLLER_QUORUM_VOTERS":       voters,
			"KAFKA_LISTENERS":                      "EXTERNAL://0.0.0.0:9092,INTERNAL://0.0.0.0:29092,CONTROLLER://0.0.0.0:29093",
			"KAFKA_ADVERTISED_LISTENERS":           fmt.Sprintf("EXTERNAL://localhost:%s,INTERNAL://%s:29092", hostPort, alias),
			"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "EXTERNAL:PLAINTEXT,INTERNAL:PLAINTEXT,CONTROLLER:PLAINTEXT",
			"KAFKA_INTER_BROKER_LISTENER_NAME":     "INTERNAL",
			"KAFKA_CONTROLLER_LISTENER_NAMES":      "CONTROLLER",
			"KAFKA_LOG_DIRS":                       "/tmp/kraft-combined-logs",
			// Production's durability settings.
			"KAFKA_DEFAULT_REPLICATION_FACTOR":               "3",
			"KAFKA_MIN_INSYNC_REPLICAS":                      "2",
			"KAFKA_UNCLEAN_LEADER_ELECTION_ENABLE":           "true",
			"KAFKA_AUTO_CREATE_TOPICS_ENABLE":                "false",
			"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":         "3",
			"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "3",
			"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":            "2",
			"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS":         "0",
			// Three JVMs share the Docker host; the image's 1 GB default each
			// is more than these tests need.
			"KAFKA_HEAP_OPTS": "-Xms256m -Xmx512m",
		},
		HostConfigModifier: func(hostConfig *container.HostConfig) {
			hostConfig.PortBindings = network.PortMap{
				network.MustParsePort(threeBrokerClientPort): []network.PortBinding{
					{HostIP: netip.IPv4Unspecified(), HostPort: hostPort},
					{HostIP: netip.IPv6Unspecified(), HostPort: hostPort},
				},
			}
		},
		WaitingFor: wait.ForLog("Kafka Server started").WithStartupTimeout(brokerReadyTimeout),
	}
}

// StopBroker stops one broker gracefully (docker stop), as a rolling update
// does: it hands its partition leadership over and leaves the in-sync replicas
// before it goes. index counts from 0.
func (c *ThreeBrokerCluster) StopBroker(tb testing.TB, index int) {
	tb.Helper()

	timeout := gracefulStopTimeout
	started := time.Now()
	if err := c.nodes[index].Stop(context.Background(), &timeout); err != nil {
		tb.Fatalf("stopping broker %d: %v", index+1, err)
	}
	c.down[index] = true
	tb.Logf("broker %d stopped gracefully in %s", index+1, time.Since(started).Round(time.Millisecond))
}

// KillBroker kills one broker (docker kill, SIGKILL), as a crash does: it
// stays in the in-sync replicas until the controller notices it is gone.
// index counts from 0.
func (c *ThreeBrokerCluster) KillBroker(tb testing.TB, index int) {
	tb.Helper()

	ctx := context.Background()
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		tb.Fatalf("connecting to docker: %v", err)
	}
	defer func() { _ = docker.Close() }()

	if _, err := docker.ContainerKill(ctx, c.nodes[index].GetContainerID(), client.ContainerKillOptions{}); err != nil {
		tb.Fatalf("killing broker %d: %v", index+1, err)
	}
	c.down[index] = true
	tb.Logf("broker %d killed", index+1)
}

// StartBroker starts a stopped or killed broker again, at the same address,
// and waits until it answers. It does not wait for it to catch up: use
// WaitForFullISR for that. index counts from 0.
func (c *ThreeBrokerCluster) StartBroker(tb testing.TB, index int) {
	tb.Helper()

	if err := c.nodes[index].Start(context.Background()); err != nil {
		tb.Fatalf("starting broker %d: %v", index+1, err)
	}
	// The container's wait strategy does not show that the broker is back:
	// its log still holds the first start's line. So wait until the cluster
	// lists it again, asking that broker itself.
	c.waitForBroker(tb, index)
	c.down[index] = false
	tb.Logf("broker %d started again", index+1)
}

// Heal starts every broker that is down, and waits until the cluster lists all
// three. Meant for t.Cleanup, so that a test sharing the cluster finds it
// whole whatever the one before it did, or how it ended.
func (c *ThreeBrokerCluster) Heal(tb testing.TB) {
	tb.Helper()

	for index, isDown := range c.down {
		if isDown {
			c.StartBroker(tb, index)
		}
	}
	c.waitForBrokers(tb, threeBrokerCount)
}

// CreateTopic creates a topic with replication factor 3 and
// min.insync.replicas=2, as in production, and waits until every partition has
// a leader and a full set of in-sync replicas.
func (c *ThreeBrokerCluster) CreateTopic(tb testing.TB, topic string, partitions int) {
	tb.Helper()

	c.createTopic(tb, kfk.TopicSpecification{
		Topic:             topic,
		NumPartitions:     partitions,
		ReplicationFactor: threeBrokerCount,
		Config:            map[string]string{"min.insync.replicas": "2"},
	})
	tb.Logf("created topic %s with %d partitions, replication factor 3", topic, partitions)
}

// createTopic creates one topic and waits until every partition has a leader
// and a full set of in-sync replicas.
//
// A failed attempt is retried, with a new admin client, for up to
// adminTimeout. Right after the cluster starts, librdkafka can replace the
// connection it first made to a broker with the broker's own, and a request
// in flight on the first is lost ("Broker handle destroyed without
// termination"). If that request had in fact created the topic, the retry
// finds it already there, which counts as done.
func (c *ThreeBrokerCluster) createTopic(tb testing.TB, specification kfk.TopicSpecification) {
	tb.Helper()

	deadline := time.Now().Add(adminTimeout)
	for attempt := 1; ; attempt++ {
		err := c.createTopicOnce(specification, attempt > 1)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatalf("creating topic %s, attempt %d: %v", specification.Topic, attempt, err)
		}
		tb.Logf("creating topic %s, attempt %d failed, retrying: %v", specification.Topic, attempt, err)
		time.Sleep(500 * time.Millisecond)
	}

	c.WaitForFullISR(tb, specification.Topic, brokerReadyTimeout)
}

// createTopicOnce makes one attempt. alreadyExistsIsDone accepts a topic that
// exists already, which on a retry means the first attempt made it.
func (c *ThreeBrokerCluster) createTopicOnce(specification kfk.TopicSpecification, alreadyExistsIsDone bool) error {
	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{"bootstrap.servers": c.liveBrokers()})
	if err != nil {
		return err
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results, err := admin.CreateTopics(ctx, []kfk.TopicSpecification{specification})
	if err != nil {
		return err
	}
	for _, result := range results {
		code := result.Error.Code()
		if code == kfk.ErrNoError || (alreadyExistsIsDone && code == kfk.ErrTopicAlreadyExists) {
			continue
		}
		return result.Error
	}
	return nil
}

// WaitForFullISR waits until every partition of topic has a leader and all
// three replicas in sync: the point at which a rolling update moves on to the
// next broker.
func (c *ThreeBrokerCluster) WaitForFullISR(tb testing.TB, topic string, timeout time.Duration) {
	tb.Helper()

	admin := c.newAdmin(tb)
	defer admin.Close()

	started := time.Now()
	deadline := started.Add(timeout)
	lastState := "not described yet"
	for time.Now().Before(deadline) {
		state, full := describeISR(admin, topic)
		if full {
			tb.Logf("topic %s fully in sync after %s", topic, time.Since(started).Round(time.Millisecond))
			return
		}
		lastState = state
		time.Sleep(250 * time.Millisecond)
	}
	tb.Fatalf("topic %s not fully in sync within %s: %s", topic, timeout, lastState)
}

// describeISR reports whether every partition of topic has a leader and three
// in-sync replicas, and otherwise what it saw instead.
func describeISR(admin *kfk.AdminClient, topic string) (state string, full bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := admin.DescribeTopics(ctx, kfk.NewTopicCollectionOfTopicNames([]string{topic}))
	if err != nil {
		return err.Error(), false
	}
	for _, description := range result.TopicDescriptions {
		if description.Error.Code() != kfk.ErrNoError {
			return description.Error.Error(), false
		}
		for _, partition := range description.Partitions {
			if partition.Leader == nil || len(partition.Isr) < threeBrokerCount {
				return fmt.Sprintf("partition %d has %d of %d replicas in sync",
					partition.Partition, len(partition.Isr), threeBrokerCount), false
			}
		}
	}
	return "", true
}

// newAdmin returns an admin client that connects only to the brokers that are
// up. Given a broker the test has just killed, it could send its request
// there, and wait for an answer that never comes.
func (c *ThreeBrokerCluster) newAdmin(tb testing.TB) *kfk.AdminClient {
	tb.Helper()

	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{"bootstrap.servers": c.liveBrokers()})
	if err != nil {
		tb.Fatalf("creating admin client: %v", err)
	}
	return admin
}

// liveBrokers returns the addresses of the brokers that are up, joined for
// bootstrap.servers.
func (c *ThreeBrokerCluster) liveBrokers() string {
	var live []string
	for index, address := range c.Brokers {
		if !c.down[index] {
			live = append(live, address)
		}
	}
	return strings.Join(live, ",")
}

// waitForBrokers waits until a metadata request lists count brokers.
func (c *ThreeBrokerCluster) waitForBrokers(tb testing.TB, count int) {
	tb.Helper()

	admin := c.newAdmin(tb)
	defer admin.Close()

	deadline := time.Now().Add(brokerReadyTimeout)
	for time.Now().Before(deadline) {
		metadata, err := admin.GetMetadata(nil, false, 3000)
		if err == nil && len(metadata.Brokers) >= count {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	tb.Fatalf("the cluster did not list %d brokers within %s", count, brokerReadyTimeout)
}

// waitForBroker waits until one broker answers a metadata request on its own
// address, and lists itself in the answer, which it does once it has
// registered with the controller again. Other brokers may still be down.
func (c *ThreeBrokerCluster) waitForBroker(tb testing.TB, index int) {
	tb.Helper()

	admin, err := kfk.NewAdminClient(&kfk.ConfigMap{"bootstrap.servers": c.Brokers[index]})
	if err != nil {
		tb.Fatalf("creating admin client for broker %d: %v", index+1, err)
	}
	defer admin.Close()

	nodeID := int32(index + 1)
	deadline := time.Now().Add(brokerReadyTimeout)
	for time.Now().Before(deadline) {
		metadata, err := admin.GetMetadata(nil, false, 3000)
		if err == nil && slices.ContainsFunc(metadata.Brokers, func(broker kfk.BrokerMetadata) bool {
			return broker.ID == nodeID
		}) {

			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	tb.Fatalf("broker %d did not answer within %s of starting", index+1, brokerReadyTimeout)
}
