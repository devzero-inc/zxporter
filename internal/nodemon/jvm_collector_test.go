package nodemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpdateContainerMap(t *testing.T) {
	c := &JVMCollector{
		containerMap: make(map[string]containerInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-abc",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "app",
					ContainerID: "containerd://abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
				},
				{
					Name:        "sidecar",
					ContainerID: "containerd://1111111111111111111111111111111111111111111111111111111111111111",
				},
			},
		},
	}

	c.updateContainerMap(pod)

	assert.Equal(t, containerInfo{Pod: "my-app-abc", Namespace: "default", Container: "app"},
		c.containerMap["abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"])
	assert.Equal(t, containerInfo{Pod: "my-app-abc", Namespace: "default", Container: "sidecar"},
		c.containerMap["1111111111111111111111111111111111111111111111111111111111111111"])
	assert.Len(t, c.containerMap, 2)
}

func TestRemoveFromContainerMap(t *testing.T) {
	c := &JVMCollector{
		containerMap: map[string]containerInfo{
			"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789": {Pod: "my-app", Namespace: "default", Container: "app"},
			"1111111111111111111111111111111111111111111111111111111111111111": {Pod: "my-app", Namespace: "default", Container: "sidecar"},
			"2222222222222222222222222222222222222222222222222222222222222222": {Pod: "other-pod", Namespace: "kube-system", Container: "dns"},
		},
	}

	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "containerd://abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"},
				{ContainerID: "containerd://1111111111111111111111111111111111111111111111111111111111111111"},
			},
		},
	}

	c.removeFromContainerMap(pod)

	assert.Len(t, c.containerMap, 1)
	assert.Contains(t, c.containerMap, "2222222222222222222222222222222222222222222222222222222222222222")
}

func TestUpdateContainerMap_SkipsEmptyID(t *testing.T) {
	c := &JVMCollector{
		containerMap: make(map[string]containerInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", ContainerID: ""},
			},
		},
	}

	c.updateContainerMap(pod)
	assert.Empty(t, c.containerMap)
}

func TestCheckHostPIDVisibility_FewPIDs(t *testing.T) {
	// Simulate a procRoot with only a few PID dirs (no hostPID).
	tmpDir := t.TempDir()
	for i := 1; i <= 5; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, fmt.Sprintf("%d", i)), 0o755))
	}

	c := &JVMCollector{
		procRoot:     tmpDir,
		log:          testr.New(t).WithName("jvm-collector"),
		containerMap: make(map[string]containerInfo),
	}

	// Should not panic; will log a warning about low PID count.
	c.checkHostPIDVisibility()
}

func TestCheckHostPIDVisibility_ManyPIDs(t *testing.T) {
	// Simulate a procRoot with many PID dirs (hostPID enabled).
	tmpDir := t.TempDir()
	for i := 1; i <= 50; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, fmt.Sprintf("%d", i)), 0o755))
	}
	// Add some non-PID entries like real /proc has.
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "self"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "meminfo"), []byte("fake"), 0o644))

	c := &JVMCollector{
		procRoot:     tmpDir,
		log:          testr.New(t).WithName("jvm-collector"),
		containerMap: make(map[string]containerInfo),
	}

	// Should log confirmation of host PID visibility.
	c.checkHostPIDVisibility()
}

func TestCheckHostPIDVisibility_InvalidProcRoot(t *testing.T) {
	c := &JVMCollector{
		procRoot:     "/nonexistent/proc",
		log:          testr.New(t).WithName("jvm-collector"),
		containerMap: make(map[string]containerInfo),
	}

	// Should not panic; will log an error about unreadable procRoot.
	c.checkHostPIDVisibility()
}

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

func TestUpdateContainerMap_OverwritesOnUpdate(t *testing.T) {
	cid := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	c := &JVMCollector{
		containerMap: map[string]containerInfo{
			cid: {Pod: "old-pod", Namespace: "default", Container: "app"},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "new-pod", Namespace: "staging"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "web", ContainerID: "containerd://" + cid},
			},
		},
	}

	c.updateContainerMap(pod)
	assert.Equal(t, containerInfo{Pod: "new-pod", Namespace: "staging", Container: "web"}, c.containerMap[cid])
}
