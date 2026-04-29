package transport

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
)

// mockK8SServiceClient is a minimal mock for the K8SServiceClient interface
// that only implements GetWorkloadContainerPercentiles.
type mockK8SServiceClient struct {
	// resp is returned for every call; override per-test.
	resp *connect.Response[gen.GetWorkloadContainerPercentilesResponse]
	err  error
	// calls records the requests received.
	calls []*connect.Request[gen.GetWorkloadContainerPercentilesRequest]
}

func (m *mockK8SServiceClient) GetWorkloadContainerPercentiles(
	_ context.Context,
	req *connect.Request[gen.GetWorkloadContainerPercentilesRequest],
) (*connect.Response[gen.GetWorkloadContainerPercentilesResponse], error) {
	m.calls = append(m.calls, req)
	return m.resp, m.err
}

// --- Tests ---

// newTestFetcher constructs a DakrPercentileFetcher with the narrow
// percentileClient interface, allowing tests to use a lightweight mock.
func newTestFetcher(client percentileClient, logger logr.Logger) *DakrPercentileFetcher {
	return &DakrPercentileFetcher{
		client:        client,
		clientHeaders: NewClientHeaders("test-token"),
		logger:        logger.WithName("dakr-percentile-fetcher"),
	}
}

func TestFetchWorkloadPercentiles_EmptyWorkloads(t *testing.T) {
	logger := zapr.NewLogger(zaptest.NewLogger(t))

	fetcher := newTestFetcher(&mockK8SServiceClient{}, logger)

	results, err := fetcher.FetchWorkloadPercentiles(context.Background(), "cluster-1", nil)
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = fetcher.FetchWorkloadPercentiles(context.Background(), "cluster-1", []collector.HistoricalWorkloadQuery{})
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestFetchWorkloadPercentiles_MapsPercentiles(t *testing.T) {
	logger := zapr.NewLogger(zaptest.NewLogger(t))

	mock := &mockK8SServiceClient{
		resp: connect.NewResponse(&gen.GetWorkloadContainerPercentilesResponse{
			Containers: []*gen.ContainerPercentileSummary{
				{
					ContainerName: "app",
					CpuUsage: &gen.MetricPercentiles{
						P50: 0.250, // 250m
						P75: 0.500,
						P80: 0.600,
						P90: 0.800,
						P95: 0.900,
						P99: 0.950,
						Max: 1.000,
					},
					MemoryUsage: &gen.MetricPercentiles{
						P50: 100_000_000, // ~100MB
						P75: 200_000_000,
						P80: 250_000_000,
						P90: 300_000_000,
						P95: 350_000_000,
						P99: 400_000_000,
						Max: 500_000_000,
					},
				},
			},
		}),
	}

	fetcher := newTestFetcher(mock, logger)

	workloads := []collector.HistoricalWorkloadQuery{
		{
			Namespace:    "default",
			WorkloadName: "web-app",
			WorkloadKind: "Deployment",
		},
	}

	results, err := fetcher.FetchWorkloadPercentiles(context.Background(), "cluster-1", workloads)
	require.NoError(t, err)
	require.Len(t, results, 1)

	summary, ok := results["default/web-app/Deployment"]
	require.True(t, ok, "expected key default/web-app/Deployment in results")

	// Verify workload identifier.
	require.NotNil(t, summary.Workload)
	assert.Equal(t, "default", summary.Workload.Namespace)
	assert.Equal(t, "web-app", summary.Workload.Name)
	assert.Equal(t, "Deployment", summary.Workload.Kind)

	// Verify container mapping.
	require.Len(t, summary.Containers, 1)
	c := summary.Containers[0]
	assert.Equal(t, "app", c.ContainerName)

	// CPU: cores → millicores
	assert.Equal(t, int64(250), c.CpuP50)
	assert.Equal(t, int64(500), c.CpuP75)
	assert.Equal(t, int64(600), c.CpuP80)
	assert.Equal(t, int64(800), c.CpuP90)
	assert.Equal(t, int64(900), c.CpuP95)
	assert.Equal(t, int64(950), c.CpuP99)
	assert.Equal(t, int64(1000), c.CpuPmax)

	// Memory: bytes passthrough
	assert.Equal(t, int64(100_000_000), c.MemP50)
	assert.Equal(t, int64(200_000_000), c.MemP75)
	assert.Equal(t, int64(250_000_000), c.MemP80)
	assert.Equal(t, int64(300_000_000), c.MemP90)
	assert.Equal(t, int64(350_000_000), c.MemP95)
	assert.Equal(t, int64(400_000_000), c.MemP99)
	assert.Equal(t, int64(500_000_000), c.MemPmax)

	// Verify the RPC received correct parameters.
	require.Len(t, mock.calls, 1)
	assert.Equal(t, "cluster-1", mock.calls[0].Msg.ClusterId)
	assert.Equal(t, "Deployment", mock.calls[0].Msg.Kind)
}

func TestFetchWorkloadPercentiles_RPCError(t *testing.T) {
	logger := zapr.NewLogger(zaptest.NewLogger(t))

	mock := &mockK8SServiceClient{
		err: errors.New("connection refused"),
	}

	fetcher := newTestFetcher(mock, logger)

	workloads := []collector.HistoricalWorkloadQuery{
		{
			Namespace:    "prod",
			WorkloadName: "api-server",
			WorkloadKind: "Deployment",
		},
	}

	// The fetcher logs errors but does not propagate them for individual
	// workloads — it returns a partial result map instead.
	results, err := fetcher.FetchWorkloadPercentiles(context.Background(), "cluster-1", workloads)
	require.NoError(t, err)
	assert.Empty(t, results, "expected empty results when RPC fails")
}

func TestFetchWorkloadPercentiles_NilCPUAndMemory(t *testing.T) {
	logger := zapr.NewLogger(zaptest.NewLogger(t))

	mock := &mockK8SServiceClient{
		resp: connect.NewResponse(&gen.GetWorkloadContainerPercentilesResponse{
			Containers: []*gen.ContainerPercentileSummary{
				{
					ContainerName: "sidecar",
					// CpuUsage and MemoryUsage are nil
				},
			},
		}),
	}

	fetcher := newTestFetcher(mock, logger)

	workloads := []collector.HistoricalWorkloadQuery{
		{
			Namespace:    "default",
			WorkloadName: "app",
			WorkloadKind: "StatefulSet",
		},
	}

	results, err := fetcher.FetchWorkloadPercentiles(context.Background(), "cluster-1", workloads)
	require.NoError(t, err)
	require.Len(t, results, 1)

	summary := results["default/app/StatefulSet"]
	require.NotNil(t, summary)
	require.Len(t, summary.Containers, 1)

	c := summary.Containers[0]
	assert.Equal(t, "sidecar", c.ContainerName)
	// All values should be zero when usage is nil.
	assert.Equal(t, int64(0), c.CpuP50)
	assert.Equal(t, int64(0), c.MemP50)
}

func TestMapContainerPercentiles(t *testing.T) {
	summaries := []*gen.ContainerPercentileSummary{
		{
			ContainerName: "main",
			CpuUsage: &gen.MetricPercentiles{
				P50: 1.5,
				Max: 3.0,
			},
			MemoryUsage: &gen.MetricPercentiles{
				P50: 1024,
				Max: 2048,
			},
		},
		{
			ContainerName: "init",
			// No metrics
		},
	}

	result := mapContainerPercentiles(summaries)
	require.Len(t, result, 2)

	assert.Equal(t, "main", result[0].ContainerName)
	assert.Equal(t, int64(1500), result[0].CpuP50) // 1.5 cores = 1500m
	assert.Equal(t, int64(3000), result[0].CpuPmax)
	assert.Equal(t, int64(1024), result[0].MemP50)
	assert.Equal(t, int64(2048), result[0].MemPmax)

	assert.Equal(t, "init", result[1].ContainerName)
	assert.Equal(t, int64(0), result[1].CpuP50)
	assert.Equal(t, int64(0), result[1].MemP50)
}

func TestCpuToMillicores(t *testing.T) {
	tests := []struct {
		cores float64
		want  int64
	}{
		{0, 0},
		{0.001, 1},
		{0.250, 250},
		{1.0, 1000},
		{2.5, 2500},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, cpuToMillicores(tt.cores), "cores=%v", tt.cores)
	}
}
