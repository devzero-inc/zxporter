package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const snapshotSchemaVersion = 1

type snapshotSectionState string

const (
	snapshotStateReady    snapshotSectionState = "ready"
	snapshotStateStale    snapshotSectionState = "stale"
	snapshotStateNotReady snapshotSectionState = "not_ready"
	snapshotStateDisabled snapshotSectionState = "disabled"
)

type snapshotSectionStatus struct {
	State       snapshotSectionState `json:"state"`
	CollectedAt *time.Time           `json:"collected_at,omitempty"`
}

type nodeSnapshotSections struct {
	Node snapshotSectionStatus `json:"node"`
	GPU  snapshotSectionStatus `json:"gpu"`
}

type nodeGPUSummary struct {
	GPUCount                  float64          `json:"gpu_count"`
	GPUInstanceCount          float64          `json:"gpu_instance_count"`
	GPUUtilizationAvg         float64          `json:"gpu_utilization_avg"`
	GPUUtilizationMax         float64          `json:"gpu_utilization_max"`
	GPUMemoryUsedTotal        float64          `json:"gpu_memory_used_total"`
	GPUMemoryFreeTotal        float64          `json:"gpu_memory_free_total"`
	GPUMemoryTotalMb          float64          `json:"gpu_memory_total_mb"`
	GPUPowerUsageTotal        float64          `json:"gpu_power_usage_total"`
	GPUTemperatureAvg         float64          `json:"gpu_temperature_avg"`
	GPUTemperatureMax         float64          `json:"gpu_temperature_max"`
	GPUMemoryTemperatureAvg   float64          `json:"gpu_memory_temperature_avg"`
	GPUMemoryTemperatureMax   float64          `json:"gpu_memory_temperature_max"`
	GPUTensorUtilizationAvg   float64          `json:"gpu_tensor_utilization_avg"`
	GPUDramUtilizationAvg     float64          `json:"gpu_dram_utilization_avg"`
	GPUPCIeTxBytesTotal       float64          `json:"gpu_pcie_tx_bytes_total"`
	GPUPCIeRxBytesTotal       float64          `json:"gpu_pcie_rx_bytes_total"`
	GPUGraphicsUtilizationAvg float64          `json:"gpu_graphics_utilization_avg"`
	GPUUsage                  float64          `json:"gpu_usage"`
	GPUModels                 []string         `json:"gpu_models"`
	GPUUUIDs                  []string         `json:"gpu_uuids"`
	GPUMigInstances           []gpuMigInstance `json:"gpu_mig_instances,omitempty"`
}

// gpuMigInstance mirrors nodemon.GPUMigInstance's JSON shape for decoding the
// composite /v2/node/snapshot response — this package decodes the wire
// payload independently rather than importing the nodemon package's types.
type gpuMigInstance struct {
	DeviceUUID    string `json:"device_uuid"`
	DeviceID      string `json:"device_id"`
	MIGProfile    string `json:"mig_profile"`
	MIGInstanceID string `json:"mig_instance_id"`
	ModelName     string `json:"model_name"`

	TensorActive         float64 `json:"tensor_active"`
	DRAMActive           float64 `json:"dram_active"`
	GraphicsEngineActive float64 `json:"graphics_engine_active"`
	FramebufferUsed      float64 `json:"framebuffer_used"`
	FramebufferTotal     float64 `json:"framebuffer_total"`
}

type nodeSnapshotResponse struct {
	SchemaVersion int                  `json:"schema_version"`
	NodeMetrics   *UnifiedNodeMetric   `json:"node_metrics,omitempty"`
	GPUSummary    *nodeGPUSummary      `json:"gpu_summary,omitempty"`
	Sections      nodeSnapshotSections `json:"sections"`
}

type containerSnapshotSections struct {
	Containers snapshotSectionStatus `json:"containers"`
	Runtime    snapshotSectionStatus `json:"runtime"`
}

type containerSnapshotResponse struct {
	SchemaVersion    int                       `json:"schema_version"`
	ContainerMetrics []UnifiedContainerMetric  `json:"container_metrics"`
	RuntimeMetrics   NodemonRuntimeMetrics     `json:"runtime_metrics"`
	Sections         containerSnapshotSections `json:"sections"`
}

type snapshotFallbackReason string

const (
	fallbackNotFound          snapshotFallbackReason = "not_found"
	fallbackNotReady          snapshotFallbackReason = "not_ready"
	fallbackUnsupportedSchema snapshotFallbackReason = "unsupported_schema"
	fallbackMalformed         snapshotFallbackReason = "malformed"
	fallbackOversized         snapshotFallbackReason = "oversized"
)

