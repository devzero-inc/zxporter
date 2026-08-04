// internal/collector/node_collector_perf_test.go
package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// This file is a characterization/perf-regression test for
// https://github.com/devzero-inc/services/issues/9410: collectAllNodeResources
// collects every node's metrics serially, on a single goroutine, making up to
// 4 blocking HTTP calls per node (2x nodemon /node/metrics — one of them an
// outright duplicate re-fetch in collectNodeNetworkIOMetrics — 1x nodemon
// /container/metrics for GPU, and 1x kubelet Summary-API fallback for nodes
// with no nodemon pod). None of it runs concurrently, so total sweep time
// scales with node count instead of being bounded by the slowest single call.
//
// No real network sockets are used anywhere here — both the nodemon HTTP
// client and the kubelet fallback's REST client are pointed at custom
// http.RoundTrippers that simulate per-node latency and, for a subset of
// nodes, force the kubelet fallback path by simply omitting them from
// NodemonClient's nodeToIP map (the same "no nodemon pod on this node" case
// FetchNodeMetricsByNode already handles by returning (nil, nil)).

// nodemonRoundTripper simulates N nodemon backends without opening any real
// sockets: each "node" is just a hostname key into perNode, and RoundTrip
// decides how long to sleep and what to respond with based on the request's
// host and path. Read-only after construction, so safe for concurrent use
// once multiple goroutines start calling it (a fixed implementation will do
// exactly that).
type nodemonRoundTripper struct {
	perNode map[string]nodemonNodeSim
}

type nodemonNodeSim struct {
	nodeMetricsDelay time.Duration
	gpuMetricsDelay  time.Duration
}

func (rt *nodemonRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	sim, ok := rt.perNode[req.URL.Hostname()]
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown node"}), nil
	}

	// Matched by suffix rather than the exact literal nodemon_client.go builds
	// its request paths from, so this simulator doesn't drift silently if
	// that path ever changes shape (e.g. gains a query string).
	switch {
	case strings.HasSuffix(req.URL.Path, "v2/node/snapshot"):
		// Composite endpoint: one cache-only response carrying both the node
		// and GPU sections. Modelled with a single node-metrics latency since
		// nodemon serves it from an already-refreshed cache (no per-request
		// scrape), which is the whole point of the composite path.
		if err := simSleep(req, sim.nodeMetricsDelay); err != nil {
			return nil, err
		}
		return jsonResponse(http.StatusOK, nodeSnapshotResponse{
			SchemaVersion: snapshotSchemaVersion,
			NodeMetrics: &UnifiedNodeMetric{
				NodeName:          req.URL.Hostname(),
				Timestamp:         time.Now(),
				CPUUsageNanoCores: 500_000_000,
				MemoryWorkingSet:  1024 * 1024 * 1024,
			},
			Sections: nodeSnapshotSections{
				Node: snapshotSectionStatus{State: snapshotStateReady},
				GPU:  snapshotSectionStatus{State: snapshotStateNotReady},
			},
		}), nil
	case strings.HasSuffix(req.URL.Path, "container/metrics"):
		if err := simSleep(req, sim.gpuMetricsDelay); err != nil {
			return nil, err
		}
		// Empty GPU metrics list is a perfectly normal, valid response (most
		// nodes in a fleet have no GPU) — the point here is exercising the
		// latency, not the GPU data shape.
		return jsonResponse(http.StatusOK, []NodemonMetric{}), nil
	case strings.HasSuffix(req.URL.Path, "node/metrics"):
		if err := simSleep(req, sim.nodeMetricsDelay); err != nil {
			return nil, err
		}
		return jsonResponse(http.StatusOK, UnifiedNodeMetric{
			NodeName:          req.URL.Hostname(),
			Timestamp:         time.Now(),
			CPUUsageNanoCores: 500_000_000,
			MemoryWorkingSet:  1024 * 1024 * 1024,
		}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown path"}), nil
	}
}

// kubeletProxyRoundTripper simulates the kubelet Summary API as reached via
// the API-server node-proxy subresource
// (GET /api/v1/nodes/{name}/proxy/stats/summary), which is what
// KubeletSummaryClient.fetchSummary issues. Keyed by node name extracted
// from the path since that's the only identifying information available.
type kubeletProxyRoundTripper struct {
	delayByNode map[string]time.Duration
}

