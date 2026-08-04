package collector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestFetchAllContainerSnapshots_UsesOneBoundedCompositeWave(t *testing.T) {
	const nodeCount = 25
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	timestamp := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	wantContainer := UnifiedContainerMetric{
		NodeName: "node", Namespace: "ns", Pod: "pod", Container: "app", Timestamp: timestamp,
		CPUUsageNanoCores: 42, MemoryWorkingSet: 84, MemoryUsageBytes: 85, MemoryRSSBytes: 86,
		NetworkRxBytes: 87, NetworkTxBytes: 88, MemoryCacheBytes: 89, MemorySwapBytes: 90,
		NetworkRxPacketsPerSec: 1, NetworkTxPacketsPerSec: 2,
		NetworkRxErrorsPerSec: 3, NetworkTxErrorsPerSec: 4,
		NetworkRxDropsPerSec: 5, NetworkTxDropsPerSec: 6,
		DiskReadBytesPerSec: 7, DiskWriteBytesPerSec: 8,
		DiskReadOpsPerSec: 9, DiskWriteOpsPerSec: 10, CPUThrottleFraction: 0.25,
		CfsPeriods: 11, CfsThrottledPeriods: 12, CfsThrottledUsec: 13,
		MemoryEventsMax: 14, CPUPressureSomeUsec: 15,
		MemoryPressureSomeUsec: 16, MemoryPressureFullUsec: 17,
		GPUUtilization: 18, GPUMemoryUsedMiB: 19, GPUMemoryFreeMiB: 20,
		GPUPowerWatts: 21, GPUTemperature: 22,
	}
	wantJVM := NodemonJVMMetrics{
		NodeName: "node", Pod: "pod", Namespace: "ns", Container: "app", ContainerID: "cid",
		PidHost: 100, PidNS: 1, JavaCommand: "java -jar app.jar", JavaVersion: "26",
		HeapSizeBytes: 122, HeapUsedBytes: 123, HeapMaxSizeBytes: 124,
		FlagsExtracted: map[string]any{"xmx_bytes": float64(124)},
		FlagSources:    map[string]any{"xmx_bytes": "cmdline"},
		RawCmdline:     "-Xmx124", Timestamp: timestamp,
	}
	wantRuntime := NodemonRuntimeProcessMetrics{
		Runtime: "go", NodeName: "node", Pod: "pod", Namespace: "ns", Container: "app",
		ContainerID: "cid", PidHost: 200, PidNS: 2, Version: "go1.26",
		VersionSource: "buildinfo", RawCmdline: "./app", Timestamp: timestamp,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, containerSnapshotPath, r.URL.Path)
		calls.Add(1)
		now := active.Add(1)
		updateAtomicMax(&maxActive, now)
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		writeNodeSnapshotTestJSON(t, w, map[string]any{
			"schema_version":    1,
			"container_metrics": []UnifiedContainerMetric{wantContainer},
			"runtime_metrics": NodemonRuntimeMetrics{
				JVM:      []NodemonJVMMetrics{wantJVM},
				Runtimes: []NodemonRuntimeProcessMetrics{wantRuntime},
			},
			"sections": map[string]any{
				"containers": map[string]any{"state": "ready"},
				"runtime":    map[string]any{"state": "stale"},
			},
		})
	}))
	defer srv.Close()

	client := newContainerSnapshotTestClient(t, srv, srv.Client(), nodeCount)
	got, err := client.FetchAllContainerSnapshots(t.Context())
	require.NoError(t, err)
	require.Equal(t, nodeCount, got.CompositeCount)
	require.Zero(t, got.LegacyFallbackCount)
	require.Equal(t, int32(nodeCount), calls.Load())
	require.LessOrEqual(t, maxActive.Load(), int32(maxConcurrentNodemonFetches))
	require.Greater(t, maxActive.Load(), int32(1))
	require.Len(t, got.ContainerMetrics, nodeCount)
	require.Len(t, got.RuntimeMetrics.JVM, nodeCount)
	require.Len(t, got.RuntimeMetrics.Runtimes, nodeCount)
	require.Empty(t, got.FailedContainerNodes)
	require.Equal(t, wantContainer, got.ContainerMetrics[0])
	require.Equal(t, wantJVM, got.RuntimeMetrics.JVM[0])
	require.Equal(t, wantRuntime, got.RuntimeMetrics.Runtimes[0])
}

func TestFetchAllContainerSnapshots_HandlesSectionStatesIndependently(t *testing.T) {
	tests := []struct {
		name           string
		containerState string
		runtimeState   string
		wantContainers int
		wantRuntime    int
		wantFailedNode bool
	}{
		{
			name: "disabled runtime", containerState: "ready", runtimeState: "disabled",
			wantContainers: 1,
		},
		{
			name: "failed containers keep runtime", containerState: "not_ready", runtimeState: "ready",
			wantRuntime: 1, wantFailedNode: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeNodeSnapshotTestJSON(t, w, map[string]any{
					"schema_version":    1,
					"container_metrics": []UnifiedContainerMetric{{NodeName: "node-a"}},
					"runtime_metrics": NodemonRuntimeMetrics{
						Runtimes: []NodemonRuntimeProcessMetrics{{Runtime: "go"}},
					},
					"sections": map[string]any{
						"containers": map[string]any{"state": tt.containerState},
						"runtime":    map[string]any{"state": tt.runtimeState},
					},
				})
			}))
			defer srv.Close()

			got, err := newContainerSnapshotTestClient(t, srv, srv.Client(), 1).
				FetchAllContainerSnapshots(t.Context())
			require.NoError(t, err)
			require.Len(t, got.ContainerMetrics, tt.wantContainers)
			require.Len(t, got.RuntimeMetrics.Runtimes, tt.wantRuntime)
			_, failed := got.FailedContainerNodes["node-00"]
			require.Equal(t, tt.wantFailedNode, failed)
		})
	}
}

