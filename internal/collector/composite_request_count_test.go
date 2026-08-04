// internal/collector/composite_request_count_test.go
package collector

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// countingRT counts nodemon requests by endpoint family and simulates a fixed
// per-call latency. It serves BOTH the composite and the legacy endpoints, so
// the exact same test runs on this branch (collectors hit the composite path)
// and on main (collectors hit the legacy path) — the request tallies below are
// what proves the 4N-vs-2N difference.
type countingRT struct {
	mu           sync.Mutex
	byPath       map[string]int
	perCallDelay time.Duration
}

func (rt *countingRT) count(key string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.byPath[key]
}

func (rt *countingRT) record(key string) {
	rt.mu.Lock()
	if rt.byPath == nil {
		rt.byPath = map[string]int{}
	}
	rt.byPath[key]++
	rt.mu.Unlock()
}

func (rt *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if rt.perCallDelay > 0 {
		select {
		case <-time.After(rt.perCallDelay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	switch {
	case strings.HasSuffix(req.URL.Path, "/v2/node/snapshot"):
		rt.record("composite")
		return jsonResponse(http.StatusOK, map[string]any{
			"schema_version": 1,
			"node_metrics": map[string]any{
				"node_name": host, "cpu_usage_nanocores": 500_000_000, "memory_working_set_bytes": 1073741824,
			},
			"gpu_summary": map[string]any{"gpu_count": 0},
			"sections":    map[string]any{"node": map[string]any{"state": "ready"}, "gpu": map[string]any{"state": "not_ready"}},
		}), nil
	case strings.HasSuffix(req.URL.Path, "/v2/container/snapshot"):
		rt.record("composite")
		return jsonResponse(http.StatusOK, map[string]any{
			"schema_version": 1,
			"container_metrics": []map[string]any{{
				"node_name": host, "namespace": "ns1", "pod": "pod-" + host, "container": "app",
				"cpu_usage_nanocores": 250_000_000, "memory_working_set_bytes": 536870912,
			}},
			"runtime_metrics": map[string]any{"jvm": []any{}, "runtimes": []any{}},
			"sections":        map[string]any{"containers": map[string]any{"state": "ready"}, "runtime": map[string]any{"state": "disabled"}},
		}), nil
	case strings.HasSuffix(req.URL.Path, "/v2/container/metrics"):
		rt.record("legacy")
		return jsonResponse(http.StatusOK, []map[string]any{{
			"node_name": host, "namespace": "ns1", "pod": "pod-" + host, "container": "app",
			"cpu_usage_nanocores": 250_000_000, "memory_working_set_bytes": 536870912,
		}}), nil
	case strings.HasSuffix(req.URL.Path, "/container/runtime-metrics"):
		rt.record("legacy")
		return jsonResponse(http.StatusOK, map[string]any{"jvm": []any{}, "runtimes": []any{}}), nil
	case strings.HasSuffix(req.URL.Path, "/container/metrics"): // legacy GPU (node collector)
		rt.record("legacy")
		return jsonResponse(http.StatusOK, []map[string]any{}), nil
	case strings.HasSuffix(req.URL.Path, "/node/metrics"):
		rt.record("legacy")
		return jsonResponse(http.StatusOK, map[string]any{
			"node_name": host, "cpu_usage_nanocores": 500_000_000, "memory_working_set_bytes": 1073741824,
		}), nil
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown " + req.URL.Path}), nil
}

// TestCollectors_CompositeAvoids4NLegacyCalls is the negative test against the
// legacy 4N request pattern. Across N nodemon-covered nodes, the two collectors
// on this branch issue exactly 2N nodemon requests (one composite call per node
// per collector) and ZERO legacy per-metric calls. On main — where NodeCollector
// still does /node/metrics + /container/metrics and ContainerResourceCollector
// still does /v2/container/metrics + /container/runtime-metrics — the identical
// test instead records 4N requests, all on the legacy endpoints, and fails these
// assertions. It also logs each collector's sweep wall-clock under a fixed
// per-call latency so the latency delta is visible in the test output on both
// branches.
func TestCollectors_CompositeAvoids4NLegacyCalls(t *testing.T) {
	const (
		numNodes     = 20 // one bounded-concurrency batch (cap is 20)
		perCallDelay = 15 * time.Millisecond
	)

	var nodes []*corev1.Node
	var pods []*corev1.Pod
	nodeToIP := make(map[string]string)
	for i := 0; i < numNodes; i++ {
		name := "cnt-node-" + strconv.Itoa(i)
		nodes = append(nodes, testNode(name))
		pods = append(pods, testPod("ns1", "pod-"+name, name, "app"))
		nodeToIP[name] = name
	}

	rt := &countingRT{perCallDelay: perCallDelay}
	newClient := func() *NodemonClient {
		return &NodemonClient{
			port: 80, httpClient: &http.Client{Transport: rt}, log: logr.Discard(),
			nodeToIP: nodeToIP, lastRefreshed: time.Now(),
		}
	}

	// --- NodeCollector sweep ---
	nodeInformer, nodeStop := newSyncedNodeInformer(t, nodes...)
	defer close(nodeStop)
	nodeBatch := make(chan CollectedResource, numNodes*8)
	go drainForever(nodeBatch)
	nc := &NodeCollector{
		metricsClient:   &metricsv1.Clientset{},
		nodemonClient:   newClient(),
		kubeletClient:   NewKubeletSummaryClient(nil, logr.Discard(), 0),
		nodeInformer:    nodeInformer,
		batchChan:       nodeBatch,
		config:          NodeCollectorConfig{DisableGPUMetrics: false},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: &fakeTelemetryLogger{},
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}
	nodeStart := time.Now()
	nc.collectAllNodeResources(context.Background())
	nodeElapsed := time.Since(nodeStart)
	nodeComposite, nodeLegacy := rt.count("composite"), rt.count("legacy")

	// --- ContainerResourceCollector sweep (fresh counter) ---
	rt2 := &countingRT{perCallDelay: perCallDelay}
	podInformer, podStop := newSyncedPodInformer(t, pods...)
	defer close(podStop)
	ctrBatch := make(chan CollectedResource, numNodes*8)
	go drainForever(ctrBatch)
	cc := &ContainerResourceCollector{
		nodemonClient: &NodemonClient{
			port: 80, httpClient: &http.Client{Transport: rt2}, log: logr.Discard(),
			nodeToIP: nodeToIP, lastRefreshed: time.Now(),
		},
		kubeletClient:    NewKubeletSummaryClient(nil, logr.Discard(), 0),
		podInformer:      podInformer,
		batchChan:        ctrBatch,
		config:           ContainerResourceCollectorConfig{DisableGPUMetrics: false},
		excludedPods:     map[types.NamespacedName]bool{},
		logger:           logr.Discard(),
		telemetryLogger:  &fakeTelemetryLogger{},
		throttle:         throttleTracker{lastEmitted: make(map[string]time.Time)},
		networkByteRates: nodemon.NewRateCalculator(),
	}
	ctrStart := time.Now()
	cc.collectAllContainerResources(context.Background())
	ctrElapsed := time.Since(ctrStart)
	ctrComposite, ctrLegacy := rt2.count("composite"), rt2.count("legacy")

	total := nodeComposite + nodeLegacy + ctrComposite + ctrLegacy
	t.Logf("N=%d per-call=%s | node: %d composite + %d legacy in %s | container: %d composite + %d legacy in %s | TOTAL requests=%d",
		numNodes, perCallDelay,
		nodeComposite, nodeLegacy, nodeElapsed,
		ctrComposite, ctrLegacy, ctrElapsed, total)

	// Negative assertions vs the legacy 4N pattern: composite path only.
	if nodeLegacy != 0 || ctrLegacy != 0 {
		t.Fatalf("collectors hit the legacy 4N endpoints (node legacy=%d, container legacy=%d); "+
			"expected the composite path with zero legacy calls", nodeLegacy, ctrLegacy)
	}
	if nodeComposite != numNodes {
		t.Fatalf("NodeCollector made %d composite requests, want exactly N=%d (one per node)", nodeComposite, numNodes)
	}
	if ctrComposite != numNodes {
		t.Fatalf("ContainerResourceCollector made %d composite requests, want exactly N=%d (one per node)", ctrComposite, numNodes)
	}
	if total != 2*numNodes {
		t.Fatalf("total nodemon requests=%d, want 2N=%d (the legacy path would be 4N=%d)", total, 2*numNodes, 4*numNodes)
	}
}

func drainForever(ch <-chan CollectedResource) {
	for range ch {
	}
}
