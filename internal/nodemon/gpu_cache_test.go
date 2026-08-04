package nodemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// mutatedMarker is the sentinel written into a returned snapshot slice to prove
// snapshot reads hand back copies, not the cache's backing arrays.
const mutatedMarker = "mutated"

// countingQuerier is a MetricsQuerier double that records how many times the
// underlying (expensive) scrape would run.
type countingQuerier struct {
	mu      sync.Mutex
	calls   int32
	metrics []nodemon.GPUMetric
	err     error
}

func (q *countingQuerier) QueryMetrics(_ context.Context) ([]nodemon.GPUMetric, error) {
	atomic.AddInt32(&q.calls, 1)
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.metrics, q.err
}

func (q *countingQuerier) set(metrics []nodemon.GPUMetric, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.metrics, q.err = metrics, err
}

func testLogger() logr.Logger {
	zapLog, _ := zap.NewDevelopment()
	return zapr.NewLogger(zapLog)
}

func TestCachedGPUExporter_ReadsDoNotScrape(t *testing.T) {
	r := require.New(t)
	src := &countingQuerier{metrics: []nodemon.GPUMetric{{Pod: "train", GPUUtilization: 90}}}

	cache := nodemon.NewCachedGPUExporter(src, 0, testLogger())

	// One refresh (as the ticker would do) then many concurrent reads.
	cache.Refresh(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cache.QueryMetrics(context.Background())
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "train", got[0].Pod)
		}()
	}
	wg.Wait()

	// The whole point: 50 reads triggered zero extra scrapes.
	r.Equal(int32(1), atomic.LoadInt32(&src.calls),
		"reads must serve the cached snapshot, not re-scrape DCGM")
}

func TestCachedGPUExporter_RetainsSnapshotOnRefreshError(t *testing.T) {
	r := require.New(t)
	good := []nodemon.GPUMetric{{Pod: "train", GPUUtilization: 90}}
	src := &countingQuerier{metrics: good}

	cache := nodemon.NewCachedGPUExporter(src, 0, testLogger())
	cache.Refresh(context.Background())

	// Next refresh fails — the previous good snapshot must survive.
	src.set(nil, errors.New("dcgm unreachable"))
	cache.Refresh(context.Background())

	got, err := cache.QueryMetrics(context.Background())
	r.NoError(err)
	r.Len(got, 1)
	r.Equal("train", got[0].Pod)
}

func TestCachedGPUExporter_EmptyBeforeFirstRefresh(t *testing.T) {
	r := require.New(t)
	src := &countingQuerier{metrics: []nodemon.GPUMetric{{Pod: "train"}}}

	cache := nodemon.NewCachedGPUExporter(src, 0, testLogger())

	got, err := cache.QueryMetrics(context.Background())
	r.NoError(err)
	r.Nil(got)
	r.Equal(int32(0), atomic.LoadInt32(&src.calls), "no scrape until first refresh")
}

func TestCachedGPUExporter_ClampsInterval(t *testing.T) {
	// A sub-minimum interval must be clamped up so we never hammer DCGM faster
	// than it produces data. Verified indirectly: construction must not panic
	// and reads must still work with a tiny requested interval.
	r := require.New(t)
	src := &countingQuerier{}
	cache := nodemon.NewCachedGPUExporter(src, 1, testLogger())
	got, err := cache.QueryMetrics(context.Background())
	r.NoError(err)
	r.Nil(got)
}

