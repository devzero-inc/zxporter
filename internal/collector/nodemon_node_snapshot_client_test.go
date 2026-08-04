package collector

import (
	"context"
	"encoding/json"
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

func TestFetchNodeSnapshotByNode_UsesCompositeResponse(t *testing.T) {
	var compositeCalls atomic.Int32
	var legacyNodeCalls atomic.Int32
	var legacyGPUCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/node/snapshot":
			compositeCalls.Add(1)
			writeNodeSnapshotTestJSON(t, w, map[string]any{
				"schema_version": 1,
				"node_metrics": map[string]any{
					"node_name":                  "node-a",
					"timestamp":                  "2026-07-30T12:00:00Z",
					"cpu_usage_nanocores":        1_000_000_000,
					"memory_working_set_bytes":   4_294_967_296,
					"network_rx_bytes_per_sec":   11.0,
					"network_tx_bytes_per_sec":   12.0,
					"network_rx_packets_per_sec": 13.0,
					"network_tx_packets_per_sec": 14.0,
					"network_rx_errors_per_sec":  15.0,
					"network_tx_errors_per_sec":  16.0,
					"network_rx_drops_per_sec":   17.0,
					"network_tx_drops_per_sec":   18.0,
					"disk_read_bytes_per_sec":    19.0,
					"disk_write_bytes_per_sec":   20.0,
					"disk_read_ops_per_sec":      21.0,
					"disk_write_ops_per_sec":     22.0,
				},
				"gpu_summary": map[string]any{
					"gpu_count":                    2.0,
					"gpu_utilization_avg":          73.5,
					"gpu_utilization_max":          91.0,
					"gpu_memory_used_total":        32_768.0,
					"gpu_memory_free_total":        16_384.0,
					"gpu_memory_total_mb":          49_152.0,
					"gpu_power_usage_total":        500.0,
					"gpu_temperature_avg":          65.0,
					"gpu_temperature_max":          70.0,
					"gpu_memory_temperature_avg":   60.0,
					"gpu_memory_temperature_max":   64.0,
					"gpu_tensor_utilization_avg":   0.8,
					"gpu_dram_utilization_avg":     0.7,
					"gpu_pcie_tx_bytes_total":      100.0,
					"gpu_pcie_rx_bytes_total":      200.0,
					"gpu_graphics_utilization_avg": 0.6,
					"gpu_usage":                    1.47,
					"gpu_models":                   []string{"2x A100"},
					"gpu_uuids":                    []string{"GPU-1", "GPU-2"},
				},
				"sections": map[string]any{
					"node": map[string]any{"state": "ready", "collected_at": "2026-07-30T12:00:00Z"},
					// ready (not stale): the collector ingests GPU only when fresh,
					// and this case asserts the full summary is parsed and mapped.
					"gpu": map[string]any{"state": "ready", "collected_at": "2026-07-30T12:00:05Z"},
				},
			})
		case "/node/metrics":
			legacyNodeCalls.Add(1)
			http.Error(w, "unexpected legacy request", http.StatusInternalServerError)
		case "/container/metrics":
			legacyGPUCalls.Add(1)
			http.Error(w, "unexpected legacy request", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := newNodeSnapshotTestClient(t, srv, srv.Client())
	got, err := client.FetchNodeSnapshotByNode(t.Context(), "node-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.False(t, got.UsedLegacy)
	require.Empty(t, got.FallbackReason)
	require.Equal(t, int32(1), compositeCalls.Load())
	require.Zero(t, legacyNodeCalls.Load())
	require.Zero(t, legacyGPUCalls.Load())

	require.Equal(t, &UnifiedNodeMetric{
		NodeName:               "node-a",
		Timestamp:              time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC),
		CPUUsageNanoCores:      1_000_000_000,
		MemoryWorkingSet:       4_294_967_296,
		NetworkRxBytesPerSec:   11,
		NetworkTxBytesPerSec:   12,
		NetworkRxPacketsPerSec: 13,
		NetworkTxPacketsPerSec: 14,
		NetworkRxErrorsPerSec:  15,
		NetworkTxErrorsPerSec:  16,
		NetworkRxDropsPerSec:   17,
		NetworkTxDropsPerSec:   18,
		DiskReadBytesPerSec:    19,
		DiskWriteBytesPerSec:   20,
		DiskReadOpsPerSec:      21,
		DiskWriteOpsPerSec:     22,
	}, got.NodeMetric)
	require.Equal(t, map[string]interface{}{
		"GPUCount":                  2.0,
		"GPUUtilizationAvg":         73.5,
		"GPUUtilizationMax":         91.0,
		"GPUMemoryUsedTotal":        32_768.0,
		"GPUMemoryFreeTotal":        16_384.0,
		"GPUMemoryTotalMb":          49_152.0,
		"GPUPowerUsageTotal":        500.0,
		"GPUTemperatureAvg":         65.0,
		"GPUTemperatureMax":         70.0,
		"GPUMemoryTemperatureAvg":   60.0,
		"GPUMemoryTemperatureMax":   64.0,
		"GPUTensorUtilizationAvg":   0.8,
		"GPUDramUtilizationAvg":     0.7,
		"GPUPCIeTxBytesTotal":       100.0,
		"GPUPCIeRxBytesTotal":       200.0,
		"GPUGraphicsUtilizationAvg": 0.6,
		"GPUUsage":                  1.47,
		"GPUModels":                 []string{"2x A100"},
		"GPUUUIDs":                  []string{"GPU-1", "GPU-2"},
	}, got.GPUMetrics)
}

func TestFetchNodeSnapshotByNode_AcceptsReadyEmptyGPU(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeNodeSnapshotTestJSON(t, w, map[string]any{
			"schema_version": 1,
			"node_metrics":   map[string]any{"node_name": "node-a"},
			"sections": map[string]any{
				"node": map[string]any{"state": "ready"},
				"gpu":  map[string]any{"state": "ready"},
			},
		})
	}))
	defer srv.Close()

	got, err := newNodeSnapshotTestClient(t, srv, srv.Client()).
		FetchNodeSnapshotByNode(t.Context(), "node-a")
	require.NoError(t, err)
	require.NotNil(t, got.NodeMetric)
	require.Empty(t, got.GPUMetrics)
	require.False(t, got.UsedLegacy)
}

