package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// TestNodemonClient_FetchContainerMetrics verifies JSON parsing of /v2/container/metrics.
func TestNodemonClient_FetchContainerMetrics(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	want := []UnifiedContainerMetric{
		{
			NodeName:               "node-1",
			Namespace:              "default",
			Pod:                    "web-abc",
			Container:              "nginx",
			Timestamp:              ts,
			CPUUsageNanoCores:      250000000,
			MemoryWorkingSet:       134217728,
			MemoryUsageBytes:       150000000,
			MemoryRSSBytes:         120000000,
			NetworkRxBytes:         1048576,
			NetworkTxBytes:         524288,
			NetworkRxPacketsPerSec: 100.5,
			NetworkTxPacketsPerSec: 80.2,
			NetworkRxErrorsPerSec:  0.0,
			NetworkTxErrorsPerSec:  0.0,
			NetworkRxDropsPerSec:   0.1,
			NetworkTxDropsPerSec:   0.0,
			DiskReadBytesPerSec:    4096.0,
			DiskWriteBytesPerSec:   8192.0,
			DiskReadOpsPerSec:      10.0,
			DiskWriteOpsPerSec:     20.0,
			CPUThrottleFraction:    0.05,
		},
		{
			NodeName:            "node-1",
			Namespace:           "ml",
			Pod:                 "trainer-xyz",
			Container:           "pytorch",
			Timestamp:           ts,
			CPUUsageNanoCores:   4000000000,
			MemoryWorkingSet:    8589934592,
			MemoryUsageBytes:    9000000000,
			MemoryRSSBytes:      8000000000,
			NetworkRxBytes:      10485760,
			NetworkTxBytes:      5242880,
			GPUUtilization:      85.5,
			GPUMemoryUsedMiB:    20480.0,
			GPUMemoryFreeMiB:    20480.0,
			GPUPowerWatts:       300.0,
			GPUTemperature:      72.0,
			CPUThrottleFraction: 0.0,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/container/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	client := newTestNodemonClient(t)
	got, err := client.fetchContainerMetrics(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("fetchContainerMetrics returned error: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d metrics, want %d", len(got), len(want))
	}

	// Verify first metric (non-GPU container)
	g0 := got[0]
	if g0.NodeName != "node-1" {
		t.Errorf("NodeName: got %q, want %q", g0.NodeName, "node-1")
	}
	if g0.Namespace != "default" {
		t.Errorf("Namespace: got %q, want %q", g0.Namespace, "default")
	}
	if g0.Pod != "web-abc" {
		t.Errorf("Pod: got %q, want %q", g0.Pod, "web-abc")
	}
	if g0.Container != "nginx" {
		t.Errorf("Container: got %q, want %q", g0.Container, "nginx")
	}
	if g0.CPUUsageNanoCores != 250000000 {
		t.Errorf("CPUUsageNanoCores: got %d, want %d", g0.CPUUsageNanoCores, 250000000)
	}
	if g0.MemoryWorkingSet != 134217728 {
		t.Errorf("MemoryWorkingSet: got %d, want %d", g0.MemoryWorkingSet, 134217728)
	}
	if g0.NetworkRxBytes != 1048576 {
		t.Errorf("NetworkRxBytes: got %d, want %d", g0.NetworkRxBytes, 1048576)
	}
	if g0.DiskReadBytesPerSec != 4096.0 {
		t.Errorf("DiskReadBytesPerSec: got %f, want %f", g0.DiskReadBytesPerSec, 4096.0)
	}
	if g0.CPUThrottleFraction != 0.05 {
		t.Errorf("CPUThrottleFraction: got %f, want %f", g0.CPUThrottleFraction, 0.05)
	}

	// Verify second metric (GPU container)
	g1 := got[1]
	if g1.GPUUtilization != 85.5 {
		t.Errorf("GPUUtilization: got %f, want %f", g1.GPUUtilization, 85.5)
	}
	if g1.GPUMemoryUsedMiB != 20480.0 {
		t.Errorf("GPUMemoryUsedMiB: got %f, want %f", g1.GPUMemoryUsedMiB, 20480.0)
	}
	if g1.GPUPowerWatts != 300.0 {
		t.Errorf("GPUPowerWatts: got %f, want %f", g1.GPUPowerWatts, 300.0)
	}
	if g1.GPUTemperature != 72.0 {
		t.Errorf("GPUTemperature: got %f, want %f", g1.GPUTemperature, 72.0)
	}
}

func TestNodemonClient_FetchContainerMetrics_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := newTestNodemonClient(t)
	got, err := client.fetchContainerMetrics(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
	if got != nil {
		t.Errorf("expected nil metrics on error, got %v", got)
	}
}

// TestNodemonClient_FetchNodeMetrics verifies JSON parsing of /node/metrics.
func TestNodemonClient_FetchNodeMetrics(t *testing.T) {
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	want := UnifiedNodeMetric{
		NodeName:             "node-2",
		Timestamp:            ts,
		NetworkRxBytesPerSec: 10485760.0,
		NetworkTxBytesPerSec: 5242880.0,
		DiskReadBytesPerSec:  204800.0,
		DiskWriteBytesPerSec: 409600.0,
		DiskReadOpsPerSec:    50.0,
		DiskWriteOpsPerSec:   100.0,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/node/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	client := newTestNodemonClient(t)
	got, err := client.fetchNodeMetrics(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("fetchNodeMetrics returned error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil metric, got nil")
	}

	if got.NodeName != "node-2" {
		t.Errorf("NodeName: got %q, want %q", got.NodeName, "node-2")
	}
	if got.NetworkRxBytesPerSec != 10485760.0 {
		t.Errorf("NetworkRxBytesPerSec: got %f, want %f", got.NetworkRxBytesPerSec, 10485760.0)
	}
	if got.NetworkTxBytesPerSec != 5242880.0 {
		t.Errorf("NetworkTxBytesPerSec: got %f, want %f", got.NetworkTxBytesPerSec, 5242880.0)
	}
	if got.DiskReadBytesPerSec != 204800.0 {
		t.Errorf("DiskReadBytesPerSec: got %f, want %f", got.DiskReadBytesPerSec, 204800.0)
	}
	if got.DiskWriteBytesPerSec != 409600.0 {
		t.Errorf("DiskWriteBytesPerSec: got %f, want %f", got.DiskWriteBytesPerSec, 409600.0)
	}
	if got.DiskReadOpsPerSec != 50.0 {
		t.Errorf("DiskReadOpsPerSec: got %f, want %f", got.DiskReadOpsPerSec, 50.0)
	}
	if got.DiskWriteOpsPerSec != 100.0 {
		t.Errorf("DiskWriteOpsPerSec: got %f, want %f", got.DiskWriteOpsPerSec, 100.0)
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("Timestamp: got %v, want %v", got.Timestamp, ts)
	}
}

func TestNodemonClient_FetchNodeMetrics_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	client := newTestNodemonClient(t)
	got, err := client.fetchNodeMetrics(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
	if got != nil {
		t.Errorf("expected nil metric on error, got %v", got)
	}
}

// TestNodemonClient_FetchPVCMetrics verifies JSON parsing of /pvc/metrics.
func TestNodemonClient_FetchPVCMetrics(t *testing.T) {
	want := []UnifiedPVCMetric{
		{
			Namespace:      "default",
			Pod:            "db-0",
			PVCName:        "data-db-0",
			UsedBytes:      10737418240,
			CapacityBytes:  107374182400,
			AvailableBytes: 96636764160,
		},
		{
			Namespace:      "monitoring",
			Pod:            "prometheus-0",
			PVCName:        "prometheus-data",
			UsedBytes:      53687091200,
			CapacityBytes:  107374182400,
			AvailableBytes: 53687091200,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pvc/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	client := newTestNodemonClient(t)
	got, err := client.fetchPVCMetrics(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("fetchPVCMetrics returned error: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d metrics, want %d", len(got), len(want))
	}

	// Verify first PVC
	g0 := got[0]
	if g0.Namespace != "default" {
		t.Errorf("Namespace: got %q, want %q", g0.Namespace, "default")
	}
	if g0.Pod != "db-0" {
		t.Errorf("Pod: got %q, want %q", g0.Pod, "db-0")
	}
	if g0.PVCName != "data-db-0" {
		t.Errorf("PVCName: got %q, want %q", g0.PVCName, "data-db-0")
	}
	if g0.UsedBytes != 10737418240 {
		t.Errorf("UsedBytes: got %d, want %d", g0.UsedBytes, 10737418240)
	}
	if g0.CapacityBytes != 107374182400 {
		t.Errorf("CapacityBytes: got %d, want %d", g0.CapacityBytes, 107374182400)
	}
	if g0.AvailableBytes != 96636764160 {
		t.Errorf("AvailableBytes: got %d, want %d", g0.AvailableBytes, 96636764160)
	}

	// Verify second PVC
	g1 := got[1]
	if g1.Namespace != "monitoring" {
		t.Errorf("Namespace: got %q, want %q", g1.Namespace, "monitoring")
	}
	if g1.PVCName != "prometheus-data" {
		t.Errorf("PVCName: got %q, want %q", g1.PVCName, "prometheus-data")
	}
	if g1.UsedBytes != 53687091200 {
		t.Errorf("UsedBytes: got %d, want %d", g1.UsedBytes, 53687091200)
	}
}

func TestNodemonClient_FetchPVCMetrics_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	client := newTestNodemonClient(t)
	got, err := client.fetchPVCMetrics(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("fetchPVCMetrics returned error for empty response: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 metrics, got %d", len(got))
	}
}

func TestNodemonClient_FetchPVCMetrics_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestNodemonClient(t)
	got, err := client.fetchPVCMetrics(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
	if got != nil {
		t.Errorf("expected nil metrics on error, got %v", got)
	}
}

// newTestNodemonClient returns a NodemonClient suitable for unit-testing the fetch* methods.
// The k8sClient and namespace are not used by the direct fetch* methods.
func newTestNodemonClient(t *testing.T) *NodemonClient {
	t.Helper()
	return &NodemonClient{
		port:       6061,
		httpClient: &http.Client{},
		log:        logr.Discard(),
	}
}
