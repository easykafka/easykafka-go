package helpers

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/easykafka/easykafka-go/subscribe"
)

// ConsumptionRecorder is a consumer handler that records what it receives.
// Each payload is expected to be a record's sequence number in decimal, as a
// sharedhelpers.PublishLoad writes it; the recorder notes which sequence numbers arrived,
// and when each partition delivered a message.
type ConsumptionRecorder struct {
	mu sync.Mutex
	// arrivalCountBySequence counts how often each sequence number arrived:
	// more than once is a redelivery, which an at-least-once consumer may do.
	// The key is the sequence number read from the payload, the value how many
	// times it arrived.
	arrivalCountBySequence map[int]int
	// arrivalTimesByPartition holds, per partition, when each of its messages
	// arrived. The key is the partition number, the value the arrival times in
	// the order the handler saw them.
	arrivalTimesByPartition map[int32][]time.Time
}

// NewConsumptionRecorder returns an empty recorder.
func NewConsumptionRecorder() *ConsumptionRecorder {
	return &ConsumptionRecorder{
		arrivalCountBySequence:  map[int]int{},
		arrivalTimesByPartition: map[int32][]time.Time{},
	}
}

// Handler is the function to pass to subscribe.WithHandler.
func (r *ConsumptionRecorder) Handler(ctx context.Context, payload []byte) *subscribe.Failure {
	message, ok := subscribe.MessageFromContext(ctx)
	if !ok {
		return &subscribe.Failure{Err: errors.New("no message metadata in the handler context")}
	}
	sequence, err := strconv.Atoi(string(payload))
	if err != nil {
		return &subscribe.Failure{Err: err}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.arrivalCountBySequence[sequence]++
	partitionArrivals := r.arrivalTimesByPartition[message.Partition]
	r.arrivalTimesByPartition[message.Partition] = append(partitionArrivals, time.Now())
	return nil
}

// WaitForSequences waits until every sequence number from 0 to count-1 has
// arrived, failing the test with how many are missing if timeout passes
// first.
func (r *ConsumptionRecorder) WaitForSequences(tb testing.TB, count int, timeout time.Duration) {
	tb.Helper()

	deadline := time.Now().Add(timeout)
	for {
		missing := r.missing(count)
		if missing == 0 {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("%d of %d records never consumed within %s", missing, count, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// missing returns how many of the sequence numbers 0 to count-1 have not
// arrived yet. WaitForSequences polls it until it reaches 0.
func (r *ConsumptionRecorder) missing(count int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	missing := 0
	for sequence := range count {
		if r.arrivalCountBySequence[sequence] == 0 {
			missing++
		}
	}
	return missing
}

// Redelivered returns how many messages arrived more than once.
func (r *ConsumptionRecorder) Redelivered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	redelivered := 0
	for _, times := range r.arrivalCountBySequence {
		// The first arrival is the delivery itself, every further one a
		// redelivery. times is at least 1: a record that never arrived has no
		// entry.
		redelivered += times - 1
	}
	return redelivered
}

// LongestGaps returns, for each partition that delivered anything, the
// longest time between two of its consecutive messages, and when that gap
// ended. Keyed by partition number. After a broker crash at 15:04:00, for
// example (EndedAt shown as clock times):
//
//	map[int32]Gap{
//		0: {Length: 13.3 * time.Second, EndedAt: 15:04:13.3}, // partition 0 paused 13.3 s
//		1: {Length: 12.3 * time.Second, EndedAt: 15:04:12.3},
//		2: {Length: 12.8 * time.Second, EndedAt: 15:04:12.8},
//	}
func (r *ConsumptionRecorder) LongestGaps() map[int32]Gap {
	r.mu.Lock()
	defer r.mu.Unlock()
	gaps := make(map[int32]Gap, len(r.arrivalTimesByPartition))
	for partition, times := range r.arrivalTimesByPartition {
		sorted := slices.Clone(times)
		slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })
		var longest Gap
		for index := 1; index < len(sorted); index++ {
			if gap := sorted[index].Sub(sorted[index-1]); gap > longest.Length {
				longest = Gap{Length: gap, EndedAt: sorted[index]}
			}
		}
		gaps[partition] = longest
	}
	return gaps
}

// Gap is a pause in one partition's messages.
type Gap struct {
	// Length is the time between the two consecutive messages either side of
	// the pause. Zero when the partition delivered fewer than two messages.
	Length time.Duration
	// EndedAt is when the message ending the pause arrived. Taking Length
	// from it gives when the pause began.
	EndedAt time.Time
}
