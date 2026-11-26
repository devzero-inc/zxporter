package collector

import "testing"

// A container's JVM with heap metrics wins over a flags-only one (no readable
// hsperfdata) in either order; otherwise the first JVM is kept.
func TestIndexJVMMetricsByContainer_PrefersHeap(t *testing.T) {
	withHeap := NodemonJVMMetrics{Pod: "p", Container: "c", Namespace: "n", PidHost: 1, HeapMaxSizeBytes: 256 << 20}
	flagsOnly := NodemonJVMMetrics{Pod: "p", Container: "c", Namespace: "n", PidHost: 2}
	otherFlagsOnly := NodemonJVMMetrics{Pod: "p", Container: "c", Namespace: "n", PidHost: 3}
	otherHeap := NodemonJVMMetrics{Pod: "p", Container: "c", Namespace: "n", PidHost: 4, HeapMaxSizeBytes: 128 << 20}

	for _, tc := range []struct {
		name    string
		metrics []NodemonJVMMetrics
		wantPid int
	}{
		{"heap first", []NodemonJVMMetrics{withHeap, flagsOnly}, 1},
		{"flags-only first", []NodemonJVMMetrics{flagsOnly, withHeap}, 1},
		{"two flags-only keep the first", []NodemonJVMMetrics{flagsOnly, otherFlagsOnly}, 2},
		{"two with heap keep the first", []NodemonJVMMetrics{withHeap, otherHeap}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index := IndexJVMMetricsByContainer(tc.metrics)
			got := index[gpuContainerKey{Pod: "p", Container: "c", Namespace: "n"}]
			if got.PidHost != tc.wantPid {
				t.Errorf("kept pid %d, want %d", got.PidHost, tc.wantPid)
			}
		})
	}
}
