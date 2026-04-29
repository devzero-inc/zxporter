package nodemon

import "time"

// ContainerMetricsResponse is the JSON response for GET /v2/container/metrics.
type ContainerMetricsResponse struct {
	NodeName  string    `json:"node_name"`
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Timestamp time.Time `json:"timestamp"`

	// From stats/summary
	CPUUsageNanoCores uint64 `json:"cpu_usage_nanocores"`
	MemoryWorkingSet  uint64 `json:"memory_working_set_bytes"`
	MemoryUsageBytes  uint64 `json:"memory_usage_bytes"`
	MemoryRSSBytes    uint64 `json:"memory_rss_bytes"`
	NetworkRxBytes    uint64 `json:"network_rx_bytes"`
	NetworkTxBytes    uint64 `json:"network_tx_bytes"`

	// From cAdvisor (rates)
	NetworkRxPacketsPerSec float64 `json:"network_rx_packets_per_sec"`
	NetworkTxPacketsPerSec float64 `json:"network_tx_packets_per_sec"`
	NetworkRxErrorsPerSec  float64 `json:"network_rx_errors_per_sec"`
	NetworkTxErrorsPerSec  float64 `json:"network_tx_errors_per_sec"`
	NetworkRxDropsPerSec   float64 `json:"network_rx_drops_per_sec"`
	NetworkTxDropsPerSec   float64 `json:"network_tx_drops_per_sec"`
	DiskReadBytesPerSec    float64 `json:"disk_read_bytes_per_sec"`
	DiskWriteBytesPerSec   float64 `json:"disk_write_bytes_per_sec"`
	DiskReadOpsPerSec      float64 `json:"disk_read_ops_per_sec"`
	DiskWriteOpsPerSec     float64 `json:"disk_write_ops_per_sec"`
	CPUThrottleFraction    float64 `json:"cpu_throttle_fraction"`

	// From GPU (optional)
	GPUUtilization   float64 `json:"gpu_utilization,omitempty"`
	GPUMemoryUsedMiB float64 `json:"gpu_memory_used_mib,omitempty"`
	GPUMemoryFreeMiB float64 `json:"gpu_memory_free_mib,omitempty"`
	GPUPowerWatts    float64 `json:"gpu_power_watts,omitempty"`
	GPUTemperature   float64 `json:"gpu_temperature_celsius,omitempty"`
}

// NodeMetricsResponse is the JSON response for GET /node/metrics.
type NodeMetricsResponse struct {
	NodeName  string    `json:"node_name"`
	Timestamp time.Time `json:"timestamp"`

	// Network rates (per second)
	NetworkRxBytesPerSec   float64 `json:"network_rx_bytes_per_sec"`
	NetworkTxBytesPerSec   float64 `json:"network_tx_bytes_per_sec"`
	NetworkRxPacketsPerSec float64 `json:"network_rx_packets_per_sec"`
	NetworkTxPacketsPerSec float64 `json:"network_tx_packets_per_sec"`
	NetworkRxErrorsPerSec  float64 `json:"network_rx_errors_per_sec"`
	NetworkTxErrorsPerSec  float64 `json:"network_tx_errors_per_sec"`
	NetworkRxDropsPerSec   float64 `json:"network_rx_drops_per_sec"`
	NetworkTxDropsPerSec   float64 `json:"network_tx_drops_per_sec"`

	// Disk I/O rates (per second)
	DiskReadBytesPerSec  float64 `json:"disk_read_bytes_per_sec"`
	DiskWriteBytesPerSec float64 `json:"disk_write_bytes_per_sec"`
	DiskReadOpsPerSec    float64 `json:"disk_read_ops_per_sec"`
	DiskWriteOpsPerSec   float64 `json:"disk_write_ops_per_sec"`

	// GPU aggregates (optional)
	GPUUtilizationAvg   float64 `json:"gpu_utilization_avg,omitempty"`
	GPUMemoryUsedMiBSum float64 `json:"gpu_memory_used_mib_sum,omitempty"`
	GPUPowerWattsSum    float64 `json:"gpu_power_watts_sum,omitempty"`
	GPUTemperatureMax   float64 `json:"gpu_temperature_max_celsius,omitempty"`
}

// PVCMetricsResponse is the JSON response for GET /pvc/metrics.
type PVCMetricsResponse struct {
	Namespace      string `json:"namespace"`
	Pod            string `json:"pod"`
	PVCName        string `json:"pvc_name"`
	UsedBytes      uint64 `json:"used_bytes"`
	CapacityBytes  uint64 `json:"capacity_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}
