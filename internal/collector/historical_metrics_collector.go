package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

const (
	historicalWindow     = 24 * time.Hour
	historicalStepInterval = "1m"
	maxConcurrentQueries = 10
)

// HistoricalWorkloadQuery defines what to query for a workload.
type HistoricalWorkloadQuery struct {
	Namespace    string
	WorkloadName string
	WorkloadKind string
	PodRegex     string   // e.g., "web-app-.*"
	Containers   []string // container names to query
}

// HistoricalMetricsCollector queries Prometheus for historical CPU/memory percentiles.
type HistoricalMetricsCollector struct {
	logger        logr.Logger
	prometheusAPI v1.API
	semaphore     chan struct{} // limits concurrent Prometheus queries
}

// NewHistoricalMetricsCollector creates a new collector.
func NewHistoricalMetricsCollector(logger logr.Logger, prometheusAPI v1.API) *HistoricalMetricsCollector {
	return &HistoricalMetricsCollector{
		logger:        logger.WithName("historical-metrics"),
		prometheusAPI: prometheusAPI,
		semaphore:     make(chan struct{}, maxConcurrentQueries),
	}
}

// FetchPercentiles queries Prometheus for 24h percentiles for a workload.
func (c *HistoricalMetricsCollector) FetchPercentiles(ctx context.Context, workload HistoricalWorkloadQuery) (*gen.HistoricalMetricsSummary, error) {
	now := time.Now()
	windowStart := now.Add(-historicalWindow)

	var containerResults []*gen.ContainerHistoricalMetrics
	var totalSamples int32

	for _, containerName := range workload.Containers {
		metrics, samples, err := c.fetchContainerPercentiles(ctx, workload, containerName, now)
		if err != nil {
			c.logger.Error(err, "Failed to fetch percentiles for container",
				"container", containerName,
				"workload", workload.WorkloadName,
			)
			continue
		}
		containerResults = append(containerResults, metrics)
		if samples > totalSamples {
			totalSamples = samples
		}
	}

	return &gen.HistoricalMetricsSummary{
		Workload: &gen.MpaWorkloadIdentifier{
			Namespace: workload.Namespace,
			Name:      workload.WorkloadName,
			Kind:      workload.WorkloadKind,
		},
		Containers:  containerResults,
		WindowStart: timestamppb.New(windowStart),
		WindowEnd:   timestamppb.New(now),
		SampleCount: totalSamples,
	}, nil
}

// FetchPercentilesForAll queries Prometheus for all workloads concurrently with rate limiting.
func (c *HistoricalMetricsCollector) FetchPercentilesForAll(ctx context.Context, workloads []HistoricalWorkloadQuery) map[string]*gen.HistoricalMetricsSummary {
	results := make(map[string]*gen.HistoricalMetricsSummary)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, w := range workloads {
		wg.Add(1)
		go func(workload HistoricalWorkloadQuery) {
			defer wg.Done()

			// Rate limit
			c.semaphore <- struct{}{}
			defer func() { <-c.semaphore }()

			summary, err := c.FetchPercentiles(ctx, workload)
			if err != nil {
				c.logger.Error(err, "Failed to fetch historical metrics",
					"workload", workload.WorkloadName)
				return
			}

			key := fmt.Sprintf("%s/%s/%s", workload.Namespace, workload.WorkloadKind, workload.WorkloadName)
			mu.Lock()
			results[key] = summary
			mu.Unlock()
		}(w)
	}

	wg.Wait()
	return results
}

// DiscoverContainers discovers container names for a workload from Prometheus.
func (c *HistoricalMetricsCollector) DiscoverContainers(ctx context.Context, namespace, podRegex string) ([]string, error) {
	query := fmt.Sprintf(
		`group by (container) (container_memory_working_set_bytes{namespace="%s", pod=~"%s", container!="", container!="POD"})`,
		namespace, podRegex,
	)
	result, _, err := c.prometheusAPI.Query(ctx, query, time.Now())
	if err != nil {
		return nil, err
	}

	vec, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	var containers []string
	for _, sample := range vec {
		if name := string(sample.Metric["container"]); name != "" {
			containers = append(containers, name)
		}
	}
	return containers, nil
}

func (c *HistoricalMetricsCollector) fetchContainerPercentiles(ctx context.Context, workload HistoricalWorkloadQuery, containerName string, now time.Time) (*gen.ContainerHistoricalMetrics, int32, error) {
	percentiles := []float64{0.50, 0.75, 0.80, 0.90, 0.99}

	cpuValues := make(map[float64]int64)
	memValues := make(map[float64]int64)

	var sampleCount int32

	for _, p := range percentiles {
		// CPU query: rate over 5m, percentile over 24h
		cpuQuery := fmt.Sprintf(
			`quantile_over_time(%.2f, rate(container_cpu_usage_seconds_total{namespace="%s", pod=~"%s", container="%s"}[5m])[24h:%s])`,
			p, workload.Namespace, workload.PodRegex, containerName, historicalStepInterval,
		)

		cpuVal, err := c.queryScalar(ctx, cpuQuery, now)
		if err != nil {
			c.logger.V(1).Info("CPU percentile query failed",
				"percentile", p, "error", err)
		} else {
			// Convert from cores (float) to millicores (int)
			cpuValues[p] = int64(cpuVal * 1000)
		}

		// Memory query: direct percentile over 24h
		memQuery := fmt.Sprintf(
			`quantile_over_time(%.2f, container_memory_working_set_bytes{namespace="%s", pod=~"%s", container="%s"}[24h])`,
			p, workload.Namespace, workload.PodRegex, containerName,
		)

		memVal, err := c.queryScalar(ctx, memQuery, now)
		if err != nil {
			c.logger.V(1).Info("Memory percentile query failed",
				"percentile", p, "error", err)
		} else {
			memValues[p] = int64(memVal)
		}
	}

	// Estimate sample count from Prometheus
	countQuery := fmt.Sprintf(
		`count_over_time(container_memory_working_set_bytes{namespace="%s", pod=~"%s", container="%s"}[24h])`,
		workload.Namespace, workload.PodRegex, containerName,
	)
	countVal, err := c.queryScalar(ctx, countQuery, now)
	if err == nil {
		sampleCount = int32(countVal)
	}

	return &gen.ContainerHistoricalMetrics{
		ContainerName: containerName,
		CpuP50:        cpuValues[0.50],
		CpuP75:        cpuValues[0.75],
		CpuP80:        cpuValues[0.80],
		CpuP90:        cpuValues[0.90],
		CpuP99:        cpuValues[0.99],
		MemP50:        memValues[0.50],
		MemP75:        memValues[0.75],
		MemP80:        memValues[0.80],
		MemP90:        memValues[0.90],
		MemP99:        memValues[0.99],
	}, sampleCount, nil
}

func (c *HistoricalMetricsCollector) queryScalar(ctx context.Context, query string, ts time.Time) (float64, error) {
	result, warnings, err := c.prometheusAPI.Query(ctx, query, ts)
	if err != nil {
		return 0, fmt.Errorf("prometheus query failed: %w", err)
	}
	if len(warnings) > 0 {
		c.logger.V(1).Info("Prometheus warnings", "warnings", warnings)
	}

	switch v := result.(type) {
	case *model.Scalar:
		return float64(v.Value), nil
	case model.Vector:
		if len(v) > 0 {
			return float64(v[0].Value), nil
		}
		return 0, fmt.Errorf("empty vector result")
	default:
		return 0, fmt.Errorf("unexpected result type: %T", result)
	}
}
