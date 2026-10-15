package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
)

// defaultKubeletFallbackTimeout is used when NewKubeletSummaryClient is
// given a non-positive timeout — preserves prior behavior for callers that
// don't (yet) expose this as configurable (see NodeCollectorConfig for the
// one that does). This call had no timeout at all before this constant was
// introduced: a hung apiserver/kubelet-proxy response could block whatever
// goroutine was waiting on it indefinitely.
const defaultKubeletFallbackTimeout = 15 * time.Second

// KubeletSummaryClient fetches per-node resource usage directly from the kubelet
// Summary API (/stats/summary) via the API-server node proxy subresource.
//
// It exists as a fallback for nodes that have NO zxporter-nodemon pod running
// (e.g. the DaemonSet pod failed to schedule, or is not yet ready). Because the
// kubelet runs on every node by construction, this gives a hard coverage
// guarantee that a DaemonSet alone cannot.
//
// The Summary API exposes the same kubelet stats nodemon itself is built on
// (usageNanoCores / workingSetBytes) plus node filesystem occupancy
// (node.fs and node.runtime.imageFs), so results are returned in the existing
// UnifiedNodeMetric / UnifiedContainerMetric shapes and reuse all downstream
// conversion. Network and disk-I/O rates, CPU-throttle, GPU and JVM metrics
// remain nodemon-only and stay zero for fallback nodes (a deliberate degraded
// mode, on par with what metrics-server would have provided).
type KubeletSummaryClient struct {
	k8sClient      kubernetes.Interface
	log            logr.Logger
	requestTimeout time.Duration
}

// NewKubeletSummaryClient creates a client that reads kubelet /stats/summary
// through the API-server node proxy. It requires the `nodes/proxy` (get) RBAC
// verb on the zxporter ClusterRole. requestTimeout bounds each call; a
// non-positive value falls back to defaultKubeletFallbackTimeout.
func NewKubeletSummaryClient(k8sClient kubernetes.Interface, log logr.Logger, requestTimeout time.Duration) *KubeletSummaryClient {
	if requestTimeout <= 0 {
		requestTimeout = defaultKubeletFallbackTimeout
	}
	return &KubeletSummaryClient{
		k8sClient:      k8sClient,
		log:            log.WithName("kubelet-summary-client"),
		requestTimeout: requestTimeout,
	}
}

// kubeletSummary is a minimal subset of k8s.io/kubelet/pkg/apis/stats/v1alpha1.Summary.
// Defined locally to avoid adding the k8s.io/kubelet dependency.
type kubeletSummary struct {
	Node kubeletNodeStats  `json:"node"`
	Pods []kubeletPodStats `json:"pods"`
}

type kubeletNodeStats struct {
	NodeName string               `json:"nodeName"`
	CPU      *kubeletCPUStats     `json:"cpu"`
	Memory   *kubeletMemoryStats  `json:"memory"`
	Fs       *kubeletFsStats      `json:"fs"`
	Runtime  *kubeletRuntimeStats `json:"runtime"`
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

type kubeletFsStats struct {
	AvailableBytes *uint64 `json:"availableBytes"`
	CapacityBytes  *uint64 `json:"capacityBytes"`
	UsedBytes      *uint64 `json:"usedBytes"`
	Inodes         *uint64 `json:"inodes"`
	InodesFree     *uint64 `json:"inodesFree"`
	InodesUsed     *uint64 `json:"inodesUsed"`
}

type kubeletRuntimeStats struct {
	ImageFs *kubeletFsStats `json:"imageFs"`
}

// fetchSummary retrieves and parses the kubelet Summary API for a single node
// via GET /api/v1/nodes/<node>/proxy/stats/summary.
func (c *KubeletSummaryClient) fetchSummary(ctx context.Context, nodeName string) (*kubeletSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

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
	applyNodeStorage(m, summary.Node.Fs, summary.Node.Runtime)
	return m
}

// applyNodeStorage copies filesystem occupancy from a kubelet node summary onto
// a UnifiedNodeMetric. Shared by the full node-metrics fallback and by the
// storage-only top-up FetchNodeStorageByNode uses.
func applyNodeStorage(m *UnifiedNodeMetric, fs *kubeletFsStats, runtime *kubeletRuntimeStats) {
	if fs != nil {
		m.NodefsUsedBytes = derefKubeletU64(fs.UsedBytes)
		m.NodefsCapacityBytes = derefKubeletU64(fs.CapacityBytes)
		m.NodefsAvailableBytes = derefKubeletU64(fs.AvailableBytes)
		m.NodefsInodesUsed = derefKubeletU64(fs.InodesUsed)
		m.NodefsInodesTotal = derefKubeletU64(fs.Inodes)
	}
	if runtime != nil && runtime.ImageFs != nil {
		m.ImagefsUsedBytes = derefKubeletU64(runtime.ImageFs.UsedBytes)
		m.ImagefsCapacityBytes = derefKubeletU64(runtime.ImageFs.CapacityBytes)
		m.ImagefsAvailableBytes = derefKubeletU64(runtime.ImageFs.AvailableBytes)
		m.ImagefsSharedDevice = kubeletImagefsShared(fs, runtime.ImageFs)
	}
}

func derefKubeletU64(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}

// kubeletImagefsShared mirrors nodemon's imagefsSharesDeviceWithNodefs so both
// collection paths classify a node identically. Shared iff total capacity and
// total inode count both match; zero capacity is not evidence of sharing.
func kubeletImagefsShared(nodefs, imagefs *kubeletFsStats) bool {
	if nodefs == nil || imagefs == nil {
		return false
	}
	capacity := derefKubeletU64(nodefs.CapacityBytes)
	inodes := derefKubeletU64(nodefs.Inodes)
	if capacity == 0 || inodes == 0 {
		return false
	}
	return capacity == derefKubeletU64(imagefs.CapacityBytes) && inodes == derefKubeletU64(imagefs.Inodes)
}

// FetchNodeStorageByNode returns ONLY the filesystem occupancy fields for a
// node, leaving CPU and memory zeroed.
//
// This exists for the rollout window: a nodemon pod that predates the storage
// fields returns a snapshot with no filesystem data, and the node collector
// tops it up from the kubelet rather than waiting for the DaemonSet to roll.
// It is deliberately NOT called unconditionally — that would reintroduce the
// per-node redundant round trip removed in issue 9410.
func (c *KubeletSummaryClient) FetchNodeStorageByNode(
	ctx context.Context,
	nodeName string,
) (*UnifiedNodeMetric, error) {
	summary, err := c.fetchSummary(ctx, nodeName)
	if err != nil {
		return nil, err
	}
	m := &UnifiedNodeMetric{NodeName: nodeName}
	applyNodeStorage(m, summary.Node.Fs, summary.Node.Runtime)
	return m, nil
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
				// The memory cache/swap/active/inactive breakdown is unavailable on
				// the kubelet Summary-API fallback path: it requires the nodemon
				// cAdvisor scrape (page cache and swap gauges), which this in-process
				// fallback does not have. Only working set, usage, and RSS are
				// available here.
			}
			out = append(out, m)
		}
	}
	return out
}
