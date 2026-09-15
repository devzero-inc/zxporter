package redmetrics

// durationBoundsUs are the latency ladder's inclusive upper bounds, in
// microseconds and ascending. A request whose duration is exactly a bound lands in
// that bound's bucket rather than the next one up.
//
// The ladder is a 1-2-5 progression from 500us to 5s, plus an overflow bucket. It
// is FIXED FOR THE LIFE OF THE FEATURE. The table stores one column per bound, so
// inserting a bound later does not add detail to history -- it reinterprets every
// row already written, because the column that used to mean "at or below 5ms" now
// means something else, and nothing in the row says which era it came from.
// Widening at the top has the same problem in reverse.
//
// The reasoning, and why a fixed ladder was chosen over a percentile sketch, is in
// docs/features/red-metrics/INVARIANTS.md.
var durationBoundsUs = [...]int64{
	500,       // 500us
	1_000,     // 1ms
	2_000,     // 2ms
	5_000,     // 5ms
	10_000,    // 10ms
	20_000,    // 20ms
	50_000,    // 50ms
	100_000,   // 100ms
	200_000,   // 200ms
	500_000,   // 500ms
	1_000_000, // 1s
	2_000_000, // 2s
	5_000_000, // 5s
}

// The wire contract and the ladder have to be the same length, and the server
// rejects the whole request when they are not. Asserted from both sides so that
// adding a bound without changing the contract fails to compile here rather than
// failing every request at runtime.
const (
	_ = uint(DurationBucketCount - (len(durationBoundsUs) + 1))
	_ = uint((len(durationBoundsUs) + 1) - DurationBucketCount)
)

// histogram counts durations per bucket EXCLUSIVELY: bucket i holds the requests
// that fell above bound i-1 and at or below bound i, and the last bucket holds
// everything slower than the last bound.
//
// The wire wants cumulative counters, which cumulative() derives at emit time.
// Counting exclusively keeps the hot path to a single increment, and makes the
// emitted counters monotonic by construction rather than by care -- a hot path that
// incremented every bucket at or above the duration would be O(buckets) per request
// and would go non-monotonic the moment one of those increments was missed.
type histogram [DurationBucketCount]uint64

// observe files one duration in microseconds.
func (h *histogram) observe(us int64) { h[bucketFor(us)]++ }

// bucketFor is the index of the bucket a duration belongs in.
func bucketFor(us int64) int {
	for i, bound := range durationBoundsUs {
		if us <= bound {
			return i
		}
	}
	return DurationBucketCount - 1
}

// cumulative turns the exclusive counts into the counters the item carries: each
// one is the number of requests that completed at or below its bound. The last is
// the total, since every request is at or below infinity.
func (h histogram) cumulative() []uint64 {
	out := make([]uint64, DurationBucketCount)
	var running uint64
	for i, count := range h {
		running += count
		out[i] = running
	}
	return out
}
