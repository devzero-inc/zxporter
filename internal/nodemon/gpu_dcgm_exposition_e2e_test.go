package nodemon_test

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// These tests drive the real production parsing path (the same
// expfmt.TextParser used by Scraper.Scrape in scraper.go) with literal DCGM
// exposition text shaped like the samples published by
// https://github.com/run-ai/fake-gpu-operator (design/samples/*/mig/metrics/
// *.ini and internal/status-exporter/export/metrics/metrics.go) — an
// open-source DCGM/MIG simulator whose exposition format and label set were
// reverse-engineered from real NVIDIA DCGM exporter output. Using that shape
// here (rather than hand-built dto.Metric structs, as mapper_test.go does)
// exercises the full text-format parse -> MapToGPUMetrics -> SummarizeNodeGPU
// pipeline the way a real scrape would, and pins down two real-DCGM behaviors
// that drove the MIG accounting bug fixed in snapshot_types.go:
//  1. every MIG instance carved from one physical GPU reports the SAME UUID
//     label, differentiated only by GPU_I_ID/GPU_I_PROFILE;
//  2. DCGM_FI_DEV_GPU_UTIL is never exported for MIG-instance rows at all
//     (see design/MIG Metrics.md: "DCGM_FI_DEV_GPU_UTIL metric should *not*
//     be exported" once a node has dynamic MIG enabled).
func parseExposition(t *testing.T, text string) nodemon.MetricFamilyMap {
	t.Helper()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	require.NoError(t, err)
	return families
}

func summarizeExposition(t *testing.T, text string) *nodemon.NodeGPUSummary {
	t.Helper()
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)
	mapper := nodemon.NewMapper("test-node", nil, log)

	families := parseExposition(t, text)
	metrics := mapper.MapToGPUMetrics(context.Background(), []nodemon.MetricFamilyMap{families})
	return nodemon.SummarizeNodeGPU(metrics)
}

// TestGPUExposition_NonMIG_TwoWholeGPUs mimics a non-MIG p4d-style node: two
// whole A100s, one running a workload, one idle. Modeled on
// fake-gpu-operator's internal/status-exporter/export/metrics/metrics.go
// gauge set (DCGM_FI_DEV_GPU_UTIL / DCGM_FI_DEV_FB_USED / DCGM_FI_DEV_FB_FREE)
// plus the temperature/power gauges present in every real DCGM scrape.
func TestGPUExposition_NonMIG_TwoWholeGPUs(t *testing.T) {
	const exposition = `
# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-11111111-1111-1111-1111-111111111111",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-a",container="trainer",namespace="ml",pod="train-0"} 87
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-22222222-2222-2222-2222-222222222222",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-a",container="",namespace="",pod=""} 0
# HELP DCGM_FI_DEV_FB_USED Framebuffer memory used (in MiB).
# TYPE DCGM_FI_DEV_FB_USED gauge
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-11111111-1111-1111-1111-111111111111",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-a",container="trainer",namespace="ml",pod="train-0"} 38000
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-22222222-2222-2222-2222-222222222222",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-a",container="",namespace="",pod=""} 6
# HELP DCGM_FI_DEV_FB_FREE Framebuffer memory free (in MiB).
# TYPE DCGM_FI_DEV_FB_FREE gauge
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-11111111-1111-1111-1111-111111111111",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-a",container="trainer",namespace="ml",pod="train-0"} 2000
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-22222222-2222-2222-2222-222222222222",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-a",container="",namespace="",pod=""} 39994
`
	r := require.New(t)
	summary := summarizeExposition(t, exposition)
	r.NotNil(summary)

	r.Equal(float64(2), summary.GPUCount)
	r.Equal(float64(2), summary.GPUInstanceCount)
	r.Equal(float64(43.5), summary.GPUUtilizationAvg, "(87+0)/2 across the 2 whole GPUs")
	r.Equal(float64(87), summary.GPUUtilizationMax)
	r.Equal(float64(38000+6), summary.GPUMemoryUsedTotal)
}

