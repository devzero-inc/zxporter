package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
)

// KubeletSummaryClient fetches per-node resource usage directly from the kubelet
// Summary API (/stats/summary) via the API-server node proxy subresource.
//
// It exists as a fallback for nodes that have NO zxporter-nodemon pod running
// (e.g. the DaemonSet pod failed to schedule, or is not yet ready). Because the
// kubelet runs on every node by construction, this gives a hard coverage
// guarantee that a DaemonSet alone cannot.
//
// The Summary API exposes the same kubelet stats nodemon itself is built on
// (usageNanoCores / workingSetBytes), so results are returned in the existing
// UnifiedNodeMetric / UnifiedContainerMetric shapes and reuse all downstream
// conversion. Only CPU + memory are available here — network/disk rates,
// CPU-throttle, GPU and JVM metrics remain nodemon-only and stay zero for
// fallback nodes (a deliberate degraded mode, on par with what metrics-server
// would have provided).
type KubeletSummaryClient struct {
	k8sClient kubernetes.Interface
	log       logr.Logger
}

// NewKubeletSummaryClient creates a client that reads kubelet /stats/summary
// through the API-server node proxy. It requires the `nodes/proxy` (get) RBAC
// verb on the zxporter ClusterRole.
func NewKubeletSummaryClient(k8sClient kubernetes.Interface, log logr.Logger) *KubeletSummaryClient {
	return &KubeletSummaryClient{
		k8sClient: k8sClient,
		log:       log.WithName("kubelet-summary-client"),
	}
}

// kubeletSummary is a minimal subset of k8s.io/kubelet/pkg/apis/stats/v1alpha1.Summary.
// Defined locally to avoid adding the k8s.io/kubelet dependency.
type kubeletSummary struct {
	Node kubeletNodeStats  `json:"node"`
	Pods []kubeletPodStats `json:"pods"`
}

type kubeletNodeStats struct {
	NodeName string              `json:"nodeName"`
	CPU      *kubeletCPUStats    `json:"cpu"`
	Memory   *kubeletMemoryStats `json:"memory"`
}

type kubeletPodStats struct {
	PodRef     kubeletPodReference     `json:"podRef"`
	Containers []kubeletContainerStats `json:"containers"`
}

type kubeletPodReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type kubeletContainerStats struct {
	Name   string              `json:"name"`
	CPU    *kubeletCPUStats    `json:"cpu"`
	Memory *kubeletMemoryStats `json:"memory"`
}

type kubeletCPUStats struct {
	Time           time.Time `json:"time"`
	UsageNanoCores *uint64   `json:"usageNanoCores"`
}

type kubeletMemoryStats struct {
	WorkingSetBytes *uint64 `json:"workingSetBytes"`
	UsageBytes      *uint64 `json:"usageBytes"`
	RSSBytes        *uint64 `json:"rssBytes"`
}

// fetchSummary retrieves and parses the kubelet Summary API for a single node
// via GET /api/v1/nodes/<node>/proxy/stats/summary.
func (c *KubeletSummaryClient) fetchSummary(ctx context.Context, nodeName string) (*kubeletSummary, error) {
	raw, err := c.k8sClient.CoreV1().RESTClient().
		Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy").
		Suffix("stats", "summary").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching kubelet summary for node %s: %w", nodeName, err)
	}

	return parseKubeletSummary(raw, nodeName)
}

// parseKubeletSummary unmarshals raw kubelet Summary API JSON. Split out from the
// HTTP call so it can be unit-tested with fixtures.
func parseKubeletSummary(raw []byte, nodeName string) (*kubeletSummary, error) {
	var summary kubeletSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("decoding kubelet summary for node %s: %w", nodeName, err)
	}
	return &summary, nil
}

// FetchNodeMetricsByNode returns node-level CPU/memory usage for the given node
// from the kubelet Summary API. Returns a metric with zeroed fields where the
// kubelet did not report a value.
func (c *KubeletSummaryClient) FetchNodeMetricsByNode(
	ctx context.Context,
	nodeName string,
) (*UnifiedNodeMetric, error) {
	summary, err := c.fetchSummary(ctx, nodeName)
	if err != nil {
		return nil, err
	}
	return nodeMetricFromSummary(nodeName, summary), nil
}

// nodeMetricFromSummary converts node-level kubelet stats into a UnifiedNodeMetric.
func nodeMetricFromSummary(nodeName string, summary *kubeletSummary) *UnifiedNodeMetric {
	m := &UnifiedNodeMetric{NodeName: nodeName}
	if cpu := summary.Node.CPU; cpu != nil {
		m.Timestamp = cpu.Time
		if cpu.UsageNanoCores != nil {
			m.CPUUsageNanoCores = *cpu.UsageNanoCores
		}
	}
	if mem := summary.Node.Memory; mem != nil && mem.WorkingSetBytes != nil {
		m.MemoryWorkingSet = *mem.WorkingSetBytes
	}
	return m
}

// FetchContainerMetricsByNode returns per-container CPU/memory usage for all pods
// on the given node from the kubelet Summary API.
func (c *KubeletSummaryClient) FetchContainerMetricsByNode(
	ctx context.Context,
	nodeName string,
) ([]UnifiedContainerMetric, error) {
	summary, err := c.fetchSummary(ctx, nodeName)
	if err != nil {
		return nil, err
	}
	return containerMetricsFromSummary(nodeName, summary), nil
}

// containerMetricsFromSummary converts per-container kubelet stats into UnifiedContainerMetric.
func containerMetricsFromSummary(nodeName string, summary *kubeletSummary) []UnifiedContainerMetric {
	var out []UnifiedContainerMetric
	for _, pod := range summary.Pods {
		for _, ctr := range pod.Containers {
			m := UnifiedContainerMetric{
				NodeName:  nodeName,
				Namespace: pod.PodRef.Namespace,
				Pod:       pod.PodRef.Name,
				Container: ctr.Name,
			}
			if cpu := ctr.CPU; cpu != nil {
				m.Timestamp = cpu.Time
				if cpu.UsageNanoCores != nil {
					m.CPUUsageNanoCores = *cpu.UsageNanoCores
				}
			}
			if mem := ctr.Memory; mem != nil {
				if mem.WorkingSetBytes != nil {
					m.MemoryWorkingSet = *mem.WorkingSetBytes
				}
				if mem.UsageBytes != nil {
					m.MemoryUsageBytes = *mem.UsageBytes
				}
				if mem.RSSBytes != nil {
					m.MemoryRSSBytes = *mem.RSSBytes
				}
			}
			out = append(out, m)
		}
	}
	return out
}
