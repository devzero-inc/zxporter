package gpuexporter

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// MetricsQuerier provides on-demand GPU metrics.
type MetricsQuerier interface {
	QueryMetrics(ctx context.Context) ([]GPUMetric, error)
}

// GPUMetricResponse is the JSON API contract for the /container/metrics endpoint.
type GPUMetricResponse struct {
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

type metricsFilter struct {
	Container string
	Pod       string
	Namespace string
	Node      string
}

func (f metricsFilter) matches(m *GPUMetricResponse) bool {
	if f.Container != "" && m.Container != f.Container {
		return false
	}
	if f.Pod != "" && m.Pod != f.Pod {
		return false
	}
	if f.Namespace != "" && m.Namespace != f.Namespace {
		return false
	}
	if f.Node != "" && m.NodeName != f.Node {
		return false
	}
	return true
}

type containerMetricsHandler struct {
	querier MetricsQuerier
	log     logr.Logger
}

// NewContainerMetricsHandler creates an HTTP handler for GET /container/metrics.
func NewContainerMetricsHandler(querier MetricsQuerier, log logr.Logger) http.Handler {
	return &containerMetricsHandler{
		querier: querier,
		log:     log.WithName("container-metrics-handler"),
	}
}

func (h *containerMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filter := metricsFilter{
		Container: r.URL.Query().Get("container"),
		Pod:       r.URL.Query().Get("pod"),
		Namespace: r.URL.Query().Get("namespace"),
		Node:      r.URL.Query().Get("node"),
	}

	gpuMetrics, err := h.querier.QueryMetrics(r.Context())
	if err != nil {
		h.log.Error(err, "Failed to query container metrics")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	responses := toResponse(gpuMetrics)
	result := filterMetrics(responses, filter)

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		h.log.Error(err, "Failed to encode container metrics response")
	}
}

func toResponse(metrics []GPUMetric) []GPUMetricResponse {
	if len(metrics) == 0 {
		return []GPUMetricResponse{}
	}

	responses := make([]GPUMetricResponse, len(metrics))
	for i, m := range metrics {
		responses[i] = GPUMetricResponse{
			NodeName:             m.NodeName,
			ModelName:            m.ModelName,
			Device:               m.Device,
			DeviceID:             m.DeviceID,
			DeviceUUID:           m.DeviceUUID,
			MIGProfile:           m.MIGProfile,
			MIGInstanceID:        m.MIGInstanceID,
			Pod:                  m.Pod,
			Container:            m.Container,
			Namespace:            m.Namespace,
			WorkloadName:         m.WorkloadName,
			WorkloadKind:         m.WorkloadKind,
			SMActive:             m.SMActive,
			SMOccupancy:          m.SMOccupancy,
			TensorActive:         m.TensorActive,
			DRAMActive:           m.DRAMActive,
			PCIeTXBytes:          m.PCIeTXBytes,
			PCIeRXBytes:          m.PCIeRXBytes,
			NVLinkTXBytes:        m.NVLinkTXBytes,
			NVLinkRXBytes:        m.NVLinkRXBytes,
			GraphicsEngineActive: m.GraphicsEngineActive,
			FramebufferTotal:     m.FramebufferTotal,
			FramebufferUsed:      m.FramebufferUsed,
			FramebufferFree:      m.FramebufferFree,
			PCIeLinkGen:          m.PCIeLinkGen,
			PCIeLinkWidth:        m.PCIeLinkWidth,
			Temperature:          m.Temperature,
			MemoryTemperature:    m.MemoryTemperature,
			PowerUsage:           m.PowerUsage,
			GPUUtilization:       m.GPUUtilization,
			IntPipeActive:        m.IntPipeActive,
			FP16PipeActive:       m.FP16PipeActive,
			FP32PipeActive:       m.FP32PipeActive,
			FP64PipeActive:       m.FP64PipeActive,
			ClocksEventReasons:   m.ClocksEventReasons,
			XIDErrors:            m.XIDErrors,
			PowerViolation:       m.PowerViolation,
			ThermalViolation:     m.ThermalViolation,
			SMClock:              m.SMClock,
			MemClock:             m.MemClock,
			Timestamp:            m.Timestamp,
		}
	}
	return responses
}

func filterMetrics(metrics []GPUMetricResponse, filter metricsFilter) []GPUMetricResponse {
	result := make([]GPUMetricResponse, 0, len(metrics))
	for i := range metrics {
		if filter.matches(&metrics[i]) {
			result = append(result, metrics[i])
		}
	}
	return result
}
