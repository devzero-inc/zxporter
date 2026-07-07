package collector

import (
	"encoding/json"
	"time"
)

// NodemonJVMMetrics represents a single JVM metric entry returned by the nodemon
// GET /container/jvm-metrics endpoint.
// Mirrors nodemon.JVMMetric but is defined here to avoid importing the nodemon package.
//
// NOTE: We intentionally keep FlagsExtracted / FlagSources as generic maps to
// preserve forward-compatibility with nodemon output.
type NodemonJVMMetrics struct {
	NodeName    string `json:"node_name"`
	Pod         string `json:"pod"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	ContainerID string `json:"container_id"`
	PidHost     int    `json:"pid_host"`
	PidNS       int    `json:"pid_ns"`

	JavaCommand string `json:"java_command,omitempty"`
	JavaVersion string `json:"java_version,omitempty"`

	HeapSizeBytes    int64 `json:"heap_size_bytes"`
	HeapUsedBytes    int64 `json:"heap_used_bytes"`
	HeapMaxSizeBytes int64 `json:"heap_max_size_bytes"`

	FlagsExtracted map[string]any `json:"flags_extracted"`
	FlagSources    map[string]any `json:"flag_sources,omitempty"`
	RawCmdline     string         `json:"raw_cmdline,omitempty"`
	Timestamp      time.Time      `json:"timestamp"`
}

// IndexJVMMetricsByContainer indexes JVM metrics by (pod, container, namespace) for O(1) lookup.
func IndexJVMMetricsByContainer(metrics []NodemonJVMMetrics) map[gpuContainerKey]NodemonJVMMetrics {
	index := make(map[gpuContainerKey]NodemonJVMMetrics)
	for _, m := range metrics {
		key := gpuContainerKey{Pod: m.Pod, Container: m.Container, Namespace: m.Namespace}
		// If multiple JVMs in one container, keep the first for now.
		if _, exists := index[key]; !exists {
			index[key] = m
		}
	}
	return index
}

// JVMMetricsFromNodemon converts a JVM metric entry into a flat map[string]interface{}
// for attachment to ContainerMetricsSnapshot.
func JVMMetricsFromNodemon(m NodemonJVMMetrics) map[string]interface{} {
	out := map[string]interface{}{
		"JavaCommand":      m.JavaCommand,
		"JavaVersion":      m.JavaVersion,
		"HeapSizeBytes":    m.HeapSizeBytes,
		"HeapUsedBytes":    m.HeapUsedBytes,
		"HeapMaxSizeBytes": m.HeapMaxSizeBytes,
		"RawCmdline":       m.RawCmdline,
	}
	if b, err := json.Marshal(m.FlagsExtracted); err == nil {
		out["FlagsExtractedJSON"] = string(b)
	}
	if b, err := json.Marshal(m.FlagSources); err == nil {
		out["FlagSourcesJSON"] = string(b)
	}
	return out
}
