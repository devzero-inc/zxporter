package nodemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRateCalculator_FirstSampleReturnsZero(t *testing.T) {
	rc := NewRateCalculator()
	ts := time.Now()
	rate := rc.Rate("pod/default/nginx", "cpu_total", 1000.0, ts)
	assert.Equal(t, 0.0, rate, "first call should return 0")
}

func TestRateCalculator_SecondSampleComputesRate(t *testing.T) {
	rc := NewRateCalculator()
	base := time.Now()

	rc.Rate("pod/default/nginx", "cpu_total", 1000.0, base)
	rate := rc.Rate("pod/default/nginx", "cpu_total", 1300.0, base.Add(30*time.Second))

	assert.InDelta(t, 10.0, rate, 0.0001, "expected (1300-1000)/30 = 10.0 per second")
}

func TestRateCalculator_CounterResetReturnsZero(t *testing.T) {
	rc := NewRateCalculator()
	base := time.Now()

	rc.Rate("pod/default/nginx", "cpu_total", 5000.0, base)
	rate := rc.Rate("pod/default/nginx", "cpu_total", 100.0, base.Add(10*time.Second))

	assert.Equal(t, 0.0, rate, "counter reset (current < previous) should return 0")
}

func TestRateCalculator_IndependentKeys(t *testing.T) {
	rc := NewRateCalculator()
	base := time.Now()

	// Seed two independent entities
	rc.Rate("pod/default/alpha", "net_bytes", 0.0, base)
	rc.Rate("pod/default/beta", "net_bytes", 0.0, base)

	rateAlpha := rc.Rate("pod/default/alpha", "net_bytes", 600.0, base.Add(60*time.Second))
	rateBeta := rc.Rate("pod/default/beta", "net_bytes", 300.0, base.Add(60*time.Second))

	assert.InDelta(t, 10.0, rateAlpha, 0.0001, "alpha: 600/60 = 10 per second")
	assert.InDelta(t, 5.0, rateBeta, 0.0001, "beta: 300/60 = 5 per second")
}

func TestRateCalculator_ZeroElapsedTimeReturnsZero(t *testing.T) {
	rc := NewRateCalculator()
	ts := time.Now()

	rc.Rate("node/worker-1", "disk_reads", 500.0, ts)
	rate := rc.Rate("node/worker-1", "disk_reads", 600.0, ts) // same timestamp

	assert.Equal(t, 0.0, rate, "zero elapsed time should return 0")
}

func TestRateCalculator_EvictStaleEntries(t *testing.T) {
	rc := NewRateCalculator()
	now := time.Now()

	// Two entries seeded at different times
	rc.Rate("entity/stale", "metric_a", 100.0, now.Add(-10*time.Minute))
	rc.Rate("entity/fresh", "metric_b", 200.0, now.Add(-1*time.Minute))

	// Evict entries older than 5 minutes
	rc.EvictOlderThan(5 * time.Minute)

	// Stale entry should be gone — first call after eviction returns 0
	rateStale := rc.Rate("entity/stale", "metric_a", 999.0, now)
	assert.Equal(t, 0.0, rateStale, "evicted entry should behave like first call")

	// Fresh entry should still be tracked — returns a non-zero rate
	rateFresh := rc.Rate("entity/fresh", "metric_b", 260.0, now)
	assert.InDelta(t, 1.0, rateFresh, 0.0001, "fresh entry: 60 delta / 60s = 1.0 per second")
}
