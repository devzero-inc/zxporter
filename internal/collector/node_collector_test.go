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
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

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
		if r.URL.Path == "/node/metrics" {
			// Simulate a watch event removing the node from the informer's
			// indexer while we're blocked in this "network" call — the
			// window the TOCTOU race lived in.
			_ = informer.GetIndexer().Delete(node)

			metric := UnifiedNodeMetric{
				NodeName:          node.Name,
				Timestamp:         time.Now(),
				CPUUsageNanoCores: 500_000_000,
				MemoryWorkingSet:  1024 * 1024 * 1024,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metric)
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
		kubeletClient:   NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard()),
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