func TestCachedGPUExporter_SnapshotState(t *testing.T) {
	r := require.New(t)
	src := &countingQuerier{}
	cache := nodemon.NewCachedGPUExporter(src, 0, testLogger())

	summary, status := cache.QueryGPUSnapshot()
	r.Nil(summary)
	r.Equal(nodemon.SnapshotStateNotReady, status.State)
	r.Nil(status.CollectedAt)
	r.Equal(int32(0), atomic.LoadInt32(&src.calls), "snapshot reads must not scrape DCGM")

	cache.Refresh(context.Background())

	summary, status = cache.QueryGPUSnapshot()
	r.Nil(summary)
	r.Equal(nodemon.SnapshotStateReady, status.State)
	r.NotNil(status.CollectedAt)

	metrics := []nodemon.GPUMetric{{
		ModelName:            "NVIDIA A100",
		DeviceUUID:           "GPU-a100-1",
		GPUUtilization:       40,
		FramebufferUsed:      10_000,
		FramebufferFree:      30_000,
		PowerUsage:           200,
		Temperature:          60,
		MemoryTemperature:    75,
		TensorActive:         0.60,
		DRAMActive:           0.30,
		PCIeTXBytes:          1_000_000,
		PCIeRXBytes:          2_000_000,
		GraphicsEngineActive: 0.70,
	}}
	want := &nodemon.NodeGPUSummary{
		GPUCount:                  1,
		GPUUtilizationAvg:         40,
		GPUUtilizationMax:         40,
		GPUMemoryUsedTotal:        10_000,
		GPUMemoryFreeTotal:        30_000,
		GPUMemoryTotalMb:          40_000,
		GPUPowerUsageTotal:        200,
		GPUTemperatureAvg:         60,
		GPUTemperatureMax:         60,
		GPUMemoryTemperatureAvg:   75,
		GPUMemoryTemperatureMax:   75,
		GPUTensorUtilizationAvg:   0.60,
		GPUDramUtilizationAvg:     0.30,
		GPUPCIeTxBytesTotal:       1_000_000,
		GPUPCIeRxBytesTotal:       2_000_000,
		GPUGraphicsUtilizationAvg: 0.70,
		GPUUsage:                  0.40,
		GPUModels:                 []string{"1x NVIDIA A100"},
		GPUUUIDs:                  []string{"GPU-a100-1"},
	}
	src.set(metrics, nil)
	cache.Refresh(context.Background())

	summary, status = cache.QueryGPUSnapshot()
	r.Equal(want, summary)
	r.Equal(nodemon.SnapshotStateReady, status.State)
	r.NotNil(status.CollectedAt)
	successfulAt := *status.CollectedAt

	summary.GPUModels[0] = mutatedMarker
	summary.GPUUUIDs[0] = mutatedMarker
	summary, _ = cache.QueryGPUSnapshot()
	r.Equal(want, summary, "snapshot reads must copy mutable summary slices")

	// A single failed refresh keeps the last-good snapshot young (well within the
	// grace window), so it stays ready rather than flipping to stale — that is
	// the whole point of the grace window. The aged-past-threshold -> stale
	// transition is covered deterministically in gpu_cache_internal_test.go.
	src.set(nil, errors.New("dcgm unavailable"))
	cache.Refresh(context.Background())

	summary, status = cache.QueryGPUSnapshot()
	r.Equal(want, summary)
	r.Equal(nodemon.SnapshotStateReady, status.State)
	r.NotNil(status.CollectedAt)
	r.Equal(successfulAt, *status.CollectedAt)
	r.Equal(int32(3), atomic.LoadInt32(&src.calls), "snapshot reads must not add source queries")
}

