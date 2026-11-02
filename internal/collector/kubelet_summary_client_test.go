package collector

import (
	"testing"
)

const sampleSummaryJSON = `{
  "node": {
    "nodeName": "node-a",
    "cpu": {"time": "2026-06-17T10:00:00Z", "usageNanoCores": 1500000000},
    "memory": {"workingSetBytes": 2147483648},
    "fs": {
      "availableBytes": 94501658624, "capacityBytes": 107294470144, "usedBytes": 12792811520,
      "inodesFree": 52254168, "inodes": 52422640, "inodesUsed": 168472
    },
    "runtime": {
      "imageFs": {
        "availableBytes": 94501658624, "capacityBytes": 107294470144, "usedBytes": 6701690880,
        "inodesFree": 52254168, "inodes": 52422640, "inodesUsed": 78769
      }
    }
  },
  "pods": [
    {
      "podRef": {"name": "web-0", "namespace": "default"},
      "containers": [
        {
          "name": "app",
          "cpu": {"time": "2026-06-17T10:00:00Z", "usageNanoCores": 250000000},
          "memory": {"workingSetBytes": 104857600, "usageBytes": 120000000, "rssBytes": 90000000}
        },
        {
          "name": "sidecar",
          "cpu": {"usageNanoCores": 50000000},
          "memory": {"workingSetBytes": 10485760}
        }
      ]
    }
  ]
}`

func TestParseKubeletSummary_NodeMetric(t *testing.T) {
	summary, err := parseKubeletSummary([]byte(sampleSummaryJSON), "node-a")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	nm := nodeMetricFromSummary("node-a", summary)
	if nm.NodeName != "node-a" {
		t.Errorf("node name = %q, want node-a", nm.NodeName)
	}
	if nm.CPUUsageNanoCores != 1500000000 {
		t.Errorf("cpu = %d, want 1500000000", nm.CPUUsageNanoCores)
	}
	if nm.MemoryWorkingSet != 2147483648 {
		t.Errorf("mem = %d, want 2147483648", nm.MemoryWorkingSet)
	}
	if nm.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp from cpu.time")
	}
}

func TestParseKubeletSummary_ContainerMetrics(t *testing.T) {
	summary, err := parseKubeletSummary([]byte(sampleSummaryJSON), "node-a")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	cms := containerMetricsFromSummary("node-a", summary)
	if len(cms) != 2 {
		t.Fatalf("got %d container metrics, want 2", len(cms))
	}

	app := cms[0]
	if app.Namespace != "default" || app.Pod != "web-0" || app.Container != "app" {
		t.Errorf("unexpected identity: %+v", app)
	}
	if app.NodeName != "node-a" {
		t.Errorf("node name = %q, want node-a", app.NodeName)
	}
	if app.CPUUsageNanoCores != 250000000 {
		t.Errorf("app cpu = %d, want 250000000", app.CPUUsageNanoCores)
	}
	if app.MemoryWorkingSet != 104857600 || app.MemoryUsageBytes != 120000000 || app.MemoryRSSBytes != 90000000 {
		t.Errorf("app memory mismatch: ws=%d usage=%d rss=%d", app.MemoryWorkingSet, app.MemoryUsageBytes, app.MemoryRSSBytes)
	}
	// The cache/swap/active/inactive breakdown is unavailable on the kubelet
	// Summary-API fallback path (it requires the nodemon cAdvisor scrape), so
	// cache and swap stay 0.
	if app.MemoryCacheBytes != 0 || app.MemorySwapBytes != 0 {
		t.Errorf("app expected zero cache/swap, got cache=%d swap=%d",
			app.MemoryCacheBytes, app.MemorySwapBytes)
	}

	// sidecar has no memory usage/rss reported — those stay zero, no panic on nil.
	side := cms[1]
	if side.Container != "sidecar" || side.CPUUsageNanoCores != 50000000 || side.MemoryWorkingSet != 10485760 {
		t.Errorf("sidecar mismatch: %+v", side)
	}
	if side.MemoryUsageBytes != 0 || side.MemoryRSSBytes != 0 {
		t.Errorf("sidecar expected zero usage/rss, got usage=%d rss=%d", side.MemoryUsageBytes, side.MemoryRSSBytes)
	}
}

func TestParseKubeletSummary_NodeStorage(t *testing.T) {
	summary, err := parseKubeletSummary([]byte(sampleSummaryJSON), "node-a")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	nm := nodeMetricFromSummary("node-a", summary)
	if nm.NodefsUsedBytes != 12792811520 {
		t.Errorf("NodefsUsedBytes = %d, want 12792811520", nm.NodefsUsedBytes)
	}
	if nm.NodefsCapacityBytes != 107294470144 {
		t.Errorf("NodefsCapacityBytes = %d, want 107294470144", nm.NodefsCapacityBytes)
	}
	if nm.NodefsInodesTotal != 52422640 {
		t.Errorf("NodefsInodesTotal = %d, want 52422640", nm.NodefsInodesTotal)
	}
	if nm.ImagefsUsedBytes != 6701690880 {
		t.Errorf("ImagefsUsedBytes = %d, want 6701690880", nm.ImagefsUsedBytes)
	}
	if !nm.ImagefsSharedDevice {
		t.Error("ImagefsSharedDevice = false, want true")
	}
}

// The kubelet fallback path must be usable for a node whose summary omits the
// storage sections entirely — older kubelets, or a runtime that reports none.
func TestParseKubeletSummary_NodeStorageAbsent(t *testing.T) {
	const noFS = `{"node": {"nodeName": "node-a", "cpu": {"usageNanoCores": 1}}, "pods": []}`
	summary, err := parseKubeletSummary([]byte(noFS), "node-a")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	nm := nodeMetricFromSummary("node-a", summary)
	if nm.NodefsCapacityBytes != 0 || nm.ImagefsSharedDevice {
		t.Errorf("expected zeroed storage, got %+v", nm)
	}
}

func TestParseKubeletSummary_MissingAndEmpty(t *testing.T) {
	// Node with no cpu/memory blocks and no pods must not panic and yield zeroed metrics.
	summary, err := parseKubeletSummary([]byte(`{"node":{"nodeName":"n"},"pods":[]}`), "n")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	nm := nodeMetricFromSummary("n", summary)
	if nm.CPUUsageNanoCores != 0 || nm.MemoryWorkingSet != 0 {
		t.Errorf("expected zeroed node metric, got %+v", nm)
	}
	if got := containerMetricsFromSummary("n", summary); len(got) != 0 {
		t.Errorf("expected no container metrics, got %d", len(got))
	}

	if _, err := parseKubeletSummary([]byte("not json"), "n"); err == nil {
		t.Error("expected error on invalid JSON")
	}
}
