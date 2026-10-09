package helpers

import (
	"slices"
	"testing"
	"time"
)

// ReportPublishThroughput reports a publishing benchmark's records/s, over
// elapsed, and the p99 of latencies, one per record, in milliseconds.
func ReportPublishThroughput(b *testing.B, elapsed time.Duration, latencies []time.Duration) {
	b.Helper()

	b.ReportMetric(float64(len(latencies))/elapsed.Seconds(), "records/s")
	if len(latencies) == 0 {
		return
	}
	// The p99: the latency 99% of the records stayed within. With the
	// latencies sorted from fastest to slowest (a copy, so the caller's slice
	// keeps its order), the record 99% of the way up the list is it: with
	// 5000 records, index 5000*99/100 = 4950, so 4950 records were at least as
	// fast and 49 slower. Converted through microseconds so the milliseconds
	// keep three decimals (17.36, not 17).
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	b.ReportMetric(float64(sorted[len(sorted)*99/100].Microseconds())/1000, "p99-ms")
}
