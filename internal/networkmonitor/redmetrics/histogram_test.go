package redmetrics

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHistogramLadderMatchesTheStoredColumns is the test that had to exist. The
// ladder is the one part of this feature that fails SILENTLY: every other mistake
// either fails to compile, fails a length check at the server, or produces a number
// somebody notices. A bound quietly changed from 20ms to 25ms keeps the ladder the
// right length, keeps it ascending, keeps every inclusivity property -- and from
// then on the column named dur_le_20ms holds a count of requests at or below 25ms,
// with nothing in the row saying so and no way to tell the two eras apart later.
//
// So the expectation is not a second copy of the numbers, which would be changed in
// the same edit as the first. It is the ClickHouse column NAMES, in order, parsed
// back into microseconds. Those names are what the stored data means; the ladder has
// to agree with them or the table is mislabelled. They cannot be imported -- the
// migration lives in the dakr module and this is the zxporter module -- so they are
// transcribed, and transcribing a NAME is a different act from restating a number.
//
// Source of truth: dakr/clickhousemigrations/m20260825_container_red_metrics.sql.
func TestHistogramLadderMatchesTheStoredColumns(t *testing.T) {
	// The column list from the migration, in the order the columns are declared,
	// which is the order the emitted ladder is read in.
	columns := []string{
		"dur_le_500us",
		"dur_le_1ms",
		"dur_le_2ms",
		"dur_le_5ms",
		"dur_le_10ms",
		"dur_le_20ms",
		"dur_le_50ms",
		"dur_le_100ms",
		"dur_le_200ms",
		"dur_le_500ms",
		"dur_le_1s",
		"dur_le_2s",
		"dur_le_5s",
		"dur_le_inf",
	}

	if len(columns) != DurationBucketCount {
		t.Fatalf("transcribed %d columns, want %d", len(columns), DurationBucketCount)
	}
	// The last column is the overflow bucket and has no bound; every other column
	// names one.
	if got := columns[len(columns)-1]; got != "dur_le_inf" {
		t.Fatalf("last column = %q, want the overflow bucket", got)
	}
	if len(columns)-1 != len(durationBoundsUs) {
		t.Fatalf("%d bounded columns, %d bounds", len(columns)-1, len(durationBoundsUs))
	}

	for i, column := range columns[:len(columns)-1] {
		want := boundFromColumnName(t, column)
		if durationBoundsUs[i] != want {
			t.Fatalf("bound %d is %dus, but column %q says %dus",
				i, durationBoundsUs[i], column, want)
		}
	}
}

// boundFromColumnName reads a bound back out of the column that stores it, so the
// test's expectation is the stored name rather than a restatement of the constant
// it is checking.
func boundFromColumnName(t *testing.T, column string) int64 {
	t.Helper()

	suffix, ok := strings.CutPrefix(column, "dur_le_")
	if !ok {
		t.Fatalf("column %q is not a latency bucket", column)
	}

	for unit, scale := range map[string]time.Duration{
		"us": time.Microsecond,
		"ms": time.Millisecond,
		"s":  time.Second,
	} {
		digits, ok := strings.CutSuffix(suffix, unit)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			continue
		}
		return (time.Duration(n) * scale).Microseconds()
	}

	t.Fatalf("column %q has no readable duration in its name", column)
	return 0
}

// TestHistogramLadderIsTheContractLength pins the one property the server enforces.
// The handler rejects an item whose ladder is not exactly this long, so a bound added
// without changing the contract would fail every request at runtime.
func TestHistogramLadderIsTheContractLength(t *testing.T) {
	if got := len(durationBoundsUs) + 1; got != DurationBucketCount {
		t.Fatalf("ladder = %d bounds plus overflow = %d, want %d", len(durationBoundsUs), got, DurationBucketCount)
	}
	var h histogram
	if got := len(h.cumulative()); got != DurationBucketCount {
		t.Fatalf("cumulative length = %d, want %d", got, DurationBucketCount)
	}
}

// TestHistogramLadderAscends pins that the bounds are ordered. bucketFor returns the
// first bound a duration fits under, so an out-of-order ladder would file durations
// in a bucket that means something else without any test of a single duration
// noticing.
func TestHistogramLadderAscends(t *testing.T) {
	for i := 1; i < len(durationBoundsUs); i++ {
		if durationBoundsUs[i] <= durationBoundsUs[i-1] {
			t.Fatalf("bound %d (%d) is not above bound %d (%d)",
				i, durationBoundsUs[i], i-1, durationBoundsUs[i-1])
		}
	}
}

