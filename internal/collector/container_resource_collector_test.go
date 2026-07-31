// internal/collector/container_resource_collector_test.go
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
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
		kubeletClient:    NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard(), 0),
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

// TestCollectAllContainerResources_MissingPodsAggregatedNotSpammed is a
// characterization test for
// https://github.com/devzero-inc/services/issues/9431: nodemon's
// /v2/container/metrics cache refreshes on a fixed 30s ticker
// (cmd/zxporter-nodemon/main.go), while ContainerResourceCollector polls
// every 10s by default and snapshots the pod informer (a near-instant k8s
// watch) fresh each cycle. A pod that terminates is gone from the
// informer snapshot almost immediately but can remain in nodemon's stale
// response for up to ~30s — an expected race on any sufficiently churny
// cluster, not a failure. Today, every such pod is reported as its own
// LOG_LEVEL_ERROR telemetry event ("pod_cache_fail"); on production
// clusters this produced hundreds of ERROR events per hour (see #9431).
// This test simulates 5 such "phantom" pods (reported by nodemon, absent
// from the informer) alongside 1 real pod, and asserts the collector
// reports one aggregated WARN summary for the cycle instead of one ERROR
// per phantom pod.
func TestCollectAllContainerResources_MissingPodsAggregatedNotSpammed(t *testing.T) {
	realPod := testPod("ns1", "real-pod", "node-a", "app")
	informer, stopCh := newSyncedPodInformer(t, realPod)
	defer close(stopCh)

	const numPhantomPods = 5

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/container/metrics" {
			metrics := []UnifiedContainerMetric{
				{
					NodeName:          "node-a",
					Namespace:         realPod.Namespace,
					Pod:               realPod.Name,
					Container:         "app",
					Timestamp:         time.Now(),
					CPUUsageNanoCores: 250_000_000,
					MemoryWorkingSet:  512 * 1024 * 1024,
				},
			}
			for i := range numPhantomPods {
				metrics = append(metrics, UnifiedContainerMetric{
					NodeName:          "node-a",
					Namespace:         "ns1",
					Pod:               fmt.Sprintf("phantom-pod-%d", i),
					Container:         "app",
					Timestamp:         time.Now(),
					CPUUsageNanoCores: 100_000_000,
					MemoryWorkingSet:  256 * 1024 * 1024,
				})
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
		kubeletClient:    NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard(), 0),
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

	select {
	case resource := <-c.batchChan:
		require.Equal(t, "ns1/real-pod/app", resource.Key,
			"the real pod's container should still be emitted regardless of the phantom pods")
	case <-time.After(time.Second):
		t.Fatal("expected the real pod's container resource to be emitted")
	}

	require.Empty(t, fakeLogger.reportsWithErrorType("pod_cache_fail"),
		"expected no per-pod ERROR reports — phantom pods from a stale nodemon cache are an "+
			"expected race, not individual failures (see #9431)")

	summaries := fakeLogger.reportsWithEventType("pod_cache_miss_summary")
	require.Lenf(t, summaries, 1,
		"expected exactly one aggregated summary report for the cycle, got %d", len(summaries))
	require.Equal(t, gen.LogLevel_LOG_LEVEL_WARN, summaries[0].level)
	require.Equal(t, fmt.Sprintf("%d", numPhantomPods), summaries[0].fields["missing_count"])
}

// TestCollectAllContainerResources_MissingPodsSummaryCapsSampleList guards
// against a gitar-bot review finding on #9432: on a very churny cluster the
// missing_pods field, which serializes the full phantom-pod list into one
// telemetry field, could grow unbounded — the exact high-volume scenario
// the aggregation fix targets in the first place. missing_count must always
// carry the true total even when the sample list is capped.
func TestCollectAllContainerResources_MissingPodsSummaryCapsSampleList(t *testing.T) {
	realPod := testPod("ns1", "real-pod", "node-a", "app")
	informer, stopCh := newSyncedPodInformer(t, realPod)
	defer close(stopCh)

	const numPhantomPods = 30 // > the 20-entry sample cap

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/container/metrics" {
			metrics := []UnifiedContainerMetric{
				{
					NodeName:          "node-a",
					Namespace:         realPod.Namespace,
					Pod:               realPod.Name,
					Container:         "app",
					Timestamp:         time.Now(),
					CPUUsageNanoCores: 250_000_000,
					MemoryWorkingSet:  512 * 1024 * 1024,
				},
			}
			for i := range numPhantomPods {
				metrics = append(metrics, UnifiedContainerMetric{
					NodeName:          "node-a",
					Namespace:         "ns1",
					Pod:               fmt.Sprintf("phantom-pod-%d", i),
					Container:         "app",
					Timestamp:         time.Now(),
					CPUUsageNanoCores: 100_000_000,
					MemoryWorkingSet:  256 * 1024 * 1024,
				})
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
		kubeletClient:    NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard(), 0),
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

	select {
	case <-c.batchChan:
	case <-time.After(time.Second):
		t.Fatal("expected the real pod's container resource to be emitted")
	}

	summaries := fakeLogger.reportsWithEventType("pod_cache_miss_summary")
	require.Len(t, summaries, 1)
	require.Equal(t, fmt.Sprintf("%d", numPhantomPods), summaries[0].fields["missing_count"],
		"missing_count must carry the true total even when the sample list is capped")

	sampleField := summaries[0].fields["missing_pods"]
	require.Contains(t, sampleField, "(+10 more)",
		"expected the sample list to be capped with a remainder suffix, got: %s", sampleField)
	// podMetricsList.Items iterates a map internally, so which specific
	// pods land in the truncated sample isn't deterministic — assert the
	// sample size is capped rather than which names survive.
	require.Equal(t, 20, strings.Count(sampleField, "ns1/phantom-pod-"),
		"expected exactly 20 sampled pod names, got: %s", sampleField)
}
