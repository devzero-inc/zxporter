package nodemon

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// UnifiedExporter combines stats/summary, cAdvisor, and GPU data into the
// unified response types consumed by the HTTP handlers. It implements UnifiedQuerier.
type UnifiedExporter struct {
	statsPoller     *StatsPoller
	cadvisorScraper *CAdvisorScraper
	gpuExporter     *Exporter // existing GPU exporter, may be nil
	nodeName        string
	log             logr.Logger

	nodeNetRates *RateCalculator // for computing node network byte rates from cumulative counters

	mu               sync.RWMutex
	containerMetrics []ContainerMetricsResponse
	nodeMetrics      *NodeMetricsResponse
	pvcMetrics       []PVCMetricsResponse
	lastCollected    time.Time
}

// NewUnifiedExporter creates a UnifiedExporter.
func NewUnifiedExporter(
	statsPoller *StatsPoller,
	cadvisorScraper *CAdvisorScraper,
	gpuExporter *Exporter,
	nodeName string,
	log logr.Logger,
) *UnifiedExporter {
	return &UnifiedExporter{
		statsPoller:     statsPoller,
		cadvisorScraper: cadvisorScraper,
		gpuExporter:     gpuExporter,
		nodeName:        nodeName,
		log:             log.WithName("unified-exporter"),
		nodeNetRates:    NewRateCalculator(),
	}
}

// StartCollectionLoop runs periodic collection. Call in a goroutine.
func (u *UnifiedExporter) StartCollectionLoop(ctx context.Context, interval time.Duration) {
	// Initial collection
	u.Collect(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.Collect(ctx)
		}
	}
}

// Collect fetches from all sources and updates cached results.
func (u *UnifiedExporter) Collect(ctx context.Context) {
	now := time.Now()

	stats := u.fetchStats(ctx)
	cadvisorMetrics := u.fetchCAdvisor(ctx, now)
	gpuMetrics := u.fetchGPU(ctx)

	cadvisorIndex := indexCAdvisorMetrics(cadvisorMetrics)
	gpuIndex := indexGPUMetrics(gpuMetrics)

	containerResults, pvcResults := u.buildContainerAndPVCMetrics(stats, cadvisorIndex, gpuIndex, now)
	nodeResult := u.buildNodeMetrics(stats, cadvisorMetrics, now)

	// Update cache
	u.mu.Lock()
	u.containerMetrics = containerResults
	u.nodeMetrics = nodeResult
	u.pvcMetrics = pvcResults
	u.lastCollected = now
	u.mu.Unlock()

	u.log.V(1).Info("Collected unified metrics",
		"containers", len(containerResults),
		"pvcs", len(pvcResults))
}

// fetchStats polls kubelet stats/summary.
func (u *UnifiedExporter) fetchStats(ctx context.Context) *StatsSummary {
	if u.statsPoller == nil {
		return nil
	}
	stats, err := u.statsPoller.Poll(ctx)
	if err != nil {
		u.log.Error(err, "Failed to poll stats/summary")
		return nil
	}
	return stats
}

// fetchCAdvisor scrapes cAdvisor counter metrics and computes rates.
func (u *UnifiedExporter) fetchCAdvisor(ctx context.Context, now time.Time) []CAdvisorContainerMetrics {
	if u.cadvisorScraper == nil {
		return nil
	}
	metrics, err := u.cadvisorScraper.Scrape(ctx, now)
	if err != nil {
		u.log.Error(err, "Failed to scrape cAdvisor")
		return nil
	}
	return metrics
}

// fetchGPU queries the DCGM exporter for GPU metrics.
func (u *UnifiedExporter) fetchGPU(ctx context.Context) []GPUMetric {
	if u.gpuExporter == nil {
		return nil
	}
	metrics, err := u.gpuExporter.QueryMetrics(ctx)
	if err != nil {
		u.log.V(1).Info("Failed to query GPU metrics", "error", err)
		return nil
	}
	return metrics
}

// indexCAdvisorMetrics builds a lookup map keyed by "namespace/pod/container".
func indexCAdvisorMetrics(metrics []CAdvisorContainerMetrics) map[string]*CAdvisorContainerMetrics {
	idx := make(map[string]*CAdvisorContainerMetrics, len(metrics))
	for i := range metrics {
		m := &metrics[i]
		idx[m.Namespace+"/"+m.Pod+"/"+m.Container] = m
	}
	return idx
}

