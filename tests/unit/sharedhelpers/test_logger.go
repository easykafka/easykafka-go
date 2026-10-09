// Package sharedhelpers holds the helpers both the subscriber's and the
// publisher's unit tests use: the fake producer, delivery-error recording, log
// capture and fixtures. A helper only one side's tests use lives in that side's
// helpers package instead.
package sharedhelpers

import "github.com/rs/zerolog"

// TestLogger returns a logger that discards everything.
func TestLogger() zerolog.Logger {
	return zerolog.Nop()
}
