package nodemon

// CAdvisorContainerMetrics holds rate-computed metrics from cAdvisor for a single container.
type CAdvisorContainerMetrics struct {
	Namespace string
	Pod       string
	Container string

	// Network rates (per second)
	NetworkRxPacketsPerSec float64
	NetworkTxPacketsPerSec float64
	NetworkRxErrorsPerSec  float64
	NetworkTxErrorsPerSec  float64
	NetworkRxDropsPerSec   float64
	NetworkTxDropsPerSec   float64

	// Disk I/O rates (per second)
	DiskReadBytesPerSec  float64
	DiskWriteBytesPerSec float64
	DiskReadOpsPerSec    float64
	DiskWriteOpsPerSec   float64

	// CPU throttle (ratio 0-1)
	CPUThrottleFraction float64
}
