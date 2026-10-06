package publish

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/easykafka/easykafka-go/internal/publishdriver"
)

// defaultDeliveryTimeout bounds how long a record may take to be acknowledged:
// long enough to ride out a leader election or a broker crash (measured at
// about 10 s), and well inside a consumer's max.poll.interval.ms (300 s by
// default) for callers that publish from a handler.
const defaultDeliveryTimeout = 30 * time.Second

// maxInFlightWithIdempotence is librdkafka's limit on
// max.in.flight.requests.per.connection while idempotence is on. Above it,
// librdkafka refuses to create the producer.
const maxInFlightWithIdempotence = 5

// config holds what the options set. It is built by New and not changed after.
type config struct {
	brokers         []string
	kafkaConfig     map[string]any
	idempotence     bool
	acksLeader      bool
	partitioner     Partitioner
	deliveryTimeout time.Duration
	logger          zerolog.Logger
	onDeliveryError DeliveryErrorFunc
	onFatal         func(error)
	// newProducer builds the producer. Defaults to the real driver; see
	// WithProducerFactory.
	newProducer func(publishdriver.Config) (publishdriver.Producer, error)
}

func defaultConfig() config {
	return config{
		kafkaConfig:     map[string]any{},
		idempotence:     true,
		partitioner:     PartitionerDefault,
		deliveryTimeout: defaultDeliveryTimeout,
		logger:          zerolog.Nop(),
		newProducer:     publishdriver.New,
	}
}

// driverConfig is what the driver needs to build the producer.
func (c *config) driverConfig() publishdriver.Config {
	return publishdriver.Config{
		Brokers:         c.brokers,
		KafkaConfig:     c.kafkaConfig,
		AcksLeader:      c.acksLeader,
		Idempotence:     c.idempotence,
		Partitioner:     string(c.partitioner),
		DeliveryTimeout: c.deliveryTimeout,
	}
}

// validate checks what no single option can: the combinations.
func (c *config) validate() error {
	if len(c.brokers) == 0 {
		return errors.New("at least one broker must be provided with WithBrokers")
	}
	if c.idempotence {
		return c.checkInFlightForIdempotence()
	}
	return nil
}

// checkInFlightForIdempotence rejects an in-flight limit librdkafka would
// refuse while idempotence is on.
func (c *config) checkInFlightForIdempotence() error {
	const key = "max.in.flight.requests.per.connection"
	raw, ok := c.kafkaConfig[key]
	if !ok {
		return nil
	}
	inFlight, err := integerValue(raw)
	if err != nil {
		return fmt.Errorf("kafka config key %q: %w", key, err)
	}
	if inFlight > maxInFlightWithIdempotence {
		return fmt.Errorf("%s must be at most %d while idempotence is on (the default); librdkafka would refuse to start",
			key, maxInFlightWithIdempotence)
	}
	return nil
}

// Option configures a Publisher. Options are applied by New, in order.
type Option func(*config) error

// WithBrokers specifies the Kafka broker addresses. Required. Empty addresses
// are rejected.
func WithBrokers(brokers ...string) Option {
	return func(c *config) error {
		if len(brokers) == 0 {
			return errors.New("at least one broker must be provided")
		}
		if slices.Contains(brokers, "") {
			return errors.New("broker address cannot be empty")
		}
		c.brokers = slices.Clone(brokers)
		return nil
	}
}

// managedKafkaKeys are librdkafka keys that WithKafkaConfig rejects, each with
// the reason. Keys with a prefix in managedKafkaKeyPrefixes are rejected too.
var managedKafkaKeys = map[string]string{
	"bootstrap.servers": "managed by WithBrokers",
	"acks":              "managed by WithAcksLeader; the default is acks=all",
	"request.required.acks": "managed by WithAcksLeader; the default is acks=all " +
		"(request.required.acks is another name for acks)",
	"enable.idempotence": "managed by the library: idempotence is on unless WithoutIdempotence or " +
		"WithAcksLeader is used",
	"enable.gapless.guarantee": "managed by the library and kept off: it turns a timed-out record into " +
		"a fatal error",
	"partitioner":         "managed by WithPartitioner",
	"message.timeout.ms":  "managed by WithDeliveryTimeout",
	"delivery.timeout.ms": "managed by WithDeliveryTimeout (delivery.timeout.ms is another name for message.timeout.ms)",
	"transactional.id":    "not supported: the publisher does not use transactions",
	"default.topic.config": "not supported: it would bypass WithAcksLeader, WithPartitioner and " +
		"WithDeliveryTimeout",
}

