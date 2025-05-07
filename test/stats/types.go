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
	ClusterAPI        string `json:"cluster_api,omitempty"`
	Provider          string `json:"provider,omitempty"`
	ProviderSpecific  string `json:"provider_specific,omitempty"`
	Name              string `json:"name,omitempty"`
	NodeCount         string `json:"node_count,omitempty"`
	Version           string `json:"version,omitempty"`
	ZxporterBuildDate string `json:"zxporter_version,omitempty"`
	ZxporterGitCommit string `json:"zxporter_git_commit,omitempty"`
	ZxporterVersion   string `json:"zxporter_build_date,omitempty"`

	FullDump string `json:"full_dump,omitempty"`
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
}

// ExpectedPods represents the expected pod resource usage
type ExpectedPods struct {
	UsageReportPods map[string]PodResourceUsage `json:"usage_report_pods"`
}
