package nodemon_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// mockUnifiedQuerier is a test double for UnifiedQuerier.
type mockUnifiedQuerier struct {
	containers []nodemon.ContainerMetricsResponse
	node       *nodemon.NodeMetricsResponse
	pvcs       []nodemon.PVCMetricsResponse
}

func (m *mockUnifiedQuerier) QueryContainerMetrics() []nodemon.ContainerMetricsResponse {
	return m.containers
}

func (m *mockUnifiedQuerier) QueryNodeMetrics() *nodemon.NodeMetricsResponse {
	return m.node
}

func (m *mockUnifiedQuerier) QueryPVCMetrics() []nodemon.PVCMetricsResponse {
	return m.pvcs
}

func sampleContainerMetrics() []nodemon.ContainerMetricsResponse {
	now := time.Date(2026, 2, 23, 12, 0, 0, 0, time.UTC)
	return []nodemon.ContainerMetricsResponse{
		{
			NodeName:            "node-1",
			Namespace:           "production",
			Pod:                 "web-pod-abc",
			Container:           "nginx",
			Timestamp:           now,
			CPUUsageNanoCores:   500_000_000,
			MemoryWorkingSet:    256 * 1024 * 1024,
			MemoryUsageBytes:    300 * 1024 * 1024,
			MemoryRSSBytes:      200 * 1024 * 1024,
			NetworkRxBytes:      1_000_000,
			NetworkTxBytes:      500_000,
			DiskReadBytesPerSec: 1024.0,
			CPUThrottleFraction: 0.05,
		},
		{
			NodeName:          "node-1",
			Namespace:         "staging",
			Pod:               "api-pod-xyz",
			Container:         "go-server",
			Timestamp:         now,
			CPUUsageNanoCores: 200_000_000,
			MemoryWorkingSet:  128 * 1024 * 1024,
		},
	}
}

func sampleNodeMetrics() *nodemon.NodeMetricsResponse {
	now := time.Date(2026, 2, 23, 12, 0, 0, 0, time.UTC)
	return &nodemon.NodeMetricsResponse{
		NodeName:             "node-1",
		Timestamp:            now,
		NetworkRxBytesPerSec: 10_000.0,
		NetworkTxBytesPerSec: 5_000.0,
		DiskReadBytesPerSec:  2048.0,
		DiskWriteBytesPerSec: 1024.0,
	}
}

func samplePVCMetrics() []nodemon.PVCMetricsResponse {
	return []nodemon.PVCMetricsResponse{
		{
			Namespace:      "production",
			Pod:            "web-pod-abc",
			PVCName:        "data-pvc",
			UsedBytes:      5 * 1024 * 1024 * 1024,
			CapacityBytes:  10 * 1024 * 1024 * 1024,
			AvailableBytes: 5 * 1024 * 1024 * 1024,
		},
		{
			Namespace:      "staging",
			Pod:            "api-pod-xyz",
			PVCName:        "logs-pvc",
			UsedBytes:      1 * 1024 * 1024 * 1024,
			CapacityBytes:  2 * 1024 * 1024 * 1024,
			AvailableBytes: 1 * 1024 * 1024 * 1024,
		},
	}
}

func newTestLogger() logr.Logger {
	zapLog, _ := zap.NewDevelopment()
	return zapr.NewLogger(zapLog)
}

// TestUnifiedContainerHandler_ReturnsJSON verifies the handler encodes ContainerMetricsResponse correctly.
func TestUnifiedContainerHandler_ReturnsJSON(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{containers: sampleContainerMetrics()}
	handler := nodemon.NewUnifiedContainerHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/v2/container/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)
	r.Equal("application/json", rec.Header().Get("Content-Type"))

	var result []nodemon.ContainerMetricsResponse
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Len(result, 2)

	first := result[0]
	r.Equal("node-1", first.NodeName)
	r.Equal("production", first.Namespace)
	r.Equal("web-pod-abc", first.Pod)
	r.Equal("nginx", first.Container)
	r.Equal(uint64(500_000_000), first.CPUUsageNanoCores)
	r.Equal(uint64(256*1024*1024), first.MemoryWorkingSet)
	r.InDelta(0.05, first.CPUThrottleFraction, 0.0001)
}

// TestUnifiedContainerHandler_FiltersNamespace verifies namespace query param filtering.
func TestUnifiedContainerHandler_FiltersNamespace(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{containers: sampleContainerMetrics()}
	handler := nodemon.NewUnifiedContainerHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/v2/container/metrics?namespace=production", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)

	var result []nodemon.ContainerMetricsResponse
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Len(result, 1)
	r.Equal("production", result[0].Namespace)
	r.Equal("nginx", result[0].Container)
}

