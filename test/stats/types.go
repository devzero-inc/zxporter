package stats

import (
	"time"
)

// PodResourceUsage represents resource usage for a pod
type PodResourceUsage struct {
	Requests   map[string]string            `json:"requests,omitempty"`
	Limits     map[string]string            `json:"limits,omitempty"`
	Containers map[string]map[string]string `json:"containers,omitempty"`
}

// NodeResourceUsage represents resource usage for a node
type NodeResourceUsage struct {
	Capacity    map[string]string `json:"capacity,omitempty"`
	Allocatable map[string]string `json:"allocatable,omitempty"`
	Usage       map[string]string `json:"usage,omitempty"`
}

type ClusterResourceUsage struct {
	Provider          string `json:"provider,omitempty"`
	Name              string `json:"name,omitempty"`
	NodeCount         string `json:"node_count,omitempty"`
	Version           string `json:"version,omitempty"`
	ZxporterBuildDate string `json:"zxporter_version,omitempty"`
	ZxporterGitCommit string `json:"zxporter_git_commit,omitempty"`
	ZxporterVersion   string `json:"zxporter_build_date,omitempty"`

	FullDump string `json:"full_dump,omitempty"`
}

// SnapshotStats represents detailed statistics about cluster snapshots
type SnapshotStats struct {
	// Basic snapshot information
	SnapshotID  string    `json:"snapshot_id"`
	ClusterID   string    `json:"cluster_id"`
	Timestamp   time.Time `json:"timestamp"`
	ChunksCount int32     `json:"chunks_count"`
	TotalSize   int64     `json:"total_size_bytes"`

	// Cluster information
	ClusterVersion string   `json:"cluster_version"`
	NodeCount      int32    `json:"node_count"`
	Namespaces     []string `json:"namespaces"`

	// Resource counts by category
	NodeResources          map[string]int `json:"node_resources"`           // node_uid -> pod_count
	NamespaceResources     map[string]int `json:"namespace_resources"`      // namespace_uid -> total_resources
	ClusterScopedResources map[string]int `json:"cluster_scoped_resources"` // resource_type -> count

	// Detailed resource breakdown per namespace
	NamespaceBreakdown map[string]NamespaceResourceBreakdown `json:"namespace_breakdown"`
}

// NamespaceResourceBreakdown provides detailed resource counts for a namespace
type NamespaceResourceBreakdown struct {
	NamespaceName            string `json:"namespace_name"`
	Deployments              int    `json:"deployments"`
	StatefulSets             int    `json:"stateful_sets"`
	DaemonSets               int    `json:"daemon_sets"`
	ReplicaSets              int    `json:"replica_sets"`
	Services                 int    `json:"services"`
	ConfigMaps               int    `json:"config_maps"`
	Secrets                  int    `json:"secrets"`
	PVCs                     int    `json:"pvcs"`
	Jobs                     int    `json:"jobs"`
	CronJobs                 int    `json:"cron_jobs"`
	Ingresses                int    `json:"ingresses"`
	NetworkPolicies          int    `json:"network_policies"`
	ServiceAccounts          int    `json:"service_accounts"`
	Roles                    int    `json:"roles"`
	RoleBindings             int    `json:"role_bindings"`
	PodDisruptionBudgets     int    `json:"pod_disruption_budgets"`
	Endpoints                int    `json:"endpoints"`
	LimitRanges              int    `json:"limit_ranges"`
	ResourceQuotas           int    `json:"resource_quotas"`
	UnscheduledPods          int    `json:"unscheduled_pods"`
	HorizontalPodAutoscalers int    `json:"horizontal_pod_autoscalers"`
	Events                   int    `json:"events"`
	KedaScaledJobs           int    `json:"keda_scaled_jobs"`
	KedaScaledObjects        int    `json:"keda_scaled_objects"`
	TotalResources           int    `json:"total_resources"`
}

// NetworkTrafficStat represents a single network traffic flow
type NetworkTrafficStat struct {
	SrcIP           string `json:"src_ip"`
	DstIP           string `json:"dst_ip"`
	SrcPodName      string `json:"src_pod_name,omitempty"`
	SrcPodNamespace string `json:"src_pod_namespace,omitempty"`
	Protocol        int32  `json:"protocol"`
	DstPort         int32  `json:"dst_port"`
	TxBytes         int64  `json:"tx_bytes"`
	RxBytes         int64  `json:"rx_bytes"`
	TxPackets       int64  `json:"tx_packets"`
	RxPackets       int64  `json:"rx_packets"`
}

// DnsLookupStat represents a captured DNS lookup
type DnsLookupStat struct {
	ClientIP        string   `json:"client_ip"`
	Domain          string   `json:"domain"`
	ResolvedIPs     []string `json:"resolved_ips,omitempty"`
	SrcPodName      string   `json:"src_pod_name,omitempty"`
	SrcPodNamespace string   `json:"src_pod_namespace,omitempty"`
	Timestamp       string   `json:"timestamp"`
}

// Stats represents the statistics about received messages
type Stats struct {
	TotalMessages      int                             `json:"total_messages"`
	MessagesByType     map[string]int                  `json:"messages_by_type"`
	UniqueResources    map[string]int                  `json:"unique_resources"`
	FirstMessageTime   *time.Time                      `json:"first_message_time,omitempty"`
	UsageReportPods    map[string]PodResourceUsage     `json:"usage_report_pods,omitempty"`
	UsageReportNodes   map[string]NodeResourceUsage    `json:"usage_report_nodes,omitempty"`
	UsageReportCluster map[string]ClusterResourceUsage `json:"usage_report_cluster,omitempty"`
	SnapshotStats      map[string]SnapshotStats        `json:"snapshot_stats,omitempty"` // snapshot_id -> stats

	// New fields for network monitor verification
	NetworkMetrics []NetworkTrafficStat `json:"network_metrics,omitempty"`
	DnsLookups     []DnsLookupStat      `json:"dns_lookups,omitempty"`
}

// Expected represents the expected resource usages
type Expected struct {
	UsageReportPods    map[string]PodResourceUsage `json:"usage_report_pods"`
	UsageReportCluster ClusterResourceUsage        `json:"usage_report_cluster"`
}
