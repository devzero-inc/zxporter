package nodemon

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Bounds for the GPU snapshot refresh interval. Refreshing faster than DCGM
// updates (DCGM_EXPORTER_INTERVAL, 5s by default) yields no new data; the
// upper bound keeps the snapshot fresh enough for the container collector,
// which polls /container/metrics every ~10s.
const (
	MinGPURefreshInterval     = 5 * time.Second
	DefaultGPURefreshInterval = 10 * time.Second

	// minGPUStalenessThreshold is the floor for how old a cached snapshot may be
	// and still be served as fresh. The effective threshold is
	// max(2*interval, minGPUStalenessThreshold), so at the default 10s interval a
	// single dropped or slow refresh tick (a GC pause, CPU starvation, one failed
	// DCGM scrape) does NOT blank GPU data — the last-good snapshot is served for
	// up to 20s. Only a sustained gap beyond the threshold marks the snapshot
	// stale everywhere (QueryMetrics returns nil, QueryGPUSnapshot reports stale),
	// so arbitrarily-old data is never served as current — this is what protects
	// both the node path (QueryGPUSnapshot) and the container/legacy path
	// (QueryMetrics, read by the unified exporter's per-container GPU merge).
	minGPUStalenessThreshold = 20 * time.Second

	// gpuRefreshCycleTimeout bounds a single refresh (getDCGMUrls' k8s List +
	// the DCGM scrape). It is comfortably above the per-URL scrape timeout (10s)
	// so a legitimately slow refresh is never cut short; it exists only so a
	// hung k8s API cannot wedge the refresher forever.
	gpuRefreshCycleTimeout = 30 * time.Second
)

// CachedGPUExporter wraps a MetricsQuerier (the DCGM-scraping *Exporter) with a
// single periodically-refreshed snapshot. It exists to collapse what used to be
// many independent DCGM scrapes — the background collection loop plus a fresh
// scrape+parse on every /container/metrics request — into exactly one
// scrape+parse per interval. The full DCGM parse is the nodemon's dominant heap
// allocation on GPU nodes, so removing overlapping/per-request parses is the
// primary defence against OOM at the 256Mi cap.
//
// QueryMetrics is a non-blocking read of the last snapshot, so it is safe to
// call from any number of concurrent HTTP handlers without triggering work.
type CachedGPUExporter struct {
	source             MetricsQuerier
	interval           time.Duration
	stalenessThreshold time.Duration
	log                logr.Logger

	mu          sync.RWMutex
	snapshot    []GPUMetric
	summary     *NodeGPUSummary
	lastSuccess time.Time
}

// NewCachedGPUExporter wraps source with a snapshot cache. interval is clamped
// to at least MinGPURefreshInterval; a non-positive interval selects the
// default.
func NewCachedGPUExporter(source MetricsQuerier, interval time.Duration, log logr.Logger) *CachedGPUExporter {
	if interval <= 0 {
		interval = DefaultGPURefreshInterval
	}
	if interval < MinGPURefreshInterval {
		interval = MinGPURefreshInterval
	}
	// Tolerate one dropped/slow refresh tick (2*interval) but never less than
	// the 20s floor, so a brief latency hiccup in the refresher does not blank
	// GPU data.
	stalenessThreshold := 2 * interval
	if stalenessThreshold < minGPUStalenessThreshold {
		stalenessThreshold = minGPUStalenessThreshold
	}
	return &CachedGPUExporter{
		source:             source,
		interval:           interval,
		stalenessThreshold: stalenessThreshold,
		log:                log.WithName("gpu-cache"),
	}
}

// Start runs an initial refresh and then refreshes on a ticker until ctx is
// cancelled. Call it once in its own goroutine.
func (c *CachedGPUExporter) Start(ctx context.Context) {
	c.log.Info("Starting GPU snapshot refresher", "interval", c.interval.String())
	c.refreshCycle(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshCycle(ctx)
		}
	}
}

