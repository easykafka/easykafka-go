package unit

import (
	"errors"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/internal/publishdriver"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishNewWithBrokersOnly verifies that brokers are the only required
// option.
func TestPublishNewWithBrokersOnly(t *testing.T) {
	publisher, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker))
	require.NoError(t, err)
	assert.NotNil(t, publisher)
}

// TestPublishNewRequiresBrokers verifies that New fails without WithBrokers,
// naming the option.
func TestPublishNewRequiresBrokers(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WithBrokers")
}

// TestPublishNewRejectsNilOption verifies that a nil option is an error, not a
// panic.
func TestPublishNewRejectsNilOption(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "option cannot be nil")
}

// TestPublishWithBrokersValidation verifies that an empty broker list and an
// empty address are both rejected.
func TestPublishWithBrokersValidation(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t, publish.WithBrokers())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one broker")

	_, err = helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker, ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker address cannot be empty")
}

// TestPublishWithKafkaConfigRejectsNil verifies that a nil map is rejected.
func TestPublishWithKafkaConfigRejectsNil(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), publish.WithKafkaConfig(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kafka config cannot be nil")
}

// TestPublishWithKafkaConfigRejectsManagedKeys verifies that every key the
// publisher manages is rejected, and that the error names what owns it.
func TestPublishWithKafkaConfigRejectsManagedKeys(t *testing.T) {
	managed := map[string]string{
		"bootstrap.servers":          "WithBrokers",
		"acks":                       "WithAcksLeader",
		"request.required.acks":      "WithAcksLeader",
		"enable.idempotence":         "WithoutIdempotence",
		"enable.gapless.guarantee":   "fatal error",
		"partitioner":                "WithPartitioner",
		"message.timeout.ms":         "WithDeliveryTimeout",
		"delivery.timeout.ms":        "WithDeliveryTimeout",
		"transactional.id":           "transactions",
		"default.topic.config":       "bypass",
		"go.delivery.reports":        "confluent-kafka-go",
		"go.events.channel.size":     "confluent-kafka-go",
		"{topic}.acks":               "default.topic.config",
		"{topic}.partitioner":        "default.topic.config",
		"{topic}.message.timeout.ms": "default.topic.config",
		"{topic}.compression.type":   "without the prefix",
	}
	for key, owner := range managed {
		t.Run(key, func(t *testing.T) {
			_, err := helpers.NewPublishWithFake(t,
				publish.WithBrokers(helpers.PublishBroker),
				publish.WithKafkaConfig(map[string]any{key: "value"}),
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			assert.Contains(t, err.Error(), owner)
		})
	}
}

// TestPublishWithKafkaConfigReportsEveryManagedKey verifies that one error
// names every rejected key, not just the first.
func TestPublishWithKafkaConfigReportsEveryManagedKey(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t,
		publish.WithBrokers(helpers.PublishBroker),
		publish.WithKafkaConfig(map[string]any{"acks": "1", "partitioner": "random", "linger.ms": 5}),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"acks"`)
	assert.Contains(t, err.Error(), `"partitioner"`)
	assert.NotContains(t, err.Error(), "linger.ms")
}

// TestPublishWithKafkaConfigPassesOtherKeys verifies that keys the publisher
// does not manage are accepted.
func TestPublishWithKafkaConfigPassesOtherKeys(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t,
		publish.WithBrokers(helpers.PublishBroker),
		publish.WithKafkaConfig(map[string]any{
			"security.protocol": "SASL_SSL",
			"sasl.mechanism":    "SCRAM-SHA-512",
			"client.id":         "invoices",
			"linger.ms":         5,
			"compression.type":  "lz4",
		}),
	)
	require.NoError(t, err)
}

// TestPublishWithKafkaConfigDoesNotModifyTheMap verifies that the caller's map
// is left as it was.
func TestPublishWithKafkaConfigDoesNotModifyTheMap(t *testing.T) {
	kafkaConfig := map[string]any{"linger.ms": 5}
	_, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), publish.WithKafkaConfig(kafkaConfig))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"linger.ms": 5}, kafkaConfig)
}

// TestPublishMaxInFlightLimitedWhileIdempotent verifies that more than 5
// requests in flight are rejected while idempotence is on, and accepted once
// either option turns it off.
func TestPublishMaxInFlightLimitedWhileIdempotent(t *testing.T) {
	const key = "max.in.flight.requests.per.connection"

	cases := []struct {
		name    string
		value   any
		options []publish.Option
		wantErr string
	}{
		{name: "6 by default", value: 6, wantErr: "at most 5 while idempotence is on"},
		{name: "6 as a string", value: "6", wantErr: "at most 5 while idempotence is on"},
		{name: "6 as a float", value: float64(6), wantErr: "at most 5 while idempotence is on"},
		{name: "5 by default", value: 5},
		{name: "1 by default", value: 1},
		{name: "6 without idempotence", value: 6, options: []publish.Option{publish.WithoutIdempotence()}},
		{name: "6 with leader acks", value: 6, options: []publish.Option{publish.WithAcksLeader()}},
		{name: "not a number", value: "lots", wantErr: "must be a number"},
		{name: "not a whole number", value: 2.5, wantErr: "whole number"},
		{name: "unsupported type", value: []int{1}, wantErr: "must be a number"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := append([]publish.Option{
				publish.WithBrokers(helpers.PublishBroker),
				publish.WithKafkaConfig(map[string]any{key: testCase.value}),
			}, testCase.options...)
			_, err := helpers.NewPublishWithFake(t, options...)
			if testCase.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.wantErr)
		})
	}
}

// TestPublishMaxInFlightCheckIgnoresOptionOrder verifies that turning
// idempotence off after setting the in-flight limit is honoured, since New
// checks the combination after every option has run.
func TestPublishMaxInFlightCheckIgnoresOptionOrder(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t,
		publish.WithKafkaConfig(map[string]any{"max.in.flight.requests.per.connection": 10}),
		publish.WithBrokers(helpers.PublishBroker),
		publish.WithoutIdempotence(),
	)
	require.NoError(t, err)
}

// TestPublishWithPartitioner verifies that every documented partitioner is
// accepted and an unknown one is rejected, listing the valid values.
func TestPublishWithPartitioner(t *testing.T) {
	for _, partitioner := range []publish.Partitioner{
		publish.PartitionerDefault,
		publish.PartitionerJavaCompatible,
		publish.PartitionerMurmur2,
		publish.PartitionerConsistent,
		publish.PartitionerRandom,
		publish.PartitionerFNV1A,
		publish.PartitionerFNV1ARandom,
	} {
		_, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), publish.WithPartitioner(partitioner))
		require.NoError(t, err, "partitioner %q", partitioner)
	}

	_, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), publish.WithPartitioner("murmur3"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown partitioner "murmur3"`)
	assert.Contains(t, err.Error(), "murmur2_random")
}

