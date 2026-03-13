/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// Helper to build a running pod with container statuses.
func makePod(name, namespace, nodeName string, ownerRefs []metav1.OwnerReference, containers []corev1.ContainerStatus, initContainers []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			UID:             types.UID(name + "-uid"),
			OwnerReferences: ownerRefs,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodRunning,
			ContainerStatuses:     containers,
			InitContainerStatuses: initContainers,
		},
	}
}

func makeContainerStatus(name, image, imageID string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:    name,
		Image:   image,
		ImageID: imageID,
	}
}

func deploymentOwnerRef(name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: name + "-7f9d8c6b5f", UID: types.UID(name + "-rs-uid")},
	}
}

func statefulsetOwnerRef(name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{Kind: "StatefulSet", Name: name, UID: types.UID(name + "-sts-uid")},
	}
}

func daemonsetOwnerRef(name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{Kind: "DaemonSet", Name: name, UID: types.UID(name + "-ds-uid")},
	}
}

func jobOwnerRef(name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{Kind: "Job", Name: name, UID: types.UID(name + "-job-uid")},
	}
}

func cronjobJobOwnerRef(cronJobName string) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{Kind: "Job", Name: cronJobName + "-1709420400", UID: types.UID(cronJobName + "-cj-uid")},
	}
}

func defaultConfig() ImageAnalysisConfig {
	cfg := DefaultImageAnalysisConfig()
	// Clear excluded namespaces/images for most tests so they don't interfere.
	cfg.ExcludedNamespaces = nil
	cfg.ExcludedImages = nil
	return cfg
}

func TestDiscover_BasicSingleNode(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			deploymentOwnerRef("nginx-deploy"),
			[]corev1.ContainerStatus{
				makeContainerStatus("nginx", "docker.io/library/nginx:1.25.3", "docker.io/library/nginx@sha256:aaa111"),
			},
			nil,
		),
		*makePod("pod-2", "default", "node-1",
			deploymentOwnerRef("redis-deploy"),
			[]corev1.ContainerStatus{
				makeContainerStatus("redis", "docker.io/library/redis:7.2", "docker.io/library/redis@sha256:bbb222"),
			},
			nil,
		),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, result.TotalPodsScanned)
	assert.Equal(t, 2, result.TotalImagesFound)
	assert.Equal(t, 2, result.TotalContainersSeen)
	assert.Len(t, result.NodeImages, 1)

	batch := result.NodeImages["node-1"]
	require.NotNil(t, batch)
	assert.Len(t, batch.Images, 2)

	// Verify workload refs.
	nginxRefs := result.WorkloadRefs["docker.io/library/nginx@sha256:aaa111"]
	require.Len(t, nginxRefs, 1)
	assert.Equal(t, "Deployment", nginxRefs[0].WorkloadType)
	assert.Equal(t, "nginx-deploy", nginxRefs[0].WorkloadName)
	assert.Contains(t, nginxRefs[0].ContainerNames, "nginx")
}

func TestDiscover_GlobalDedup_MultiNode(t *testing.T) {
	// Same image on two different nodes — should only be assigned to the first node.
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			deploymentOwnerRef("nginx"),
			[]corev1.ContainerStatus{
				makeContainerStatus("nginx", "docker.io/library/nginx:1.25.3", "docker.io/library/nginx@sha256:aaa111"),
			},
			nil,
		),
		*makePod("pod-2", "default", "node-2",
			deploymentOwnerRef("nginx"),
			[]corev1.ContainerStatus{
				makeContainerStatus("nginx", "docker.io/library/nginx:1.25.3", "docker.io/library/nginx@sha256:aaa111"),
			},
			nil,
		),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Image deduped — only 1 unique image.
	assert.Equal(t, 1, result.TotalImagesFound)
	// But container count is 2.
	assert.Equal(t, 2, result.TotalContainersSeen)

	// Image should be assigned to exactly one node.
	totalImages := 0
	for _, batch := range result.NodeImages {
		totalImages += len(batch.Images)
	}
	assert.Equal(t, 1, totalImages)

	// Workload refs should have entries from BOTH nodes.
	refs := result.WorkloadRefs["docker.io/library/nginx@sha256:aaa111"]
	require.Len(t, refs, 1) // Same workload on both nodes, should be 1 ref with the container name.
}

