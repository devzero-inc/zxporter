package redmetrics

import "time"

// Clock is this package's only source of wall clock time. Minute buckets are
// stamped by the agent, so tests need to move time deliberately rather than wait
// for it.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the real wall clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }
