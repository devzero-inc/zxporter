package nodemon

import "time"

// StatsSummary is the top-level response from the kubelet /stats/summary endpoint.
type StatsSummary struct {
	Pods []PodStats `json:"pods"`
	Node NodeStats  `json:"node"`
}

// PodStats holds per-pod resource metrics.
type PodStats struct {
	PodRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"podRef"`
	Containers  []ContainerStats `json:"containers"`
	Network     NetworkStats     `json:"network"`
	VolumeStats []VolumeStats    `json:"volume"`
}

// ContainerStats holds CPU and memory metrics for a single container.
type ContainerStats struct {
	Name   string   `json:"name"`
	CPU    CPUStats `json:"cpu"`
	Memory MemStats `json:"memory"`
}

// CPUStats holds CPU usage metrics. Pointer fields are omitted by kubelet when unavailable.
type CPUStats struct {
	Time                 time.Time `json:"time"`
	UsageNanoCores       *uint64   `json:"usageNanoCores"`
	UsageCoreNanoSeconds *uint64   `json:"usageCoreNanoSeconds"`
}

// MemStats holds memory usage metrics. Pointer fields are omitted by kubelet when unavailable.
type MemStats struct {
	Time            time.Time `json:"time"`
	AvailableBytes  *uint64   `json:"availableBytes"`
	UsageBytes      *uint64   `json:"usageBytes"`
	WorkingSetBytes *uint64   `json:"workingSetBytes"`
	RSSBytes        *uint64   `json:"rssBytes"`
	PageFaults      *uint64   `json:"pageFaults"`
	MajorPageFaults *uint64   `json:"majorPageFaults"`
}

// NetworkStats holds network interface metrics.
type NetworkStats struct {
	Interfaces []InterfaceStats `json:"interfaces"`
}

// InterfaceStats holds per-interface RX/TX byte counters.
type InterfaceStats struct {
	Name    string  `json:"name"`
	RxBytes *uint64 `json:"rxBytes"`
	TxBytes *uint64 `json:"txBytes"`
}

// VolumeStats holds PVC/volume usage metrics.
type VolumeStats struct {
	Name           string  `json:"name"`
	PVCRef         *PVCRef `json:"pvcRef,omitempty"`
	UsedBytes      *uint64 `json:"usedBytes"`
	CapacityBytes  *uint64 `json:"capacityBytes"`
	AvailableBytes *uint64 `json:"availableBytes"`
}

// PVCRef identifies the PersistentVolumeClaim backing a volume.
type PVCRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// FsStats holds filesystem usage for one filesystem, as reported by the kubelet
// Summary API. Pointer fields are omitted by kubelet when unavailable, matching
// the convention used by CPUStats and MemStats above.
type FsStats struct {
	Time           time.Time `json:"time"`
	AvailableBytes *uint64   `json:"availableBytes"`
	CapacityBytes  *uint64   `json:"capacityBytes"`
	UsedBytes      *uint64   `json:"usedBytes"`
	Inodes         *uint64   `json:"inodes"`
	InodesFree     *uint64   `json:"inodesFree"`
	InodesUsed     *uint64   `json:"inodesUsed"`
}

// RuntimeStats holds the container runtime's filesystem stats. containerFs is
// deliberately not modeled: it equals imageFs on every runtime configuration
// except split-disk containerd, which we have not observed in the fleet.
type RuntimeStats struct {
	ImageFs *FsStats `json:"imageFs"`
}

// NodeStats holds node-level metrics from kubelet stats/summary.
type NodeStats struct {
	NodeName string        `json:"nodeName"`
	CPU      CPUStats      `json:"cpu"`
	Memory   MemStats      `json:"memory"`
	Network  NetworkStats  `json:"network"`
	Fs       *FsStats      `json:"fs"`
	Runtime  *RuntimeStats `json:"runtime"`
}