func TestSummarizeNodeGPUParity(t *testing.T) {
	tests := []struct {
		name    string
		metrics []nodemon.GPUMetric
	}{
		{
			name: "empty input",
		},
		{
			name: "one GPU",
			metrics: []nodemon.GPUMetric{
				{
					ModelName: "NVIDIA T4", DeviceUUID: "GPU-t4", GPUUtilization: 30,
					FramebufferUsed: 2_000, FramebufferFree: 13_095, PowerUsage: 14.5,
					Temperature: 38, MemoryTemperature: 48, TensorActive: 0.25,
					DRAMActive: 0.40, PCIeTXBytes: 1_000, PCIeRXBytes: 2_000,
					GraphicsEngineActive: 0.35,
				},
			},
		},
		{
			name: "multiple GPUs",
			metrics: []nodemon.GPUMetric{
				{
					ModelName: "NVIDIA A100", DeviceUUID: "GPU-a100-1", GPUUtilization: 40,
					FramebufferUsed: 10_000, FramebufferFree: 30_000, PowerUsage: 200,
					Temperature: 60, MemoryTemperature: 75, TensorActive: 0.60,
					DRAMActive: 0.30, PCIeTXBytes: 1_000_000, PCIeRXBytes: 2_000_000,
					GraphicsEngineActive: 0.70,
				},
				{
					ModelName: "NVIDIA A100", DeviceUUID: "GPU-a100-2", GPUUtilization: 80,
					FramebufferUsed: 20_000, FramebufferFree: 20_000, PowerUsage: 300,
					Temperature: 75, MemoryTemperature: 90, TensorActive: 0.80,
					DRAMActive: 0.50, PCIeTXBytes: 3_000_000, PCIeRXBytes: 4_000_000,
					GraphicsEngineActive: 0.30,
				},
			},
		},
		{
			name: "mixed models",
			metrics: []nodemon.GPUMetric{
				{
					ModelName: "NVIDIA A100", DeviceUUID: "GPU-a100", GPUUtilization: 90,
					FramebufferUsed: 30_000, FramebufferFree: 10_000, PowerUsage: 350,
					Temperature: 78, MemoryTemperature: 92, TensorActive: 0.90,
					DRAMActive: 0.75, PCIeTXBytes: 5_000_000, PCIeRXBytes: 6_000_000,
					GraphicsEngineActive: 0.85,
				},
				{
					ModelName: "NVIDIA V100", DeviceUUID: "GPU-v100", GPUUtilization: 20,
					FramebufferUsed: 4_000, FramebufferFree: 12_000, PowerUsage: 125,
					Temperature: 52, MemoryTemperature: 64, TensorActive: 0.15,
					DRAMActive: 0.25, PCIeTXBytes: 7_000, PCIeRXBytes: 8_000,
					GraphicsEngineActive: 0.10,
				},
			},
		},
		{
			name: "MIG-labelled inputs",
			metrics: []nodemon.GPUMetric{
				{
					ModelName: "NVIDIA A100-SXM4-40GB", DeviceUUID: "MIG-GPU-1/1/0",
					MIGProfile: "1g.5gb", MIGInstanceID: "1", GPUUtilization: 55,
					FramebufferUsed: 3_000, FramebufferFree: 2_000, PowerUsage: 80,
					Temperature: 62, MemoryTemperature: 72, TensorActive: 0.45,
					DRAMActive: 0.35, PCIeTXBytes: 11_000, PCIeRXBytes: 12_000,
					GraphicsEngineActive: 0.50,
				},
				{
					ModelName: "NVIDIA A100-SXM4-40GB", DeviceUUID: "MIG-GPU-1/2/0",
					MIGProfile: "2g.10gb", MIGInstanceID: "2", GPUUtilization: 65,
					FramebufferUsed: 7_000, FramebufferFree: 3_000, PowerUsage: 110,
					Temperature: 67, MemoryTemperature: 79, TensorActive: 0.65,
					DRAMActive: 0.55, PCIeTXBytes: 13_000, PCIeRXBytes: 14_000,
					GraphicsEngineActive: 0.60,
				},
			},
		},
	}

	numericKeys := []string{
		"GPUCount",
		"GPUUtilizationAvg",
		"GPUUtilizationMax",
		"GPUMemoryUsedTotal",
		"GPUMemoryFreeTotal",
		"GPUMemoryTotalMb",
		"GPUPowerUsageTotal",
		"GPUTemperatureAvg",
		"GPUTemperatureMax",
		"GPUMemoryTemperatureAvg",
		"GPUMemoryTemperatureMax",
		"GPUTensorUtilizationAvg",
		"GPUDramUtilizationAvg",
		"GPUPCIeTxBytesTotal",
		"GPUPCIeRxBytesTotal",
		"GPUGraphicsUtilizationAvg",
		"GPUUsage",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typed := nodemon.SummarizeNodeGPU(tt.metrics)
			legacy := collector.NodeGPUMetricsFromNodemon(toCollectorMetrics(tt.metrics))

			if len(tt.metrics) == 0 {
				require.Nil(t, typed)
				require.Empty(t, legacy)
				return
			}

			downstream := summaryToDownstream(typed)
			require.Len(t, downstream, len(numericKeys)+2)
			require.Len(t, legacy, len(numericKeys)+2)

			for _, key := range numericKeys {
				require.Equal(t, legacy[key], downstream[key], key)
			}
			require.ElementsMatch(t, legacy["GPUModels"], downstream["GPUModels"], "GPUModels")
			require.ElementsMatch(t, legacy["GPUUUIDs"], downstream["GPUUUIDs"], "GPUUUIDs")
		})
	}
}

