package nodemon

import (
	"fmt"
	"time"
)

const SnapshotSchemaVersion = 1

type SnapshotSectionState string

const (
	SnapshotStateReady    SnapshotSectionState = "ready"
	SnapshotStateStale    SnapshotSectionState = "stale"
	SnapshotStateNotReady SnapshotSectionState = "not_ready"
	SnapshotStateDisabled SnapshotSectionState = "disabled"
)

type SnapshotSectionStatus struct {
	State       SnapshotSectionState `json:"state"`
	CollectedAt *time.Time           `json:"collected_at,omitempty"`
}

type NodeSnapshotSections struct {
	Node SnapshotSectionStatus `json:"node"`
	GPU  SnapshotSectionStatus `json:"gpu"`
}

type ContainerSnapshotSections struct {
	Containers SnapshotSectionStatus `json:"containers"`
	Runtime    SnapshotSectionStatus `json:"runtime"`
}

type NodeSnapshotResponse struct {
	SchemaVersion int                  `json:"schema_version"`
	NodeMetrics   *NodeMetricsResponse `json:"node_metrics,omitempty"`
	GPUSummary    *NodeGPUSummary      `json:"gpu_summary,omitempty"`
	Sections      NodeSnapshotSections `json:"sections"`
}

type ContainerSnapshotResponse struct {
	SchemaVersion    int                        `json:"schema_version"`
	ContainerMetrics []ContainerMetricsResponse `json:"container_metrics"`
	RuntimeMetrics   RuntimeMetrics             `json:"runtime_metrics"`
	Sections         ContainerSnapshotSections  `json:"sections"`
}

type NodeGPUSummary struct {
	GPUCount                  float64  `json:"gpu_count"`
	GPUUtilizationAvg         float64  `json:"gpu_utilization_avg"`
	GPUUtilizationMax         float64  `json:"gpu_utilization_max"`
	GPUMemoryUsedTotal        float64  `json:"gpu_memory_used_total"`
	GPUMemoryFreeTotal        float64  `json:"gpu_memory_free_total"`
	GPUMemoryTotalMb          float64  `json:"gpu_memory_total_mb"`
	GPUPowerUsageTotal        float64  `json:"gpu_power_usage_total"`
	GPUTemperatureAvg         float64  `json:"gpu_temperature_avg"`
	GPUTemperatureMax         float64  `json:"gpu_temperature_max"`
	GPUMemoryTemperatureAvg   float64  `json:"gpu_memory_temperature_avg"`
	GPUMemoryTemperatureMax   float64  `json:"gpu_memory_temperature_max"`
	GPUTensorUtilizationAvg   float64  `json:"gpu_tensor_utilization_avg"`
	GPUDramUtilizationAvg     float64  `json:"gpu_dram_utilization_avg"`
	GPUPCIeTxBytesTotal       float64  `json:"gpu_pcie_tx_bytes_total"`
	GPUPCIeRxBytesTotal       float64  `json:"gpu_pcie_rx_bytes_total"`
	GPUGraphicsUtilizationAvg float64  `json:"gpu_graphics_utilization_avg"`
	GPUUsage                  float64  `json:"gpu_usage"`
	GPUModels                 []string `json:"gpu_models"`
	GPUUUIDs                  []string `json:"gpu_uuids"`
}

// SummarizeNodeGPU aggregates per-GPU metrics using the controller's existing
// node-level metric semantics.
func SummarizeNodeGPU(metrics []GPUMetric) *NodeGPUSummary {
	if len(metrics) == 0 {
		return nil
	}

	gpuCount := float64(len(metrics))

	var totalUtil, maxUtil float64
	var totalMemUsed, totalMemFree, totalPower float64
	var totalTemp, maxTemp, totalMemTemp, maxMemTemp float64
	var totalTensor, totalDram float64
	var totalPCIeTx, totalPCIeRx float64
	var totalGraphics float64

	gpuUUIDSet := make(map[string]bool)
	gpuModels := make(map[string]int)

	for i, metric := range metrics {
		totalUtil += metric.GPUUtilization
		if i == 0 || metric.GPUUtilization > maxUtil {
			maxUtil = metric.GPUUtilization
		}

		totalMemUsed += metric.FramebufferUsed
		totalMemFree += metric.FramebufferFree
		totalPower += metric.PowerUsage

		totalTemp += metric.Temperature
		if i == 0 || metric.Temperature > maxTemp {
			maxTemp = metric.Temperature
		}

		totalMemTemp += metric.MemoryTemperature
		if i == 0 || metric.MemoryTemperature > maxMemTemp {
			maxMemTemp = metric.MemoryTemperature
		}

		totalTensor += metric.TensorActive
		totalDram += metric.DRAMActive
		totalPCIeTx += metric.PCIeTXBytes
		totalPCIeRx += metric.PCIeRXBytes
		totalGraphics += metric.GraphicsEngineActive

		if metric.DeviceUUID != "" {
			gpuUUIDSet[metric.DeviceUUID] = true
		}
		if metric.ModelName != "" {
			gpuModels[metric.ModelName]++
		}
	}

	gpuUUIDs := make([]string, 0, len(gpuUUIDSet))
	for uuid := range gpuUUIDSet {
		gpuUUIDs = append(gpuUUIDs, uuid)
	}
	modelSummary := make([]string, 0, len(gpuModels))
	for model, count := range gpuModels {
		modelSummary = append(modelSummary, fmt.Sprintf("%dx %s", count, model))
	}

	return &NodeGPUSummary{
		GPUCount:                  gpuCount,
		GPUUtilizationAvg:         totalUtil / gpuCount,
		GPUUtilizationMax:         maxUtil,
		GPUMemoryUsedTotal:        totalMemUsed,
		GPUMemoryFreeTotal:        totalMemFree,
		GPUMemoryTotalMb:          totalMemUsed + totalMemFree,
		GPUPowerUsageTotal:        totalPower,
		GPUTemperatureAvg:         totalTemp / gpuCount,
		GPUTemperatureMax:         maxTemp,
		GPUMemoryTemperatureAvg:   totalMemTemp / gpuCount,
		GPUMemoryTemperatureMax:   maxMemTemp,
		GPUTensorUtilizationAvg:   totalTensor / gpuCount,
		GPUDramUtilizationAvg:     totalDram / gpuCount,
		GPUPCIeTxBytesTotal:       totalPCIeTx,
		GPUPCIeRxBytesTotal:       totalPCIeRx,
		GPUGraphicsUtilizationAvg: totalGraphics / gpuCount,
		GPUUsage:                  totalUtil / 100.0,
		GPUModels:                 modelSummary,
		GPUUUIDs:                  gpuUUIDs,
	}
}
