package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/health"
)

const (
	// cacheRefreshInterval is how often Refresh is called by the background loop.
	cacheRefreshInterval = 15 * time.Minute

	// refreshTimeout caps a single Refresh call so a slow DAKR response cannot
	// block the refresh loop indefinitely.
	refreshTimeout = 30 * time.Second
)

// PercentileFetcher abstracts the DAKR control plane call to fetch per-workload
// pre-computed percentiles.
type PercentileFetcher interface {
	FetchWorkloadPercentiles(
		ctx context.Context,
		clusterID string,
		workloads []HistoricalWorkloadQuery,
	) (map[string]*gen.HistoricalMetricsSummary, error)
}

// HistoricalPercentileCache fetches pre-computed percentiles from the DAKR
// control plane and caches them in-memory. It implements
// HistoricalPercentileProvider as a drop-in replacement for
// HistoricalMetricsCollector without a local Prometheus dependency.
type HistoricalPercentileCache struct {
	logger        logr.Logger
	fetcher       PercentileFetcher
	clusterID     string
	healthManager *health.HealthManager

	mu    sync.RWMutex
	cache map[string]*gen.HistoricalMetricsSummary // key: "namespace/name/kind"
}

// NewHistoricalPercentileCache constructs a ready-to-use cache. Call Start to
// begin the periodic background refresh.
func NewHistoricalPercentileCache(
	logger logr.Logger,
	fetcher PercentileFetcher,
	clusterID string,
	healthManager *health.HealthManager,
) *HistoricalPercentileCache {
	return &HistoricalPercentileCache{
		logger:        logger.WithName("historical-percentile-cache"),
		fetcher:       fetcher,
		clusterID:     clusterID,
		healthManager: healthManager,
		cache:         make(map[string]*gen.HistoricalMetricsSummary),
	}
}

// Start performs an initial Refresh and then refreshes on a 15-minute ticker
// until ctx is cancelled. It blocks; run it in a goroutine.
func (c *HistoricalPercentileCache) Start(ctx context.Context) {
	c.Refresh(ctx)

	ticker := time.NewTicker(cacheRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.V(1).Info("historical percentile cache shutting down")
			return
		case <-ticker.C:
			c.Refresh(ctx)
		}
	}
}

// Refresh fetches percentiles from DAKR for all currently-known workloads and
// atomically replaces the cache. On error the stale cache is preserved and
// health is reported as Degraded. This method is safe to call directly in
// tests.
func (c *HistoricalPercentileCache) Refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	// Collect the workload queries from the current cache so we refresh all
	// known workloads. On the very first call the cache is empty — the fetcher
	// is expected to return whatever DAKR knows about this cluster.
	c.mu.RLock()
	workloads := make([]HistoricalWorkloadQuery, 0, len(c.cache))
	for key, summary := range c.cache {
		if summary.Workload != nil {
			workloads = append(workloads, HistoricalWorkloadQuery{
				Namespace:    summary.Workload.Namespace,
				WorkloadName: summary.Workload.Name,
				WorkloadKind: summary.Workload.Kind,
			})
		} else {
			// Fallback: parse key "namespace/name/kind"
			var ns, name, kind string
			if _, err := fmt.Sscanf(key, "%s/%s/%s", &ns, &name, &kind); err == nil {
				workloads = append(workloads, HistoricalWorkloadQuery{
					Namespace:    ns,
					WorkloadName: name,
					WorkloadKind: kind,
				})
			}
		}
	}
	c.mu.RUnlock()

	results, err := c.fetcher.FetchWorkloadPercentiles(fetchCtx, c.clusterID, workloads)
	if err != nil {
		c.logger.Error(err, "failed to fetch percentiles from DAKR; keeping stale cache")
		c.updateHealthStatus(
			health.HealthStatusDegraded,
			"DAKR percentile fetch failed",
			map[string]string{"error": err.Error()},
		)
		return
	}

	// Atomically replace the entire cache.
	c.mu.Lock()
	c.cache = results
	c.mu.Unlock()

	c.logger.V(1).Info("historical percentile cache refreshed", "workloads", len(results))
	c.updateHealthStatus(
		health.HealthStatusHealthy,
		"DAKR percentile fetch succeeded",
		map[string]string{"workload_count": fmt.Sprintf("%d", len(results))},
	)
}

// FetchPercentiles returns the cached percentile summary for a single workload.
// If the workload is not in the cache an empty (non-nil) summary is returned
// rather than an error, so callers do not need to special-case cache misses.
func (c *HistoricalPercentileCache) FetchPercentiles(
	ctx context.Context,
	workload HistoricalWorkloadQuery,
) (*gen.HistoricalMetricsSummary, error) {
	key := workloadKey(workload.Namespace, workload.WorkloadName, workload.WorkloadKind)

	c.mu.RLock()
	summary, ok := c.cache[key]
	c.mu.RUnlock()

	if !ok {
		return &gen.HistoricalMetricsSummary{
			Workload: &gen.MpaWorkloadIdentifier{
				Namespace: workload.Namespace,
				Name:      workload.WorkloadName,
				Kind:      workload.WorkloadKind,
			},
		}, nil
	}
	return summary, nil
}

// FetchPercentilesForAll returns cached percentile summaries for all of the
// requested workloads. Only workloads present in the cache are included in the
// returned map.
func (c *HistoricalPercentileCache) FetchPercentilesForAll(
	ctx context.Context,
	workloads []HistoricalWorkloadQuery,
) map[string]*gen.HistoricalMetricsSummary {
	out := make(map[string]*gen.HistoricalMetricsSummary, len(workloads))

	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, w := range workloads {
		key := workloadKey(w.Namespace, w.WorkloadName, w.WorkloadKind)
		if summary, ok := c.cache[key]; ok {
			out[key] = summary
		}
	}
	return out
}

// DiscoverContainers returns a deduplicated list of container names found
// across all cached workload summaries whose namespace matches the given
// namespace. The podRegex parameter is accepted for interface compatibility but
// is not used; filtering by namespace is sufficient for the cache-backed path.
func (c *HistoricalPercentileCache) DiscoverContainers(
	ctx context.Context,
	namespace, podRegex string,
) ([]string, error) {
	seen := make(map[string]struct{})

	c.mu.RLock()
	for _, summary := range c.cache {
		if summary.Workload == nil || summary.Workload.Namespace != namespace {
			continue
		}
		for _, cm := range summary.Containers {
			if cm.ContainerName != "" {
				seen[cm.ContainerName] = struct{}{}
			}
		}
	}
	c.mu.RUnlock()

	containers := make([]string, 0, len(seen))
	for name := range seen {
		containers = append(containers, name)
	}
	return containers, nil
}

// workloadKey returns the canonical cache key for a workload.
func workloadKey(namespace, name, kind string) string {
	return namespace + "/" + name + "/" + kind
}

// updateHealthStatus reports status to the HealthManager if one was provided.
func (c *HistoricalPercentileCache) updateHealthStatus(
	status health.HealthStatus,
	message string,
	metadata map[string]string,
) {
	if c.healthManager != nil {
		c.healthManager.UpdateStatus(health.ComponentPrometheus, status, message, metadata)
	}
}