func TestSnapshotResponseJSON(t *testing.T) {
	type section struct {
		State string `json:"state"`
	}
	type contract struct {
		SchemaVersion int                `json:"schema_version"`
		Sections      map[string]section `json:"sections"`
	}

	tests := []struct {
		name         string
		response     any
		wantSections map[string]section
	}{
		{
			name: "node snapshot",
			response: nodemon.NodeSnapshotResponse{
				SchemaVersion: nodemon.SnapshotSchemaVersion,
				Sections: nodemon.NodeSnapshotSections{
					Node: nodemon.SnapshotSectionStatus{State: nodemon.SnapshotStateReady},
					GPU:  nodemon.SnapshotSectionStatus{State: nodemon.SnapshotStateStale},
				},
			},
			wantSections: map[string]section{
				"node": {State: "ready"},
				"gpu":  {State: "stale"},
			},
		},
		{
			name: "container snapshot",
			response: nodemon.ContainerSnapshotResponse{
				SchemaVersion: nodemon.SnapshotSchemaVersion,
				Sections: nodemon.ContainerSnapshotSections{
					Containers: nodemon.SnapshotSectionStatus{State: nodemon.SnapshotStateNotReady},
					Runtime:    nodemon.SnapshotSectionStatus{State: nodemon.SnapshotStateDisabled},
				},
			},
			wantSections: map[string]section{
				"containers": {State: "not_ready"},
				"runtime":    {State: "disabled"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.response)
			require.NoError(t, err)

			var got contract
			require.NoError(t, json.Unmarshal(data, &got))
			require.Equal(t, 1, got.SchemaVersion, `{"schema_version":1}`)
			require.Equal(t, tt.wantSections, got.Sections)
		})
	}
}

func toCollectorMetrics(metrics []nodemon.GPUMetric) []collector.NodemonMetric {
	result := make([]collector.NodemonMetric, 0, len(metrics))
	for _, metric := range metrics {
		result = append(result, collector.NodemonMetric{
			ModelName:            metric.ModelName,
			DeviceUUID:           metric.DeviceUUID,
			MIGProfile:           metric.MIGProfile,
			MIGInstanceID:        metric.MIGInstanceID,
			TensorActive:         metric.TensorActive,
			DRAMActive:           metric.DRAMActive,
			PCIeTXBytes:          metric.PCIeTXBytes,
			PCIeRXBytes:          metric.PCIeRXBytes,
			GraphicsEngineActive: metric.GraphicsEngineActive,
			FramebufferUsed:      metric.FramebufferUsed,
			FramebufferFree:      metric.FramebufferFree,
			Temperature:          metric.Temperature,
			MemoryTemperature:    metric.MemoryTemperature,
			PowerUsage:           metric.PowerUsage,
			GPUUtilization:       metric.GPUUtilization,
		})
	}
	return result
}

func summaryToDownstream(summary *nodemon.NodeGPUSummary) map[string]interface{} {
	if summary == nil {
		return map[string]interface{}{}
	}

	return map[string]interface{}{
		"GPUCount":                  summary.GPUCount,
		"GPUUtilizationAvg":         summary.GPUUtilizationAvg,
		"GPUUtilizationMax":         summary.GPUUtilizationMax,
		"GPUMemoryUsedTotal":        summary.GPUMemoryUsedTotal,
		"GPUMemoryFreeTotal":        summary.GPUMemoryFreeTotal,
		"GPUMemoryTotalMb":          summary.GPUMemoryTotalMb,
		"GPUPowerUsageTotal":        summary.GPUPowerUsageTotal,
		"GPUTemperatureAvg":         summary.GPUTemperatureAvg,
		"GPUTemperatureMax":         summary.GPUTemperatureMax,
		"GPUMemoryTemperatureAvg":   summary.GPUMemoryTemperatureAvg,
		"GPUMemoryTemperatureMax":   summary.GPUMemoryTemperatureMax,
		"GPUTensorUtilizationAvg":   summary.GPUTensorUtilizationAvg,
		"GPUDramUtilizationAvg":     summary.GPUDramUtilizationAvg,
		"GPUPCIeTxBytesTotal":       summary.GPUPCIeTxBytesTotal,
		"GPUPCIeRxBytesTotal":       summary.GPUPCIeRxBytesTotal,
		"GPUGraphicsUtilizationAvg": summary.GPUGraphicsUtilizationAvg,
		"GPUUsage":                  summary.GPUUsage,
		"GPUModels":                 summary.GPUModels,
		"GPUUUIDs":                  summary.GPUUUIDs,
	}
}