// indexGPUMetrics builds a lookup map keyed by "namespace/pod/container".
func indexGPUMetrics(metrics []GPUMetric) map[string]*GPUMetric {
	idx := make(map[string]*GPUMetric, len(metrics))
	for i := range metrics {
		m := &metrics[i]
		idx[m.Namespace+"/"+m.Pod+"/"+m.Container] = m
	}
	return idx
}

// buildContainerAndPVCMetrics converts stats/summary pods into response types,
// merging cAdvisor rates and GPU data.
func (u *UnifiedExporter) buildContainerAndPVCMetrics(
	stats *StatsSummary,
	cadvisorIndex map[string]*CAdvisorContainerMetrics,
	gpuIndex map[string]*GPUMetric,
	now time.Time,
) ([]ContainerMetricsResponse, []PVCMetricsResponse) {
	if stats == nil {
		return nil, nil
	}

	var containerResults []ContainerMetricsResponse
	var pvcResults []PVCMetricsResponse

	for _, pod := range stats.Pods {
		rxBytes, txBytes := aggregatePodNetwork(pod)

		for _, container := range pod.Containers {
			resp := u.buildSingleContainerMetric(pod, container, cadvisorIndex, gpuIndex, rxBytes, txBytes, now)
			containerResults = append(containerResults, resp)
		}

		pvcResults = append(pvcResults, extractPVCMetrics(pod)...)
	}

	return containerResults, pvcResults
}

// aggregatePodNetwork sums rx/tx bytes across all interfaces for a pod.
func aggregatePodNetwork(pod PodStats) (rxBytes, txBytes uint64) {
	for _, iface := range pod.Network.Interfaces {
		if iface.RxBytes != nil {
			rxBytes += *iface.RxBytes
		}
		if iface.TxBytes != nil {
			txBytes += *iface.TxBytes
		}
	}
	return
}

// buildSingleContainerMetric creates a ContainerMetricsResponse for one container,
// merging stats/summary, cAdvisor, and GPU data.
func (u *UnifiedExporter) buildSingleContainerMetric(
	pod PodStats,
	container ContainerStats,
	cadvisorIndex map[string]*CAdvisorContainerMetrics,
	gpuIndex map[string]*GPUMetric,
	rxBytes, txBytes uint64,
	now time.Time,
) ContainerMetricsResponse {
	resp := ContainerMetricsResponse{
		NodeName:       u.nodeName,
		Namespace:      pod.PodRef.Namespace,
		Pod:            pod.PodRef.Name,
		Container:      container.Name,
		Timestamp:      now,
		NetworkRxBytes: rxBytes,
		NetworkTxBytes: txBytes,
	}

	if container.CPU.UsageNanoCores != nil {
		resp.CPUUsageNanoCores = *container.CPU.UsageNanoCores
	}
	if container.Memory.WorkingSetBytes != nil {
		resp.MemoryWorkingSet = *container.Memory.WorkingSetBytes
	}
	if container.Memory.UsageBytes != nil {
		resp.MemoryUsageBytes = *container.Memory.UsageBytes
	}
	if container.Memory.RSSBytes != nil {
		resp.MemoryRSSBytes = *container.Memory.RSSBytes
	}

	// Merge cAdvisor rates
	cadKey := pod.PodRef.Namespace + "/" + pod.PodRef.Name + "/" + container.Name
	if cm, ok := cadvisorIndex[cadKey]; ok {
		resp.NetworkRxPacketsPerSec = cm.NetworkRxPacketsPerSec
		resp.NetworkTxPacketsPerSec = cm.NetworkTxPacketsPerSec
		resp.NetworkRxErrorsPerSec = cm.NetworkRxErrorsPerSec
		resp.NetworkTxErrorsPerSec = cm.NetworkTxErrorsPerSec
		resp.NetworkRxDropsPerSec = cm.NetworkRxDropsPerSec
		resp.NetworkTxDropsPerSec = cm.NetworkTxDropsPerSec
		resp.DiskReadBytesPerSec = cm.DiskReadBytesPerSec
		resp.DiskWriteBytesPerSec = cm.DiskWriteBytesPerSec
		resp.DiskReadOpsPerSec = cm.DiskReadOpsPerSec
		resp.DiskWriteOpsPerSec = cm.DiskWriteOpsPerSec
		resp.CPUThrottleFraction = cm.CPUThrottleFraction
	}

	// Merge GPU metrics
	gpuKey := pod.PodRef.Namespace + "/" + pod.PodRef.Name + "/" + container.Name
	if gm, ok := gpuIndex[gpuKey]; ok {
		resp.GPUUtilization = gm.GPUUtilization
		resp.GPUMemoryUsedMiB = gm.FramebufferUsed
		resp.GPUMemoryFreeMiB = gm.FramebufferFree
		resp.GPUPowerWatts = gm.PowerUsage
		resp.GPUTemperature = gm.Temperature
	}

	return resp
}

