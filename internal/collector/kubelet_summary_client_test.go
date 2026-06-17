package collector

import (
	"testing"
)

const sampleSummaryJSON = `{
  "node": {
    "nodeName": "node-a",
    "cpu": {"time": "2026-06-17T10:00:00Z", "usageNanoCores": 1500000000},
    "memory": {"workingSetBytes": 2147483648}
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

	// sidecar has no memory usage/rss reported — those stay zero, no panic on nil.
	side := cms[1]
	if side.Container != "sidecar" || side.CPUUsageNanoCores != 50000000 || side.MemoryWorkingSet != 10485760 {
		t.Errorf("sidecar mismatch: %+v", side)
	}
	if side.MemoryUsageBytes != 0 || side.MemoryRSSBytes != 0 {
		t.Errorf("sidecar expected zero usage/rss, got usage=%d rss=%d", side.MemoryUsageBytes, side.MemoryRSSBytes)
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
