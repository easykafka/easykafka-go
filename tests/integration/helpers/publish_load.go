package helpers

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/publish"
)

// SendRecordFunc sends one record of a PublishLoad: the one numbered
// sequence, counting from 0. It returns what Writer.Send returns, so it is
// usually a closure around a writer's Send.
type SendRecordFunc func(sequence int) (*publish.Delivery, error)

// PublishLoad sends records at a steady rate until stopped, and records the
// outcome of each one and its latency, from Send to its delivery report.
type PublishLoad struct {
	send SendRecordFunc
	stop chan struct{}
	done chan struct{}

	// pending counts the records sent whose outcome is not known yet.
	pending sync.WaitGroup

	mu        sync.Mutex
	sent      int
	latencies []time.Duration
	failures  []error
}

// PublishLoadResult is what a PublishLoad saw.
type PublishLoadResult struct {
	Sent int
	// Failures holds every record that failed, at Send or on its report.
	Failures []error
	// Latencies of the acknowledged records.
	P50, P99, Max time.Duration
}

// StartPublishLoad calls send at rate records per second, with a sequence
// number counting from 0, until Stop.
func StartPublishLoad(rate int, send SendRecordFunc) *PublishLoad {
	load := &PublishLoad{send: send, stop: make(chan struct{}), done: make(chan struct{})}
	go load.run(time.Second / time.Duration(rate))
	return load
}

func (l *PublishLoad) run(interval time.Duration) {
	defer close(l.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for sequence := 0; ; sequence++ {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
		}
		l.sendOne(sequence)
	}
}

func (l *PublishLoad) sendOne(sequence int) {
	sentAt := time.Now()
	delivery, err := l.send(sequence)

	l.mu.Lock()
	l.sent++
	if err != nil {
		l.failures = append(l.failures, err)
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	l.pending.Go(func() {
		_, err := delivery.Wait(context.Background())
		latency := time.Since(sentAt)
		l.mu.Lock()
		defer l.mu.Unlock()
		if err != nil {
			l.failures = append(l.failures, err)
			return
		}
		l.latencies = append(l.latencies, latency)
	})
}

// Stop stops sending, waits up to timeout for every record sent to be
// reported, and returns what the load saw. It fails the test if a record is
// still unreported then.
func (l *PublishLoad) Stop(tb testing.TB, timeout time.Duration) PublishLoadResult {
	tb.Helper()

	close(l.stop)
	<-l.done

	reported := make(chan struct{})
	go func() {
		l.pending.Wait()
		close(reported)
	}()
	select {
	case <-reported:
	case <-time.After(timeout):
		tb.Fatalf("records still unreported %s after the load stopped", timeout)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	result := PublishLoadResult{Sent: l.sent, Failures: slices.Clone(l.failures)}
	if len(l.latencies) > 0 {
		latencies := slices.Clone(l.latencies)
		slices.Sort(latencies)
		result.P50 = latencies[len(latencies)*50/100]
		result.P99 = latencies[len(latencies)*99/100]
		result.Max = latencies[len(latencies)-1]
	}
	return result
}

// Log writes the result to the test log.
func (r PublishLoadResult) Log(tb testing.TB) {
	tb.Helper()
	tb.Logf("records sent %d, failed %d; latency p50 %s, p99 %s, max %s",
		r.Sent, len(r.Failures), r.P50.Round(time.Millisecond), r.P99.Round(time.Millisecond),
		r.Max.Round(time.Millisecond))
}