func TestFetchNodeSnapshotByNode_PreservesUsableSectionsIndependently(t *testing.T) {
	tests := []struct {
		name         string
		nodeState    string
		gpuState     string
		wantNode     bool
		wantGPUCount bool
	}{
		{name: "node usable", nodeState: "stale", gpuState: "not_ready", wantNode: true},
		{name: "gpu usable", nodeState: "not_ready", gpuState: "ready", wantGPUCount: true},
		// A stale GPU section is dropped, not ingested: it can be of unbounded
		// age during a DCGM outage, so we emit no GPU rather than stale values.
		{name: "gpu stale dropped", nodeState: "ready", gpuState: "stale", wantNode: true, wantGPUCount: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeNodeSnapshotTestJSON(t, w, map[string]any{
					"schema_version": 1,
					"node_metrics":   map[string]any{"node_name": "node-a"},
					"gpu_summary":    map[string]any{"gpu_count": 1.0},
					"sections": map[string]any{
						"node": map[string]any{"state": tt.nodeState},
						"gpu":  map[string]any{"state": tt.gpuState},
					},
				})
			}))
			defer srv.Close()

			got, err := newNodeSnapshotTestClient(t, srv, srv.Client()).
				FetchNodeSnapshotByNode(t.Context(), "node-a")
			require.NoError(t, err)
			require.Equal(t, tt.wantNode, got.NodeMetric != nil)
			_, hasGPUCount := got.GPUMetrics["GPUCount"]
			require.Equal(t, tt.wantGPUCount, hasGPUCount)
		})
	}
}

func TestFetchNodeSnapshotByNode_FallsBackForCompatibilityFailures(t *testing.T) {
	tests := []struct {
		name      string
		composite func(http.ResponseWriter)
		reason    string
	}{
		{name: "not found", reason: "not_found", composite: func(w http.ResponseWriter) {
			http.Error(w, "missing", http.StatusNotFound)
		}},
		{name: "not ready", reason: "not_ready", composite: func(w http.ResponseWriter) {
			http.Error(w, "warming", http.StatusServiceUnavailable)
		}},
		{name: "unsupported schema", reason: "unsupported_schema", composite: func(w http.ResponseWriter) {
			writeNodeSnapshotTestJSON(t, w, map[string]any{"schema_version": 2})
		}},
		{name: "malformed", reason: "malformed", composite: func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"schema_version":`)
		}},
		{name: "trailing JSON", reason: "malformed", composite: func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"schema_version":1,"sections":{"node":{"state":"ready"},"gpu":{"state":"ready"}}} {}`)
		}},
		{name: "oversized", reason: "oversized", composite: func(w http.ResponseWriter) {
			_, _ = w.Write(make([]byte, nodeSnapshotResponseLimit+1))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var legacyNodeCalls atomic.Int32
			var legacyGPUCalls atomic.Int32
			var active atomic.Int32
			var maxActive atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/node/snapshot":
					tt.composite(w)
				case "/node/metrics":
					legacyNodeCalls.Add(1)
					trackConcurrentLegacyRequest(&active, &maxActive)
					writeNodeSnapshotTestJSON(t, w, UnifiedNodeMetric{NodeName: "legacy-node"})
				case "/container/metrics":
					legacyGPUCalls.Add(1)
					trackConcurrentLegacyRequest(&active, &maxActive)
					writeNodeSnapshotTestJSON(t, w, []NodemonMetric{{GPUUtilization: 50}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			got, err := newNodeSnapshotTestClient(t, srv, srv.Client()).
				FetchNodeSnapshotByNode(t.Context(), "node-a")
			require.NoError(t, err)
			require.True(t, got.UsedLegacy)
			require.Equal(t, tt.reason, got.FallbackReason)
			require.Equal(t, "legacy-node", got.NodeMetric.NodeName)
			require.Equal(t, 0.5, got.GPUMetrics["GPUUsage"])
			require.Equal(t, int32(1), legacyNodeCalls.Load())
			require.Equal(t, int32(1), legacyGPUCalls.Load())
			require.Equal(t, int32(2), maxActive.Load(), "legacy requests must overlap")
		})
	}
}

func TestFetchNodeSnapshotByNode_DoesNotRetryTransportFailures(t *testing.T) {
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
			name: "request timeout",
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

			got, err := client.FetchNodeSnapshotByNode(tt.context(t), "node-a")
			require.Error(t, err)
			require.Nil(t, got)
			require.Equal(t, int32(1), calls.Load(), "transport failures must not retry legacy routes")
		})
	}
}

func newNodeSnapshotTestClient(t *testing.T, srv *httptest.Server, httpClient *http.Client) *NodemonClient {
	t.Helper()
	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	return &NodemonClient{
		port:          port,
		httpClient:    httpClient,
		log:           logr.Discard(),
		nodeToIP:      map[string]string{"node-a": parsed.Hostname()},
		lastRefreshed: time.Now(),
	}
}

func writeNodeSnapshotTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(value))
}

func trackConcurrentLegacyRequest(active, maxActive *atomic.Int32) {
	now := active.Add(1)
	for {
		old := maxActive.Load()
		if now <= old || maxActive.CompareAndSwap(old, now) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	active.Add(-1)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
