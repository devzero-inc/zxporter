package nodemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeapMaxBytes(t *testing.T) {
	const mib = int64(1) << 20

	tests := []struct {
		name     string
		counters map[string]any
		want     int64
	}{
		{
			name:     "aggregate counter is authoritative",
			counters: map[string]any{"sun.gc.heap.maxCapacity": 512 * mib},
			want:     512 * mib,
		},
		{
			name: "serial GC sums physically-partitioned generations",
			counters: map[string]any{
				"sun.gc.collector.0.name":         "copy",
				"sun.gc.collector.1.name":         "MarkSweepCompact",
				"sun.gc.generation.0.maxCapacity": 128 * mib, // young
				"sun.gc.generation.1.maxCapacity": 384 * mib, // old
			},
			want: 512 * mib,
		},
		{
			name: "G1 equal generations are not double-counted",
			counters: map[string]any{
				"sun.gc.collector.0.name": "G1 incremental collections",
				"sun.gc.collector.1.name": "G1 stop-the-world full collections",
				// G1 reports ~the whole heap as each generation's max.
				"sun.gc.generation.0.maxCapacity": 538 * mib,
				"sun.gc.generation.1.maxCapacity": 538 * mib,
			},
			want: 538 * mib, // NOT 1076 MiB (the original bug)
		},
		{
			name: "G1 unequal generations take the larger (old == whole heap)",
			counters: map[string]any{
				"sun.gc.collector.0.name":         "G1 incremental collections",
				"sun.gc.generation.0.maxCapacity": 300 * mib, // young capped below heap
				"sun.gc.generation.1.maxCapacity": 538 * mib, // old == whole heap
			},
			want: 538 * mib,
		},
		{
			name: "Shenandoah is region-based",
			counters: map[string]any{
				"sun.gc.collector.0.name":         "Shenandoah Pauses",
				"sun.gc.generation.0.maxCapacity": 256 * mib,
				"sun.gc.generation.1.maxCapacity": 256 * mib,
			},
			want: 256 * mib,
		},
		{
			name:     "no counters present yields zero",
			counters: map[string]any{},
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, heapMaxBytes(tt.counters))
		})
	}
}

func TestBuildJVMMetric_G1HeapMaxNotDoubled(t *testing.T) {
	const mib = int64(1) << 20
	counters := map[string]any{
		"sun.gc.collector.0.name":         "G1 incremental collections",
		"sun.gc.generation.0.maxCapacity": 538 * mib,
		"sun.gc.generation.1.maxCapacity": 538 * mib,
		// per-space used/capacity sum without overlap (these stay correct).
		"sun.gc.generation.0.space.0.used":     20 * mib,
		"sun.gc.generation.0.space.0.capacity": 64 * mib,
		"sun.gc.generation.1.space.0.used":     6 * mib,
		"sun.gc.generation.1.space.0.capacity": 64 * mib,
	}

	m := buildJVMMetric(counters, JavaProcess{ContainerID: "containerd://abc"}, containerInfo{}, "node-1")

	assert.Equal(t, 538*mib, m.HeapMaxSizeBytes, "G1 heap max must not be the sum of both generations")
	assert.Equal(t, 26*mib, m.HeapUsedBytes, "heap used sums per-space counters (no overlap)")
	assert.Equal(t, 128*mib, m.HeapSizeBytes, "committed capacity sums per-space counters")
}

// TestJVMCollector_QueryJVMMetrics_ServesCachedSnapshot proves
// QueryJVMMetrics reads the snapshot populated by the last Collect call
// instead of doing a live /proc walk + build on every call.
func TestJVMCollector_QueryJVMMetrics_ServesCachedSnapshot(t *testing.T) {
	c := NewJVMCollector("node-1", nil, testr.New(t))

	var calls int32
	c.discover = func(_ string) ([]JavaProcess, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	c.build = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return nil, nil
	}

	c.Collect(context.Background())
	require.EqualValues(t, 1, atomic.LoadInt32(&calls), "Collect should invoke discover exactly once")

	for range 5 {
		_, err := c.QueryJVMMetrics(context.Background())
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, atomic.LoadInt32(&calls),
		"QueryJVMMetrics must serve the cached snapshot from Collect, not recompute on every call")
}

// TestJVMCollector_StartCollectionLoop_RefreshesPeriodically proves the
// background loop keeps the cache warm on a tick, not just at startup.
func TestJVMCollector_StartCollectionLoop_RefreshesPeriodically(t *testing.T) {
	c := NewJVMCollector("node-1", nil, testr.New(t))

	var calls int32
	c.discover = func(_ string) ([]JavaProcess, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	c.build = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return nil, nil
	}

	go c.StartCollectionLoop(t.Context(), 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 3
	}, time.Second, 5*time.Millisecond, "expected multiple collection cycles from the ticker")
}

// TestJVMCollector_StartCollectionLoop_BoundsEachCycle proves each collection
// cycle runs under a per-cycle deadline rather than the loop's long-lived
// (shutdown-only) context. build blocks on its ctx; if Collect were still
// called with the unbounded loop context (the bug), it would never unblock
// and the ticker could never fire a second cycle.
func TestJVMCollector_StartCollectionLoop_BoundsEachCycle(t *testing.T) {
	c := NewJVMCollector("node-1", nil, testr.New(t))

	var calls int32
	c.discover = func(_ string) ([]JavaProcess, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	c.build = func(ctx context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	go c.StartCollectionLoop(t.Context(), 20*time.Millisecond)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 2
	}, 2*time.Second, 10*time.Millisecond,
		"expected the per-cycle context to expire so a second cycle can start")
}

// TestJVMCollector_Collect_KeepsLastGoodSnapshotOnTotalFailure proves a cycle
// that produces nothing usable (build returns no metrics and an error)
// doesn't blank a previously-good cache.
func TestJVMCollector_Collect_KeepsLastGoodSnapshotOnTotalFailure(t *testing.T) {
	c := NewJVMCollector("node-1", nil, testr.New(t))

	goodJVM := []JVMMetric{{NodeName: "node-1", Pod: "app"}}
	c.discover = func(_ string) ([]JavaProcess, error) { return nil, nil }
	c.build = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return goodJVM, nil
	}
	c.Collect(context.Background())
	metrics, err := c.QueryJVMMetrics(context.Background())
	require.NoError(t, err)
	require.Equal(t, goodJVM, metrics)

	c.build = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return nil, errors.New("boom")
	}
	c.Collect(context.Background())

	metrics, err = c.QueryJVMMetrics(context.Background())
	assert.Error(t, err, "the failure should still be surfaced")
	assert.Equal(t, goodJVM, metrics, "a total failure must not blank the last good snapshot")
}

// TestJVMCollector_QueryJVMMetrics_NotYetCollected proves a fresh collector
// (no Collect run yet) reports an error rather than a silent,
// successful-looking empty result.
func TestJVMCollector_QueryJVMMetrics_NotYetCollected(t *testing.T) {
	c := NewJVMCollector("node-1", nil, testr.New(t))
	_, err := c.QueryJVMMetrics(context.Background())
	assert.ErrorIs(t, err, errMetricsNotYetCollected)
}
