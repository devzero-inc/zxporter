// internal/collector/container_resource_collector_perf_test.go
package collector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devzero-inc/zxporter/internal/nodemon"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// This file is a characterization/perf-regression test for
// https://github.com/devzero-inc/services/issues/9417: collectAllContainerResources
// fetches cluster-wide container metrics from nodemon via three separate
// serial, single-goroutine sweeps over every discovered nodemon pod:
//
//   - buildPodMetricsFromNodemon -> FetchAllContainerMetrics (/v2/container/metrics)
//   - a second, redundant FetchAllContainerMetrics call (the exact same
//     endpoint, same data, called again to populate nodemonContainerMetricsCache)
//   - FetchAllRuntimeMetrics (/container/runtime-metrics)
//
// None of it runs concurrently, either across the three calls or within each
// call's own per-node loop, so total sweep time scales with node count
// instead of being bounded by the slowest single call — the same class of
// bug fixed for NodeCollector in #9410/#9411, plus a genuine duplicate fetch
// NodeCollector didn't have.
//
// No real network sockets are used anywhere here — the nodemon HTTP client
// is pointed at a custom http.RoundTripper that simulates per-node latency
// on both endpoints.

// containerNodemonRoundTripper simulates N nodemon backends without opening
// any real sockets: each "node" is just a hostname key into perNode, and
// RoundTrip decides how long to sleep and what to respond with based on the
// request's host and path.
type containerNodemonRoundTripper struct {
	perNode map[string]containerNodemonNodeSim
}

type containerNodemonNodeSim struct {
	containerMetricsDelay time.Duration
	runtimeMetricsDelay   time.Duration
	nodeName              string
}

func (rt *containerNodemonRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	sim, ok := rt.perNode[req.URL.Hostname()]
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown node"}), nil
	}

	switch {
	case strings.HasSuffix(req.URL.Path, "v2/container/metrics"):
		if err := simSleep(req, sim.containerMetricsDelay); err != nil {
			return nil, err
		}
		return jsonResponse(http.StatusOK, []UnifiedContainerMetric{
			{
				NodeName:          sim.nodeName,
				Namespace:         "ns1",
				Pod:               "pod-" + sim.nodeName,
				Container:         "app",
				Timestamp:         time.Now(),
				CPUUsageNanoCores: 250_000_000,
				MemoryWorkingSet:  512 * 1024 * 1024,
			},
		}), nil
	case strings.HasSuffix(req.URL.Path, "container/runtime-metrics"):
		if err := simSleep(req, sim.runtimeMetricsDelay); err != nil {
			return nil, err
		}
		// Empty runtime metrics is a perfectly normal, valid response (most
		// pods aren't JVM/runtime-detected) — the point here is exercising
		// the latency, not the payload shape.
		return jsonResponse(http.StatusOK, NodemonRuntimeMetrics{}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown path"}), nil
	}
}

// TestCollectAllContainerResources_PerfUnderLatency_KnownRegression_Issue9417
// is a deliberately *failing* (red) characterization test for the current
// serial, duplicate-fetching implementation, tracked at
// https://github.com/devzero-inc/services/issues/9417. It is expected to
// keep failing until the fix lands (parallelize NodemonClient's per-node
// fan-out and eliminate the duplicate FetchAllContainerMetrics call); once
// fixed, elapsed time should drop from scaling with node count to a small
// multiple of the slowest single simulated call, and this test should pass.
func TestCollectAllContainerResources_PerfUnderLatency_KnownRegression_Issue9417(t *testing.T) {
	const (
		numNodes = 50

		// Arbitrary but realistic per-call latencies, comparable to #9410's
		// evidence (sweeps observed ballooning well past their nominal tick
		// under real churn).
		containerMetricsDelay = 40 * time.Millisecond
		runtimeMetricsDelay   = 30 * time.Millisecond
	)

	// Today's known cost per node: buildPodMetricsFromNodemon's
	// FetchAllContainerMetrics call + the redundant second
	// FetchAllContainerMetrics call (container_resource_collector.go:310) +
	// FetchAllRuntimeMetrics — three serial N-node sweeps, none concurrent
	// with each other or internally. Dividing by 4 gives any reasonable
	// concurrent implementation (which should land closer to a small
	// multiple of the single slowest per-node call, especially once the
	// duplicate fetch is gone entirely) ample headroom against CI runner
	// jitter/GC pauses, while still catching the current implementation
	// with a wide margin.
	serialBaseline := time.Duration(numNodes) * (2*containerMetricsDelay + runtimeMetricsDelay)
	maxAcceptableElapsed := serialBaseline / 4

	var pods []*corev1.Pod
	nodemonRT := &containerNodemonRoundTripper{perNode: make(map[string]containerNodemonNodeSim)}
	nodeToIP := make(map[string]string)

	for i := 0; i < numNodes; i++ {
		nodeName := fmt.Sprintf("node-%02d", i)
		nodeToIP[nodeName] = nodeName // hostname doubles as a fake "pod IP" — no real DNS/socket involved
		nodemonRT.perNode[nodeName] = containerNodemonNodeSim{
			containerMetricsDelay: containerMetricsDelay,
			runtimeMetricsDelay:   runtimeMetricsDelay,
			nodeName:              nodeName,
		}
		pods = append(pods, testPod("ns1", "pod-"+nodeName, nodeName, "app"))
	}

	informer, stopInformerCh := newSyncedPodInformer(t, pods...)
	defer close(stopInformerCh)

	nmClient := &NodemonClient{
		port:          80, // irrelevant: the custom RoundTripper never opens a real socket
		httpClient:    &http.Client{Transport: nodemonRT},
		log:           logr.Discard(),
		nodeToIP:      nodeToIP,
		lastRefreshed: time.Now(),
	}

	fakeLogger := &fakeTelemetryLogger{}

	c := &ContainerResourceCollector{
		nodemonClient:    nmClient,
		kubeletClient:    NewKubeletSummaryClient(nil, logr.Discard(), 0), // unused: every node has nodemon coverage
		podInformer:      informer,
		batchChan:        make(chan CollectedResource, numNodes*4),
		config:           ContainerResourceCollectorConfig{DisableGPUMetrics: true},
		excludedPods:     map[types.NamespacedName]bool{},
		logger:           logr.Discard(),
		telemetryLogger:  fakeLogger,
		throttle:         throttleTracker{lastEmitted: make(map[string]time.Time)},
		networkByteRates: nodemon.NewRateCalculator(),
	}

	start := time.Now()
	c.collectAllContainerResources(context.Background())
	elapsed := time.Since(start)

	// Correctness sanity check first: every pod's container should still
	// have been processed and emitted, regardless of how long it took.
	close(c.batchChan)
	emitted := make(map[string]bool, numNodes)
	for res := range c.batchChan {
		emitted[res.Key] = true
	}
	require.Len(t, emitted, numNodes, "expected every pod's container to be emitted regardless of collection latency")

	require.Lessf(t, elapsed, maxAcceptableElapsed,
		"collectAllContainerResources took %s to process %d nodes with per-call latencies of "+
			"containerMetrics=%s/runtimeMetrics=%s — this scales with node count rather than being "+
			"bounded by the slowest single call, meaning nodemon fetches are neither deduplicated nor "+
			"running concurrently (see https://github.com/devzero-inc/services/issues/9417)",
		elapsed, numNodes, containerMetricsDelay, runtimeMetricsDelay)
}