// TestPublishPartitionerConstantsMatchLibrdkafka pins the constants to
// librdkafka's names, which are what reach the producer configuration.
func TestPublishPartitionerConstantsMatchLibrdkafka(t *testing.T) {
	assert.Equal(t, publish.PartitionerDefault, publish.Partitioner("consistent_random"))
	assert.Equal(t, publish.PartitionerJavaCompatible, publish.Partitioner("murmur2_random"))
	assert.Equal(t, publish.PartitionerMurmur2, publish.Partitioner("murmur2"))
	assert.Equal(t, publish.PartitionerConsistent, publish.Partitioner("consistent"))
	assert.Equal(t, publish.PartitionerRandom, publish.Partitioner("random"))
	assert.Equal(t, publish.PartitionerFNV1A, publish.Partitioner("fnv1a"))
	assert.Equal(t, publish.PartitionerFNV1ARandom, publish.Partitioner("fnv1a_random"))
}

// TestPublishWithDeliveryTimeout verifies the bounds: at least 1 ms, at most
// what librdkafka's message.timeout.ms can hold.
func TestPublishWithDeliveryTimeout(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		wantErr string
	}{
		{name: "30 s", timeout: 30 * time.Second},
		{name: "1 ms", timeout: time.Millisecond},
		{name: "24 days", timeout: 24 * 24 * time.Hour},
		{name: "zero", timeout: 0, wantErr: "at least 1ms"},
		{name: "negative", timeout: -time.Second, wantErr: "at least 1ms"},
		{name: "under 1 ms", timeout: 500 * time.Microsecond, wantErr: "at least 1ms"},
		{name: "over the int32 millisecond limit", timeout: 25 * 24 * time.Hour, wantErr: "at most"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), publish.WithDeliveryTimeout(testCase.timeout))
			if testCase.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.wantErr)
		})
	}
}

