// internal/transport/percentile_fetcher.go
package transport

import (
	"context"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	genconnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	"github.com/devzero-inc/zxporter/internal/collector"
)

const (
	// maxConcurrentPercentileRequests limits how many DAKR API calls run in parallel.
	maxConcurrentPercentileRequests = 10
)

// percentileClient is the subset of genconnect.K8SServiceClient that the
// fetcher actually needs. Using a narrow interface makes the code easier to
// test without stubbing dozens of unrelated RPCs.
type percentileClient interface {
	GetWorkloadContainerPercentiles(
		context.Context,
		*connect.Request[gen.GetWorkloadContainerPercentilesRequest],
	) (*connect.Response[gen.GetWorkloadContainerPercentilesResponse], error)
}

// Compile-time check: the generated K8SServiceClient satisfies our narrow
// interface.
var _ percentileClient = (genconnect.K8SServiceClient)(nil)

// DakrPercentileFetcher implements collector.PercentileFetcher using the DAKR
// control plane's GetWorkloadContainerPercentiles RPC.
type DakrPercentileFetcher struct {
	client        percentileClient
	clientHeaders *ClientHeaders
	logger        logr.Logger
}

// NewDakrPercentileFetcher creates a new fetcher that calls the DAKR control
// plane to retrieve pre-computed percentiles for workload containers.
func NewDakrPercentileFetcher(
	client genconnect.K8SServiceClient,
	headers *ClientHeaders,
	logger logr.Logger,
) *DakrPercentileFetcher {
	return &DakrPercentileFetcher{
		client:        client,
		clientHeaders: headers,
		logger:        logger.WithName("dakr-percentile-fetcher"),
	}
}

// FetchWorkloadPercentiles calls the DAKR control plane for each workload and
// maps the response into gen.HistoricalMetricsSummary entries keyed by
// "namespace/workloadName/workloadKind".
func (f *DakrPercentileFetcher) FetchWorkloadPercentiles(
	ctx context.Context,
	clusterID string,
	workloads []collector.HistoricalWorkloadQuery,
) (map[string]*gen.HistoricalMetricsSummary, error) {
	if len(workloads) == 0 {
		return make(map[string]*gen.HistoricalMetricsSummary), nil
	}

	results := make(map[string]*gen.HistoricalMetricsSummary, len(workloads))
	var mu sync.Mutex
	var wg sync.WaitGroup

	semaphore := make(chan struct{}, maxConcurrentPercentileRequests)

	now := time.Now()
	windowStart := now.Add(-24 * time.Hour)

	for _, w := range workloads {
		wg.Add(1)
		go func(workload collector.HistoricalWorkloadQuery) {
			defer wg.Done()

			// Rate-limit concurrent requests.
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			summary, err := f.fetchSingleWorkload(ctx, clusterID, workload, windowStart, now)
			if err != nil {
				f.logger.Error(err, "failed to fetch percentiles for workload",
					"namespace", workload.Namespace,
					"workload", workload.WorkloadName,
					"kind", workload.WorkloadKind,
				)
				return
			}

			key := workload.Namespace + "/" + workload.WorkloadName + "/" + workload.WorkloadKind
			mu.Lock()
			results[key] = summary
			mu.Unlock()
		}(w)
	}

	wg.Wait()
	return results, nil
}

// fetchSingleWorkload issues one GetWorkloadContainerPercentiles RPC and
// converts the response to a gen.HistoricalMetricsSummary.
func (f *DakrPercentileFetcher) fetchSingleWorkload(
	ctx context.Context,
	clusterID string,
	workload collector.HistoricalWorkloadQuery,
	windowStart, windowEnd time.Time,
) (*gen.HistoricalMetricsSummary, error) {
	req := connect.NewRequest(&gen.GetWorkloadContainerPercentilesRequest{
		ClusterId: clusterID,
		Kind:      workload.WorkloadKind,
		// TeamId and Uid are not available from HistoricalWorkloadQuery;
		// the DAKR server resolves the workload from the cluster token + kind.
		StartTime: timestamppb.New(windowStart),
		EndTime:   timestamppb.New(windowEnd),
	})

	// Attach auth and operator headers.
	f.clientHeaders.AttachToRequest(req.Header())

	resp, err := f.client.GetWorkloadContainerPercentiles(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("GetWorkloadContainerPercentiles RPC failed: %w", err)
	}

	containers := mapContainerPercentiles(resp.Msg.GetContainers())

	return &gen.HistoricalMetricsSummary{
		Workload: &gen.MpaWorkloadIdentifier{
			Namespace: workload.Namespace,
			Name:      workload.WorkloadName,
			Kind:      workload.WorkloadKind,
		},
		Containers:  containers,
		WindowStart: timestamppb.New(windowStart),
		WindowEnd:   timestamppb.New(windowEnd),
	}, nil
}

// mapContainerPercentiles converts a slice of DAKR ContainerPercentileSummary
// (float64 values) into the MPA ContainerHistoricalMetrics (int64 millicores /
// bytes).
func mapContainerPercentiles(
	summaries []*gen.ContainerPercentileSummary,
) []*gen.ContainerHistoricalMetrics {
	out := make([]*gen.ContainerHistoricalMetrics, 0, len(summaries))

	for _, s := range summaries {
		m := &gen.ContainerHistoricalMetrics{
			ContainerName: s.GetContainerName(),
		}

		// Map CPU usage percentiles (DAKR returns cores as float64, MPA wants millicores).
		if cpu := s.GetCpuUsage(); cpu != nil {
			m.CpuP50 = cpuToMillicores(cpu.GetP50())
			m.CpuP75 = cpuToMillicores(cpu.GetP75())
			m.CpuP80 = cpuToMillicores(cpu.GetP80())
			m.CpuP90 = cpuToMillicores(cpu.GetP90())
			m.CpuP95 = cpuToMillicores(cpu.GetP95())
			m.CpuP99 = cpuToMillicores(cpu.GetP99())
			m.CpuPmax = cpuToMillicores(cpu.GetMax())
		}

		// Map memory usage percentiles (DAKR returns bytes as float64, MPA wants bytes as int64).
		if mem := s.GetMemoryUsage(); mem != nil {
			m.MemP50 = int64(mem.GetP50())
			m.MemP75 = int64(mem.GetP75())
			m.MemP80 = int64(mem.GetP80())
			m.MemP90 = int64(mem.GetP90())
			m.MemP95 = int64(mem.GetP95())
			m.MemP99 = int64(mem.GetP99())
			m.MemPmax = int64(mem.GetMax())
		}

		out = append(out, m)
	}

	return out
}

// cpuToMillicores converts CPU cores (float64) to millicores (int64).
func cpuToMillicores(cores float64) int64 {
	return int64(cores * 1000)
}
