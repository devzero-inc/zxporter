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
	// GPUCount is the number of distinct physical GPUs (deduped by DeviceUUID).
	// A MIG-partitioned physical GPU reports one DCGM row per MIG instance but
	// shares a single DeviceUUID across them, so this stays accurate whether or
	// not MIG is in use.
	GPUCount float64 `json:"gpu_count"`
	// GPUInstanceCount is the raw number of DCGM-reported rows (whole GPUs plus
	// every MIG instance) — i.e. the schedulable GPU-shaped unit count.
	GPUInstanceCount          float64  `json:"gpu_instance_count"`
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
	// GPUMigInstances is populated only when the node has MIG-partitioned
	// GPUs — nil/omitted for the overwhelming non-MIG majority.
	GPUMigInstances []GPUMigInstance `json:"gpu_mig_instances,omitempty"`
}

// SummarizeNodeGPU aggregates per-GPU metrics using the controller's existing
// node-level metric semantics.
//
// DCGM reports one row per physical GPU, but for a MIG-partitioned GPU it
// instead reports one row per MIG instance — all sharing the same DeviceUUID
// as the physical GPU they're carved from (confirmed against real DCGM
// exposition samples, see snapshot_types_migsim_test.go). Treating every row
// as an independent GPU inflates GPUCount by the MIG instance count (e.g. 8
// physical A100s partitioned into 19 MIG slices previously reported as 22
// "GPUs"). DCGM also never populates DCGM_FI_DEV_GPU_UTIL for MIG rows, so
// including them in the utilization average silently drags it toward zero
// whenever any MIG partitioning exists on the node.
func SummarizeNodeGPU(metrics []GPUMetric) *NodeGPUSummary {
	if len(metrics) == 0 {
		return nil
	}

	gpuInstanceCount := float64(len(metrics))

	var totalMemUsed, totalMemFree, totalPower float64
	var totalTemp, maxTemp, totalMemTemp, maxMemTemp float64
	var totalTensor, totalDram float64
	var totalPCIeTx, totalPCIeRx float64
	var totalGraphics float64

	gpuUUIDSet := make(map[string]bool)
	gpuModels := make(map[string]int)
	var migInstances []GPUMigInstance

	// GPU_UTIL is only reported per physical GPU; MIG-instance rows never
	// carry it, so they're excluded from the utilization average rather than
	// counted as 0% busy.
	var totalUtil, maxUtil float64
	var utilSamples float64

	for i, metric := range metrics {
		if metric.MIGInstanceID == "" {
			if utilSamples == 0 || metric.GPUUtilization > maxUtil {
				maxUtil = metric.GPUUtilization
			}
			totalUtil += metric.GPUUtilization
			utilSamples++
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

		if metric.MIGInstanceID != "" {
			migInstances = append(migInstances, GPUMigInstance{
				DeviceUUID:           metric.DeviceUUID,
				DeviceID:             metric.DeviceID,
				MIGProfile:           metric.MIGProfile,
				MIGInstanceID:        metric.MIGInstanceID,
				ModelName:            metric.ModelName,
				TensorActive:         metric.TensorActive,
				DRAMActive:           metric.DRAMActive,
				GraphicsEngineActive: metric.GraphicsEngineActive,
				FramebufferUsed:      metric.FramebufferUsed,
				FramebufferTotal:     metric.FramebufferUsed + metric.FramebufferFree,
			})
		}
	}

	// A node with only MIG rows has no non-MIG utilization sample to average;
	// fall back to including all rows rather than reporting a zero average.
	if utilSamples == 0 {
		for i, metric := range metrics {
			if i == 0 || metric.GPUUtilization > maxUtil {
				maxUtil = metric.GPUUtilization
			}
			totalUtil += metric.GPUUtilization
		}
		utilSamples = gpuInstanceCount
	}

	// Physical GPU count dedupes by DeviceUUID: a MIG-partitioned GPU's
	// instances all share the physical GPU's UUID. Rows with no UUID can't be
	// deduped against each other, so each counts as its own physical GPU.
	physicalGPUs := make(map[string]bool)
	gpuCount := float64(0)
	for _, metric := range metrics {
		if metric.DeviceUUID == "" {
			gpuCount++
			continue
		}
		if !physicalGPUs[metric.DeviceUUID] {
			physicalGPUs[metric.DeviceUUID] = true
			gpuCount++
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

	// Unlike GPU_UTIL, DCGM's profiling metrics (the DCGM_FI_PROF_* family —
	// tensor/DRAM/graphics-engine-active, temperature) ARE reported per MIG
	// instance with real values, not omitted: confirmed against real
	// captured DCGM output for a MIG-partitioned GPU (run-ai/fake-gpu-operator
	// design/samples/<2.9/mig/metrics/*.ini), where DCGM_FI_PROF_PIPE_TENSOR_ACTIVE,
	// DCGM_FI_PROF_DRAM_ACTIVE, and DCGM_FI_PROF_GR_ENGINE_ACTIVE all carry
	// distinct non-zero values per GPU_I_ID. So dividing by gpuInstanceCount
	// (all rows) here is correct and not subject to the same dilution this
	// fix addresses for GPUUtilizationAvg above.
	return &NodeGPUSummary{
		GPUCount:                  gpuCount,
		GPUInstanceCount:          gpuInstanceCount,
		GPUUtilizationAvg:         totalUtil / utilSamples,
		GPUUtilizationMax:         maxUtil,
		GPUMemoryUsedTotal:        totalMemUsed,
		GPUMemoryFreeTotal:        totalMemFree,
		GPUMemoryTotalMb:          totalMemUsed + totalMemFree,
		GPUPowerUsageTotal:        totalPower,
		GPUTemperatureAvg:         totalTemp / gpuInstanceCount,
		GPUTemperatureMax:         maxTemp,
		GPUMemoryTemperatureAvg:   totalMemTemp / gpuInstanceCount,
		GPUMemoryTemperatureMax:   maxMemTemp,
		GPUTensorUtilizationAvg:   totalTensor / gpuInstanceCount,
		GPUDramUtilizationAvg:     totalDram / gpuInstanceCount,
		GPUPCIeTxBytesTotal:       totalPCIeTx,
		GPUPCIeRxBytesTotal:       totalPCIeRx,
		GPUGraphicsUtilizationAvg: totalGraphics / gpuInstanceCount,
		GPUUsage:                  totalUtil / 100.0,
		GPUModels:                 modelSummary,
		GPUUUIDs:                  gpuUUIDs,
		GPUMigInstances:           migInstances,
	}
}