func (rt *kubeletProxyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Path shape: /api/v1/nodes/{name}/proxy/stats/summary
	const prefix = "/api/v1/nodes/"
	const suffix = "/proxy/stats/summary"
	path := req.URL.Path
	if len(path) <= len(prefix)+len(suffix) || path[:len(prefix)] != prefix {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unexpected path " + path}), nil
	}
	nodeName := path[len(prefix) : len(path)-len(suffix)]

	delay, ok := rt.delayByNode[nodeName]
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "no kubelet sim for " + nodeName}), nil
	}
	if err := simSleep(req, delay); err != nil {
		return nil, err
	}

	usage := uint64(300_000_000)
	working := uint64(512 * 1024 * 1024)
	summary := kubeletSummary{
		Node: kubeletNodeStats{
			NodeName: nodeName,
			CPU:      &kubeletCPUStats{Time: time.Now(), UsageNanoCores: &usage},
			Memory:   &kubeletMemoryStats{WorkingSetBytes: &working},
		},
	}
	return jsonResponse(http.StatusOK, summary), nil
}

// simSleep waits out the simulated network latency, honoring context
// cancellation the same way a real http.Client.Do would.
func simSleep(req *http.Request, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-req.Context().Done():
		return req.Context().Err()
	}
}

func jsonResponse(status int, body interface{}) *http.Response {
	b, err := json.Marshal(body)
	if err != nil {
		panic(fmt.Sprintf("test bug: failed to marshal fake response body: %v", err))
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     make(http.Header),
	}
}

