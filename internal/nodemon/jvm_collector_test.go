package nodemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
			"1111111111111111111111111111111111111111111111111111111111111111":   {Pod: "my-app", Namespace: "default", Container: "sidecar"},
			"2222222222222222222222222222222222222222222222222222222222222222":   {Pod: "other-pod", Namespace: "kube-system", Container: "dns"},
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