func TestDiscover_WorkloadRefsGlobal(t *testing.T) {
	// Same image used by different workloads on different nodes.
	pods := []corev1.Pod{
		*makePod("pod-1", "ns-a", "node-1",
			deploymentOwnerRef("web"),
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "nginx:1.25", "nginx@sha256:aaa111"),
			},
			nil,
		),
		*makePod("pod-2", "ns-b", "node-2",
			statefulsetOwnerRef("cache"),
			[]corev1.ContainerStatus{
				makeContainerStatus("sidecar", "nginx:1.25", "nginx@sha256:aaa111"),
			},
			nil,
		),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["nginx@sha256:aaa111"]
	require.Len(t, refs, 2, "two different workloads should be tracked")

	types := map[string]bool{}
	for _, ref := range refs {
		types[ref.WorkloadType] = true
	}
	assert.True(t, types["Deployment"])
	assert.True(t, types["StatefulSet"])
}

func TestDiscover_InitContainers(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			deploymentOwnerRef("app"),
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "myapp:v1", "myapp@sha256:app111"),
			},
			[]corev1.ContainerStatus{
				makeContainerStatus("init-db", "flyway:9", "flyway@sha256:init222"),
			},
		),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, result.TotalImagesFound, "both regular and init container images should be found")
	assert.Equal(t, 2, result.TotalContainersSeen)

	batch := result.NodeImages["node-1"]
	require.NotNil(t, batch)
	assert.Len(t, batch.Images, 2)
}

func TestDiscover_SkipUnscheduledPods(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-pending", "default", "", // empty NodeName = unscheduled
			nil,
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "myapp:v1", "myapp@sha256:aaa111"),
			},
			nil,
		),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 0, result.TotalPodsScanned)
	assert.Equal(t, 0, result.TotalImagesFound)
}

func TestDiscover_SkipEmptyImageID(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			nil,
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "myapp:v1", ""), // empty ImageID
			},
			nil,
		),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalPodsScanned)
	assert.Equal(t, 0, result.TotalImagesFound)
}

func TestDiscover_NamespaceFiltering_TargetNamespaces(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "production", "node-1", nil,
			[]corev1.ContainerStatus{makeContainerStatus("app", "app:v1", "app@sha256:aaa")}, nil),
		*makePod("pod-2", "staging", "node-1", nil,
			[]corev1.ContainerStatus{makeContainerStatus("app", "app:v2", "app@sha256:bbb")}, nil),
		*makePod("pod-3", "kube-system", "node-1", nil,
			[]corev1.ContainerStatus{makeContainerStatus("kube", "kube:v1", "kube@sha256:ccc")}, nil),
	}

	cfg := defaultConfig()
	cfg.TargetNamespaces = []string{"production"}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, cfg, logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalPodsScanned)
	assert.Equal(t, 1, result.TotalImagesFound)
}

func TestDiscover_NamespaceFiltering_ExcludedNamespaces(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1", nil,
			[]corev1.ContainerStatus{makeContainerStatus("app", "app:v1", "app@sha256:aaa")}, nil),
		*makePod("pod-2", "kube-system", "node-1", nil,
			[]corev1.ContainerStatus{makeContainerStatus("kube", "kube:v1", "kube@sha256:bbb")}, nil),
		*makePod("pod-3", "kube-public", "node-1", nil,
			[]corev1.ContainerStatus{makeContainerStatus("pub", "pub:v1", "pub@sha256:ccc")}, nil),
	}

	cfg := defaultConfig()
	cfg.ExcludedNamespaces = []string{"kube-system", "kube-public"}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, cfg, logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalPodsScanned)
	assert.Equal(t, 1, result.TotalImagesFound)
}

