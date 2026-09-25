// Command producer sends playground messages: each argument is one message
// whose payload is a script such as "ko/ko/ok". "9xok" sends nine copies, and
// "bin" sends raw bytes that are not valid UTF-8 — a malformed record.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"

	kfk "github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// exitUsage is the exit status for a command-line mistake, as the flag package
// uses.
const exitUsage = 2

// binWord is the argument that stands for a malformed record.
const binWord = "bin"

// binPayload is what "bin" sends: bytes that are not valid UTF-8, so the
// consumer cannot parse them and the DLQ record shows them surviving unchanged.
var binPayload = []byte{0xff, 0xfe, 0x00, 0x80, 'b', 'i', 'n', 0xc3, 0x28}

// repeatPrefix matches "<N>x<script>", such as "9xok".
var repeatPrefix = regexp.MustCompile(`^(\d+)x(.+)$`)

func main() {
	brokers := flag.String("brokers", "localhost:9092", "Kafka bootstrap servers")
	topic := flag.String("topic", "demo.orders", "topic to send to")
	keyPrefix := flag.String("key-prefix", "msg", "keys are <prefix>-0001, <prefix>-0002, ...")
	partition := flag.Int("partition", -1, "send every message to this partition (-1: let the producer choose)")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "usage: producer [flags] <script> [<script> ...]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	payloads, err := expand(flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "producer:", err)
		flag.Usage()
		os.Exit(exitUsage)
	}

	pinned := int32(*partition) //nolint:gosec // a partition number typed on the command line
	if err := send(*brokers, *topic, *keyPrefix, pinned, payloads); err != nil {
		fmt.Fprintln(os.Stderr, "producer:", err)
		os.Exit(1)
	}
}

// expand turns the command-line arguments into one payload per message,
// resolving "<N>x" repeats and the "bin" word.
func expand(args []string) ([][]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("no messages given")
	}

	var payloads [][]byte
	for _, arg := range args {
		count, text := 1, arg
		if m := repeatPrefix.FindStringSubmatch(arg); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%q: repeat count must be a positive number", arg)
			}
			count, text = n, m[2]
		}

		payload := []byte(text)
		if text == binWord {
			payload = binPayload
		}
		for range count {
			payloads = append(payloads, payload)
		}
	}
	return payloads, nil
}

// send produces the payloads in order, waiting for each delivery so the printed
// partition and offset are the record's real ones.
func send(brokers, topic, keyPrefix string, partition int32, payloads [][]byte) error {
	producer, err := kfk.NewProducer(&kfk.ConfigMap{"bootstrap.servers": brokers})
	if err != nil {
		return fmt.Errorf("creating producer: %w", err)
	}
	defer producer.Close()

	if partition < 0 {
		partition = kfk.PartitionAny
	}

	delivery := make(chan kfk.Event, 1)
	for i, payload := range payloads {
		key := fmt.Sprintf("%s-%04d", keyPrefix, i+1)
		err := producer.Produce(&kfk.Message{
			TopicPartition: kfk.TopicPartition{Topic: &topic, Partition: partition},
			Key:            []byte(key),
			Value:          payload,
		}, delivery)
		if err != nil {
			return fmt.Errorf("producing %s: %w", key, err)
		}

		report, ok := (<-delivery).(*kfk.Message)
		if !ok {
			return fmt.Errorf("producing %s: unexpected delivery event", key)
		}
		if report.TopicPartition.Error != nil {
			return fmt.Errorf("delivering %s: %w", key, report.TopicPartition.Error)
		}
		fmt.Printf("sent key=%s partition=%d offset=%d payload=%s\n",
			key, report.TopicPartition.Partition, report.TopicPartition.Offset, describe(payload))
	}
	return nil
}

// describe renders a payload for the log: the script as text, or a note for the
// binary payload, which would garble the terminal.
func describe(payload []byte) string {
	if string(payload) == string(binPayload) {
		return fmt.Sprintf("<binary, %d bytes>", len(payload))
	}
	return string(payload)
}
