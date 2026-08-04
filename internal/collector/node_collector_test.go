// internal/collector/node_collector_test.go
package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// Legacy nodemon endpoints the node collector falls back to when the composite
// /v2/node/snapshot is unavailable.
const (
	legacyNodeMetricsPath    = "/node/metrics"
	legacyNodeGPUMetricsPath = "/container/metrics"
)

// newFakeKubeletClientForNode returns a KubeletSummaryClient backed by an
// in-memory API-server proxy round tripper (see kubeletProxyRoundTripper) that
// serves a Summary API response for nodeName with the given CPU (nanocores) and
// working-set (bytes) usage. Used to exercise the node-section kubelet fallback
// without opening a real socket.
func newFakeKubeletClientForNode(t *testing.T, nodeName string, usageNanoCores, workingSetBytes uint64) *KubeletSummaryClient {
	t.Helper()
	kubeletRT := &kubeletProxyRoundTripper{delayByNode: map[string]time.Duration{nodeName: 0}}
	_ = usageNanoCores // the round tripper serves fixed sample values
	_ = workingSetBytes
	client, err := kubernetes.NewForConfigAndClient(
		&rest.Config{Host: "http://fake-apiserver", QPS: -1},
		&http.Client{Transport: kubeletRT},
	)
	require.NoError(t, err)
	return NewKubeletSummaryClient(client, logr.Discard(), 0)
}

// pathCountingNodemon is a fake nodemon that records how many requests hit each
// path, so tests can assert the collector makes exactly one composite request
// per node and never falls back to the legacy endpoints in steady state.
type pathCountingNodemon struct {
	mu     sync.Mutex
	counts map[string]int
}

func (p *pathCountingNodemon) record(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		p.counts = map[string]int{}
	}
	p.counts[path]++
}

func (p *pathCountingNodemon) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[path]
}

// newNodemonClientForServer wires a NodemonClient to a test server, mapping the
// single node to the server's host:port so discovery is bypassed.
func newNodemonClientForServer(t *testing.T, server *httptest.Server, nodeName string) *NodemonClient {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	return &NodemonClient{
		port:          port,
		httpClient:    server.Client(),
		log:           logr.Discard(),
		nodeToIP:      map[string]string{nodeName: parsed.Hostname()},
		lastRefreshed: time.Now(),
	}
}

func newNodeCollectorForTest(nmClient *NodemonClient, informer cache.SharedIndexInformer) *NodeCollector {
	return &NodeCollector{
		metricsClient:   &metricsv1.Clientset{},
		nodemonClient:   nmClient,
		kubeletClient:   NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard(), 0),
		nodeInformer:    informer,
		batchChan:       make(chan CollectedResource, 10),
		config:          NodeCollectorConfig{DisableGPUMetrics: false},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: &fakeTelemetryLogger{},
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}
}