// refreshCycle bounds a single refresh with a per-cycle deadline so a hung
// dependency cannot wedge the refresher indefinitely. In particular
// getDCGMUrls' k8s List is otherwise unbounded (the per-URL scrape already has
// its own 10s timeout); gpuRefreshCycleTimeout is generous relative to that
// scrape budget, so only a genuine hang trips it, not a slow-but-legitimate
// refresh. Mirrors the per-cycle timeout the JVM/runtime collectors use.
func (c *CachedGPUExporter) refreshCycle(parentCtx context.Context) {
	cycleCtx, cancel := context.WithTimeout(parentCtx, gpuRefreshCycleTimeout)
	defer cancel()
	c.Refresh(cycleCtx)
}

// Refresh performs one DCGM scrape+parse via the wrapped source and atomically
// swaps in the new snapshot. On error it keeps the previous snapshot (serving
// slightly stale data beats serving nothing) and, once refreshes have been
// failing for gpuStalenessFactor intervals, logs a staleness warning so the
// gap is visible rather than silent.
func (c *CachedGPUExporter) Refresh(ctx context.Context) {
	metrics, err := c.source.QueryMetrics(ctx)
	var summary *NodeGPUSummary
	if err == nil {
		summary = SummarizeNodeGPU(metrics)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		age := time.Since(c.lastSuccess)
		if !c.lastSuccess.IsZero() && age > c.stalenessThreshold {
			c.log.Error(err, "GPU snapshot is stale; DCGM refresh has been failing",
				"staleFor", age.String())
		} else {
			c.log.V(1).Info("GPU refresh failed; serving previous snapshot", "error", err.Error())
		}
		return
	}

	c.snapshot = metrics
	c.summary = summary
	c.lastSuccess = time.Now()
}

// staleLocked reports whether the cached snapshot has aged past the staleness
// threshold (or was never populated). Callers must hold at least the read lock.
func (c *CachedGPUExporter) staleLocked() bool {
	if c.lastSuccess.IsZero() {
		return true
	}
	return time.Since(c.lastSuccess) > c.stalenessThreshold
}

// QueryMetrics returns the most recent snapshot without scraping, or nil once
// the snapshot has aged past the staleness threshold. It satisfies
// MetricsQuerier so it drops in wherever the raw *Exporter was used, and the
// unified exporter's per-container GPU merge reads through it — so returning nil
// when stale is what stops the container path from emitting arbitrarily-old GPU
// values during a sustained DCGM outage (a gap, matching a live scrape's
// failure, rather than frozen last-good data). The error return is always nil:
// staleness is a nil result plus a log, not a per-reader error.
func (c *CachedGPUExporter) QueryMetrics(_ context.Context) ([]GPUMetric, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.staleLocked() {
		return nil, nil
	}
	return c.snapshot, nil
}

// QueryGPUSnapshot returns the cached node-level GPU summary and its
// publication state without scraping DCGM.
func (c *CachedGPUExporter) QueryGPUSnapshot() (*NodeGPUSummary, SnapshotSectionStatus) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.lastSuccess.IsZero() {
		return nil, SnapshotSectionStatus{State: SnapshotStateNotReady}
	}

	// Fresh within the grace window (age <= threshold); stale beyond it. A
	// single failed/slow refresh keeps the snapshot young, so it stays ready —
	// only a sustained gap marks it stale, which the node collector then drops.
	state := SnapshotStateReady
	if c.staleLocked() {
		state = SnapshotStateStale
	}
	collectedAt := c.lastSuccess

	return cloneNodeGPUSummary(c.summary), SnapshotSectionStatus{
		State:       state,
		CollectedAt: &collectedAt,
	}
}

func cloneNodeGPUSummary(summary *NodeGPUSummary) *NodeGPUSummary {
	if summary == nil {
		return nil
	}

	cloned := *summary
	cloned.GPUModels = slices.Clone(summary.GPUModels)
	cloned.GPUUUIDs = slices.Clone(summary.GPUUUIDs)
	return &cloned
}
