package nodemon

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

type fakeNodeSnapshotQuerier struct {
	metrics *NodeMetricsResponse
	status  SnapshotSectionStatus
	calls   int
}

func (f *fakeNodeSnapshotQuerier) QueryNodeSnapshot() (*NodeMetricsResponse, SnapshotSectionStatus) {
	f.calls++
	return f.metrics, f.status
}

type fakeGPUSnapshotQuerier struct {
	summary *NodeGPUSummary
	status  SnapshotSectionStatus
	calls   int
}

func (f *fakeGPUSnapshotQuerier) QueryGPUSnapshot() (*NodeGPUSummary, SnapshotSectionStatus) {
	f.calls++
	return f.summary, f.status
}

type fakeContainerSnapshotQuerier struct {
	metrics []ContainerMetricsResponse
	status  SnapshotSectionStatus
	calls   int
}

func (f *fakeContainerSnapshotQuerier) QueryContainerSnapshot() ([]ContainerMetricsResponse, SnapshotSectionStatus) {
	f.calls++
	return f.metrics, f.status
}

type fakeRuntimeSnapshotQuerier struct {
	metrics RuntimeMetrics
	status  SnapshotSectionStatus
	calls   int
}

func (f *fakeRuntimeSnapshotQuerier) QueryRuntimeSnapshot() (RuntimeMetrics, SnapshotSectionStatus) {
	f.calls++
	return f.metrics, f.status
}

