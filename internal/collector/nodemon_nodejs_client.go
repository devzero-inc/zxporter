package collector

import (
	"time"
)

// NodemonNodeJSMetrics represents a single Node.js detection entry returned by the
// nodemon GET /container/nodejs-metrics endpoint. Mirrors nodemon.NodeJSMetric but
// is defined here to avoid importing the nodemon package.
type NodemonNodeJSMetrics struct {
	NodeName    string `json:"node_name"`
	Pod         string `json:"pod"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	ContainerID string `json:"container_id"`
	PidHost     int    `json:"pid_host"`
	PidNS       int    `json:"pid_ns"`

	NodeVersion       string `json:"node_version,omitempty"`
	NodeVersionSource string `json:"node_version_source,omitempty"`

	RawCmdline string    `json:"raw_cmdline,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// IndexNodeJSMetricsByContainer indexes Node.js metrics by (pod, container, namespace)
// for O(1) lookup.
func IndexNodeJSMetricsByContainer(metrics []NodemonNodeJSMetrics) map[gpuContainerKey]NodemonNodeJSMetrics {
	index := make(map[gpuContainerKey]NodemonNodeJSMetrics)
	for _, m := range metrics {
		key := gpuContainerKey{Pod: m.Pod, Container: m.Container, Namespace: m.Namespace}
		// If multiple Node.js processes in one container, keep the first for now.
		if _, exists := index[key]; !exists {
			index[key] = m
		}
	}
	return index
}

// NodeJSMetricsFromNodemon converts a Node.js metric entry into a flat
// map[string]interface{} for attachment to ContainerMetricsSnapshot.
func NodeJSMetricsFromNodemon(m NodemonNodeJSMetrics) map[string]interface{} {
	return map[string]interface{}{
		"NodeJsDetected":      true,
		"NodeJsVersion":       m.NodeVersion,
		"NodeJsVersionSource": m.NodeVersionSource,
		"RawCmdline":          m.RawCmdline,
	}
}
