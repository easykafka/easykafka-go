// Command consumer runs two easykafka consumers in one process — one on the
// source topic and one on its retry topic — with a handler that carries out
// each message's payload script. easykafka writes to the retry topic but never
// reads it, so the playground does.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	easykafka "github.com/easykafka/easykafka-go"
	"github.com/rs/zerolog"
)

const (
	modeSingle = "single"
	modeBatch  = "batch"

	strategyRetry    = "retry"
	strategySkip     = "skip"
	strategyFailFast = "fail-fast"

	defaultBatchSize    = 10
	defaultBatchTimeout = 2 * time.Second
	defaultMaxAttempts  = 3
	defaultInitialDelay = 2 * time.Second
	defaultMaxDelay     = 30 * time.Second
	pollTimeout         = 100 * time.Millisecond

	// exitUsage is the exit status for a command-line mistake, as the flag
	// package uses.
	exitUsage = 2
)

// config holds the command-line flags.
type config struct {
	brokers         []string
	topic           string
	group           string
	mode            string
	batchSize       int
	batchTimeout    time.Duration
	strategy        string
	maxAttempts     int
	initialDelay    time.Duration
	maxDelay        time.Duration
	noRetryConsumer bool
	processingDelay time.Duration
}

func main() {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "consumer:", err)
		os.Exit(exitUsage)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "consumer:", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop is called explicitly above; exitAfterDefer does not see it
	}
}

func parseFlags() (config, error) {
	var cfg config
	brokers := flag.String("brokers", "localhost:9092", "Kafka bootstrap servers, comma-separated")
	flag.StringVar(&cfg.topic, "topic", "demo.orders",
		"source topic; retry and DLQ topics are <topic>.retry and <topic>.dlq")
	flag.StringVar(&cfg.group, "group", "demo", "source consumer group; the retry consumer uses <group>-retry")
	flag.StringVar(&cfg.mode, "mode", modeSingle, "single or batch")
	flag.IntVar(&cfg.batchSize, "batch-size", defaultBatchSize, "batch mode only")
	flag.DurationVar(&cfg.batchTimeout, "batch-timeout", defaultBatchTimeout, "batch mode only")
	flag.StringVar(&cfg.strategy, "strategy", strategyRetry, "retry, skip or fail-fast")
	flag.IntVar(&cfg.maxAttempts, "max-attempts", defaultMaxAttempts, "retry strategy: attempts before the DLQ")
	flag.DurationVar(&cfg.initialDelay, "initial-delay", defaultInitialDelay,
		"retry strategy: backoff before the first retry")
	flag.DurationVar(&cfg.maxDelay, "max-delay", defaultMaxDelay, "retry strategy: backoff cap")
	flag.BoolVar(&cfg.noRetryConsumer, "no-retry-consumer", false, "do not consume the retry topic")
	flag.DurationVar(&cfg.processingDelay, "processing-delay", 0,
		"block for this long on each message (single mode) or batch (batch mode)")
	flag.Parse()

	cfg.brokers = strings.Split(*brokers, ",")
	if flag.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if cfg.mode != modeSingle && cfg.mode != modeBatch {
		return cfg, fmt.Errorf("--mode must be %s or %s, not %q", modeSingle, modeBatch, cfg.mode)
	}
	switch cfg.strategy {
	case strategyRetry, strategySkip, strategyFailFast:
	default:
		return cfg, fmt.Errorf("--strategy must be %s, %s or %s, not %q",
			strategyRetry, strategySkip, strategyFailFast, cfg.strategy)
	}
	return cfg, nil
}

// run starts the source consumer and, unless disabled, the retry consumer, and
// blocks until both have stopped. If either stops with an error — fail-fast,
// say — the other is stopped too and the error is returned.
func run(ctx context.Context, cfg config) error {
	// The library's own log lines — failures, retries, DLQ writes — go to
	// stderr at warning level; the playground's lines go to stdout.
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.000"}).
		Level(zerolog.WarnLevel).With().Timestamp().Logger()
	out := &printer{}

	type named struct {
		name     string
		consumer easykafka.Consumer
	}
	source, err := newConsumer("source", cfg.topic, cfg.group, cfg, logger, out)
	if err != nil {
		return err
	}
	consumers := []named{{"source", source}}
	if !cfg.noRetryConsumer {
		retry, err := newConsumer("retry", cfg.topic+".retry", cfg.group+"-retry", cfg, logger, out)
		if err != nil {
			return err
		}
		consumers = append(consumers, named{"retry", retry})
	}

	out.printf("consumer: mode=%s strategy=%s max-attempts=%d initial-delay=%s processing-delay=%s retry-consumer=%t\n",
		cfg.mode, cfg.strategy, cfg.maxAttempts, cfg.initialDelay, cfg.processingDelay, !cfg.noRetryConsumer)

	// A child of the signal context, so it is cancelled either by Ctrl+C or
	// SIGTERM through the parent, or by calling cancel — which a consumer that
	// stops with an error does, to take the other one down with it. Cancelling
	// the child never cancels the parent.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, c := range consumers {
		wg.Go(func() {
			if err := c.consumer.Start(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s consumer: %w", c.name, err))
				mu.Unlock()
				cancel() // take the other consumer down with it
			}
		})
	}
	wg.Wait()
	out.printf("consumer: stopped\n")
	return errors.Join(errs...)
}

// newConsumer builds one consumer. Each gets its own strategy instance, because
// a consumer initializes its strategy's producers when it starts.
func newConsumer(
	name, topic, group string,
	cfg config,
	logger zerolog.Logger,
	out *printer,
) (easykafka.Consumer, error) {

	strategy, err := newStrategy(cfg, logger)
	if err != nil {
		return nil, err
	}

	h := &handler{name: name, processingDelay: cfg.processingDelay, out: out}
	opts := []easykafka.Option{
		easykafka.WithTopic(topic),
		easykafka.WithBrokers(cfg.brokers...),
		easykafka.WithConsumerGroup(group),
		easykafka.WithErrorStrategy(strategy),
		easykafka.WithLogger(logger),
		easykafka.WithPollTimeout(pollTimeout),
	}
	if cfg.mode == modeBatch {
		opts = append(opts,
			easykafka.WithBatchHandler(h.handleBatch),
			easykafka.WithBatchSize(cfg.batchSize),
			easykafka.WithBatchTimeout(cfg.batchTimeout),
		)
	} else {
		opts = append(opts, easykafka.WithHandler(h.handle))
	}

	consumer, err := easykafka.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("%s consumer: %w", name, err)
	}
	return consumer, nil
}

func newStrategy(cfg config, logger zerolog.Logger) (easykafka.ErrorStrategy, error) {
	switch cfg.strategy {
	case strategySkip:
		return easykafka.NewSkipStrategy(logger), nil
	case strategyFailFast:
		return easykafka.NewFailFastStrategy(), nil
	default:
		s, err := easykafka.NewRetryStrategy(
			easykafka.WithRetryTopic(cfg.topic+".retry"),
			easykafka.WithDLQTopic(cfg.topic+".dlq"),
			easykafka.WithMaxAttempts(cfg.maxAttempts),
			easykafka.WithInitialDelay(cfg.initialDelay),
			easykafka.WithMaxDelay(cfg.maxDelay),
		)
		if err != nil {
			return nil, fmt.Errorf("retry strategy: %w", err)
		}
		return s, nil
	}
}
