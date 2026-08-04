package nodemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeRuntimeQuerier is a test double for RuntimeMetricsQuerier.
type fakeRuntimeQuerier struct {
	metrics RuntimeMetrics
	err     error
}

func (f *fakeRuntimeQuerier) QueryRuntimeMetrics(_ context.Context) (RuntimeMetrics, error) {
	return f.metrics, f.err
}

func TestRuntimeCollector_SnapshotState(t *testing.T) {
	r := require.New(t)
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	collector := NewRuntimeCollector("node-1", nil, log)
	collector.procRoot = t.TempDir()

	metrics, status := collector.QueryRuntimeSnapshot()
	r.Empty(metrics.JVM)
	r.Empty(metrics.Runtimes)
	r.Equal(SnapshotStateNotReady, status.State)
	r.Nil(status.CollectedAt)

	var buildCalls int32
	collector.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		atomic.AddInt32(&buildCalls, 1)
		return nil, nil
	}
	collector.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return nil, cache, nil
	}
	collector.Collect(context.Background())

	metrics, status = collector.QueryRuntimeSnapshot()
	r.Empty(metrics.JVM)
	r.Empty(metrics.Runtimes)
	r.Equal(SnapshotStateReady, status.State)
	r.NotNil(status.CollectedAt)
	r.Equal(int32(1), atomic.LoadInt32(&buildCalls), "snapshot reads must not rebuild runtime metrics")

	xmsBytes := int64(512)
	xmxBytes := int64(1_024)
	maxRAMPercentage := 75.0
	useContainerSupport := true
	wantPopulated := RuntimeMetrics{
		JVM: []JVMMetric{{
			NodeName: "node-1",
			Pod:      "java-app",
			GCTimeSecondsTotal: map[string]float64{
				"G1 Young Generation": 1.5,
			},
			FlagsExtracted: JVMFlagsExtracted{
				XmsBytes:            &xmsBytes,
				XmxBytes:            &xmxBytes,
				MaxRamPercentage:    &maxRAMPercentage,
				UseContainerSupport: &useContainerSupport,
			},
		}},
		Runtimes: []RuntimeProcessMetric{{Runtime: "go", NodeName: "node-1", Pod: "go-app"}},
	}
	collector.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return wantPopulated.JVM, nil
	}
	collector.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return wantPopulated.Runtimes, cache, nil
	}
	collector.Collect(context.Background())

	metrics, status = collector.QueryRuntimeSnapshot()
	r.Equal(wantPopulated, metrics)
	r.Equal(SnapshotStateReady, status.State)
	r.NotNil(status.CollectedAt)

	metrics.JVM[0].Pod = "mutated"
	metrics.JVM[0].GCTimeSecondsTotal["G1 Young Generation"] = 99
	*metrics.JVM[0].FlagsExtracted.XmsBytes = 99
	*metrics.JVM[0].FlagsExtracted.XmxBytes = 99
	*metrics.JVM[0].FlagsExtracted.MaxRamPercentage = 99
	*metrics.JVM[0].FlagsExtracted.UseContainerSupport = false
	metrics.Runtimes[0].Pod = "mutated"
	metrics, _ = collector.QueryRuntimeSnapshot()
	r.Equal("java-app", metrics.JVM[0].Pod)
	r.Equal(float64(1.5), metrics.JVM[0].GCTimeSecondsTotal["G1 Young Generation"])
	r.Equal(int64(512), *metrics.JVM[0].FlagsExtracted.XmsBytes)
	r.Equal(int64(1_024), *metrics.JVM[0].FlagsExtracted.XmxBytes)
	r.Equal(75.0, *metrics.JVM[0].FlagsExtracted.MaxRamPercentage)
	r.True(*metrics.JVM[0].FlagsExtracted.UseContainerSupport)
	r.Equal("go-app", metrics.Runtimes[0].Pod, "snapshot reads must deep-copy runtime payloads")

	partial := RuntimeMetrics{
		Runtimes: []RuntimeProcessMetric{{Runtime: "python", NodeName: "node-1", Pod: "worker"}},
	}
	collector.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return nil, errors.New("jvm collection failed")
	}
	collector.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return partial.Runtimes, cache, nil
	}
	collector.Collect(context.Background())

	metrics, status = collector.QueryRuntimeSnapshot()
	r.Equal(partial, metrics)
	r.Equal(SnapshotStateReady, status.State)
	r.NotNil(status.CollectedAt)
	partialCollectedAt := *status.CollectedAt
	_, err := collector.QueryRuntimeMetrics(context.Background())
	r.ErrorContains(err, "jvm collection failed", "legacy query must still surface partial collection errors")

	collector.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return nil, errors.New("jvm collection failed")
	}
	collector.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return nil, cache, errors.New("runtime collection failed")
	}
	collector.Collect(context.Background())

	metrics, status = collector.QueryRuntimeSnapshot()
	r.Equal(partial, metrics)
	r.Equal(SnapshotStateStale, status.State)
	r.NotNil(status.CollectedAt)
	r.Equal(partialCollectedAt, *status.CollectedAt)

	unpublished := NewRuntimeCollector("node-1", nil, log)
	unpublished.procRoot = t.TempDir()
	unpublished.buildJVM = collector.buildJVM
	unpublished.buildRuntime = collector.buildRuntime
	unpublished.Collect(context.Background())

	metrics, status = unpublished.QueryRuntimeSnapshot()
	r.Empty(metrics.JVM)
	r.Empty(metrics.Runtimes)
	r.Equal(SnapshotStateNotReady, status.State)
	r.Nil(status.CollectedAt)
}

// TestRuntimeMetricsHandler_CompactJSON asserts the response body is compact
// (no indentation) — the zxporter collector parses this programmatically, so
// pretty-printing only costs CPU and bytes on every request.
func TestRuntimeMetricsHandler_CompactJSON(t *testing.T) {
	r := require.New(t)
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	metrics := RuntimeMetrics{
		JVM: []JVMMetric{
			{NodeName: "node-1", Pod: "app", Namespace: "default", Container: "main"},
		},
		Runtimes: []RuntimeProcessMetric{
			{Runtime: "python", NodeName: "node-1", Pod: "worker", Namespace: "default", Container: "main"},
		},
	}
	handler := NewRuntimeMetricsHandler(&fakeRuntimeQuerier{metrics: metrics}, log, 0)

	req := httptest.NewRequest(http.MethodGet, "/container/runtime-metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)

	wantBody, err := json.Marshal(metrics)
	r.NoError(err)
	wantBody = append(wantBody, '\n')

	r.Equal(string(wantBody), rec.Body.String())
}