// TestPublishFunctionOptionsRejectNil verifies that the callback options refuse
// a nil function.
func TestPublishFunctionOptionsRejectNil(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), publish.WithDeliveryErrorFunc(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delivery error func cannot be nil")

	_, err = helpers.NewPublishWithFake(t, publish.WithBrokers(helpers.PublishBroker), publish.WithFatalHandler(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fatal handler cannot be nil")
}

// TestPublishAcceptsEveryOption verifies that all options combine.
func TestPublishAcceptsEveryOption(t *testing.T) {
	_, err := helpers.NewPublishWithFake(t,
		publish.WithBrokers(helpers.PublishBroker, "localhost:2"),
		publish.WithKafkaConfig(map[string]any{"linger.ms": 1}),
		publish.WithoutIdempotence(),
		publish.WithAcksLeader(),
		publish.WithPartitioner(publish.PartitionerJavaCompatible),
		publish.WithDeliveryTimeout(10*time.Second),
		publish.WithLogger(zerolog.Nop()),
		publish.WithDeliveryErrorFunc(func(publish.DeliveryError) {}),
		publish.WithFatalHandler(func(error) {}),
	)
	require.NoError(t, err)
}

// TestPublishOptionsReachTheDriver verifies what New hands the driver, for
// the defaults and with every option that changes it.
func TestPublishOptionsReachTheDriver(t *testing.T) {
	_, fake := helpers.NewFakePublisher(t)
	assert.Equal(t, publishdriver.Config{
		Brokers:         []string{helpers.PublishBroker},
		KafkaConfig:     map[string]any{},
		Idempotence:     true,
		Partitioner:     "consistent_random",
		DeliveryTimeout: 30 * time.Second,
	}, fake.Config())

	_, fake = helpers.NewFakePublisher(t,
		publish.WithKafkaConfig(map[string]any{"linger.ms": 1}),
		publish.WithAcksLeader(),
		publish.WithPartitioner(publish.PartitionerJavaCompatible),
		publish.WithDeliveryTimeout(10*time.Second),
	)
	assert.Equal(t, publishdriver.Config{
		Brokers:         []string{helpers.PublishBroker},
		KafkaConfig:     map[string]any{"linger.ms": 1},
		AcksLeader:      true,
		Partitioner:     "murmur2_random",
		DeliveryTimeout: 10 * time.Second,
	}, fake.Config())

	_, fake = helpers.NewFakePublisher(t, publish.WithoutIdempotence())
	assert.False(t, fake.Config().Idempotence)
	assert.False(t, fake.Config().AcksLeader, "WithoutIdempotence keeps acks=all")
}

// TestPublishWithProducerFactoryRejectsNil verifies that a nil factory is
// rejected.
func TestPublishWithProducerFactoryRejectsNil(t *testing.T) {
	_, err := publish.New(publish.WithBrokers(helpers.PublishBroker), publish.WithProducerFactory(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "producer factory cannot be nil")
}

// TestPublishNewReportsFactoryError verifies that a producer that cannot be
// built fails New.
func TestPublishNewReportsFactoryError(t *testing.T) {
	_, err := publish.New(
		publish.WithBrokers(helpers.PublishBroker),
		publish.WithProducerFactory(func(publishdriver.Config) (publishdriver.Producer, error) {
			return nil, errors.New("no producer today")
		}),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating the producer: no producer today")
}
