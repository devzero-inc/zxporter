package collector

// ContainerMetricsSnapshot represents a strongly-typed snapshot of container resource metrics
type ContainerMetricsSnapshot struct {
	// Container identification
	ContainerName string `json:"containerName"`
	PodName       string `json:"podName"`
	Namespace     string `json:"namespace"`
	NodeName      string `json:"nodeName"`

	// Inferred Workload (Top-level owner)
	WorkloadName string `json:"workloadName,omitempty"`
	WorkloadKind string `json:"workloadKind,omitempty"`

	// CPU/Memory resource usage
	CpuUsageMillis   int64 `json:"cpuUsageMillis"`
	MemoryUsageBytes int64 `json:"memoryUsageBytes"`

	// Resource requests and limits
	CpuRequestMillis   int64 `json:"cpuRequestMillis"`
	CpuLimitMillis     int64 `json:"cpuLimitMillis"`
	MemoryRequestBytes int64 `json:"memoryRequestBytes"`
	MemoryLimitBytes   int64 `json:"memoryLimitBytes"`

	// Labels from the pod for correlation
	PodLabels map[string]string `json:"podLabels"`

	// Container metadata for reference
	ContainerImage string `json:"containerImage"`

	// Status info
	ContainerRunning      bool   `json:"containerRunning"`
	ContainerRestarts     bool   `json:"containerRestarts"`
	RestartCount          int64  `json:"restartCount"`
	LastTerminationReason string `json:"lastTerminationReason"`

	// Network metrics (pod-level aggregated - shared across all containers in pod)
	// NetworkMetricsArePodLevel indicates that network metrics represent the entire pod,
	// not just this container. In Kubernetes, network metrics are only available at the
	// pod level because all containers share the same network namespace.
	NetworkMetricsArePodLevel bool    `json:"networkMetricsArePodLevel,omitempty"`
	PodContainerCount         int     `json:"podContainerCount,omitempty"` // Number of containers in the pod
	NetworkReceiveBytes       float64 `json:"networkReceiveBytes,omitempty"`
	NetworkTransmitBytes      float64 `json:"networkTransmitBytes,omitempty"`
	NetworkReceivePackets     float64 `json:"networkReceivePackets,omitempty"`
	NetworkTransmitPackets    float64 `json:"networkTransmitPackets,omitempty"`
	NetworkReceiveErrors      float64 `json:"networkReceiveErrors,omitempty"`
	NetworkTransmitErrors     float64 `json:"networkTransmitErrors,omitempty"`
	NetworkReceiveDropped     float64 `json:"networkReceiveDropped,omitempty"`
	NetworkTransmitDropped    float64 `json:"networkTransmitDropped,omitempty"`

	// I/O metrics
	FsReadBytes  float64 `json:"fsReadBytes,omitempty"`
	FsWriteBytes float64 `json:"fsWriteBytes,omitempty"`
	FsReads      float64 `json:"fsReads,omitempty"`
	FsWrites     float64 `json:"fsWrites,omitempty"`

	// GPU metrics
	GpuUsage                 interface{} `json:"gpuUsage,omitempty"`
	GpuMetricsCount          interface{} `json:"gpuMetricsCount,omitempty"`
	GpuUtilizationPercentage interface{} `json:"gpuUtilizationPercentage,omitempty"`
	GpuMemoryUsedMb          interface{} `json:"gpuMemoryUsedMb,omitempty"`
	GpuMemoryFreeMb          interface{} `json:"gpuMemoryFreeMb,omitempty"`
	GpuPowerUsageWatts       interface{} `json:"gpuPowerUsageWatts,omitempty"`
	GpuTemperatureCelsius    interface{} `json:"gpuTemperatureCelsius,omitempty"`
	GpuSMClockMHz            interface{} `json:"gpuSMClockMHz,omitempty"`
	GpuMemClockMHz           interface{} `json:"gpuMemClockMHz,omitempty"`
	GpuModels                interface{} `json:"gpuModels,omitempty"`
	GpuUUIDs                 interface{} `json:"gpuUUIDs,omitempty"`
	GpuRequestCount          interface{} `json:"gpuRequestCount,omitempty"`
	GpuLimitCount            interface{} `json:"gpuLimitCount,omitempty"`
	GpuTotalMemoryMb         interface{} `json:"gpuTotalMemoryMb,omitempty"`
	IndividualGPUMetrics     string      `json:"individualGPUMetrics,omitempty"` // JSON string
}
