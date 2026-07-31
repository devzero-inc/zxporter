// internal/collector/pvc_metrics_collector_perf_test.go
package collector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

// This file is a characterization/perf-regression test for
// https://github.com/devzero-inc/services/issues/9429: the PVC-metrics
// N+1 fetch bug. PersistentVolumeClaimMetricsCollector.collectAllPVCMetrics
// loops over every PVC and, via processPVCMetrics -> getFilesystemUsage,
// calls NodemonClient.FetchAllPVCMetrics once PER PVC. FetchAllPVCMetrics
// does a full fan-out HTTP call to every nodemon pod in the cluster, so
// total nodemon request volume for one collection sweep is
// O(pvcCount * nodeCount) instead of O(nodeCount). #9418 parallelized the
// fan-out *inside* a single FetchAllPVCMetrics call, but did not stop it
// from being re-invoked once per PVC — this is a distinct bug in the same
// family, root-caused on two production clusters via ClickHouse (multi-
// minute PVC collection cycles) and Datadog (thousands of per-hour
// "Failed to collect PVC metrics from nodemon" telemetry warnings, and
// >90% of PVCs reporting no stats on the larger cluster).
//
// No real network sockets are used — the nodemon HTTP client is pointed at
// a custom http.RoundTripper that counts every request to /pvc/metrics.

// testPVC builds a bound, filesystem-mode PVC ready for
// collectAllPVCMetrics to process.
func testPVC(namespace, name, volumeName string, capacityBytes int64) *corev1.PersistentVolumeClaim {
	mode := corev1.PersistentVolumeFilesystem
	quantity := *resource.NewQuantity(capacityBytes, resource.BinarySI)
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:  volumeName,
			VolumeMode:  &mode,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: quantity},
		},
	}
}

// newSyncedPVCInformerFactory builds a real SharedInformerFactory backed by a
// fake clientset seeded with the given PVCs, registers PVC and PV informers
// on it (collectAllPVCMetrics/processPVCMetrics read through
// factory.Core().V1()...Lister(), not a standalone informer), and waits for
// the initial cache sync.
func newSyncedPVCInformerFactory(t *testing.T, pvcs ...*corev1.PersistentVolumeClaim) (informers.SharedInformerFactory, chan struct{}) {
	t.Helper()
	objs := make([]runtime.Object, len(pvcs))
	for i, p := range pvcs {
		objs[i] = p
	}
	client := k8sfake.NewSimpleClientset(objs...)
	factory := newInformerFactory(client, nil)
	pvcInformer := factory.Core().V1().PersistentVolumeClaims().Informer()
	pvInformer := factory.Core().V1().PersistentVolumes().Informer()
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	require.True(t, cache.WaitForCacheSync(stopCh, pvcInformer.HasSynced, pvInformer.HasSynced), "PVC/PV informer failed to sync")
	return factory, stopCh
}

// pvcNodemonRoundTripper simulates numNodes nodemon backends, each owning a
// disjoint slice of the PVC fleet (mirroring how a real nodemon only
// reports volumes actually mounted on its own node). It counts every
// request made to /pvc/metrics across all simulated nodes, regardless of
// which node served it — that count is exactly the signal this test cares
// about: it scales with node count alone if the fetch is deduplicated per
// sweep, or with pvcCount*nodeCount if it is not.
type pvcNodemonRoundTripper struct {
	perNode      map[string][]UnifiedPVCMetric // hostname -> this node's PVC metrics
	requestCount int64
}

func (rt *pvcNodemonRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/pvc/metrics") {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown path"}), nil
	}
	atomic.AddInt64(&rt.requestCount, 1)

	metrics, ok := rt.perNode[req.URL.Hostname()]
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown node"}), nil
	}
	return jsonResponse(http.StatusOK, metrics), nil
}