func TestNodeSnapshotHandler_CombinesCacheSections(t *testing.T) {
	nodeCollectedAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	gpuCollectedAt := nodeCollectedAt.Add(5 * time.Second)
	node := &fakeNodeSnapshotQuerier{
		metrics: &NodeMetricsResponse{NodeName: "node-a", CPUUsageNanoCores: 1_000_000_000},
		status:  SnapshotSectionStatus{State: SnapshotStateReady, CollectedAt: &nodeCollectedAt},
	}
	gpu := &fakeGPUSnapshotQuerier{
		summary: &NodeGPUSummary{GPUCount: 2, GPUUtilizationAvg: 73.5},
		status:  SnapshotSectionStatus{State: SnapshotStateReady, CollectedAt: &gpuCollectedAt},
	}

	rec := serveSnapshotRequest(t, NewNodeSnapshotHandler(node, gpu, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var got NodeSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, SnapshotSchemaVersion, got.SchemaVersion)
	require.Equal(t, node.metrics, got.NodeMetrics)
	require.Equal(t, gpu.summary, got.GPUSummary)
	require.Equal(t, node.status, got.Sections.Node)
	require.Equal(t, gpu.status, got.Sections.GPU)
	require.Equal(t, 1, node.calls)
	require.Equal(t, 1, gpu.calls)
}

func TestNodeSnapshotHandler_ReadySectionPreservesNotReadyPeer(t *testing.T) {
	collectedAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	node := &fakeNodeSnapshotQuerier{
		metrics: &NodeMetricsResponse{NodeName: "node-a"},
		status:  SnapshotSectionStatus{State: SnapshotStateReady, CollectedAt: &collectedAt},
	}
	gpu := &fakeGPUSnapshotQuerier{
		summary: &NodeGPUSummary{GPUCount: 99},
		status:  SnapshotSectionStatus{State: SnapshotStateNotReady},
	}

	rec := serveSnapshotRequest(t, NewNodeSnapshotHandler(node, gpu, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusOK, rec.Code)
	var got NodeSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, node.metrics, got.NodeMetrics)
	require.Nil(t, got.GPUSummary)
	require.Equal(t, SnapshotStateNotReady, got.Sections.GPU.State)
	require.NotContains(t, rec.Body.String(), `"gpu_summary"`)
	require.Equal(t, 1, node.calls)
	require.Equal(t, 1, gpu.calls)
}

func TestNodeSnapshotHandler_IncludesStaleDataAndTimestamp(t *testing.T) {
	collectedAt := time.Date(2026, time.July, 30, 11, 58, 0, 0, time.UTC)
	node := &fakeNodeSnapshotQuerier{
		metrics: &NodeMetricsResponse{NodeName: "unusable"},
		status:  SnapshotSectionStatus{State: SnapshotStateNotReady},
	}
	gpu := &fakeGPUSnapshotQuerier{
		summary: &NodeGPUSummary{GPUCount: 1, GPUUtilizationMax: 91},
		status:  SnapshotSectionStatus{State: SnapshotStateStale, CollectedAt: &collectedAt},
	}

	rec := serveSnapshotRequest(t, NewNodeSnapshotHandler(node, gpu, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusOK, rec.Code)
	var got NodeSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Nil(t, got.NodeMetrics)
	require.Equal(t, gpu.summary, got.GPUSummary)
	require.Equal(t, SnapshotStateStale, got.Sections.GPU.State)
	require.Equal(t, &collectedAt, got.Sections.GPU.CollectedAt)
	require.Equal(t, 1, node.calls)
	require.Equal(t, 1, gpu.calls)
}

func TestNodeSnapshotHandler_AllSectionsNotReadyReturnsStatusEnvelope(t *testing.T) {
	node := &fakeNodeSnapshotQuerier{
		metrics: &NodeMetricsResponse{NodeName: "unusable"},
		status:  SnapshotSectionStatus{State: SnapshotStateNotReady},
	}
	gpu := &fakeGPUSnapshotQuerier{
		summary: &NodeGPUSummary{GPUCount: 99},
		status:  SnapshotSectionStatus{State: SnapshotStateNotReady},
	}

	rec := serveSnapshotRequest(t, NewNodeSnapshotHandler(node, gpu, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var got NodeSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, SnapshotSchemaVersion, got.SchemaVersion)
	require.Nil(t, got.NodeMetrics)
	require.Nil(t, got.GPUSummary)
	require.Equal(t, SnapshotStateNotReady, got.Sections.Node.State)
	require.Equal(t, SnapshotStateNotReady, got.Sections.GPU.State)
	require.Equal(t, 1, node.calls)
	require.Equal(t, 1, gpu.calls)
}

func TestNodeSnapshotHandler_RejectsNonGETWithoutQueryingCaches(t *testing.T) {
	node := &fakeNodeSnapshotQuerier{}
	gpu := &fakeGPUSnapshotQuerier{}

	rec := serveSnapshotRequest(t, NewNodeSnapshotHandler(node, gpu, logr.Discard()), http.MethodPost)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"error":"method not allowed"}`, rec.Body.String())
	require.Zero(t, node.calls)
	require.Zero(t, gpu.calls)
}

func TestNodeSnapshotHandler_DoesNotExposeEncodingErrors(t *testing.T) {
	node := &fakeNodeSnapshotQuerier{
		metrics: &NodeMetricsResponse{NodeName: "node-a", NetworkRxBytesPerSec: math.NaN()},
		status:  SnapshotSectionStatus{State: SnapshotStateReady},
	}
	gpu := &fakeGPUSnapshotQuerier{status: SnapshotSectionStatus{State: SnapshotStateNotReady}}

	rec := serveSnapshotRequest(t, NewNodeSnapshotHandler(node, gpu, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"error":"internal server error"}`, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "unsupported value")
	require.Equal(t, 1, node.calls)
	require.Equal(t, 1, gpu.calls)
}

func TestContainerSnapshotHandler_CombinesCacheSections(t *testing.T) {
	containerCollectedAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	runtimeCollectedAt := containerCollectedAt.Add(2 * time.Second)
	containers := &fakeContainerSnapshotQuerier{
		metrics: []ContainerMetricsResponse{{NodeName: "node-a", Pod: "web", Container: "app"}},
		status:  SnapshotSectionStatus{State: SnapshotStateReady, CollectedAt: &containerCollectedAt},
	}
	runtime := &fakeRuntimeSnapshotQuerier{
		metrics: RuntimeMetrics{
			JVM:      []JVMMetric{},
			Runtimes: []RuntimeProcessMetric{{Runtime: "go", Pod: "web"}},
		},
		status: SnapshotSectionStatus{State: SnapshotStateReady, CollectedAt: &runtimeCollectedAt},
	}

	rec := serveSnapshotRequest(t, NewContainerSnapshotHandler(containers, runtime, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var got ContainerSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, SnapshotSchemaVersion, got.SchemaVersion)
	require.Equal(t, containers.metrics, got.ContainerMetrics)
	require.Equal(t, runtime.metrics, got.RuntimeMetrics)
	require.Equal(t, containers.status, got.Sections.Containers)
	require.Equal(t, runtime.status, got.Sections.Runtime)
	require.Equal(t, 1, containers.calls)
	require.Equal(t, 1, runtime.calls)
}

func TestContainerSnapshotHandler_NilRuntimeIsDisabled(t *testing.T) {
	collectedAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	containers := &fakeContainerSnapshotQuerier{
		metrics: []ContainerMetricsResponse{{NodeName: "node-a", Pod: "web"}},
		status:  SnapshotSectionStatus{State: SnapshotStateReady, CollectedAt: &collectedAt},
	}

	rec := serveSnapshotRequest(t, NewContainerSnapshotHandler(containers, nil, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusOK, rec.Code)
	var got ContainerSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, containers.metrics, got.ContainerMetrics)
	require.Equal(t, SnapshotStateDisabled, got.Sections.Runtime.State)
	require.NotContains(t, rec.Body.String(), `"runtime_metrics"`)
	require.Equal(t, 1, containers.calls)
}

func TestContainerSnapshotHandler_NotReadyRuntimeDoesNotHideContainers(t *testing.T) {
	containers := &fakeContainerSnapshotQuerier{
		metrics: []ContainerMetricsResponse{{NodeName: "node-a", Pod: "web"}},
		status:  SnapshotSectionStatus{State: SnapshotStateReady},
	}
	runtime := &fakeRuntimeSnapshotQuerier{
		metrics: RuntimeMetrics{Runtimes: []RuntimeProcessMetric{{Runtime: "unusable"}}},
		status:  SnapshotSectionStatus{State: SnapshotStateNotReady},
	}

	rec := serveSnapshotRequest(t, NewContainerSnapshotHandler(containers, runtime, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusOK, rec.Code)
	var got ContainerSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, containers.metrics, got.ContainerMetrics)
	require.Empty(t, got.RuntimeMetrics)
	require.Equal(t, SnapshotStateNotReady, got.Sections.Runtime.State)
	require.NotContains(t, rec.Body.String(), `"runtime_metrics"`)
	require.Equal(t, 1, containers.calls)
	require.Equal(t, 1, runtime.calls)
}

func TestContainerSnapshotHandler_AllEnabledSectionsNotReadyReturns503(t *testing.T) {
	containers := &fakeContainerSnapshotQuerier{
		metrics: []ContainerMetricsResponse{{NodeName: "unusable"}},
		status:  SnapshotSectionStatus{State: SnapshotStateNotReady},
	}
	runtime := &fakeRuntimeSnapshotQuerier{
		metrics: RuntimeMetrics{Runtimes: []RuntimeProcessMetric{{Runtime: "unusable"}}},
		status:  SnapshotSectionStatus{State: SnapshotStateNotReady},
	}

	rec := serveSnapshotRequest(t, NewContainerSnapshotHandler(containers, runtime, logr.Discard()), http.MethodGet)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var got ContainerSnapshotResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, SnapshotSchemaVersion, got.SchemaVersion)
	require.Empty(t, got.ContainerMetrics)
	require.Empty(t, got.RuntimeMetrics)
	require.Equal(t, SnapshotStateNotReady, got.Sections.Containers.State)
	require.Equal(t, SnapshotStateNotReady, got.Sections.Runtime.State)
	require.NotContains(t, rec.Body.String(), `"container_metrics"`)
	require.NotContains(t, rec.Body.String(), `"runtime_metrics"`)
	require.Equal(t, 1, containers.calls)
	require.Equal(t, 1, runtime.calls)
}

func TestContainerSnapshotHandler_RejectsNonGETWithoutQueryingCaches(t *testing.T) {
	containers := &fakeContainerSnapshotQuerier{}
	runtime := &fakeRuntimeSnapshotQuerier{}

	rec := serveSnapshotRequest(t, NewContainerSnapshotHandler(containers, runtime, logr.Discard()), http.MethodPost)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"error":"method not allowed"}`, rec.Body.String())
	require.Zero(t, containers.calls)
	require.Zero(t, runtime.calls)
}

func serveSnapshotRequest(t *testing.T, handler http.Handler, method string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/snapshot", strings.NewReader(""))
	handler.ServeHTTP(rec, req)
	return rec
}
