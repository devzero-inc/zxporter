package nodemon

import (
	"sync"
	"time"
)

type counterSample struct {
	value     float64
	timestamp time.Time
}

// RateCalculator converts monotonically-increasing counter observations into
// per-second rates. It is safe for concurrent use.
type RateCalculator struct {
	mu       sync.Mutex
	previous map[string]counterSample // key: "entity\x00metric"
}

// NewRateCalculator returns an initialised RateCalculator.
func NewRateCalculator() *RateCalculator {
	return &RateCalculator{
		previous: make(map[string]counterSample),
	}
}

// Rate records a counter value and returns the per-second rate since the last
// observation for the same (entity, metric) pair.
//
// Returns 0 on:
//   - first call for a key (no baseline yet)
//   - counter reset (current value < previous value)
//   - zero elapsed time between observations
func (rc *RateCalculator) Rate(entity, metric string, value float64, ts time.Time) float64 {
	key := entity + "\x00" + metric

	rc.mu.Lock()
	defer rc.mu.Unlock()

	prev, exists := rc.previous[key]
	rc.previous[key] = counterSample{value: value, timestamp: ts}

	if !exists {
		return 0
	}

	elapsed := ts.Sub(prev.timestamp).Seconds()
	if elapsed <= 0 {
		return 0
	}

	delta := value - prev.value
	if delta < 0 {
		// Counter reset — new baseline is already stored above.
		return 0
	}

	return delta / elapsed
}

// EvictOlderThan removes entries whose last observation timestamp is older than
// maxAge relative to the current wall-clock time. This prevents unbounded
// memory growth for short-lived workloads.
func (rc *RateCalculator) EvictOlderThan(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)

	rc.mu.Lock()
	defer rc.mu.Unlock()

	for key, sample := range rc.previous {
		if sample.timestamp.Before(cutoff) {
			delete(rc.previous, key)
		}
	}
}