func TestFetchAllContainerSnapshots_FallsBackForCompatibilityFailures(t *testing.T) {
	tests := []struct {
		name      string
		composite func(http.ResponseWriter)
	}{
		{name: "not found", composite: func(w http.ResponseWriter) {
			http.Error(w, "missing", http.StatusNotFound)
		}},
		{name: "not ready", composite: func(w http.ResponseWriter) {
			http.Error(w, "warming", http.StatusServiceUnavailable)
		}},
		{name: "unsupported schema", composite: func(w http.ResponseWriter) {
			writeNodeSnapshotTestJSON(t, w, map[string]any{"schema_version": 2})
		}},
		{name: "malformed", composite: func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"schema_version":`)
		}},
		{name: "oversized", composite: func(w http.ResponseWriter) {
			_, _ = w.Write(make([]byte, containerSnapshotResponseLimit+1))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var legacyContainerCalls atomic.Int32
			var legacyRuntimeCalls atomic.Int32
			var active atomic.Int32
			var maxActive atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case containerSnapshotPath:
					tt.composite(w)
				case "/v2/container/metrics":
					legacyContainerCalls.Add(1)
					trackConcurrentLegacyRequest(&active, &maxActive)
					writeNodeSnapshotTestJSON(t, w, []UnifiedContainerMetric{{NodeName: "legacy"}})
				case "/container/runtime-metrics":
					legacyRuntimeCalls.Add(1)
					trackConcurrentLegacyRequest(&active, &maxActive)
					writeNodeSnapshotTestJSON(t, w, NodemonRuntimeMetrics{
						Runtimes: []NodemonRuntimeProcessMetrics{{Runtime: "go"}},
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			got, err := newContainerSnapshotTestClient(t, srv, srv.Client(), 1).
				FetchAllContainerSnapshots(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, got.CompositeCount)
			require.Equal(t, 1, got.LegacyFallbackCount)
			require.Len(t, got.ContainerMetrics, 1)
			require.Len(t, got.RuntimeMetrics.Runtimes, 1)
			require.Empty(t, got.FailedContainerNodes)
			require.Equal(t, int32(1), legacyContainerCalls.Load())
			require.Equal(t, int32(1), legacyRuntimeCalls.Load())
			require.Equal(t, int32(2), maxActive.Load())
		})
	}
}

func TestFetchAllContainerSnapshots_LegacyRuntime404IsDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case containerSnapshotPath:
			http.NotFound(w, r)
		case "/v2/container/metrics":
			writeNodeSnapshotTestJSON(t, w, []UnifiedContainerMetric{{NodeName: "legacy"}})
		case "/container/runtime-metrics":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := newContainerSnapshotTestClient(t, srv, srv.Client(), 1).
		FetchAllContainerSnapshots(t.Context())
	require.NoError(t, err)
	require.Len(t, got.ContainerMetrics, 1)
	require.Empty(t, got.RuntimeMetrics)
	require.Empty(t, got.FailedContainerNodes)
}

func TestFetchAllContainerSnapshots_DoesNotRetryTransportFailures(t *testing.T) {
	tests := []struct {
		name    string
		context func(t *testing.T) context.Context
		client  *http.Client
	}{
		{
			name: "connection failure",
			context: func(t *testing.T) context.Context {
				return t.Context()
			},
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			})},
		},
		{
			name: "canceled context",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return nil, r.Context().Err()
			})},
		},
		{
			name: "timeout",
			context: func(t *testing.T) context.Context {
				return t.Context()
			},
			client: &http.Client{
				Timeout: time.Millisecond,
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			transport := tt.client.Transport
			tt.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return transport.RoundTrip(r)
			})
			client := &NodemonClient{
				port:          6061,
				httpClient:    tt.client,
				log:           logr.Discard(),
				nodeToIP:      map[string]string{"node-a": "127.0.0.1"},
				lastRefreshed: time.Now(),
			}

			got, err := client.FetchAllContainerSnapshots(tt.context(t))
			require.NoError(t, err)
			require.Equal(t, int32(1), calls.Load())
			require.Contains(t, got.FailedContainerNodes, "node-a")
			require.Zero(t, got.LegacyFallbackCount)
		})
	}
}

func newContainerSnapshotTestClient(
	t *testing.T,
	srv *httptest.Server,
	httpClient *http.Client,
	nodeCount int,
) *NodemonClient {
	t.Helper()
	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	nodeToIP := make(map[string]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodeToIP["node-"+leftPadTwoDigits(i)] = parsed.Hostname()
	}
	return &NodemonClient{
		port:          port,
		httpClient:    httpClient,
		log:           logr.Discard(),
		nodeToIP:      nodeToIP,
		lastRefreshed: time.Now(),
	}
}

func leftPadTwoDigits(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

func updateAtomicMax(maxValue *atomic.Int32, value int32) {
	for {
		old := maxValue.Load()
		if value <= old || maxValue.CompareAndSwap(old, value) {
			return
		}
	}
}
