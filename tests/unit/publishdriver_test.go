package unit

import (
	"context"
	"errors"
	"testing"
	"time"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/easykafka/easykafka-go/internal/publishdriver"
	"github.com/easykafka/easykafka-go/publish"
	"github.com/easykafka/easykafka-go/tests/unit/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishDriverConfigMap verifies the librdkafka configuration: the
// caller's keys, every managed key set explicitly, and the Go-side report
// settings.
func TestPublishDriverConfigMap(t *testing.T) {
	config := helpers.PublishDriverConfig()
	configMap, err := publishdriver.ConfigMap(config)
	require.NoError(t, err)
	assert.Equal(t, kfk.ConfigMap{
		"linger.ms":                 1,
		"client.id":                 "invoices",
		"bootstrap.servers":         "localhost:1,localhost:2",
		"acks":                      "all",
		"enable.idempotence":        true,
		"partitioner":               "murmur2_random",
		"message.timeout.ms":        30000,
		"go.delivery.reports":       true,
		"go.delivery.report.fields": "none",
	}, *configMap)
	assert.Equal(t, map[string]any{"linger.ms": 1, "client.id": "invoices"}, config.KafkaConfig, "the caller's map is not modified")
}

// TestPublishDriverConfigMapLeaderAcks verifies acks=1 with idempotence off.
func TestPublishDriverConfigMapLeaderAcks(t *testing.T) {
	config := helpers.PublishDriverConfig()
	config.AcksLeader, config.Idempotence = true, false
	configMap, err := publishdriver.ConfigMap(config)
	require.NoError(t, err)
	assert.Equal(t, "1", (*configMap)["acks"])
	assert.Equal(t, false, (*configMap)["enable.idempotence"])
}

// TestPublishDriverConstructionError verifies that a configuration
// librdkafka refuses fails New with librdkafka's reason.
func TestPublishDriverConstructionError(t *testing.T) {
	_, err := publish.New(
		publish.WithBrokers(helpers.PublishBroker),
		publish.WithKafkaConfig(map[string]any{"no.such.property": "x"}),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating kafka producer")
	assert.Contains(t, err.Error(), `No such configuration property: "no.such.property"`)
}

// TestPublishDriverTranslatesReports verifies a report's translation: the
// token from Opaque, the position, and the error flags.
func TestPublishDriverTranslatesReports(t *testing.T) {
	topic := "invoices"
	token := &struct{ name string }{"record"}

	event, ok := publishdriver.TranslateEvent(&kfk.Message{
		TopicPartition: kfk.TopicPartition{Topic: &topic, Partition: 2, Offset: 41},
		Opaque:         token,
	})
	require.True(t, ok)
	assert.Equal(t, publishdriver.Report{Token: token, Partition: 2, Offset: 41}, event)

	cases := []struct {
		name string
		code kfk.ErrorCode
		want publishdriver.KafkaError
	}{
		{name: "timed out", code: kfk.ErrMsgTimedOut, want: publishdriver.KafkaError{Sentinel: publishdriver.ErrDeliveryTimeout}},
		{name: "purged in queue", code: kfk.ErrPurgeQueue, want: publishdriver.KafkaError{Sentinel: publishdriver.ErrNotDelivered}},
		{name: "purged in flight", code: kfk.ErrPurgeInflight, want: publishdriver.KafkaError{Sentinel: publishdriver.ErrNotDelivered}},
		{name: "rejected", code: kfk.ErrMsgSizeTooLarge, want: publishdriver.KafkaError{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			event, ok := publishdriver.TranslateEvent(&kfk.Message{
				TopicPartition: kfk.TopicPartition{Topic: &topic, Partition: -1, Offset: kfk.OffsetInvalid,
					Error: kfk.NewError(testCase.code, "", false)},
				Opaque: token,
			})
			require.True(t, ok)
			report, isReport := event.(publishdriver.Report)
			require.True(t, isReport)
			assert.Equal(t, int32(-1), report.Partition)
			require.NotNil(t, report.Err)
			want := testCase.want
			want.Code, want.Message = testCase.code.String(), testCase.code.String()
			assert.Equal(t, want, *report.Err)
		})
	}
}

// TestPublishDriverTranslatesClientErrors verifies a client error's flags, and
// that other events are dropped.
func TestPublishDriverTranslatesClientErrors(t *testing.T) {
	cases := []struct {
		name string
		err  kfk.Error
		want publishdriver.KafkaError
	}{
		{name: "all brokers down", err: kfk.NewError(kfk.ErrAllBrokersDown, "3/3 brokers are down", false),
			want: publishdriver.KafkaError{Code: kfk.ErrAllBrokersDown.String(), Message: "3/3 brokers are down", Disconnected: true}},
		{name: "transport", err: kfk.NewError(kfk.ErrTransport, "connection refused", false),
			want: publishdriver.KafkaError{Code: kfk.ErrTransport.String(), Message: "connection refused", Disconnected: true}},
		// A fatal event carries its cause's code, here an idempotent producer
		// refused for lack of IDEMPOTENT_WRITE: only IsFatal marks it fatal.
		{name: "fatal", err: kfk.NewError(kfk.ErrClusterAuthorizationFailed, "not authorized", true),
			want: publishdriver.KafkaError{Code: kfk.ErrClusterAuthorizationFailed.String(), Message: "Fatal error: not authorized", Fatal: true, Sentinel: publishdriver.ErrFatal}},
		{name: "fatal code", err: kfk.NewError(kfk.ErrFatal, "fatal", false),
			want: publishdriver.KafkaError{Code: kfk.ErrFatal.String(), Message: "fatal", Fatal: true, Sentinel: publishdriver.ErrFatal}},
		{name: "queue full", err: kfk.NewError(kfk.ErrQueueFull, "", false),
			want: publishdriver.KafkaError{Code: kfk.ErrQueueFull.String(), Message: kfk.ErrQueueFull.String(), Sentinel: publishdriver.ErrQueueFull}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			event, ok := publishdriver.TranslateEvent(testCase.err)
			require.True(t, ok)
			assert.Equal(t, publishdriver.ClientError{Err: &testCase.want}, event)
		})
	}

	_, ok := publishdriver.TranslateEvent(&kfk.Stats{})
	assert.False(t, ok, "statistics are dropped")
}

// TestPublishDriverTranslatesForeignError verifies that an error from outside
// confluent-kafka-go keeps its message and has no code.
func TestPublishDriverTranslatesForeignError(t *testing.T) {
	assert.Equal(t, &publishdriver.KafkaError{Message: "elsewhere"}, publishdriver.TranslateError(errors.New("elsewhere")))
	assert.Equal(t, "Local: Queue full", (&publishdriver.KafkaError{Code: "Local: Queue full"}).Error())

	withSentinel := &publishdriver.KafkaError{Message: "Local: Message timed out", Sentinel: publishdriver.ErrDeliveryTimeout}
	assert.Equal(t, "publish: not acknowledged within the delivery timeout: Local: Message timed out", withSentinel.Error())
	require.ErrorIs(t, withSentinel, publishdriver.ErrDeliveryTimeout)
}

// TestPublishDriverPurgeReportsCarryTheToken verifies, against real
// librdkafka with no broker, that a purged record is reported on Reports with
// its token and the purge flag, and that Close then closes Reports.
//
// No broker is needed: Produce only enqueues locally, and with nothing
// listening the record stays queued. Purge removes it without sending it, and
// librdkafka then generates its report itself ("Local: Purged in queue"),
// which takes the same path to Reports as a broker's would.
func TestPublishDriverPurgeReportsCarryTheToken(t *testing.T) {
	producer, err := publishdriver.New(helpers.PublishDriverConfig())
	require.NoError(t, err)

	token := &struct{ name string }{"record"}
	require.NoError(t, producer.Produce(publishdriver.Record{
		Topic: "invoices", Key: []byte("k"), Value: []byte("v"),
		Headers: []publishdriver.Header{{Key: "trace", Value: []byte("t-1")}},
	}, token))
	assert.Equal(t, 1, producer.Len())
	require.NoError(t, producer.Purge())

	// Connection errors from the unreachable broker come on the same channel.
	report := helpers.NextPublishReport(t, producer.Reports())
	assert.Same(t, token, report.Token)
	require.NotNil(t, report.Err)
	require.ErrorIs(t, report.Err, publishdriver.ErrNotDelivered)
	assert.False(t, report.Err.Fatal)
	assert.Equal(t, "Local: Purged in queue", report.Err.Code)

	producer.Close()
	helpers.RequireReportsClosed(t, producer.Reports())
}

// TestPublishDriverAfterClose verifies that every call after Close is safe,
// and Close is idempotent.
func TestPublishDriverAfterClose(t *testing.T) {
	producer, err := publishdriver.New(helpers.PublishDriverConfig())
	require.NoError(t, err)
	producer.Close()
	producer.Close()

	require.ErrorIs(t, producer.Produce(publishdriver.Record{Topic: "invoices"}, nil), publishdriver.ErrClosed)
	require.ErrorIs(t, producer.Purge(), publishdriver.ErrClosed)
	_, err = producer.TopicPartitions(context.Background(), []string{"invoices"})
	require.ErrorIs(t, err, publishdriver.ErrClosed)
	assert.Zero(t, producer.Flush(time.Second))
	assert.Zero(t, producer.Len())
	helpers.RequireReportsClosed(t, producer.Reports())
}

// TestPublishDriverTopicPartitionsHonoursContext verifies that the metadata
// request against an unreachable broker ends with its context.
func TestPublishDriverTopicPartitionsHonoursContext(t *testing.T) {
	producer, err := publishdriver.New(helpers.PublishDriverConfig())
	require.NoError(t, err)
	defer producer.Close()

	for _, topics := range [][]string{{"invoices"}, nil} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		started := time.Now()
		_, err = producer.TopicPartitions(ctx, topics)
		cancel()
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 2*time.Second)
	}
}