func TestDiscover_ImageFiltering_Excluded(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1", nil,
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "myapp:v1", "myapp@sha256:aaa"),
				makeContainerStatus("pause", "registry.k8s.io/pause:3.9", "registry.k8s.io/pause@sha256:bbb"),
			}, nil),
	}

	cfg := defaultConfig()
	cfg.ExcludedImages = []string{"registry.k8s.io/pause:*"}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, cfg, logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalImagesFound)
	batch := result.NodeImages["node-1"]
	require.NotNil(t, batch)
	assert.Equal(t, "myapp@sha256:aaa", batch.Images[0].Digest)
}

func TestDiscover_ImageFiltering_Included(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1", nil,
			[]corev1.ContainerStatus{
				makeContainerStatus("nginx", "nginx:1.25", "nginx@sha256:aaa"),
				makeContainerStatus("redis", "redis:7.2", "redis@sha256:bbb"),
				makeContainerStatus("app", "myapp:v1", "myapp@sha256:ccc"),
			}, nil),
	}

	cfg := defaultConfig()
	cfg.IncludedImages = []string{"nginx:*"}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, cfg, logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalImagesFound)
}

func TestDiscover_OwnerResolution_Deployment(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			deploymentOwnerRef("web-server"),
			[]corev1.ContainerStatus{makeContainerStatus("web", "nginx:1.25", "nginx@sha256:aaa")}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["nginx@sha256:aaa"]
	require.Len(t, refs, 1)
	assert.Equal(t, "Deployment", refs[0].WorkloadType)
	assert.Equal(t, "web-server", refs[0].WorkloadName)
}

func TestDiscover_OwnerResolution_StatefulSet(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			statefulsetOwnerRef("postgres"),
			[]corev1.ContainerStatus{makeContainerStatus("db", "postgres:15", "postgres@sha256:aaa")}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["postgres@sha256:aaa"]
	require.Len(t, refs, 1)
	assert.Equal(t, "StatefulSet", refs[0].WorkloadType)
	assert.Equal(t, "postgres", refs[0].WorkloadName)
}

func TestDiscover_OwnerResolution_DaemonSet(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			daemonsetOwnerRef("fluentd"),
			[]corev1.ContainerStatus{makeContainerStatus("log", "fluentd:1.16", "fluentd@sha256:aaa")}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["fluentd@sha256:aaa"]
	require.Len(t, refs, 1)
	assert.Equal(t, "DaemonSet", refs[0].WorkloadType)
	assert.Equal(t, "fluentd", refs[0].WorkloadName)
}

func TestDiscover_OwnerResolution_Job(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			jobOwnerRef("migration-v3"),
			[]corev1.ContainerStatus{makeContainerStatus("migrate", "flyway:9", "flyway@sha256:aaa")}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["flyway@sha256:aaa"]
	require.Len(t, refs, 1)
	assert.Equal(t, "Job", refs[0].WorkloadType)
	assert.Equal(t, "migration-v3", refs[0].WorkloadName)
}

func TestDiscover_OwnerResolution_CronJob(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			cronjobJobOwnerRef("nightly-backup"),
			[]corev1.ContainerStatus{makeContainerStatus("backup", "backup:v1", "backup@sha256:aaa")}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["backup@sha256:aaa"]
	require.Len(t, refs, 1)
	assert.Equal(t, "CronJob", refs[0].WorkloadType)
	assert.Equal(t, "nightly-backup", refs[0].WorkloadName)
}

func TestDiscover_OwnerResolution_NakedPod(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("standalone-pod", "default", "node-1",
			nil, // no owner references
			[]corev1.ContainerStatus{makeContainerStatus("app", "app:v1", "app@sha256:aaa")}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["app@sha256:aaa"]
	require.Len(t, refs, 1)
	assert.Equal(t, "Pod", refs[0].WorkloadType)
	assert.Equal(t, "standalone-pod", refs[0].WorkloadName)
}

func TestDiscover_ContainerCount(t *testing.T) {
	// Same image used by 3 containers across 2 pods on 2 nodes.
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			deploymentOwnerRef("app"),
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "myapp:v1", "myapp@sha256:aaa"),
				makeContainerStatus("sidecar", "myapp:v1", "myapp@sha256:aaa"),
			}, nil),
		*makePod("pod-2", "default", "node-2",
			deploymentOwnerRef("app"),
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "myapp:v1", "myapp@sha256:aaa"),
			}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, result.TotalImagesFound)
	assert.Equal(t, 3, result.TotalContainersSeen)

	// Find the image in whichever node it was assigned to.
	var img *ImageInfo
	for _, batch := range result.NodeImages {
		for i := range batch.Images {
			if batch.Images[i].Digest == "myapp@sha256:aaa" {
				img = &batch.Images[i]
			}
		}
	}
	require.NotNil(t, img)
	assert.Equal(t, 3, img.ContainerCount)
}

