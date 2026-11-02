package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

func TestCalculateNodeWorkloadResources_EphemeralStorage(t *testing.T) {
	c := &NodeCollector{
		nodeToPodsMap: map[string]map[string]*corev1.Pod{
			"node-a": {
				"default/web-0": {
					ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
					Spec: corev1.PodSpec{
						NodeName: "node-a",
						Containers: []corev1.Container{{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
								},
							},
						}},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
		},
	}

	got := c.calculateNodeWorkloadResources("node-a")

	if got["ephemeralStorageRequestsBytes"].(int64) != 1073741824 {
		t.Errorf("requests = %v, want 1073741824", got["ephemeralStorageRequestsBytes"])
	}
	if got["ephemeralStorageLimitsBytes"].(int64) != 2147483648 {
		t.Errorf("limits = %v, want 2147483648", got["ephemeralStorageLimitsBytes"])
	}
}

// A node with no pods must still return the keys, zeroed — callers copy the
// whole map into resourceData and a missing key would drop the column.
func TestCalculateNodeWorkloadResources_EphemeralStorageEmptyNode(t *testing.T) {
	c := &NodeCollector{nodeToPodsMap: map[string]map[string]*corev1.Pod{}}

	got := c.calculateNodeWorkloadResources("node-a")

	if got["ephemeralStorageRequestsBytes"].(int64) != 0 {
		t.Errorf("requests = %v, want 0", got["ephemeralStorageRequestsBytes"])
	}
	if got["ephemeralStorageLimitsBytes"].(int64) != 0 {
		t.Errorf("limits = %v, want 0", got["ephemeralStorageLimitsBytes"])
	}
}

func TestNodeStatusChanged_EphemeralStorage(t *testing.T) {
	node := func(capacity, allocatable string) *corev1.Node {
		return &corev1.Node{
			Status: corev1.NodeStatus{
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("4"),
					corev1.ResourceMemory:           resource.MustParse("16Gi"),
					corev1.ResourceEphemeralStorage: resource.MustParse(capacity),
				},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("4"),
					corev1.ResourceMemory:           resource.MustParse("16Gi"),
					corev1.ResourceEphemeralStorage: resource.MustParse(allocatable),
				},
			},
		}
	}

	c := &NodeCollector{}

	if c.nodeStatusChanged(node("100Gi", "90Gi"), node("100Gi", "90Gi")) {
		t.Error("identical nodes reported as changed")
	}
	if !c.nodeStatusChanged(node("100Gi", "90Gi"), node("200Gi", "90Gi")) {
		t.Error("ephemeral-storage capacity change not detected")
	}
	if !c.nodeStatusChanged(node("100Gi", "90Gi"), node("100Gi", "80Gi")) {
		t.Error("ephemeral-storage allocatable change not detected")
	}
}

func TestNodeStorageResourceData(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceEphemeralStorage: resource.MustParse("90Gi"),
			},
		},
	}
	metric := &UnifiedNodeMetric{
		NodeName:              "node-a",
		NodefsUsedBytes:       12792811520,
		NodefsCapacityBytes:   107294470144,
		NodefsAvailableBytes:  94501658624,
		NodefsInodesUsed:      168472,
		NodefsInodesTotal:     52422640,
		ImagefsUsedBytes:      6701690880,
		ImagefsCapacityBytes:  107294470144,
		ImagefsAvailableBytes: 94501658624,
		ImagefsSharedDevice:   true,
	}

	got := map[string]interface{}{}
	applyNodeStorageToResourceData(got, node, metric)

	want := map[string]interface{}{
		"nodefsUsedBytes":                  int64(12792811520),
		"nodefsCapacityBytes":              int64(107294470144),
		"nodefsAvailableBytes":             int64(94501658624),
		"nodefsInodesUsed":                 int64(168472),
		"nodefsInodesTotal":                int64(52422640),
		"imagefsUsedBytes":                 int64(6701690880),
		"imagefsCapacityBytes":             int64(107294470144),
		"imagefsAvailableBytes":            int64(94501658624),
		"imagefsSharedDevice":              true,
		"ephemeralStorageCapacityBytes":    int64(107374182400),
		"ephemeralStorageAllocatableBytes": int64(96636764160),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v (%T), want %v (%T)", k, got[k], got[k], v, v)
		}
	}
}

// A nil metric still yields the ephemeral-storage values, which come from the
// Node object and are available even when no metrics source reached the node.
func TestNodeStorageResourceData_NoMetric(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("100Gi")},
			Allocatable: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("90Gi")},
		},
	}

	got := map[string]interface{}{}
	applyNodeStorageToResourceData(got, node, nil)

	if got["ephemeralStorageCapacityBytes"].(int64) != 107374182400 {
		t.Errorf("capacity = %v, want 107374182400", got["ephemeralStorageCapacityBytes"])
	}
	if got["nodefsCapacityBytes"].(int64) != 0 {
		t.Errorf("nodefs capacity = %v, want 0", got["nodefsCapacityBytes"])
	}
}

