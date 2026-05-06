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

	nodeNetRates      *RateCalculator // for computing node network byte rates from cumulative counters

	mu                sync.RWMutex
	containerMetrics  []ContainerMetricsResponse
	nodeMetrics       *NodeMetricsResponse
	pvcMetrics        []PVCMetricsResponse
	lastCollected     time.Time
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

	// Fetch stats/summary
	var stats *StatsSummary
	if u.statsPoller != nil {
		var err error
		stats, err = u.statsPoller.Poll(ctx)
		if err != nil {
			u.log.Error(err, "Failed to poll stats/summary")
		}
	}

	// Fetch cAdvisor rates
	var cadvisorMetrics []CAdvisorContainerMetrics
	if u.cadvisorScraper != nil {
		var err error
		cadvisorMetrics, err = u.cadvisorScraper.Scrape(ctx, now)
		if err != nil {
			u.log.Error(err, "Failed to scrape cAdvisor")
		}
	}

	// Fetch GPU metrics
	var gpuMetrics []GPUMetric
	if u.gpuExporter != nil {
		var err error
		gpuMetrics, err = u.gpuExporter.QueryMetrics(ctx)
		if err != nil {
			u.log.V(1).Info("Failed to query GPU metrics", "error", err)
		}
	}

	// Index cAdvisor metrics by pod/container
	cadvisorIndex := make(map[string]*CAdvisorContainerMetrics)
	for i := range cadvisorMetrics {
		m := &cadvisorMetrics[i]
		key := m.Namespace + "/" + m.Pod + "/" + m.Container
		cadvisorIndex[key] = m
	}

	// Index GPU metrics by pod/container
	gpuIndex := make(map[string]*GPUMetric)
	for i := range gpuMetrics {
		m := &gpuMetrics[i]
		key := m.Namespace + "/" + m.Pod + "/" + m.Container
		gpuIndex[key] = m
	}

	// Build container metrics from stats/summary
	var containerResults []ContainerMetricsResponse
	var pvcResults []PVCMetricsResponse

	if stats != nil {
		for _, pod := range stats.Pods {
			// Aggregate pod-level network bytes
			var rxBytes, txBytes uint64
			for _, iface := range pod.Network.Interfaces {
				if iface.RxBytes != nil {
					rxBytes += *iface.RxBytes
				}
				if iface.TxBytes != nil {
					txBytes += *iface.TxBytes
				}
			}

			for _, container := range pod.Containers {
				resp := ContainerMetricsResponse{
					NodeName:  u.nodeName,
					Namespace: pod.PodRef.Namespace,
					Pod:       pod.PodRef.Name,
					Container: container.Name,
					Timestamp: now,
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

				containerResults = append(containerResults, resp)
			}

			// PVC metrics from volume stats
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
				pvcResults = append(pvcResults, pvc)
			}
		}
	}

	// Build node metrics by aggregating cAdvisor per-container rates
	nodeResult := &NodeMetricsResponse{
		NodeName:  u.nodeName,
		Timestamp: now,
	}
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
	// Node-level CPU/memory from stats/summary (includes system processes, not just containers)
	if stats != nil {
		if stats.Node.CPU.UsageNanoCores != nil {
			nodeResult.CPUUsageNanoCores = *stats.Node.CPU.UsageNanoCores
		}
		if stats.Node.Memory.WorkingSetBytes != nil {
			nodeResult.MemoryWorkingSet = *stats.Node.Memory.WorkingSetBytes
		}
	}

	// Node-level network bytes rate from stats/summary node section
	if stats != nil {
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
