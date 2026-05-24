package nodemon

import "time"

// JVMMetric holds per-container JVM metrics extracted from hsperfdata.
type JVMMetric struct {
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

	GCTimeSecondsTotal            map[string]float64 `json:"gc_time_seconds_total"`
	SafepointTimeSecondsTotal     float64            `json:"safepoint_time_seconds_total"`
	SafepointSyncTimeSecondsTotal float64            `json:"safepoint_sync_time_seconds_total"`

	FlagsExtracted JVMFlagsExtracted `json:"flags_extracted"`
	RawCmdline     string            `json:"raw_cmdline,omitempty"`
	Timestamp      time.Time         `json:"timestamp"`
}

// JVMFlagsExtracted holds JVM flags parsed from the process cmdline.
type JVMFlagsExtracted struct {
	XmsBytes            *int64   `json:"xms_bytes,omitempty"`
	XmxBytes            *int64   `json:"xmx_bytes,omitempty"`
	MaxRamPercentage    *float64 `json:"max_ram_percentage,omitempty"`
	UseContainerSupport *bool    `json:"use_container_support,omitempty"`
}