// TestCollectAllNodeResources_UsesSingleCompositeRequest asserts the steady-state
// path: exactly one /v2/node/snapshot request per covered node, zero legacy
// requests, and both node and GPU fields emitted from the composite response.
func TestCollectAllNodeResources_UsesSingleCompositeRequest(t *testing.T) {
	node := testNode("gpu-node")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	counter := &pathCountingNodemon{}
	collectedAt := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.record(r.URL.Path)
		if r.URL.Path != nodeSnapshotPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := nodeSnapshotResponse{
			SchemaVersion: snapshotSchemaVersion,
			NodeMetrics: &UnifiedNodeMetric{
				NodeName:             node.Name,
				Timestamp:            collectedAt,
				CPUUsageNanoCores:    1_000_000_000,
				MemoryWorkingSet:     2 * 1024 * 1024 * 1024,
				NetworkRxBytesPerSec: 42,
			},
			GPUSummary: &nodeGPUSummary{
				GPUCount:          2,
				GPUUtilizationAvg: 73.5,
				GPUUtilizationMax: 91,
				GPUModels:         []string{"A100"},
			},
			Sections: nodeSnapshotSections{
				Node: snapshotSectionStatus{State: snapshotStateReady, CollectedAt: &collectedAt},
				GPU:  snapshotSectionStatus{State: snapshotStateReady, CollectedAt: &collectedAt},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	nmClient := newNodemonClientForServer(t, server, node.Name)
	c := newNodeCollectorForTest(nmClient, informer)

	c.collectAllNodeResources(context.Background())

	require.Equal(t, 1, counter.count(nodeSnapshotPath), "expected exactly one composite request")
	require.Equal(t, 0, counter.count(legacyNodeMetricsPath), "no legacy node-metrics call in steady state")
	require.Equal(t, 0, counter.count(legacyNodeGPUMetricsPath), "no legacy GPU call in steady state")

	select {
	case res := <-c.batchChan:
		require.Equal(t, node.Name, res.Key)
		data := res.Object.(map[string]interface{})
		require.EqualValues(t, 42, data["networkReceiveBytes"], "node network fields from composite")
		require.EqualValues(t, 2, data["gpuCount"], "GPU fields from composite")
		require.EqualValues(t, 73.5, data["gpuUtilizationAvg"])
	case <-time.After(time.Second):
		t.Fatal("expected a node resource to be emitted")
	}
}

// TestCollectAllNodeResources_NodeNotReadyFallsBackToKubelet asserts that when
// the composite node section is not_ready, the collector uses the kubelet
// fallback for node metrics while still attaching a usable GPU section.
func TestCollectAllNodeResources_NodeNotReadyFallsBackToKubelet(t *testing.T) {
	node := testNode("gpu-node")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	counter := &pathCountingNodemon{}
	collectedAt := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.record(r.URL.Path)
		if r.URL.Path != nodeSnapshotPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Node section not_ready (so node_metrics is omitted), GPU ready.
		resp := nodeSnapshotResponse{
			SchemaVersion: snapshotSchemaVersion,
			GPUSummary:    &nodeGPUSummary{GPUCount: 1, GPUUtilizationAvg: 50},
			Sections: nodeSnapshotSections{
				Node: snapshotSectionStatus{State: snapshotStateNotReady},
				GPU:  snapshotSectionStatus{State: snapshotStateReady, CollectedAt: &collectedAt},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	nmClient := newNodemonClientForServer(t, server, node.Name)
	c := newNodeCollectorForTest(nmClient, informer)
	// Composite node section is not_ready, so node CPU/memory must come from the
	// kubelet Summary API fallback — while the usable GPU section is preserved.
	c.kubeletClient = newFakeKubeletClientForNode(t, node.Name, 300_000_000, 512*1024*1024)

	c.collectAllNodeResources(context.Background())

	require.Equal(t, 1, counter.count(nodeSnapshotPath))

	select {
	case res := <-c.batchChan:
		require.Equal(t, node.Name, res.Key)
		data := res.Object.(map[string]interface{})
		require.EqualValues(t, 1, data["gpuCount"], "usable GPU section survives node fallback")
		require.EqualValues(t, 300, data["cpuUsageMillis"], "node CPU came from the kubelet fallback")
	case <-time.After(time.Second):
		t.Fatal("expected a node resource to be emitted via kubelet fallback")
	}
}

// TestCollectAllNodeResources_StaleGPUDropped asserts that a GPU section marked
// "stale" (nodemon's DCGM refresh is failing and it is serving a last-good
// snapshot of unbounded age) is NOT ingested — the node is still emitted from
// its (ready) node section, but with no GPU fields, matching main's behavior of
// emitting a gap rather than arbitrarily old GPU values stamped as current.
func TestCollectAllNodeResources_StaleGPUDropped(t *testing.T) {
	node := testNode("gpu-node")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	collectedAt := time.Now().Add(-45 * time.Second) // deliberately old
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeSnapshotPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := nodeSnapshotResponse{
			SchemaVersion: snapshotSchemaVersion,
			NodeMetrics: &UnifiedNodeMetric{
				NodeName: node.Name, Timestamp: time.Now(),
				CPUUsageNanoCores: 1_000_000_000, MemoryWorkingSet: 2 * 1024 * 1024 * 1024,
			},
			GPUSummary: &nodeGPUSummary{GPUCount: 4, GPUUtilizationAvg: 88}, // last-good, but stale
			Sections: nodeSnapshotSections{
				Node: snapshotSectionStatus{State: snapshotStateReady, CollectedAt: &collectedAt},
				GPU:  snapshotSectionStatus{State: snapshotStateStale, CollectedAt: &collectedAt},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	nmClient := newNodemonClientForServer(t, server, node.Name)
	fakeLogger := &fakeTelemetryLogger{}
	c := newNodeCollectorForTest(nmClient, informer)
	c.telemetryLogger = fakeLogger

	c.collectAllNodeResources(context.Background())

	select {
	case res := <-c.batchChan:
		require.Equal(t, node.Name, res.Key)
		data := res.Object.(map[string]interface{})
		require.NotContains(t, data, "gpuCount", "stale GPU must not be ingested")
		require.NotContains(t, data, "gpuUtilizationAvg", "stale GPU must not be ingested")
		require.EqualValues(t, 1000, data["cpuUsageMillis"], "the ready node section is still emitted")
	case <-time.After(time.Second):
		t.Fatal("expected the node to still be emitted (from its ready node section)")
	}

	// The dropped-because-stale GPU must be visible as DAKR telemetry, not just
	// a nodemon-side log.
	staleWarns := fakeLogger.reportsWithEventType("gpu_dropped_stale")
	require.Len(t, staleWarns, 1, "expected one gpu_dropped_stale WARN summary")
	require.Equal(t, gen.LogLevel_LOG_LEVEL_WARN, staleWarns[0].level)
	require.Equal(t, "1", staleWarns[0].fields["gpu_dropped_stale"])
}

// TestCollectAllNodeResources_LegacyFallbackEmitsTelemetry asserts that when a
// nodemon pod predates the composite endpoint (404 on /v2/node/snapshot) and
// the collector falls back to the legacy /node/metrics + /container/metrics
// pair, a WARN telemetry summary is emitted so a stalled rollout on the old
// 2-calls-per-node path is observable.
func TestCollectAllNodeResources_LegacyFallbackEmitsTelemetry(t *testing.T) {
	node := testNode("legacy-node")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case legacyNodeMetricsPath:
			_ = json.NewEncoder(w).Encode(UnifiedNodeMetric{
				NodeName: node.Name, Timestamp: time.Now(),
				CPUUsageNanoCores: 500_000_000, MemoryWorkingSet: 1024 * 1024 * 1024,
			})
		case legacyNodeGPUMetricsPath:
			_ = json.NewEncoder(w).Encode([]NodemonMetric{})
		default: // /v2/node/snapshot → 404, forcing the legacy fallback
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	nmClient := newNodemonClientForServer(t, server, node.Name)
	fakeLogger := &fakeTelemetryLogger{}
	c := &NodeCollector{
		metricsClient:   &metricsv1.Clientset{},
		nodemonClient:   nmClient,
		kubeletClient:   NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard(), 0),
		nodeInformer:    informer,
		batchChan:       make(chan CollectedResource, 10),
		config:          NodeCollectorConfig{DisableGPUMetrics: false},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: fakeLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}

	c.collectAllNodeResources(context.Background())

	warns := fakeLogger.reportsWithEventType("nodemon_legacy_fallback")
	require.Len(t, warns, 1, "expected one legacy-fallback WARN summary for the sweep")
	require.Equal(t, gen.LogLevel_LOG_LEVEL_WARN, warns[0].level)
	require.Equal(t, "1", warns[0].fields["legacy_fallback"])

	// The success summary should also carry the legacy_fallback count.
	success := fakeLogger.reportsWithEventType("node_metrics_query_success")
	require.Len(t, success, 1)
	require.Equal(t, "1", success[0].fields["legacy_fallback"])
}

// fakeTelemetryLogger captures Report calls for assertions instead of sending
// them anywhere, mirroring the shape of telemetry_logger.Logger.
type fakeTelemetryLogger struct {
	mu      sync.Mutex
	reports []fakeReport
}

type fakeReport struct {
	level  gen.LogLevel
	source string
	msg    string
	err    error
	fields map[string]string
}

func (f *fakeTelemetryLogger) Report(level gen.LogLevel, source string, msg string, err error, fields map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, fakeReport{level: level, source: source, msg: msg, err: err, fields: fields})
}

func (f *fakeTelemetryLogger) Stop() {}

func (f *fakeTelemetryLogger) reportsWithErrorType(errorType string) []fakeReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReport
	for _, r := range f.reports {
		if r.fields["error_type"] == errorType {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeTelemetryLogger) reportsWithEventType(eventType string) []fakeReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReport
	for _, r := range f.reports {
		if r.fields["event_type"] == eventType {
			out = append(out, r)
		}
	}
	return out
}

// newSyncedNodeInformer builds a real SharedIndexInformer for Nodes, backed by
// a fake clientset seeded with the given nodes, and waits for the initial
// cache sync so the indexer is populated before the test runs.
func newSyncedNodeInformer(t *testing.T, nodes ...*corev1.Node) (cache.SharedIndexInformer, chan struct{}) {
	t.Helper()
	objs := make([]runtime.Object, len(nodes))
	for i, n := range nodes {
		objs[i] = n
	}
	client := k8sfake.NewSimpleClientset(objs...)
	factory := newInformerFactory(client, nil)
	informer := factory.Core().V1().Nodes().Informer()
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	require.True(t, cache.WaitForCacheSync(stopCh, informer.HasSynced), "node informer failed to sync")
	return informer, stopCh
}

// testNode builds a minimal, valid node object with just enough status set to
// avoid nil derefs in collectAllNodeResources' resourceData construction.
func testNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
}

// TestCollectAllNodeResources_SurvivesNodeDeletedDuringNodemonFetch reproduces
// the TOCTOU race described in services/operators/zxporter: the informer cache
// is snapshotted once at the top of collectAllNodeResources, then a slow,
// real network round trip to nodemon runs per node. Before the fix, a second,
// separate GetIndexer().GetByKey() lookup after that network call would race
// against concurrent node deletions (e.g. spot instance termination) and
// silently drop the node's containers for the cycle. This test deletes the
// node from the live informer indexer as a side effect of the nodemon HTTP
// call — i.e. strictly *after* the node was captured in step one but
// strictly *before* any later re-read of the cache — and asserts the node is
// still emitted, because the collector now reuses the object it already had
// in hand instead of reading the cache a second time.
func TestCollectAllNodeResources_SurvivesNodeDeletedDuringNodemonFetch(t *testing.T) {
	node := testNode("node-a")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == nodeSnapshotPath {
			// Simulate a watch event removing the node from the informer's
			// indexer while we're blocked in this "network" call — the
			// window the TOCTOU race lived in.
			_ = informer.GetIndexer().Delete(node)

			resp := nodeSnapshotResponse{
				SchemaVersion: snapshotSchemaVersion,
				NodeMetrics: &UnifiedNodeMetric{
					NodeName:          node.Name,
					Timestamp:         time.Now(),
					CPUUsageNanoCores: 500_000_000,
					MemoryWorkingSet:  1024 * 1024 * 1024,
				},
				Sections: nodeSnapshotSections{
					Node: snapshotSectionStatus{State: snapshotStateReady},
					GPU:  snapshotSectionStatus{State: snapshotStateNotReady},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
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
		nodeToIP:      map[string]string{node.Name: parsed.Hostname()},
		lastRefreshed: time.Now(),
	}

	fakeLogger := &fakeTelemetryLogger{}

	c := &NodeCollector{
		metricsClient:   &metricsv1.Clientset{},
		nodemonClient:   nmClient,
		kubeletClient:   NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard(), 0),
		nodeInformer:    informer,
		batchChan:       make(chan CollectedResource, 10),
		config:          NodeCollectorConfig{DisableGPUMetrics: true},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: fakeLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}

	c.collectAllNodeResources(context.Background())

	// Sanity check: the race actually happened — the node really is gone
	// from the live indexer by the time the (now-eliminated) second lookup
	// would have run.
	_, exists, err := informer.GetIndexer().GetByKey(node.Name)
	require.NoError(t, err)
	require.False(t, exists, "expected node to have been deleted from the informer during the nodemon call")

	select {
	case resource := <-c.batchChan:
		require.Equal(t, node.Name, resource.Key)
		require.Equal(t, NodeResource, resource.ResourceType)
	case <-time.After(time.Second):
		t.Fatal("expected node resource to be emitted despite the concurrent deletion")
	}

	require.Empty(t, fakeLogger.reportsWithErrorType("node_cache_fail"),
		"collector should not report a cache miss when the node was captured before the race window")
}
