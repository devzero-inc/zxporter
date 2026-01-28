package collector

// PersistentVolumeClaimMetricsSnapshot represents a strongly-typed snapshot of PVC storage usage metrics
type PersistentVolumeClaimMetricsSnapshot struct {
	// PVC identification
	PvcName   string `json:"pvcName"`
	Namespace string `json:"namespace"`
	PvcUID    string `json:"pvcUID"`

	// Volume binding information
	PvName           string `json:"pvName,omitempty"`
	StorageClassName string `json:"storageClassName,omitempty"`
	VolumeMode       string `json:"volumeMode,omitempty"` // "Filesystem" or "Block"

	// Additional metadata for UI and cost attribution
	AccessModes    []string `json:"accessModes,omitempty"`    // e.g., ["ReadWriteOnce"]
	RequestedBytes int64    `json:"requestedBytes,omitempty"` // From PVC spec resources.requests.storage

	// Storage usage metrics
	UsedBytes      int64   `json:"usedBytes"`
	CapacityBytes  int64   `json:"capacityBytes"`
	AvailableBytes int64   `json:"availableBytes"`
	UtilizationPct float64 `json:"utilizationPct"`

	// Quality and source information
	StatsAvailable    bool   `json:"statsAvailable"`
	StatsSource       string `json:"statsSource"` // "csi", "kubelet", "unknown"
	UnavailableReason string `json:"unavailableReason,omitempty"`
}