// managedKafkaKeyPrefixes are key prefixes that WithKafkaConfig rejects, each
// with the reason.
var managedKafkaKeyPrefixes = []struct{ prefix, reason string }{
	{"go.", "managed by the library: confluent-kafka-go's own settings decide how delivery " +
		"reports reach the publisher"},
	// confluent-kafka-go moves a "{topic}." key into default.topic.config, so
	// "{topic}.acks" is default.topic.config's acks under another spelling.
	{"{topic}.", "not supported: confluent-kafka-go moves it into default.topic.config, which would " +
		"bypass WithAcksLeader, WithPartitioner and WithDeliveryTimeout; set the property without the prefix"},
}

// managedKeyReason returns why WithKafkaConfig rejects key, if it does.
func managedKeyReason(key string) (string, bool) {
	if reason, managed := managedKafkaKeys[key]; managed {
		return reason, true
	}
	for _, managed := range managedKafkaKeyPrefixes {
		if strings.HasPrefix(key, managed.prefix) {
			return managed.reason, true
		}
	}
	return "", false
}

// WithKafkaConfig passes librdkafka configuration through to the producer:
// security, SASL and TLS settings, client.id, linger.ms, compression.type, and
// so on. The map is copied, never modified, and a later call replaces an
// earlier one.
//
// Keys the publisher manages are rejected, each with the reason: bootstrap.servers,
// acks, request.required.acks, enable.idempotence, enable.gapless.guarantee,
// partitioner, message.timeout.ms, delivery.timeout.ms, transactional.id,
// default.topic.config, and every key starting with "go." or "{topic}.". While idempotence
// is on, max.in.flight.requests.per.connection must be at most 5; New checks
// that, since it depends on other options.
func WithKafkaConfig(kafkaConfig map[string]any) Option {
	return func(c *config) error {
		if kafkaConfig == nil {
			return errors.New("kafka config cannot be nil")
		}
		var rejected []error
		for _, key := range slices.Sorted(maps.Keys(kafkaConfig)) {
			if reason, managed := managedKeyReason(key); managed {
				rejected = append(rejected, fmt.Errorf("kafka config key %q cannot be set: %s", key, reason))
			}
		}
		if len(rejected) > 0 {
			return errors.Join(rejected...)
		}
		c.kafkaConfig = maps.Clone(kafkaConfig)
		return nil
	}
}

// WithoutIdempotence turns idempotence off, keeping acks=all. Internal retries,
// which every broker restart causes, may then duplicate records and reorder
// them within a partition. Meant as an escape hatch, for example if a pattern of
// fatal idempotence errors ever appears.
func WithoutIdempotence() Option {
	return func(c *config) error {
		c.idempotence = false
		return nil
	}
}

// WithAcksLeader makes a record count as acknowledged once the partition leader
// has written it (acks=1), instead of every in-sync replica. It also turns
// idempotence off, which librdkafka cannot provide without acks=all. Meant for
// clusters whose min.insync.replicas equals their replication factor, where
// acks=all fails every write while a broker is down.
func WithAcksLeader() Option {
	return func(c *config) error {
		c.acksLeader = true
		c.idempotence = false
		return nil
	}
}

