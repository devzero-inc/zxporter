package nodemon

import "time"

// NodeJSMetric holds per-container Node.js runtime detection extracted from /proc.
// This is intentionally scoped to existence + version for now — unlike JVM, V8 has
// no hsperfdata-equivalent live counters file, so heap usage/flags capture is a
// separate follow-up.
type NodeJSMetric struct {
	NodeName    string `json:"node_name"`
	Pod         string `json:"pod"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	ContainerID string `json:"container_id"`
	PidHost     int    `json:"pid_host"`
	PidNS       int    `json:"pid_ns"`

	// NodeVersion is best-effort: resolved from the NODE_VERSION env var (set by
	// official Docker Hub node images) or, failing that, a read-only scan of the
	// node binary for its embedded release-URL string. Empty if neither resolves —
	// the process is still reported as a detected Node.js workload.
	NodeVersion       string `json:"node_version,omitempty"`
	NodeVersionSource string `json:"node_version_source,omitempty"` // "env" | "binary-scan"

	RawCmdline string    `json:"raw_cmdline,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}