// TestGPUExposition_MIG_PartitionedGPUAlongsideWhole mimics the kind of node
// this bug was found on: one whole A100 running a workload, plus a
// second physical A100 dynamically MIG-partitioned into two instances
// (1g.5gb idle, 3g.20gb active) — both MIG rows share the physical GPU's
// UUID and carry empty pod/namespace/container labels and no
// DCGM_FI_DEV_GPU_UTIL series at all, exactly as fake-gpu-operator's
// "design/samples/<2.9/mig/metrics/*.ini" fixtures and MIG Metrics.md
// document for real DCGM.
func TestGPUExposition_MIG_PartitionedGPUAlongsideWhole(t *testing.T) {
	const exposition = `
# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-11111111-1111-1111-1111-111111111111",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-b",container="trainer",namespace="ml",pod="train-0"} 92
# HELP DCGM_FI_DEV_FB_USED Framebuffer memory used (in MiB).
# TYPE DCGM_FI_DEV_FB_USED gauge
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-11111111-1111-1111-1111-111111111111",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-b",container="trainer",namespace="ml",pod="train-0"} 39000
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-7099682b-a20d-2bcc-74ab-b9060454d81e",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="8",Hostname="node-b",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-7099682b-a20d-2bcc-74ab-b9060454d81e",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="9",Hostname="node-b",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4384
# HELP DCGM_FI_DEV_FB_FREE Framebuffer memory free (in MiB).
# TYPE DCGM_FI_DEV_FB_FREE gauge
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-11111111-1111-1111-1111-111111111111",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="node-b",container="trainer",namespace="ml",pod="train-0"} 1000
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-7099682b-a20d-2bcc-74ab-b9060454d81e",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="8",Hostname="node-b",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-7099682b-a20d-2bcc-74ab-b9060454d81e",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="9",Hostname="node-b",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 15616
`
	r := require.New(t)
	summary := summarizeExposition(t, exposition)
	r.NotNil(summary)

	// 2 physical GPUs (1 whole + 1 MIG-partitioned), 3 DCGM rows (1 whole + 2
	// MIG instances). Before the fix this reported GPUCount=3.
	r.Equal(float64(2), summary.GPUCount)
	r.Equal(float64(3), summary.GPUInstanceCount)

	// Only the whole GPU reports DCGM_FI_DEV_GPU_UTIL; the average must not
	// be diluted by the 2 unreported (zero-valued) MIG rows down to 30.67.
	r.Equal(float64(92), summary.GPUUtilizationAvg)
	r.Equal(float64(92), summary.GPUUtilizationMax)

	// Framebuffer sums stay a straight sum across all 3 rows — MIG slice
	// memory is real physical memory, correctly additive.
	r.Equal(float64(39000+6+4384), summary.GPUMemoryUsedTotal)
	r.Equal(float64(1000+4857+15616), summary.GPUMemoryFreeTotal)

	// The 2 MIG rows are carried through as GPUMigInstances with their real
	// per-instance identity and framebuffer values. Looked up by
	// MIGInstanceID rather than slice position: the exposition-parsing
	// pipeline correlates DCGM label sets via a map internally, so the
	// resulting slice order isn't guaranteed to match the exposition text's
	// row order (this flaked in CI on the interpretation "9" then "8").
	r.Len(summary.GPUMigInstances, 2)
	inst8 := migInstanceByID(t, summary.GPUMigInstances, "8")
	r.Equal("1g.5gb", inst8.MIGProfile)
	r.Equal(float64(6), inst8.FramebufferUsed)
	inst9 := migInstanceByID(t, summary.GPUMigInstances, "9")
	r.Equal("3g.20gb", inst9.MIGProfile)
	r.Equal(float64(4384), inst9.FramebufferUsed)

	// Every MIG row's pod/namespace/container come back empty, matching real
	// DCGM MIG behavior — mapper.go must not have tried (and failed) to
	// resolve a workload for them.
	for _, m := range mapper(t, exposition) {
		if m.MIGInstanceID != "" {
			r.Empty(m.Pod)
			r.Empty(m.Namespace)
			r.Empty(m.Container)
		}
	}
}

