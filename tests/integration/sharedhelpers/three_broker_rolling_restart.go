package sharedhelpers

import (
	"testing"
	"time"
)

// RollingRestart restarts the brokers one at a time, as a rolling update does:
// each is stopped gracefully, left down for downFor, started, and the next is
// stopped only once every partition of topic is fully in sync again.
func (c *ThreeBrokerCluster) RollingRestart(tb testing.TB, topic string, downFor time.Duration) {
	tb.Helper()

	for index := range threeBrokerCount {
		c.StopBroker(tb, index)
		time.Sleep(downFor)
		c.StartBroker(tb, index)
		c.WaitForFullISR(tb, topic, brokerReadyTimeout)
	}
}