func TestDiscover_MultipleContainersPerPod_DifferentImages(t *testing.T) {
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			deploymentOwnerRef("app"),
			[]corev1.ContainerStatus{
				makeContainerStatus("app", "myapp:v1", "myapp@sha256:aaa"),
				makeContainerStatus("sidecar", "envoy:1.28", "envoy@sha256:bbb"),
			}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, result.TotalImagesFound)

	// Both images should have workload refs pointing to the same deployment.
	appRefs := result.WorkloadRefs["myapp@sha256:aaa"]
	require.Len(t, appRefs, 1)
	assert.Equal(t, "app", appRefs[0].WorkloadName)
	assert.Contains(t, appRefs[0].ContainerNames, "app")

	envoyRefs := result.WorkloadRefs["envoy@sha256:bbb"]
	require.Len(t, envoyRefs, 1)
	assert.Equal(t, "app", envoyRefs[0].WorkloadName)
	assert.Contains(t, envoyRefs[0].ContainerNames, "sidecar")
}

func TestDiscover_WorkloadRefDedup_SameWorkloadMultiplePods(t *testing.T) {
	// Two pods from the same deployment, same image — workload ref should appear once
	// but with all container names collected.
	pods := []corev1.Pod{
		*makePod("pod-1", "default", "node-1",
			deploymentOwnerRef("web"),
			[]corev1.ContainerStatus{
				makeContainerStatus("nginx", "nginx:1.25", "nginx@sha256:aaa"),
			}, nil),
		*makePod("pod-2", "default", "node-1",
			deploymentOwnerRef("web"),
			[]corev1.ContainerStatus{
				makeContainerStatus("nginx", "nginx:1.25", "nginx@sha256:aaa"),
			}, nil),
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	refs := result.WorkloadRefs["nginx@sha256:aaa"]
	require.Len(t, refs, 1, "same workload should only appear once")
	assert.Equal(t, "web", refs[0].WorkloadName)
	assert.Contains(t, refs[0].ContainerNames, "nginx")
}

func TestDiscover_EmptyCluster(t *testing.T) {
	client := fake.NewSimpleClientset()
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 0, result.TotalPodsScanned)
	assert.Equal(t, 0, result.TotalImagesFound)
	assert.Empty(t, result.NodeImages)
}

func TestNormalizeDigest(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"sha256:abc123", "sha256:abc123"},
		{"docker.io/library/nginx@sha256:abc123", "docker.io/library/nginx@sha256:abc123"},
		{"docker-pullable://docker.io/library/nginx@sha256:abc123", "docker.io/library/nginx@sha256:abc123"},
		{"some-other-format", "some-other-format"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, normalizeDigest(tt.input))
		})
	}
}

