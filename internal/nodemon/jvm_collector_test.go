package nodemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
