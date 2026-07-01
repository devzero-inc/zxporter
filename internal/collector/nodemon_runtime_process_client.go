package collector

import (
	"time"
)

// NodemonRuntimeProcessMetrics represents a single generic-runtime detection
// entry (.NET, Go, GraalVM native-image, Python, Ruby, Deno, Bun) returned in
// the "runtimes" bucket of the nodemon GET /container/runtime-metrics endpoint.
// Mirrors nodemon.RuntimeProcessMetric but is defined here to avoid importing
// the nodemon package.
type NodemonRuntimeProcessMetrics struct {
	Runtime     string `json:"runtime"`
	NodeName    string `json:"node_name"`
	Pod         string `json:"pod"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	ContainerID string `json:"container_id"`
	PidHost     int    `json:"pid_host"`
	PidNS       int    `json:"pid_ns"`

	Version       string `json:"version,omitempty"`
	VersionSource string `json:"version_source,omitempty"`

	RawCmdline string    `json:"raw_cmdline,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// IndexRuntimeProcessMetricsByContainer indexes generic-runtime metrics by
// (pod, container, namespace). Unlike the JVM/Node.js indexes, the value is a
// slice — a single container can legitimately host processes of more than one
// runtime (e.g. a Python entrypoint that spawns a Go sidecar binary).
func IndexRuntimeProcessMetricsByContainer(metrics []NodemonRuntimeProcessMetrics) map[gpuContainerKey][]NodemonRuntimeProcessMetrics {
	index := make(map[gpuContainerKey][]NodemonRuntimeProcessMetrics)
	for _, m := range metrics {
		key := gpuContainerKey{Pod: m.Pod, Container: m.Container, Namespace: m.Namespace}
		// Keep one entry per runtime per container — multiple processes of the
		// same runtime in one container add no signal.
		duplicate := false
		for _, existing := range index[key] {
			if existing.Runtime == m.Runtime {
				duplicate = true
				break
			}
		}
		if !duplicate {
			index[key] = append(index[key], m)
		}
	}
	return index
}

// ContainerRuntimeProcess is the per-runtime detection attached to
// ContainerMetricsSnapshot.RuntimeProcesses.
type ContainerRuntimeProcess struct {
	Runtime       string `json:"runtime"`
	Version       string `json:"version,omitempty"`
	VersionSource string `json:"versionSource,omitempty"`
	RawCmdline    string `json:"rawCmdline,omitempty"`
}

// RuntimeProcessesFromNodemon converts the generic-runtime metric entries for a
// container into the snapshot attachment shape.
func RuntimeProcessesFromNodemon(metrics []NodemonRuntimeProcessMetrics) []ContainerRuntimeProcess {
	out := make([]ContainerRuntimeProcess, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, ContainerRuntimeProcess{
			Runtime:       m.Runtime,
			Version:       m.Version,
			VersionSource: m.VersionSource,
			RawCmdline:    m.RawCmdline,
		})
	}
	return out
}