func TestNodeStorageNeedsTopUp(t *testing.T) {
	tests := []struct {
		name        string
		fromNodemon bool
		metric      *UnifiedNodeMetric
		want        bool
	}{
		{"nodemon with storage needs nothing", true, &UnifiedNodeMetric{NodefsCapacityBytes: 1}, false},
		{"nodemon without storage needs top-up", true, &UnifiedNodeMetric{}, true},
		{"kubelet fallback already has storage", false, &UnifiedNodeMetric{}, false},
		{"no metric at all", true, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeStorageNeedsTopUp(tt.fromNodemon, tt.metric); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// u64ptr is a small helper for populating kubeletFsStats literals, whose
// fields are *uint64 so the kubelet Summary API can distinguish "reported
// zero" from "not reported".
func u64ptr(v uint64) *uint64 { return &v }

// TestNodeStorageTopUp_FiresExactlyOnceAndPopulatesPayload is the wired
// integration test for the top-up: it drives collectAllNodeResources end to
// end, rather than exercising nodeStorageNeedsTopUp and
// applyNodeStorageToResourceData in isolation as the tests above do. A
// regression that reorders the top-up relative to the fromNodemon capture, or
// past the isExcluded return, would still pass those two unit tests while
// silently breaking the feature — this test is what would catch it.
func TestNodeStorageTopUp_FiresExactlyOnceAndPopulatesPayload(t *testing.T) {
	node := testNode("storage-node")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	// nodemon reports CPU/memory but no filesystem section — the shape an
	// un-upgraded nodemon produces, which is what the top-up bridges.
	collectedAt := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeSnapshotPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := nodeSnapshotResponse{
			SchemaVersion: snapshotSchemaVersion,
			NodeMetrics: &UnifiedNodeMetric{
				NodeName:          node.Name,
				Timestamp:         collectedAt,
				CPUUsageNanoCores: 500_000_000,
				MemoryWorkingSet:  1024 * 1024 * 1024,
			},
			Sections: nodeSnapshotSections{
				Node: snapshotSectionStatus{State: snapshotStateReady, CollectedAt: &collectedAt},
				GPU:  snapshotSectionStatus{State: snapshotStateDisabled},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	nmClient := newNodemonClientForServer(t, server, node.Name)

	kubeletRT := &kubeletProxyRoundTripper{
		delayByNode: map[string]time.Duration{node.Name: 0},
		nodefsByNode: map[string]*kubeletFsStats{
			node.Name: {
				UsedBytes:      u64ptr(12792811520),
				CapacityBytes:  u64ptr(107294470144),
				AvailableBytes: u64ptr(94501658624),
				Inodes:         u64ptr(52422640),
				InodesUsed:     u64ptr(168472),
			},
		},
		imagefsByNode: map[string]*kubeletFsStats{
			node.Name: {
				UsedBytes:      u64ptr(6701690880),
				CapacityBytes:  u64ptr(107294470144),
				AvailableBytes: u64ptr(94501658624),
				Inodes:         u64ptr(52422640),
			},
		},
	}
	kubeletK8sClient, err := kubernetes.NewForConfigAndClient(
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
		batchChan:       make(chan CollectedResource, 10),
		config:          NodeCollectorConfig{DisableGPUMetrics: true},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: fakeLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}

	c.collectAllNodeResources(context.Background())

	// The call count is the point: it proves the top-up is conditional, not
	// an unconditional second fetch (exactly what issue 9410 removed).
	if got := kubeletRT.count(node.Name); got != 1 {
		t.Fatalf("kubelet storage top-up called %d times for %s, want exactly 1", got, node.Name)
	}

	// A recovered node is reported as topped-up, not as missing: it shipped with
	// complete storage and merely cost an extra API-server call.
	events := fakeLogger.reportsWithEventType("node_storage_topped_up")
	if len(events) != 1 {
		t.Fatalf("expected one node_storage_topped_up telemetry event, got %d", len(events))
	}
	if events[0].fields["storage_topped_up"] != "1" {
		t.Errorf("storage_topped_up = %q, want \"1\"", events[0].fields["storage_topped_up"])
	}
	if missing := fakeLogger.reportsWithEventType("node_storage_missing"); len(missing) != 0 {
		t.Errorf("expected no node_storage_missing telemetry for a recovered node, got %d", len(missing))
	}

	select {
	case res := <-c.batchChan:
		data := res.Object.(map[string]interface{})
		want := map[string]interface{}{
			"nodefsUsedBytes":       int64(12792811520),
			"nodefsCapacityBytes":   int64(107294470144),
			"nodefsAvailableBytes":  int64(94501658624),
			"nodefsInodesUsed":      int64(168472),
			"nodefsInodesTotal":     int64(52422640),
			"imagefsUsedBytes":      int64(6701690880),
			"imagefsCapacityBytes":  int64(107294470144),
			"imagefsAvailableBytes": int64(94501658624),
			"imagefsSharedDevice":   true,
		}
		for k, v := range want {
			if data[k] != v {
				t.Errorf("%s = %v (%T), want %v (%T)", k, data[k], data[k], v, v)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("expected a node resource to be emitted")
	}
}

// TestNodeStorageTopUp_SkippedWhenNodemonAlreadyHasStorage is the negative
// pairing for the test above: a nodemon-covered node whose snapshot already
// carries a nonzero NodefsCapacityBytes must make zero kubelet storage calls.
// Without this pairing, an unconditional top-up would still pass the positive
// test's assertions (a second write of the same values is invisible to it).
func TestNodeStorageTopUp_SkippedWhenNodemonAlreadyHasStorage(t *testing.T) {
	node := testNode("storage-node-2")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	collectedAt := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeSnapshotPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := nodeSnapshotResponse{
			SchemaVersion: snapshotSchemaVersion,
			NodeMetrics: &UnifiedNodeMetric{
				NodeName:             node.Name,
				Timestamp:            collectedAt,
				CPUUsageNanoCores:    500_000_000,
				MemoryWorkingSet:     1024 * 1024 * 1024,
				NodefsUsedBytes:      12792811520,
				NodefsCapacityBytes:  107294470144,
				NodefsAvailableBytes: 94501658624,
			},
			Sections: nodeSnapshotSections{
				Node: snapshotSectionStatus{State: snapshotStateReady, CollectedAt: &collectedAt},
				GPU:  snapshotSectionStatus{State: snapshotStateDisabled},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	nmClient := newNodemonClientForServer(t, server, node.Name)

	// Kubelet would succeed if called — delayByNode includes the node — so a
	// nonzero count here can only mean the top-up fired when it shouldn't have.
	kubeletRT := &kubeletProxyRoundTripper{delayByNode: map[string]time.Duration{node.Name: 0}}
	kubeletK8sClient, err := kubernetes.NewForConfigAndClient(
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
		batchChan:       make(chan CollectedResource, 10),
		config:          NodeCollectorConfig{DisableGPUMetrics: true},
		excludedNodes:   map[string]bool{},
		logger:          logr.Discard(),
		telemetryLogger: fakeLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}

	c.collectAllNodeResources(context.Background())

	if got := kubeletRT.count(node.Name); got != 0 {
		t.Fatalf("kubelet storage top-up called %d times for %s, want 0 (nodemon already had storage)", got, node.Name)
	}
	if events := fakeLogger.reportsWithEventType("node_storage_missing"); len(events) != 0 {
		t.Errorf("expected no node_storage_missing telemetry, got %d", len(events))
	}
}

// newStorageTopUpCollector wires a collector whose nodemon reports CPU/memory
// but no filesystem section -- the shape that triggers the kubelet top-up --
// against a kubelet fake the caller configures. Returns the collector, the
// kubelet round tripper (for call counts) and the telemetry sink.
func newStorageTopUpCollector(
	t *testing.T,
	node *corev1.Node,
	informer cache.SharedIndexInformer,
	kubeletRT *kubeletProxyRoundTripper,
	excluded map[string]bool,
) (*NodeCollector, *fakeTelemetryLogger) {
	t.Helper()

	collectedAt := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeSnapshotPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := nodeSnapshotResponse{
			SchemaVersion: snapshotSchemaVersion,
			NodeMetrics: &UnifiedNodeMetric{
				NodeName:          node.Name,
				Timestamp:         collectedAt,
				CPUUsageNanoCores: 500_000_000,
				MemoryWorkingSet:  1024 * 1024 * 1024,
			},
			Sections: nodeSnapshotSections{
				Node: snapshotSectionStatus{State: snapshotStateReady, CollectedAt: &collectedAt},
				GPU:  snapshotSectionStatus{State: snapshotStateDisabled},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)

	kubeletK8sClient, err := kubernetes.NewForConfigAndClient(
		&rest.Config{Host: "http://fake-apiserver", QPS: -1},
		&http.Client{Transport: kubeletRT},
	)
	require.NoError(t, err)

	fakeLogger := &fakeTelemetryLogger{}
	return &NodeCollector{
		metricsClient:   &metricsv1.Clientset{},
		nodemonClient:   newNodemonClientForServer(t, server, node.Name),
		kubeletClient:   NewKubeletSummaryClient(kubeletK8sClient, logr.Discard(), 0),
		nodeInformer:    informer,
		batchChan:       make(chan CollectedResource, 10),
		config:          NodeCollectorConfig{DisableGPUMetrics: true},
		excludedNodes:   excluded,
		logger:          logr.Discard(),
		telemetryLogger: fakeLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}, fakeLogger
}

// node_storage_missing has to mean what its name says: a node that shipped
// without filesystem data. A kubelet that answers the top-up but carries no
// filesystem section of its own is that case -- and it does NOT return an
// error, so classifying on the call's error would file it as a success.
func TestNodeStorageTopUp_MissingWhenKubeletHasNoFilesystemEither(t *testing.T) {
	node := testNode("storage-node-no-fs")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	// Node is present in delayByNode (so the request succeeds) but absent from
	// nodefsByNode/imagefsByNode, so the summary carries cpu/memory only.
	kubeletRT := &kubeletProxyRoundTripper{delayByNode: map[string]time.Duration{node.Name: 0}}
	c, fakeLogger := newStorageTopUpCollector(t, node, informer, kubeletRT, map[string]bool{})

	c.collectAllNodeResources(context.Background())

	if got := kubeletRT.count(node.Name); got != 1 {
		t.Fatalf("kubelet storage top-up called %d times, want exactly 1", got)
	}
	events := fakeLogger.reportsWithEventType("node_storage_missing")
	if len(events) != 1 {
		t.Fatalf("expected one node_storage_missing telemetry event, got %d", len(events))
	}
	if events[0].fields["storage_missing"] != "1" {
		t.Errorf("storage_missing = %q, want \"1\"", events[0].fields["storage_missing"])
	}
	if toppedUp := fakeLogger.reportsWithEventType("node_storage_topped_up"); len(toppedUp) != 0 {
		t.Errorf("expected no node_storage_topped_up telemetry, got %d", len(toppedUp))
	}
}

// A failed top-up counts as missing, since the node ships without storage.
func TestNodeStorageTopUp_MissingWhenKubeletCallFails(t *testing.T) {
	node := testNode("storage-node-kubelet-down")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	// No entry in delayByNode at all: the round tripper answers 404, so
	// FetchNodeStorageByNode returns an error.
	kubeletRT := &kubeletProxyRoundTripper{delayByNode: map[string]time.Duration{}}
	c, fakeLogger := newStorageTopUpCollector(t, node, informer, kubeletRT, map[string]bool{})

	c.collectAllNodeResources(context.Background())

	events := fakeLogger.reportsWithEventType("node_storage_missing")
	if len(events) != 1 {
		t.Fatalf("expected one node_storage_missing telemetry event, got %d", len(events))
	}
	if toppedUp := fakeLogger.reportsWithEventType("node_storage_topped_up"); len(toppedUp) != 0 {
		t.Errorf("expected no node_storage_topped_up telemetry, got %d", len(toppedUp))
	}
}

// An excluded node is never enriched and never sent, so it belongs in neither
// storage count -- and must not spend an API-server call on data that is
// immediately discarded.
func TestNodeStorageTopUp_ExcludedNodeIsNeitherFetchedNorCounted(t *testing.T) {
	node := testNode("storage-node-excluded")
	informer, stopCh := newSyncedNodeInformer(t, node)
	defer close(stopCh)

	// The kubelet would happily serve a full filesystem section if asked.
	kubeletRT := &kubeletProxyRoundTripper{
		delayByNode: map[string]time.Duration{node.Name: 0},
		nodefsByNode: map[string]*kubeletFsStats{
			node.Name: {
				UsedBytes:      u64ptr(12792811520),
				CapacityBytes:  u64ptr(107294470144),
				AvailableBytes: u64ptr(94501658624),
				Inodes:         u64ptr(52422640),
				InodesUsed:     u64ptr(168472),
			},
		},
	}
	c, fakeLogger := newStorageTopUpCollector(t, node, informer, kubeletRT,
		map[string]bool{node.Name: true})

	c.collectAllNodeResources(context.Background())

	if got := kubeletRT.count(node.Name); got != 0 {
		t.Errorf("kubelet storage top-up called %d times for an excluded node, want 0", got)
	}
	if events := fakeLogger.reportsWithEventType("node_storage_missing"); len(events) != 0 {
		t.Errorf("expected no node_storage_missing telemetry for an excluded node, got %d", len(events))
	}
	if events := fakeLogger.reportsWithEventType("node_storage_topped_up"); len(events) != 0 {
		t.Errorf("expected no node_storage_topped_up telemetry for an excluded node, got %d", len(events))
	}
}