// TestHistogramBoundsAreInclusive pins which side of a bound a duration falls on. A
// request that took exactly 1ms belongs in the bucket that says "at or below 1ms",
// which is what the emitted counter is read as.
func TestHistogramBoundsAreInclusive(t *testing.T) {
	for i, bound := range durationBoundsUs {
		if got := bucketFor(bound); got != i {
			t.Fatalf("a duration of exactly %dus went to bucket %d, want %d", bound, got, i)
		}
		if got := bucketFor(bound + 1); got != i+1 {
			t.Fatalf("a duration of %dus went to bucket %d, want %d", bound+1, got, i+1)
		}
	}
}

// TestHistogramOverflowCatchesEverythingSlower pins the top of the ladder. A request
// slower than the last bound still has to land somewhere, or the total the last
// counter carries would disagree with request_count for slow services specifically.
func TestHistogramOverflowCatchesEverythingSlower(t *testing.T) {
	last := durationBoundsUs[len(durationBoundsUs)-1]
	for _, us := range []int64{last + 1, 10 * last, 1 << 40} {
		if got := bucketFor(us); got != DurationBucketCount-1 {
			t.Fatalf("a duration of %dus went to bucket %d, want the overflow bucket %d",
				us, got, DurationBucketCount-1)
		}
	}
}

// TestHistogramCountsAZeroDuration pins the bottom. A duration of zero is what a
// clock adjustment between two syscalls produces, and it is a measurement of
// something -- it belongs in the fastest bucket, not below the ladder.
func TestHistogramCountsAZeroDuration(t *testing.T) {
	if got := bucketFor(0); got != 0 {
		t.Fatalf("a zero duration went to bucket %d, want 0", got)
	}
}

// TestHistogramCumulativeIsMonotonicAndTotalled pins the shape the wire expects.
// Each counter is the number of requests at or below its bound, so the counters can
// only ascend, and the last one is the total because every request is at or below
// infinity. A consumer computing a quantile reads the ladder as a distribution
// function; a dip in it is not a slow bucket, it is a nonsensical answer.
func TestHistogramCumulativeIsMonotonicAndTotalled(t *testing.T) {
	var h histogram
	durations := []int64{0, 400, 500, 501, 1_500, 9_999, 250_000, 4_000_000, 30_000_000}
	for _, us := range durations {
		h.observe(us)
	}

	got := h.cumulative()
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("counter %d (%d) is below counter %d (%d): %v", i, got[i], i-1, got[i-1], got)
		}
	}
	if want := uint64(len(durations)); got[len(got)-1] != want {
		t.Fatalf("last counter = %d, want %d (every request is at or below infinity)", got[len(got)-1], want)
	}
}

// TestHistogramCumulativeCountsEachDurationOnce walks a known set through to the
// emitted counters. It is the one test that would catch an off-by-one between
// observe's exclusive counting and cumulative's running total.
func TestHistogramCumulativeCountsEachDurationOnce(t *testing.T) {
	var h histogram
	// 500us at bound 0, 2ms at bound 2, 2ms again, 5s at the last bound, and one
	// slower than the ladder.
	for _, us := range []int64{500, 2_000, 2_000, 5_000_000, 6_000_000} {
		h.observe(us)
	}

	want := []uint64{1, 1, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 4, 5}
	got := h.cumulative()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("counter %d = %d, want %d (full ladder %v)", i, got[i], want[i], got)
		}
	}
}

// TestHistogramIsEmptyUntilSomethingIsObserved pins that a bucket with no completed
// requests emits a full ladder of zeroes rather than a short one. The length is what
// says the two sides agree about what the buckets mean, so it cannot depend on
// whether anything happened.
func TestHistogramIsEmptyUntilSomethingIsObserved(t *testing.T) {
	var h histogram
	got := h.cumulative()
	if len(got) != DurationBucketCount {
		t.Fatalf("ladder length = %d, want %d", len(got), DurationBucketCount)
	}
	for i, v := range got {
		if v != 0 {
			t.Fatalf("counter %d = %d, want 0", i, v)
		}
	}
}