// TestCollectAllNodeResources_PerfUnderLatency_KnownRegression_Issue9410 is a
// deliberately *failing* (red) characterization test for the current serial
// implementation, tracked at
// https://github.com/devzero-inc/services/issues/9410. It is expected to
// keep failing until collectAllNodeResources' per-node work is parallelized;
// once fixed, elapsed time should drop from ~seconds (scaling with node
// count) to a small multiple of the slowest single simulated call,
// regardless of node count, and this test should pass.
func TestCollectAllNodeResources_PerfUnderLatency_KnownRegression_Issue9410(t *testing.T) {
	const (
		numNodes = 60 // half nodemon-covered, half forced through the kubelet fallback

		// Arbitrary but realistic per-call latencies — comparable to what a
		// loaded nodemon pod or a busy API server can add under real churn,
		// per the Datadog gap evidence in issue #9410 (sweeps observed
		// ballooning to 60-90s+ against a nominal 10s tick).
		nodemonNodeMetricsDelay = 40 * time.Millisecond
		nodemonGPUMetricsDelay  = 20 * time.Millisecond
		kubeletFallbackDelay    = 60 * time.Millisecond
	)

	// The ceiling is derived from the serial baseline these constants imply,
	// rather than a fixed wall-clock number, so it scales automatically if
	// the latencies above ever change and stays a meaningful regression
	// signal instead of a flakiness-prone magic number. Serial cost per
	// nodemon-covered node is 2x nodeMetricsDelay (the real fetch plus the
	// duplicate re-fetch in collectNodeNetworkIOMetrics) + gpuMetricsDelay;
	// per kubelet-fallback node it's just kubeletFallbackDelay. Dividing by 4
	// gives any reasonable concurrent implementation (which should land
	// closer to a small multiple of the single slowest per-node call) ample
	// headroom against CI runner jitter/GC pauses, while still catching the
	// current serial implementation with a wide margin.
	serialBaseline := time.Duration(numNodes/2)*(2*nodemonNodeMetricsDelay+nodemonGPUMetricsDelay) +
		time.Duration(numNodes/2)*kubeletFallbackDelay
	maxAcceptableElapsed := serialBaseline / 4

	var nodes []*corev1.Node
	nodemonRT := &nodemonRoundTripper{perNode: make(map[string]nodemonNodeSim)}
	kubeletRT := &kubeletProxyRoundTripper{delayByNode: make(map[string]time.Duration)}
	nodeToIP := make(map[string]string)

	for i := 0; i < numNodes; i++ {
		name := fmt.Sprintf("node-%02d", i)
		nodes = append(nodes, testNode(name))

		if i%2 == 0 {
			// nodemon-covered: goes through the fast path in the first loop,
			// then pays the redundant re-fetch + GPU fetch in the second.
			nodeToIP[name] = name // hostname doubles as a fake "pod IP" — no real DNS/socket involved
			nodemonRT.perNode[name] = nodemonNodeSim{
				nodeMetricsDelay: nodemonNodeMetricsDelay,
				gpuMetricsDelay:  nodemonGPUMetricsDelay,
			}
		} else {
			// No nodemon pod on this node at all (absent from nodeToIP is
			// exactly how FetchNodeMetricsByNode signals "not covered",
			// which is what triggers the kubelet Summary-API fallback).
			kubeletRT.delayByNode[name] = kubeletFallbackDelay
		}
	}

	informer, stopCh := newSyncedNodeInformer(t, nodes...)
	defer close(stopCh)

	nmClient := &NodemonClient{
		port:          80, // irrelevant: the custom RoundTripper never opens a real socket
		httpClient:    &http.Client{Transport: nodemonRT},
		log:           logr.Discard(),
		nodeToIP:      nodeToIP,
		lastRefreshed: time.Now(),
	}

	kubeletK8sClient, err := kubernetes.NewForConfigAndClient(
		// QPS: -1 disables client-go's client-side rate limiter (its default,
		// unset-QPS behavior throttles to ~5 QPS/10 burst), matching how
		// production actually configures this client — via
		// ctrl.GetConfigOrDie(), which sets QPS: -1 itself to rely on
		// server-side API Priority and Fairness instead. Without this, the
		// simulated concurrent kubelet-fallback calls below get serialized by
		// the client-side limiter rather than by collectAllNodeResources'
		// own (fixed) concurrency, which would make this test measure the
		// wrong thing.
		&rest.Config{Host: "http://fake-apiserver", QPS: -1},
		&http.Client{Transport: kubeletRT},
	)
	require.NoError(t, err)

	fakeLogger := &fakeTelemetryLogger{}

	c := &NodeCollector{
		metricsClient:   &metricsv1.Clientset{},
		nodemonClient:   nmClient,
		kubeletClient:   NewKubeletSummaryClient(kubeletK8sClient, logr.Discard(), 0),
		nodeInformer:    informer,
		batchChan:       make(chan CollectedResource, numNodes),
		config:          NodeCollectorConfig{DisableGPUMetrics: false},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: fakeLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}

	start := time.Now()
	c.collectAllNodeResources(context.Background())
	elapsed := time.Since(start)

	// Correctness sanity check first: every node should still have been
	// processed and emitted, regardless of how long it took — a fast but
	// incomplete implementation would be a worse bug than a slow correct one.
	close(c.batchChan)
	emitted := make(map[string]bool, numNodes)
	for res := range c.batchChan {
		emitted[res.Key] = true
	}
	require.Len(t, emitted, numNodes, "expected every node to be emitted regardless of collection latency")

	fakeLogger.mu.Lock()
	var successReports []fakeReport
	for _, r := range fakeLogger.reports {
		if r.fields["event_type"] == "node_metrics_query_success" {
			successReports = append(successReports, r)
		}
	}
	fakeLogger.mu.Unlock()
	require.Len(t, successReports, 1)
	require.Equal(t, fmt.Sprintf("%d", numNodes/2), successReports[0].fields["nodemon_covered"])
	require.Equal(t, fmt.Sprintf("%d", numNodes/2), successReports[0].fields["kubelet_fallback"])

	// The actual regression assertion. This is expected to FAIL today —
	// collectAllNodeResources has no concurrency, so elapsed scales with
	// numNodes instead of being bounded by the slowest single call. It
	// should start passing once per-node collection is parallelized (see
	// issue #9410).
	require.Lessf(t, elapsed, maxAcceptableElapsed,
		"collectAllNodeResources took %s to process %d nodes (%d nodemon-covered, %d kubelet-fallback) "+
			"with per-call latencies of nodeMetrics=%s/gpuMetrics=%s/kubeletFallback=%s — this scales with "+
			"node count rather than being bounded by the slowest single call, meaning per-node network "+
			"calls are not running concurrently (see https://github.com/devzero-inc/services/issues/9410)",
		elapsed, numNodes, numNodes/2, numNodes/2,
		nodemonNodeMetricsDelay, nodemonGPUMetricsDelay, kubeletFallbackDelay)
}