func TestResolveWorkloadOwner(t *testing.T) {
	tests := []struct {
		name         string
		ownerRefs    []metav1.OwnerReference
		expectedName string
		expectedKind string
	}{
		{
			name:         "no owner",
			ownerRefs:    nil,
			expectedName: "my-pod",
			expectedKind: "Pod",
		},
		{
			name: "deployment via replicaset",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "web-server-7f9d8c6b5f", UID: "rs-uid"},
			},
			expectedName: "web-server",
			expectedKind: "Deployment",
		},
		{
			name: "standalone replicaset (no dash)",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "standalone", UID: "rs-uid"},
			},
			expectedName: "standalone",
			expectedKind: "ReplicaSet",
		},
		{
			name: "statefulset",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "postgres", UID: "sts-uid"},
			},
			expectedName: "postgres",
			expectedKind: "StatefulSet",
		},
		{
			name: "daemonset",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "fluentd", UID: "ds-uid"},
			},
			expectedName: "fluentd",
			expectedKind: "DaemonSet",
		},
		{
			name: "plain job",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "Job", Name: "migration-v3", UID: "job-uid"},
			},
			expectedName: "migration-v3",
			expectedKind: "Job",
		},
		{
			name: "cronjob job",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "Job", Name: "nightly-backup-1709420400", UID: "cj-uid"},
			},
			expectedName: "nightly-backup",
			expectedKind: "CronJob",
		},
		{
			name: "custom CRD",
			ownerRefs: []metav1.OwnerReference{
				{Kind: "ArgoRollout", Name: "canary-deploy", UID: "argo-uid"},
			},
			expectedName: "canary-deploy",
			expectedKind: "ArgoRollout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "my-pod",
					UID:             "pod-uid",
					OwnerReferences: tt.ownerRefs,
				},
			}
			name, kind, _ := resolveWorkloadOwner(pod)
			assert.Equal(t, tt.expectedName, name)
			assert.Equal(t, tt.expectedKind, kind)
		})
	}
}

func TestIsNumeric(t *testing.T) {
	assert.True(t, isNumeric("12345678"))
	assert.True(t, isNumeric("1709420400"))
	assert.False(t, isNumeric(""))
	assert.False(t, isNumeric("abc"))
	assert.False(t, isNumeric("123abc"))
	assert.False(t, isNumeric("7f9d8c6b5f"))
}

func TestShortImageRef(t *testing.T) {
	assert.Equal(t, "nginx:1.25.3", shortImageRef("docker.io/library/nginx:1.25.3"))
	assert.Equal(t, "pause:3.9", shortImageRef("registry.k8s.io/pause:3.9"))
	assert.Equal(t, "myapp:v1", shortImageRef("myapp:v1"))
	assert.Equal(t, "image", shortImageRef("gcr.io/project/image"))
}

func TestDiscover_LargeScale(t *testing.T) {
	// Simulate 100 pods across 10 nodes, each with 3 containers (some sharing images).
	var pods []corev1.Pod
	for i := 0; i < 100; i++ {
		nodeName := fmt.Sprintf("node-%d", i%10)
		namespace := fmt.Sprintf("ns-%d", i%5)
		// Each pod has 3 containers. Images repeat across pods (10 unique images total).
		imgIdx := i % 10
		pods = append(pods, *makePod(
			fmt.Sprintf("pod-%d", i), namespace, nodeName,
			deploymentOwnerRef(fmt.Sprintf("deploy-%d", imgIdx)),
			[]corev1.ContainerStatus{
				makeContainerStatus("main",
					fmt.Sprintf("app-%d:v1", imgIdx),
					fmt.Sprintf("app-%d@sha256:digest%d", imgIdx, imgIdx)),
				makeContainerStatus("sidecar",
					fmt.Sprintf("sidecar-%d:v1", imgIdx),
					fmt.Sprintf("sidecar-%d@sha256:sdigest%d", imgIdx, imgIdx)),
				makeContainerStatus("monitor", "monitor:v1", "monitor@sha256:mon000"),
			}, nil))
	}

	client := fake.NewSimpleClientset(&corev1.PodList{Items: pods})
	discoverer := NewImageDiscoverer(client, defaultConfig(), logr.Discard())

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// 10 unique app images + 10 unique sidecar images + 1 monitor image = 21 unique images.
	assert.Equal(t, 21, result.TotalImagesFound)
	assert.Equal(t, 100, result.TotalPodsScanned)
	assert.Equal(t, 300, result.TotalContainersSeen) // 100 pods * 3 containers

	// Monitor image appears in 100 containers.
	var monitorImg *ImageInfo
	for _, batch := range result.NodeImages {
		for i := range batch.Images {
			if batch.Images[i].Digest == "monitor@sha256:mon000" {
				monitorImg = &batch.Images[i]
			}
		}
	}
	require.NotNil(t, monitorImg)
	assert.Equal(t, 100, monitorImg.ContainerCount)
}