// TestUnifiedContainerHandler_FiltersPod verifies pod query param filtering.
func TestUnifiedContainerHandler_FiltersPod(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{containers: sampleContainerMetrics()}
	handler := nodemon.NewUnifiedContainerHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/v2/container/metrics?pod=api-pod-xyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)

	var result []nodemon.ContainerMetricsResponse
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Len(result, 1)
	r.Equal("api-pod-xyz", result[0].Pod)
}

// TestUnifiedContainerHandler_FiltersContainer verifies container query param filtering.
func TestUnifiedContainerHandler_FiltersContainer(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{containers: sampleContainerMetrics()}
	handler := nodemon.NewUnifiedContainerHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/v2/container/metrics?container=nginx", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)

	var result []nodemon.ContainerMetricsResponse
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Len(result, 1)
	r.Equal("nginx", result[0].Container)
}

// TestUnifiedContainerHandler_MethodNotAllowed verifies POST returns HTTP 405.
func TestUnifiedContainerHandler_MethodNotAllowed(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{containers: sampleContainerMetrics()}
	handler := nodemon.NewUnifiedContainerHandler(q, log)

	req := httptest.NewRequest(http.MethodPost, "/v2/container/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusMethodNotAllowed, rec.Code)
}

// TestNodeMetricsHandler_ReturnsJSON verifies the node metrics handler encodes NodeMetricsResponse.
func TestNodeMetricsHandler_ReturnsJSON(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{node: sampleNodeMetrics()}
	handler := nodemon.NewNodeMetricsHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/node/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)
	r.Equal("application/json", rec.Header().Get("Content-Type"))

	var result nodemon.NodeMetricsResponse
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Equal("node-1", result.NodeName)
	r.InDelta(10_000.0, result.NetworkRxBytesPerSec, 0.001)
	r.InDelta(2048.0, result.DiskReadBytesPerSec, 0.001)
}

// TestNodeMetricsHandler_NotFound verifies 404 is returned when no node metrics are available.
func TestNodeMetricsHandler_NotFound(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{node: nil}
	handler := nodemon.NewNodeMetricsHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/node/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusNotFound, rec.Code)
}

// TestNodeMetricsHandler_MethodNotAllowed verifies POST returns HTTP 405.
func TestNodeMetricsHandler_MethodNotAllowed(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{node: sampleNodeMetrics()}
	handler := nodemon.NewNodeMetricsHandler(q, log)

	req := httptest.NewRequest(http.MethodPost, "/node/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusMethodNotAllowed, rec.Code)
}

// TestPVCMetricsHandler_ReturnsJSON verifies the PVC metrics handler encodes PVCMetricsResponse.
func TestPVCMetricsHandler_ReturnsJSON(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{pvcs: samplePVCMetrics()}
	handler := nodemon.NewPVCMetricsHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/pvc/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)
	r.Equal("application/json", rec.Header().Get("Content-Type"))

	var result []nodemon.PVCMetricsResponse
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Len(result, 2)

	first := result[0]
	r.Equal("production", first.Namespace)
	r.Equal("web-pod-abc", first.Pod)
	r.Equal("data-pvc", first.PVCName)
	r.Equal(uint64(5*1024*1024*1024), first.UsedBytes)
	r.Equal(uint64(10*1024*1024*1024), first.CapacityBytes)
}

// TestPVCMetricsHandler_FiltersNamespace verifies namespace filtering on PVC endpoint.
func TestPVCMetricsHandler_FiltersNamespace(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{pvcs: samplePVCMetrics()}
	handler := nodemon.NewPVCMetricsHandler(q, log)

	req := httptest.NewRequest(http.MethodGet, "/pvc/metrics?namespace=staging", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)

	var result []nodemon.PVCMetricsResponse
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Len(result, 1)
	r.Equal("staging", result[0].Namespace)
	r.Equal("logs-pvc", result[0].PVCName)
}

// TestPVCMetricsHandler_MethodNotAllowed verifies POST returns HTTP 405.
func TestPVCMetricsHandler_MethodNotAllowed(t *testing.T) {
	r := require.New(t)
	log := newTestLogger()

	q := &mockUnifiedQuerier{pvcs: samplePVCMetrics()}
	handler := nodemon.NewPVCMetricsHandler(q, log)

	req := httptest.NewRequest(http.MethodPost, "/pvc/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusMethodNotAllowed, rec.Code)
}