// WithPartitioner selects librdkafka's partitioner. The default is
// PartitionerDefault; PartitionerJavaCompatible matches the Java client.
func WithPartitioner(partitioner Partitioner) Option {
	return func(c *config) error {
		if !partitioner.valid() {
			known := make([]string, len(partitioners))
			for index, value := range partitioners {
				known[index] = string(value)
			}
			return fmt.Errorf("unknown partitioner %q; valid values are %s", partitioner, strings.Join(known, ", "))
		}
		c.partitioner = partitioner
		return nil
	}
}

// WithDeliveryTimeout sets how long a record may take to be acknowledged
// (librdkafka's message.timeout.ms). It is the upper bound of every wait on a
// record. The default is 30 s; it must be at least 1 ms.
//
// Under idempotence, the default, a record can be reported up to about 10 s
// after an outage starts even with a shorter timeout, so values below about
// 10 s are not honoured precisely.
func WithDeliveryTimeout(timeout time.Duration) Option {
	return func(c *config) error {
		if timeout < time.Millisecond {
			return fmt.Errorf("delivery timeout must be at least 1ms, got %s", timeout)
		}
		if timeout.Milliseconds() > math.MaxInt32 {
			return fmt.Errorf("delivery timeout must be at most %s, got %s",
				time.Duration(math.MaxInt32)*time.Millisecond, timeout)
		}
		c.deliveryTimeout = timeout
		return nil
	}
}

// WithLogger sets the logger. By default the publisher logs nothing.
func WithLogger(logger zerolog.Logger) Option {
	return func(c *config) error {
		c.logger = logger
		return nil
	}
}

// WithDeliveryErrorFunc sets a function called for every record that was not
// acknowledged, whether or not anyone waits on it. See DeliveryErrorFunc for
// its contract.
func WithDeliveryErrorFunc(onError DeliveryErrorFunc) Option {
	return func(c *config) error {
		if onError == nil {
			return errors.New("delivery error func cannot be nil")
		}
		c.onDeliveryError = onError
		return nil
	}
}

// WithFatalHandler sets a function called once, with the error, when the
// producer fails fatally. After that every send fails, and the publisher does
// not recover. A service typically uses it to start a graceful shutdown, so the
// process restarts with a fresh publisher, and Err for its liveness probe.
//
// It runs on the publisher's report goroutine, so it must not block, and it
// must not call Close or os.Exit: Close waits for that goroutine, and would
// never return. Cancel a context that the application's shutdown waits on
// instead. A panic in it is recovered and logged.
func WithFatalHandler(onFatal func(error)) Option {
	return func(c *config) error {
		if onFatal == nil {
			return errors.New("fatal handler cannot be nil")
		}
		c.onFatal = onFatal
		return nil
	}
}

// WithProducerFactory replaces the function that builds the publisher's
// librdkafka producer.
//
// This is a testing seam. It lets the publisher's logic — resolving
// deliveries, the error mapping, the callbacks — be driven by a scripted fake,
// with no broker and no Docker.
//
// It is exported only because the tests live in a separate package. It cannot
// be used from outside this module: its argument names types under internal/,
// which Go forbids other modules from importing, so no caller can construct
// one.
func WithProducerFactory(factory func(publishdriver.Config) (publishdriver.Producer, error)) Option {
	return func(c *config) error {
		if factory == nil {
			return errors.New("producer factory cannot be nil")
		}
		c.newProducer = factory
		return nil
	}
}

// integerValue reads a configuration value given as an integer type, a
// float64 (as JSON decoding produces) or a decimal string.
func integerValue(raw any) (int64, error) {
	switch value := raw.(type) {
	case int:
		return int64(value), nil
	case int8:
		return int64(value), nil
	case int16:
		return int64(value), nil
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	case uint8:
		return int64(value), nil
	case uint16:
		return int64(value), nil
	case uint32:
		return int64(value), nil
	case float64:
		if value != math.Trunc(value) {
			return 0, fmt.Errorf("must be a whole number, got %v", value)
		}
		return int64(value), nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64) //nolint:mnd // base 10, 64-bit
		if err != nil {
			return 0, fmt.Errorf("must be a number, got %q", value)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("must be a number, got %T", raw)
	}
}