// extractPVCMetrics converts volume stats from a pod into PVC response types.
func extractPVCMetrics(pod PodStats) []PVCMetricsResponse {
	var results []PVCMetricsResponse
	for _, vol := range pod.VolumeStats {
		if vol.PVCRef == nil {
			continue
		}
		pvc := PVCMetricsResponse{
			Namespace: pod.PodRef.Namespace,
			Pod:       pod.PodRef.Name,
			PVCName:   vol.PVCRef.Name,
		}
		if vol.UsedBytes != nil {
			pvc.UsedBytes = *vol.UsedBytes
		}
		if vol.CapacityBytes != nil {
			pvc.CapacityBytes = *vol.CapacityBytes
		}
		if vol.AvailableBytes != nil {
			pvc.AvailableBytes = *vol.AvailableBytes
		}
		results = append(results, pvc)
	}
	return results
}

// buildNodeMetrics aggregates cAdvisor per-container rates and stats/summary
// node-level data into a single NodeMetricsResponse.
func (u *UnifiedExporter) buildNodeMetrics(
	stats *StatsSummary,
	cadvisorMetrics []CAdvisorContainerMetrics,
	now time.Time,
) *NodeMetricsResponse {
	nodeResult := &NodeMetricsResponse{
		NodeName:  u.nodeName,
		Timestamp: now,
	}

	// Aggregate cAdvisor per-container rates into node totals
	for _, cm := range cadvisorMetrics {
		nodeResult.NetworkRxPacketsPerSec += cm.NetworkRxPacketsPerSec
		nodeResult.NetworkTxPacketsPerSec += cm.NetworkTxPacketsPerSec
		nodeResult.NetworkRxErrorsPerSec += cm.NetworkRxErrorsPerSec
		nodeResult.NetworkTxErrorsPerSec += cm.NetworkTxErrorsPerSec
		nodeResult.NetworkRxDropsPerSec += cm.NetworkRxDropsPerSec
		nodeResult.NetworkTxDropsPerSec += cm.NetworkTxDropsPerSec
		nodeResult.DiskReadBytesPerSec += cm.DiskReadBytesPerSec
		nodeResult.DiskWriteBytesPerSec += cm.DiskWriteBytesPerSec
		nodeResult.DiskReadOpsPerSec += cm.DiskReadOpsPerSec
		nodeResult.DiskWriteOpsPerSec += cm.DiskWriteOpsPerSec
	}

	if stats != nil {
		// Node-level CPU/memory (includes system processes)
		if stats.Node.CPU.UsageNanoCores != nil {
			nodeResult.CPUUsageNanoCores = *stats.Node.CPU.UsageNanoCores
		}
		if stats.Node.Memory.WorkingSetBytes != nil {
			nodeResult.MemoryWorkingSet = *stats.Node.Memory.WorkingSetBytes
		}

		// Node-level network bytes rate from cumulative counters
		var nodeRxBytes, nodeTxBytes uint64
		for _, iface := range stats.Node.Network.Interfaces {
			if iface.RxBytes != nil {
				nodeRxBytes += *iface.RxBytes
			}
			if iface.TxBytes != nil {
				nodeTxBytes += *iface.TxBytes
			}
		}
		nodeResult.NetworkRxBytesPerSec = u.nodeNetRates.Rate(u.nodeName, "rx_bytes", float64(nodeRxBytes), now)
		nodeResult.NetworkTxBytesPerSec = u.nodeNetRates.Rate(u.nodeName, "tx_bytes", float64(nodeTxBytes), now)
	}

	return nodeResult
}

// QueryContainerMetrics implements UnifiedQuerier.
func (u *UnifiedExporter) QueryContainerMetrics() []ContainerMetricsResponse {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.containerMetrics
}

// QueryNodeMetrics implements UnifiedQuerier.
func (u *UnifiedExporter) QueryNodeMetrics() *NodeMetricsResponse {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.nodeMetrics
}

// QueryPVCMetrics implements UnifiedQuerier.
func (u *UnifiedExporter) QueryPVCMetrics() []PVCMetricsResponse {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.pvcMetrics
}