// TestContainerResourceCollector_Stop_DoesNotRaceInFlightSweep mirrors
// NodeCollector's TestStop_DoesNotRaceInFlightSweep (#9411): Stop() used to
// close batchChan (then nil out the field) immediately after signaling
// stopCh, without waiting for a sweep already in flight to finish.
// collectResourcesLoop's single goroutine can be blocked deep in a slow
// nodemon fetch at the moment Stop() runs; without a wait, Stop() could
// close/nil batchChan before that goroutine reaches its own send, causing a
// panic (send on closed channel) or a permanent hang (send on a nil
// channel), depending on the exact unsynchronized read/write interleaving
// on c.batchChan. This doesn't need many concurrent senders to matter — a
// single one racing Stop() is already unsafe.
//
// All nodes here are given a delay comfortably longer than the pause before
// Stop() is called, so the sweep is still in flight (not yet at its
// batchChan sends) when Stop() runs — the timing margin is deliberately
// wide to make this deterministic rather than a rare/flaky reproduction.
func TestContainerResourceCollector_Stop_DoesNotRaceInFlightSweep(t *testing.T) {
	const (
		numNodes  = 20
		callDelay = 100 * time.Millisecond
		leadTime  = 5 * time.Millisecond // << callDelay: Stop() must land mid-sweep, not after it
	)

	var pods []*corev1.Pod
	nodemonRT := &containerNodemonRoundTripper{perNode: make(map[string]containerNodemonNodeSim)}
	nodeToIP := make(map[string]string)
	for i := 0; i < numNodes; i++ {
		nodeName := fmt.Sprintf("stop-node-%02d", i)
		nodeToIP[nodeName] = nodeName
		nodemonRT.perNode[nodeName] = containerNodemonNodeSim{
			containerMetricsDelay: callDelay,
			runtimeMetricsDelay:   callDelay,
			nodeName:              nodeName,
		}
		pods = append(pods, testPod("ns1", "pod-"+nodeName, nodeName, "app"))
	}

	informer, stopInformerCh := newSyncedPodInformer(t, pods...)
	defer close(stopInformerCh)

	nmClient := &NodemonClient{
		port:          80,
		httpClient:    &http.Client{Transport: nodemonRT},
		log:           logr.Discard(),
		nodeToIP:      nodeToIP,
		lastRefreshed: time.Now(),
	}

	c := &ContainerResourceCollector{
		nodemonClient:    nmClient,
		kubeletClient:    NewKubeletSummaryClient(nil, logr.Discard(), 0),
		podInformer:      informer,
		batchChan:        make(chan CollectedResource, numNodes*4),
		stopCh:           make(chan struct{}),
		config:           ContainerResourceCollectorConfig{DisableGPUMetrics: true},
		excludedPods:     map[types.NamespacedName]bool{},
		logger:           logr.Discard(),
		telemetryLogger:  &fakeTelemetryLogger{},
		throttle:         throttleTracker{lastEmitted: make(map[string]time.Time)},
		networkByteRates: nodemon.NewRateCalculator(),
	}

	// Mirrors what Start()/collectResourcesLoop actually do: track the sweep
	// with loopWG so Stop() has something to wait on.
	c.loopWG.Add(1)
	sweepDone := make(chan struct{})
	go func() {
		defer c.loopWG.Done()
		defer close(sweepDone)
		c.collectAllContainerResources(context.Background())
	}()

	time.Sleep(leadTime)

	// A panic here happens on a goroutine this test can't recover from — the
	// whole test binary would crash rather than report a clean failure.
	// Without the fix, the more commonly observed outcome is the timeout
	// below instead (the sweep goroutine left blocked forever sending on
	// the nil'd-out channel).
	require.NoError(t, c.Stop())

	select {
	case <-sweepDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep goroutine never finished after Stop() returned — " +
			"likely blocked forever sending on the nil'd-out batchChan")
	}
}
