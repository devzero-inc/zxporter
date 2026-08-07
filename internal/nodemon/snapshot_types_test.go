package nodemon_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// TestSummarizeNodeGPU_MIGPhysicalGPUDedup reproduces the accounting bug found
// while investigating a customer's MIG-partitioned p4d.24xlarge node: DCGM
// reports one row per MIG instance, all sharing the physical GPU's DeviceUUID,
// but before this fix SummarizeNodeGPU counted every row as its own GPU. A
// node with 3 whole A100s plus one A100 split into 2 MIG slices previously
// reported GPUCount=5 physical GPUs instead of the true 4.
func TestSummarizeNodeGPU_MIGPhysicalGPUDedup(t *testing.T) {
	r := require.New(t)

	metrics := []nodemon.GPUMetric{
		{ModelName: "NVIDIA A100", DeviceUUID: "GPU-0", GPUUtilization: 80, FramebufferUsed: 30_000, FramebufferFree: 10_000},
		{ModelName: "NVIDIA A100", DeviceUUID: "GPU-1", GPUUtilization: 60, FramebufferUsed: 20_000, FramebufferFree: 20_000},
		{ModelName: "NVIDIA A100", DeviceUUID: "GPU-2", GPUUtilization: 0, FramebufferUsed: 0, FramebufferFree: 40_000},
		// GPU-3 is MIG-partitioned into 2 slices. Real DCGM never reports
		// DCGM_FI_DEV_GPU_UTIL for MIG rows (see design doc from
		// run-ai/fake-gpu-operator), so GPUUtilization is left at its zero
		// value here exactly as the real exporter would leave it unset.
		{ModelName: "NVIDIA A100", DeviceUUID: "GPU-3", MIGProfile: "1g.5gb", MIGInstanceID: "8", FramebufferUsed: 6, FramebufferFree: 4_857},
		{ModelName: "NVIDIA A100", DeviceUUID: "GPU-3", MIGProfile: "3g.20gb", MIGInstanceID: "9", FramebufferUsed: 4_384, FramebufferFree: 15_616},
	}

	summary := nodemon.SummarizeNodeGPU(metrics)
	r.NotNil(summary)

	r.Equal(float64(4), summary.GPUCount, "4 physical GPUs, not 5 DCGM rows")
	r.Equal(float64(5), summary.GPUInstanceCount, "5 DCGM rows: 3 whole + 2 MIG instances")

	// Utilization average must only be taken over the 3 whole GPUs that
	// actually report it (80+60+0)/3 = 46.67 — including the 2 MIG rows'
	// zero-value GPUUtilization would have diluted this to 28.
	r.InDelta(46.666666, summary.GPUUtilizationAvg, 0.001)
	r.Equal(float64(80), summary.GPUUtilizationMax)

	// Framebuffer sums are unaffected: MIG slice memory is real physical
	// memory carved from the card, correctly additive alongside whole GPUs.
	r.Equal(float64(30_000+20_000+0+6+4_384), summary.GPUMemoryUsedTotal)
	r.Equal(float64(10_000+20_000+40_000+4_857+15_616), summary.GPUMemoryFreeTotal)

	// The 2 MIG rows on GPU-3 are carried through as GPUMigInstances; the 3
	// whole-GPU rows (no MIGInstanceID) are excluded.
	r.Len(summary.GPUMigInstances, 2)
	r.Equal(nodemon.GPUMigInstance{
		DeviceUUID: "GPU-3", MIGProfile: "1g.5gb", MIGInstanceID: "8", ModelName: "NVIDIA A100",
		FramebufferUsed: 6, FramebufferTotal: 6 + 4_857,
	}, summary.GPUMigInstances[0])
	r.Equal(nodemon.GPUMigInstance{
		DeviceUUID: "GPU-3", MIGProfile: "3g.20gb", MIGInstanceID: "9", ModelName: "NVIDIA A100",
		FramebufferUsed: 4_384, FramebufferTotal: 4_384 + 15_616,
	}, summary.GPUMigInstances[1])
}

// TestSummarizeNodeGPU_AllMIGFallsBackToAllRows covers a node that is 100%
// MIG-partitioned (no whole-GPU rows at all): there's no non-MIG utilization
// sample to average, so the average must fall back to including every row
// rather than silently reporting zero.
func TestSummarizeNodeGPU_AllMIGFallsBackToAllRows(t *testing.T) {
	r := require.New(t)

	metrics := []nodemon.GPUMetric{
		{ModelName: "NVIDIA A100", DeviceUUID: "GPU-0", MIGProfile: "1g.5gb", MIGInstanceID: "1", GPUUtilization: 40},
		{ModelName: "NVIDIA A100", DeviceUUID: "GPU-0", MIGProfile: "1g.5gb", MIGInstanceID: "2", GPUUtilization: 60},
	}

	summary := nodemon.SummarizeNodeGPU(metrics)
	r.NotNil(summary)

	r.Equal(float64(1), summary.GPUCount, "both MIG instances share one physical GPU UUID")
	r.Equal(float64(2), summary.GPUInstanceCount)
	r.Equal(float64(50), summary.GPUUtilizationAvg, "falls back to averaging all rows when none are non-MIG")
	r.Equal(float64(60), summary.GPUUtilizationMax)
}

// TestSummarizeNodeGPU_NoUUIDNotCollapsed guards against rows with no
// DeviceUUID (e.g. a DCGM exporter version that omits the UUID label) being
// incorrectly deduped into a single phantom GPU.
func TestSummarizeNodeGPU_NoUUIDNotCollapsed(t *testing.T) {
	r := require.New(t)

	metrics := []nodemon.GPUMetric{
		{ModelName: "NVIDIA A100", GPUUtilization: 10},
		{ModelName: "NVIDIA A100", GPUUtilization: 20},
	}

	summary := nodemon.SummarizeNodeGPU(metrics)
	r.NotNil(summary)
	r.Equal(float64(2), summary.GPUCount)
	r.Equal(float64(2), summary.GPUInstanceCount)
}
