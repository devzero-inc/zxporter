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