// TestCollectAllPVCMetrics_PerfUnderLatency_KnownRegression_PVCFetchPerPVC
// proves the fetch-per-PVC bug by counting total /pvc/metrics requests
// across a whole sweep. With the bug, that count is
// numPVCs*numNodes (each PVC re-triggers a full fan-out); fixed, it's
// numNodes (one fan-out for the whole sweep, PVCs looked up from the
// cached result).
func TestCollectAllPVCMetrics_PerfUnderLatency_KnownRegression_PVCFetchPerPVC(t *testing.T) {
	const (
		numNodes       = 5
		pvcsPerNode    = 8
		numPVCs        = numNodes * pvcsPerNode
		capacityBytes  = 100 << 30 // 100Gi
		usedBytes      = 40 << 30
		availableBytes = 60 << 30
	)

	// A fetch-per-PVC ceiling would be numPVCs*numNodes (200). A
	// fetch-once-per-sweep implementation should land at exactly numNodes
	// (5) requests. Multiplying by 3 gives headroom for the constructor's
	// initial node-discovery call and any reasonable retry, while still
	// catching the current implementation (200 requests) with a huge
	// margin.
	maxAcceptableRequests := int64(numNodes * 3)

	rt := &pvcNodemonRoundTripper{perNode: make(map[string][]UnifiedPVCMetric)}
	nodeToIP := make(map[string]string)
	var pvcs []*corev1.PersistentVolumeClaim

	for n := range numNodes {
		nodeName := fmt.Sprintf("node-%02d", n)
		nodeToIP[nodeName] = nodeName // hostname doubles as a fake "pod IP" — no real DNS/socket involved

		var nodeMetrics []UnifiedPVCMetric
		for p := range pvcsPerNode {
			pvcName := fmt.Sprintf("pvc-%s-%02d", nodeName, p)
			pvcs = append(pvcs, testPVC("ns1", pvcName, "pv-"+pvcName, capacityBytes))
			nodeMetrics = append(nodeMetrics, UnifiedPVCMetric{
				Namespace:      "ns1",
				PVCName:        pvcName,
				UsedBytes:      usedBytes,
				CapacityBytes:  capacityBytes,
				AvailableBytes: availableBytes,
			})
		}
		rt.perNode[nodeName] = nodeMetrics
	}

	require.Len(t, pvcs, numPVCs)

	informerFactory, stopInformerCh := newSyncedPVCInformerFactory(t, pvcs...)
	defer close(stopInformerCh)

	nmClient := &NodemonClient{
		port:          80, // irrelevant: the custom RoundTripper never opens a real socket
		httpClient:    &http.Client{Transport: rt},
		log:           logr.Discard(),
		nodeToIP:      nodeToIP,
		lastRefreshed: time.Now(),
	}

	c := &PersistentVolumeClaimMetricsCollector{
		nodemonClient:   nmClient,
		informerFactory: informerFactory,
		batchChan:       make(chan CollectedResource, numPVCs*2),
		excludedPVCs:    map[types.NamespacedName]bool{},
		logger:          logr.Discard(),
		telemetryLogger: &fakeTelemetryLogger{},
	}

	c.collectAllPVCMetrics(context.Background())

	close(c.batchChan)
	statsAvailable := 0
	for res := range c.batchChan {
		snapshot, ok := res.Object.(*PersistentVolumeClaimMetricsSnapshot)
		require.True(t, ok)
		if snapshot.StatsAvailable {
			statsAvailable++
		}
	}
	require.Equal(t, numPVCs, statsAvailable,
		"expected every PVC to get stats from the (deterministic, always-succeeding) simulated nodemon backends")

	got := atomic.LoadInt64(&rt.requestCount)
	require.LessOrEqualf(t, got, maxAcceptableRequests,
		"collectAllPVCMetrics made %d requests to /pvc/metrics for %d PVCs across %d nodes — "+
			"this scales with PVC count instead of being bounded by node count, meaning "+
			"FetchAllPVCMetrics is being re-invoked once per PVC instead of once per sweep",
		got, numPVCs, numNodes)
}

// TestIndexPVCMetricsByNamespacedName_FirstOccurrenceWins guards against a
// review finding on this PR: a ReadWriteMany PVC mounted on multiple nodes
// can produce more than one UnifiedPVCMetric row for the same
// (namespace, pvcName) — one per node/pod that mounted it. The old linear
// scan in getFilesystemUsage returned the first matching row; a naive
// map-building loop would let the last row silently win instead, changing
// which node's numbers get reported for no functional reason.
func TestIndexPVCMetricsByNamespacedName_FirstOccurrenceWins(t *testing.T) {
	metrics := []UnifiedPVCMetric{
		{Namespace: "ns1", Pod: "pod-a", PVCName: "shared-pvc", UsedBytes: 111, CapacityBytes: 1000, AvailableBytes: 889},
		{Namespace: "ns1", Pod: "pod-b", PVCName: "shared-pvc", UsedBytes: 222, CapacityBytes: 1000, AvailableBytes: 778},
		{Namespace: "ns1", Pod: "pod-c", PVCName: "other-pvc", UsedBytes: 333, CapacityBytes: 500, AvailableBytes: 167},
	}

	index := indexPVCMetricsByNamespacedName(metrics)

	require.Len(t, index, 2)
	shared, ok := index[types.NamespacedName{Namespace: "ns1", Name: "shared-pvc"}]
	require.True(t, ok)
	require.Equal(t, "pod-a", shared.Pod, "expected the first row for a duplicate (namespace, pvcName) to win, matching the previous linear-scan first-match behavior")
	require.EqualValues(t, 111, shared.UsedBytes)

	other, ok := index[types.NamespacedName{Namespace: "ns1", Name: "other-pvc"}]
	require.True(t, ok)
	require.Equal(t, "pod-c", other.Pod)
}