// TestGPUExposition_MIG_EightPhysicalGPUsTwentyTwoRows reproduces the shape of a real customer node that surfaced this
// bug on a p4d.24xlarge (8x A100): 3 whole GPUs (2 actively claimed by
// application workloads, 1 idle/unrequested) plus 5
// MIG-partitioned physical GPUs carrying 12x "1g.5gb" + 7x "3g.20gb"
// instances, matching that node's live NodePool status.resources
// (nvidia.com/gpu: 3, nvidia.com/mig-1g.5gb: 12, nvidia.com/mig-3g.20gb: 7)
// exactly — 8 physical GPUs, 22 DCGM rows. All 19 MIG slices are idle: the
// investigation confirmed neither workload on that node requests a
// MIG-typed resource, so they're unclaimed capacity, not active load. Same
// fixture as test/e2e/gpu_mig_test.go's live-cluster spec; this is the
// no-cluster-needed fast-signal twin.
func TestGPUExposition_MIG_EightPhysicalGPUsTwentyTwoRows(t *testing.T) {
	//nolint:lll // real DCGM exposition text — the label set can't be wrapped without breaking the fixture
	const exposition = `
# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-a0000000-0000-0000-0000-000000000000",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="model-eval",namespace="batch-ml",pod="model-eval-job-7d9f4c8b6-x2k9p"} 90
DCGM_FI_DEV_GPU_UTIL{gpu="1",UUID="GPU-a0000001-0000-0000-0000-000000000000",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="inference",namespace="serving-ml",pod="inference-svc-6b8f9c7d5-m4n2q"} 15
DCGM_FI_DEV_GPU_UTIL{gpu="2",UUID="GPU-a0000002-0000-0000-0000-000000000000",device="nvidia2",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="",namespace="",pod=""} 0
# HELP DCGM_FI_DEV_FB_USED Framebuffer memory used (in MiB).
# TYPE DCGM_FI_DEV_FB_USED gauge
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-a0000000-0000-0000-0000-000000000000",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="model-eval",namespace="batch-ml",pod="model-eval-job-7d9f4c8b6-x2k9p"} 39000
DCGM_FI_DEV_FB_USED{gpu="1",UUID="GPU-a0000001-0000-0000-0000-000000000000",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="inference",namespace="serving-ml",pod="inference-svc-6b8f9c7d5-m4n2q"} 36000
DCGM_FI_DEV_FB_USED{gpu="2",UUID="GPU-a0000002-0000-0000-0000-000000000000",device="nvidia2",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="",namespace="",pod=""} 0
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="8",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="9",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="10",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="11",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="12",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="13",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="14",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="15",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="16",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="17",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="18",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="19",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="20",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="21",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="22",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="23",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="24",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="25",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 6
DCGM_FI_DEV_FB_USED{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="26",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 18
# HELP DCGM_FI_DEV_FB_FREE Framebuffer memory free (in MiB).
# TYPE DCGM_FI_DEV_FB_FREE gauge
DCGM_FI_DEV_FB_FREE{gpu="0",UUID="GPU-a0000000-0000-0000-0000-000000000000",device="nvidia0",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="model-eval",namespace="batch-ml",pod="model-eval-job-7d9f4c8b6-x2k9p"} 1536
DCGM_FI_DEV_FB_FREE{gpu="1",UUID="GPU-a0000001-0000-0000-0000-000000000000",device="nvidia1",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="inference",namespace="serving-ml",pod="inference-svc-6b8f9c7d5-m4n2q"} 4536
DCGM_FI_DEV_FB_FREE{gpu="2",UUID="GPU-a0000002-0000-0000-0000-000000000000",device="nvidia2",modelName="NVIDIA A100-SXM4-40GB",Hostname="gpu-mig-fixture-node",container="",namespace="",pod=""} 40536
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="8",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="9",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="3",UUID="GPU-b0000000-0000-0000-0000-000000000000",device="nvidia3",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="10",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="11",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="12",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="4",UUID="GPU-b0000001-0000-0000-0000-000000000000",device="nvidia4",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="13",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="14",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="15",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="16",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="17",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="5",UUID="GPU-b0000002-0000-0000-0000-000000000000",device="nvidia5",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="18",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="19",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="20",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="21",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="6",UUID="GPU-b0000003-0000-0000-0000-000000000000",device="nvidia6",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="22",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="23",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="24",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="1g.5gb",GPU_I_ID="25",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 4857
DCGM_FI_DEV_FB_FREE{gpu="7",UUID="GPU-b0000004-0000-0000-0000-000000000000",device="nvidia7",modelName="NVIDIA A100-SXM4-40GB",GPU_I_PROFILE="3g.20gb",GPU_I_ID="26",Hostname="gpu-mig-fixture-node",DCGM_FI_DRIVER_VERSION="520.56.06",container="",namespace="",pod=""} 20462
`
	r := require.New(t)
	summary := summarizeExposition(t, exposition)
	r.NotNil(summary)

	// 8 physical GPUs (3 whole + 5 MIG-partitioned), 22 DCGM rows (3 whole +
	// 19 MIG instances: 12x 1g.5gb + 7x 3g.20gb). Before the fix this
	// reported GPUCount=22, matching the "gpu_capacity_sum≈22 instead of 8"
	// discrepancy found on the real customer node.
	r.Equal(float64(8), summary.GPUCount)
	r.Equal(float64(22), summary.GPUInstanceCount)

	// Only the 3 whole GPUs report DCGM_FI_DEV_GPU_UTIL: (90+15+0)/3=35. The
	// pre-fix average, diluted across all 22 rows, was 105/22=4.77.
	r.Equal(float64(35), summary.GPUUtilizationAvg)
	r.Equal(float64(90), summary.GPUUtilizationMax)

	// Framebuffer sums stay a straight sum across all 22 rows — MIG slice
	// memory is real physical memory, correctly additive.
	r.Equal(float64(75198), summary.GPUMemoryUsedTotal)
	r.Equal(float64(248126), summary.GPUMemoryFreeTotal)

	// All 19 MIG rows are carried through as GPUMigInstances, matching
	// test/e2e/gpu_mig_test.go's live-cluster assertion on the same fixture.
	r.Len(summary.GPUMigInstances, 19)
	for _, mi := range summary.GPUMigInstances {
		r.NotEmpty(mi.DeviceUUID)
		r.NotEmpty(mi.MIGInstanceID)
		r.NotEmpty(mi.MIGProfile)
	}
}

func mapper(t *testing.T, exposition string) []nodemon.GPUMetric {
	t.Helper()
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)
	m := nodemon.NewMapper("test-node", nil, log)
	families := parseExposition(t, exposition)
	return m.MapToGPUMetrics(context.Background(), []nodemon.MetricFamilyMap{families})
}

// migInstanceByID finds a MIG instance by ID rather than assuming slice
// position, since GPUMigInstances' order isn't guaranteed by the exposition-
// parsing pipeline (it correlates DCGM label sets via a map internally).
func migInstanceByID(t *testing.T, instances []nodemon.GPUMigInstance, id string) nodemon.GPUMigInstance {
	t.Helper()
	for _, inst := range instances {
		if inst.MIGInstanceID == id {
			return inst
		}
	}
	t.Fatalf("no MIG instance with ID %q found in %+v", id, instances)
	return nodemon.GPUMigInstance{}
}
