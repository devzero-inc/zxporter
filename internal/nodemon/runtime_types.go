package nodemon

import "time"

// RuntimeProcessMetric holds per-container detection for the generic runtimes
// (.NET, Go, GraalVM native-image, Python, Ruby, Deno, Bun) — everything beyond
// JVM and Node.js, which have their own dedicated metric types for historical
// and payload-shape reasons. Scoped to existence + version, same as Node.js.
type RuntimeProcessMetric struct {
	// Runtime is the detected runtime name: "dotnet" | "go" |
	// "graalvm-native-image" | "python" | "ruby" | "deno" | "bun".
	Runtime string `json:"runtime"`

	NodeName    string `json:"node_name"`
	Pod         string `json:"pod"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	ContainerID string `json:"container_id"`
	PidHost     int    `json:"pid_host"`
	PidNS       int    `json:"pid_ns"`

	// Version is best-effort and passive (env var, /proc/<pid>/maps, embedded
	// build info, or a read-only binary scan depending on the runtime). Empty if
	// nothing resolves — the process is still reported as a detected workload.
	Version       string `json:"version,omitempty"`
	VersionSource string `json:"version_source,omitempty"` // "env" | "maps" | "exe-path" | "comm" | "buildinfo" | "binary-scan"

	RawCmdline string    `json:"raw_cmdline,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// RuntimeProcess is a discovered generic-runtime process prior to version
// resolution (which the collector layer caches per container), the analog of
// NodeJSProcess for the probed/table-detected runtimes.
type RuntimeProcess struct {
	Kind        processKind
	Runtime     string
	PidHost     int
	PidNS       int
	ContainerID string
	CmdLine     string

	// PidDir is /proc/<pid> on procRoot, retained so the collector can resolve
	// the version without re-walking /proc.
	PidDir string
}