type snapshotFallbackError struct {
	reason snapshotFallbackReason
	cause  error
}

func (e *snapshotFallbackError) Error() string {
	if e.cause == nil {
		return fmt.Sprintf("composite snapshot fallback required: %s", e.reason)
	}
	return fmt.Sprintf("composite snapshot fallback required: %s: %v", e.reason, e.cause)
}

func (e *snapshotFallbackError) Unwrap() error {
	return e.cause
}

func fallbackReasonFromError(err error) (snapshotFallbackReason, bool) {
	var fallbackErr *snapshotFallbackError
	if !errors.As(err, &fallbackErr) {
		return "", false
	}
	return fallbackErr.reason, true
}

func decodeLimitedSnapshotJSON(body io.Reader, limit int64, dst any) error {
	// Stream-decode straight off the reader without buffering the whole body
	// (matching the legacy per-metric endpoints' memory profile — no io.ReadAll
	// "photocopy" of the payload). The LimitedReader permits up to limit+1 bytes:
	// the underlying reader can never yield more than the body actually holds, so
	// a body of <= limit bytes always leaves at least one byte of budget, and
	// draining the budget to zero unambiguously means the body exceeded limit.
	lr := &io.LimitedReader{R: body, N: limit + 1}
	decoder := json.NewDecoder(lr)

	if err := decoder.Decode(dst); err != nil {
		return classifyLimitedDecodeError(lr, err)
	}

	// Reject trailing data after the single JSON value.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return classifyLimitedDecodeError(lr, err)
	}

	// A clean single value — but the body itself may still have exceeded the cap
	// (e.g. a valid value of exactly limit+1 bytes).
	if lr.N <= 0 {
		return &snapshotFallbackError{reason: fallbackOversized}
	}
	return nil
}

// classifyLimitedDecodeError decides whether a decode/limit failure was caused
// by the body exceeding limit (oversized) or by genuinely malformed JSON. It
// drains any bytes the decoder left unread into io.Discard — which copies
// through a small pooled buffer and never retains the payload — so the size
// check stays content-agnostic (matching the original io.ReadAll length check,
// e.g. a limit+1 blob of non-JSON bytes is oversized, not malformed) without
// buffering the whole body on the valid hot path, where this is never reached.
func classifyLimitedDecodeError(lr *io.LimitedReader, cause error) error {
	if lr.N > 0 {
		_, _ = io.Copy(io.Discard, lr)
	}
	if lr.N <= 0 {
		return &snapshotFallbackError{reason: fallbackOversized, cause: cause}
	}
	return &snapshotFallbackError{reason: fallbackMalformed, cause: cause}
}

func snapshotSectionHasData(state snapshotSectionState) bool {
	return state == snapshotStateReady || state == snapshotStateStale
}

func (summary *nodeGPUSummary) downstreamMetrics() map[string]interface{} {
	if summary == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"GPUCount":                  summary.GPUCount,
		"GPUInstanceCount":          summary.GPUInstanceCount,
		"GPUUtilizationAvg":         summary.GPUUtilizationAvg,
		"GPUUtilizationMax":         summary.GPUUtilizationMax,
		"GPUMemoryUsedTotal":        summary.GPUMemoryUsedTotal,
		"GPUMemoryFreeTotal":        summary.GPUMemoryFreeTotal,
		"GPUMemoryTotalMb":          summary.GPUMemoryTotalMb,
		"GPUPowerUsageTotal":        summary.GPUPowerUsageTotal,
		"GPUTemperatureAvg":         summary.GPUTemperatureAvg,
		"GPUTemperatureMax":         summary.GPUTemperatureMax,
		"GPUMemoryTemperatureAvg":   summary.GPUMemoryTemperatureAvg,
		"GPUMemoryTemperatureMax":   summary.GPUMemoryTemperatureMax,
		"GPUTensorUtilizationAvg":   summary.GPUTensorUtilizationAvg,
		"GPUDramUtilizationAvg":     summary.GPUDramUtilizationAvg,
		"GPUPCIeTxBytesTotal":       summary.GPUPCIeTxBytesTotal,
		"GPUPCIeRxBytesTotal":       summary.GPUPCIeRxBytesTotal,
		"GPUGraphicsUtilizationAvg": summary.GPUGraphicsUtilizationAvg,
		"GPUUsage":                  summary.GPUUsage,
		"GPUModels":                 append([]string(nil), summary.GPUModels...),
		"GPUUUIDs":                  append([]string(nil), summary.GPUUUIDs...),
		"GPUMigInstances":           append([]gpuMigInstance(nil), summary.GPUMigInstances...),
	}
}
