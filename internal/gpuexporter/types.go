package gpuexporter

import (
	"time"

	dto "github.com/prometheus/client_model/go"
)

// MetricName identifies a DCGM metric by its Prometheus metric name.
type MetricName = string

// DCGM metric names scraped from the DCGM exporter.
const (
	MetricStreamingMultiProcessorActive    MetricName = "DCGM_FI_PROF_SM_ACTIVE"
	MetricStreamingMultiProcessorOccupancy MetricName = "DCGM_FI_PROF_SM_OCCUPANCY"
	MetricStreamingMultiProcessorTensor    MetricName = "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE"
	MetricDRAMActive                       MetricName = "DCGM_FI_PROF_DRAM_ACTIVE"
	MetricPCIeTXBytes                      MetricName = "DCGM_FI_PROF_PCIE_TX_BYTES"
	MetricPCIeRXBytes                      MetricName = "DCGM_FI_PROF_PCIE_RX_BYTES"
	MetricNVLinkTXBytes                    MetricName = "DCGM_FI_PROF_NVLINK_TX_BYTES"
	MetricNVLinkRXBytes                    MetricName = "DCGM_FI_PROF_NVLINK_RX_BYTES"
	MetricGraphicsEngineActive             MetricName = "DCGM_FI_PROF_GR_ENGINE_ACTIVE"
	MetricFrameBufferTotal                 MetricName = "DCGM_FI_DEV_FB_TOTAL"
	MetricFrameBufferUsed                  MetricName = "DCGM_FI_DEV_FB_USED"
	MetricFrameBufferFree                  MetricName = "DCGM_FI_DEV_FB_FREE"
	MetricPCIeLinkGen                      MetricName = "DCGM_FI_DEV_PCIE_LINK_GEN"
	MetricPCIeLinkWidth                    MetricName = "DCGM_FI_DEV_PCIE_LINK_WIDTH"
	MetricGPUTemperature                   MetricName = "DCGM_FI_DEV_GPU_TEMP"
	MetricMemoryTemperature                MetricName = "DCGM_FI_DEV_MEMORY_TEMP"
	MetricPowerUsage                       MetricName = "DCGM_FI_DEV_POWER_USAGE"
	MetricGPUUtilization                   MetricName = "DCGM_FI_DEV_GPU_UTIL"
	MetricIntPipeActive                    MetricName = "DCGM_FI_PROF_PIPE_INT_ACTIVE"
	MetricFloat16PipeActive                MetricName = "DCGM_FI_PROF_PIPE_FP16_ACTIVE"
	MetricFloat32PipeActive                MetricName = "DCGM_FI_PROF_PIPE_FP32_ACTIVE"
	MetricFloat64PipeActive                MetricName = "DCGM_FI_PROF_PIPE_FP64_ACTIVE"
	MetricClocksEventReasons               MetricName = "DCGM_FI_DEV_CLOCKS_EVENT_REASONS"
	MetricXIDErrors                        MetricName = "DCGM_FI_DEV_XID_ERRORS"
	MetricPowerViolation                   MetricName = "DCGM_FI_DEV_POWER_VIOLATION"
	MetricThermalViolation                 MetricName = "DCGM_FI_DEV_THERMAL_VIOLATION"
	MetricSMClock                          MetricName = "DCGM_FI_DEV_SM_CLOCK"
	MetricMemClock                         MetricName = "DCGM_FI_DEV_MEM_CLOCK"
)

// EnabledMetrics is the set of DCGM metrics to scrape and process.
var EnabledMetrics = map[MetricName]struct{}{
	MetricStreamingMultiProcessorActive:    {},
	MetricStreamingMultiProcessorOccupancy: {},
	MetricStreamingMultiProcessorTensor:    {},
	MetricDRAMActive:                       {},
	MetricPCIeTXBytes:                      {},
	MetricPCIeRXBytes:                      {},
	MetricNVLinkTXBytes:                    {},
	MetricNVLinkRXBytes:                    {},
	MetricGraphicsEngineActive:             {},
	MetricFrameBufferTotal:                 {},
	MetricFrameBufferUsed:                  {},
	MetricFrameBufferFree:                  {},
	MetricPCIeLinkGen:                      {},
	MetricPCIeLinkWidth:                    {},
	MetricGPUTemperature:                   {},
	MetricMemoryTemperature:                {},
	MetricPowerUsage:                       {},
	MetricGPUUtilization:                   {},
	MetricIntPipeActive:                    {},
	MetricFloat16PipeActive:                {},
	MetricFloat32PipeActive:                {},
	MetricFloat64PipeActive:                {},
	MetricClocksEventReasons:               {},
	MetricXIDErrors:                        {},
	MetricPowerViolation:                   {},
	MetricThermalViolation:                 {},
	MetricSMClock:                          {},
	MetricMemClock:                         {},
}

// MetricFamilyMap maps a Prometheus metric name to its parsed metric family.
type MetricFamilyMap map[string]*dto.MetricFamily

// GPUMetric represents a single GPU's metrics for a container.
type GPUMetric struct {
	NodeName      string `json:"node_name"`
	ModelName     string `json:"model_name"`
	Device        string `json:"device"`
	DeviceID      string `json:"device_id"`
	DeviceUUID    string `json:"device_uuid"`
	MIGProfile    string `json:"mig_profile,omitempty"`
	MIGInstanceID string `json:"mig_instance_id,omitempty"`

	Pod          string `json:"pod"`
	Container    string `json:"container"`
	Namespace    string `json:"namespace"`
	WorkloadName string `json:"workload_name,omitempty"`
	WorkloadKind string `json:"workload_kind,omitempty"`

	SMActive             float64 `json:"sm_active"`
	SMOccupancy          float64 `json:"sm_occupancy"`
	TensorActive         float64 `json:"tensor_active"`
	DRAMActive           float64 `json:"dram_active"`
	PCIeTXBytes          float64 `json:"pcie_tx_bytes"`
	PCIeRXBytes          float64 `json:"pcie_rx_bytes"`
	NVLinkTXBytes        float64 `json:"nvlink_tx_bytes"`
	NVLinkRXBytes        float64 `json:"nvlink_rx_bytes"`
	GraphicsEngineActive float64 `json:"graphics_engine_active"`
	FramebufferTotal     float64 `json:"framebuffer_total"`
	FramebufferUsed      float64 `json:"framebuffer_used"`
	FramebufferFree      float64 `json:"framebuffer_free"`
	PCIeLinkGen          float64 `json:"pcie_link_gen"`
	PCIeLinkWidth        float64 `json:"pcie_link_width"`
	Temperature          float64 `json:"temperature"`
	MemoryTemperature    float64 `json:"memory_temperature"`
	PowerUsage           float64 `json:"power_usage"`
	GPUUtilization       float64 `json:"gpu_utilization"`
	IntPipeActive        float64 `json:"int_pipe_active"`
	FP16PipeActive       float64 `json:"fp16_pipe_active"`
	FP32PipeActive       float64 `json:"fp32_pipe_active"`
	FP64PipeActive       float64 `json:"fp64_pipe_active"`
	ClocksEventReasons   float64 `json:"clocks_event_reasons"`
	XIDErrors            float64 `json:"xid_errors"`
	PowerViolation       float64 `json:"power_violation"`
	ThermalViolation     float64 `json:"thermal_violation"`
	SMClock              float64 `json:"sm_clock"`
	MemClock             float64 `json:"mem_clock"`

	Timestamp time.Time `json:"timestamp"`
}
