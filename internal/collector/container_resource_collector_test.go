// internal/collector/container_resource_collector_test.go
package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/devzero-inc/zxporter/internal/nodemon"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

// testPod builds a minimal pod, scheduled onto nodeName, with a single
// container named containerName — enough to survive processContainerMetrics'
// containerSpec lookup.
func testPod(namespace, name, nodeName, containerName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: containerName}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// newSyncedPodInformer builds a real SharedIndexInformer for Pods, backed by a
// fake clientset seeded with the given pods, and waits for the initial cache
// sync so the indexer is populated before the test runs.
func newSyncedPodInformer(t *testing.T, pods ...*corev1.Pod) (cache.SharedIndexInformer, chan struct{}) {
	t.Helper()
	objs := make([]runtime.Object, len(pods))
	for i, p := range pods {
		objs[i] = p
	}
	client := k8sfake.NewSimpleClientset(objs...)
	factory := newInformerFactory(client, nil)
	informer := factory.Core().V1().Pods().Informer()
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	require.True(t, cache.WaitForCacheSync(stopCh, informer.HasSynced), "pod informer failed to sync")
	return informer, stopCh
}

// TestCollectAllContainerResources_SurvivesPodDeletedDuringNodemonFetch
// reproduces the TOCTOU race in collectAllContainerResources: the pod
// informer cache is snapshotted once at the top of the cycle, then several
// slow, real network round trips to nodemon run to build the pod/container
// metrics list. Before the fix, the container loop looked the pod back up in
// the live informer cache (via getPodFromCache) *after* those calls, which
// raced against concurrent pod deletions (e.g. spot instance termination)
// and silently dropped that pod's containers for the cycle — invisibly,
// since the failure path only logged locally and never reported to
// telemetry. This test deletes the pod from the live informer indexer as a
// side effect of the nodemon HTTP call and asserts the container is still
// emitted and no cache-miss telemetry is reported, because the collector now
// reuses the pod object it already had in hand instead of reading the cache
// a second time.
func TestCollectAllContainerResources_SurvivesPodDeletedDuringNodemonFetch(t *testing.T) {
	pod := testPod("ns1", "pod-a", "node-a", "app")
	informer, stopCh := newSyncedPodInformer(t, pod)
	defer close(stopCh)

	var deleteOnce bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/container/metrics" {
			// Simulate a watch event removing the pod from the informer's
			// indexer while we're blocked in this "network" call — the
			// window the TOCTOU race lived in. Only needs to happen once;
			// deleting an already-deleted key is a harmless no-op.
			if !deleteOnce {
				_ = informer.GetIndexer().Delete(pod)
				deleteOnce = true
			}

			metrics := []UnifiedContainerMetric{
				{
					NodeName:          "node-a",
					Namespace:         pod.Namespace,
					Pod:               pod.Name,
					Container:         "app",
					Timestamp:         time.Now(),
					CPUUsageNanoCores: 250_000_000,
					MemoryWorkingSet:  512 * 1024 * 1024,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metrics)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)

	nmClient := &NodemonClient{
		port:          port,
		httpClient:    server.Client(),
		log:           logr.Discard(),
		nodeToIP:      map[string]string{"node-a": parsed.Hostname()},
		lastRefreshed: time.Now(),
	}

	fakeLogger := &fakeTelemetryLogger{}

	c := &ContainerResourceCollector{
		nodemonClient:    nmClient,
		kubeletClient:    NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard()),
		podInformer:      informer,
		batchChan:        make(chan CollectedResource, 10),
		config:           ContainerResourceCollectorConfig{DisableGPUMetrics: true},
		excludedPods:     map[types.NamespacedName]bool{},
		logger:           logr.Discard(),
		telemetryLogger:  fakeLogger,
		throttle:         throttleTracker{lastEmitted: make(map[string]time.Time)},
		networkByteRates: nodemon.NewRateCalculator(),
	}

	c.collectAllContainerResources(context.Background())

	// Sanity check: the race actually happened — the pod really is gone
	// from the live indexer by the time the (now-eliminated) second lookup
	// would have run.
	podKey, err := cache.MetaNamespaceKeyFunc(pod)
	require.NoError(t, err)
	_, exists, err := informer.GetIndexer().GetByKey(podKey)
	require.NoError(t, err)
	require.False(t, exists, "expected pod to have been deleted from the informer during the nodemon call")

	select {
	case resource := <-c.batchChan:
		require.Equal(t, "ns1/pod-a/app", resource.Key)
		require.Equal(t, ContainerResource, resource.ResourceType)
	case <-time.After(time.Second):
		t.Fatal("expected container resource to be emitted despite the concurrent deletion")
	}

	require.Empty(t, fakeLogger.reportsWithErrorType("pod_cache_fail"),
		"collector should not report a cache miss when the pod was captured before the race window")
}