// TestStop_DoesNotRaceInFlightSweep reproduces the shutdown race flagged
// during review of the parallelization fix: Stop() used to close batchChan
// (and then nil out the field) immediately after signaling stopCh, without
// waiting for a sweep already in flight to finish. With
// collectAllNodeResources now fanning out to up to
// maxConcurrentNodeCollections goroutines per sweep (instead of one), any of
// them still blocked on a slow "network" call at the moment of Stop() runs
// would later evaluate the now-nil/closed c.batchChan for their send —
// racing unsynchronized against Stop()'s writes to that same field. Depending
// on the exact interleaving this either panics (send on a closed channel) or
// blocks forever (send on a nil channel, which is exactly what was observed
// reproducing this without the fix: every worker hangs indefinitely and
// sweepDone below never fires) — a race that existed before this change too,
// but with a much narrower window (only one goroutine could ever be
// mid-send).
//
// All nodes here are given a delay comfortably longer than the pause before
// Stop() is called, so every worker is still in flight (not yet at its
// batchChan send) when Stop() runs — the timing margin is deliberately wide
// to make this deterministic rather than a rare/flaky reproduction.
func TestStop_DoesNotRaceInFlightSweep(t *testing.T) {
	const (
		numNodes  = 20
		callDelay = 100 * time.Millisecond
		leadTime  = 5 * time.Millisecond // << callDelay: Stop() must land mid-sweep, not after it
	)

	var nodes []*corev1.Node
	nodemonRT := &nodemonRoundTripper{perNode: make(map[string]nodemonNodeSim)}
	nodeToIP := make(map[string]string)
	for i := 0; i < numNodes; i++ {
		name := fmt.Sprintf("stop-node-%02d", i)
		nodes = append(nodes, testNode(name))
		nodeToIP[name] = name
		nodemonRT.perNode[name] = nodemonNodeSim{nodeMetricsDelay: callDelay, gpuMetricsDelay: callDelay}
	}

	informer, stopInformerCh := newSyncedNodeInformer(t, nodes...)
	defer close(stopInformerCh)

	nmClient := &NodemonClient{
		port:          80,
		httpClient:    &http.Client{Transport: nodemonRT},
		log:           logr.Discard(),
		nodeToIP:      nodeToIP,
		lastRefreshed: time.Now(),
	}

	c := &NodeCollector{
		metricsClient:   &metricsv1.Clientset{},
		nodemonClient:   nmClient,
		kubeletClient:   NewKubeletSummaryClient(k8sfake.NewSimpleClientset(), logr.Discard(), 0),
		nodeInformer:    informer,
		batchChan:       make(chan CollectedResource, numNodes),
		stopCh:          make(chan struct{}),
		config:          NodeCollectorConfig{DisableGPUMetrics: false},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: &fakeTelemetryLogger{},
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}

	// Mirrors what Start()/collectNodeResourcesLoop actually do: track the
	// sweep with loopWG so Stop() has something to wait on.
	c.loopWG.Add(1)
	sweepDone := make(chan struct{})
	go func() {
		defer c.loopWG.Done()
		defer close(sweepDone)
		c.collectAllNodeResources(context.Background())
	}()

	time.Sleep(leadTime)

	// A panic here happens on a goroutine this test can't recover from — the
	// whole test binary would crash rather than report a clean failure.
	// That's inherent to testing for this class of bug; without the fix, the
	// more commonly observed outcome is the timeout below instead (workers
	// left blocked forever on the nil'd-out channel).
	require.NoError(t, c.Stop())

	select {
	case <-sweepDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep goroutine never finished after Stop() returned — " +
			"workers are likely blocked forever sending on the nil'd-out batchChan")
	}
}
