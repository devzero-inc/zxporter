package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// UnifiedContainerMetric mirrors nodemon's ContainerMetricsResponse.
// Defined here to avoid a direct import dependency on the nodemon package.
type UnifiedContainerMetric struct {
	NodeName         string    `json:"node_name"`
	Namespace        string    `json:"namespace"`
	Pod              string    `json:"pod"`
	Container        string    `json:"container"`
	Timestamp        time.Time `json:"timestamp"`
	CPUUsageNanoCores uint64   `json:"cpu_usage_nanocores"`
	MemoryWorkingSet  uint64   `json:"memory_working_set_bytes"`
	MemoryUsageBytes  uint64   `json:"memory_usage_bytes"`
	MemoryRSSBytes    uint64   `json:"memory_rss_bytes"`
	NetworkRxBytes    uint64   `json:"network_rx_bytes"`
	NetworkTxBytes    uint64   `json:"network_tx_bytes"`
	// cAdvisor rates
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
	// GPU (optional)
	GPUUtilization   float64 `json:"gpu_utilization,omitempty"`
	GPUMemoryUsedMiB float64 `json:"gpu_memory_used_mib,omitempty"`
	GPUMemoryFreeMiB float64 `json:"gpu_memory_free_mib,omitempty"`
	GPUPowerWatts    float64 `json:"gpu_power_watts,omitempty"`
	GPUTemperature   float64 `json:"gpu_temperature_celsius,omitempty"`
}

// UnifiedNodeMetric mirrors nodemon's NodeMetricsResponse.
// Defined here to avoid a direct import dependency on the nodemon package.
type UnifiedNodeMetric struct {
	NodeName                  string    `json:"node_name"`
	Timestamp                 time.Time `json:"timestamp"`
	CPUUsageNanoCores         uint64    `json:"cpu_usage_nanocores"`
	MemoryWorkingSet          uint64    `json:"memory_working_set_bytes"`
	NetworkRxBytesPerSec      float64   `json:"network_rx_bytes_per_sec"`
	NetworkTxBytesPerSec      float64   `json:"network_tx_bytes_per_sec"`
	NetworkRxPacketsPerSec    float64   `json:"network_rx_packets_per_sec"`
	NetworkTxPacketsPerSec    float64   `json:"network_tx_packets_per_sec"`
	NetworkRxErrorsPerSec     float64   `json:"network_rx_errors_per_sec"`
	NetworkTxErrorsPerSec     float64   `json:"network_tx_errors_per_sec"`
	NetworkRxDropsPerSec      float64   `json:"network_rx_drops_per_sec"`
	NetworkTxDropsPerSec      float64   `json:"network_tx_drops_per_sec"`
	DiskReadBytesPerSec       float64   `json:"disk_read_bytes_per_sec"`
	DiskWriteBytesPerSec      float64   `json:"disk_write_bytes_per_sec"`
	DiskReadOpsPerSec         float64   `json:"disk_read_ops_per_sec"`
	DiskWriteOpsPerSec        float64   `json:"disk_write_ops_per_sec"`
}

// UnifiedPVCMetric mirrors nodemon's PVCMetricsResponse.
// Defined here to avoid a direct import dependency on the nodemon package.
type UnifiedPVCMetric struct {
	Namespace      string `json:"namespace"`
	Pod            string `json:"pod"`
	PVCName        string `json:"pvc_name"`
	UsedBytes      uint64 `json:"used_bytes"`
	CapacityBytes  uint64 `json:"capacity_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// NodemonMetric represents a single GPU metric entry returned by the nodemon.
// This mirrors nodemon.GPUMetricResponse but is defined here to avoid a direct import
// dependency on the nodemon package (which is a separate binary).
type NodemonMetric struct {
	NodeName      string `json:"node_name"`
	ModelName     string `json:"model_name"`
	Device        string `json:"device"`
	DeviceID      string `json:"device_id"`
	DeviceUUID    string `json:"device_uuid"`
	MIGProfile    string `json:"mig_profile,omitempty"`
	MIGInstanceID string `json:"mig_instance_id,omitempty"`

	Pod          string `json:"pod"`
	Container    string `json:"container"`
	Namespace    string `json:"namespace"`
	WorkloadName string `json:"workload_name,omitempty"`
	WorkloadKind string `json:"workload_kind,omitempty"`

	SMActive             float64 `json:"sm_active"`
	SMOccupancy          float64 `json:"sm_occupancy"`
	TensorActive         float64 `json:"tensor_active"`
	DRAMActive           float64 `json:"dram_active"`
	PCIeTXBytes          float64 `json:"pcie_tx_bytes"`
	PCIeRXBytes          float64 `json:"pcie_rx_bytes"`
	NVLinkTXBytes        float64 `json:"nvlink_tx_bytes"`
	NVLinkRXBytes        float64 `json:"nvlink_rx_bytes"`
	GraphicsEngineActive float64 `json:"graphics_engine_active"`
	FramebufferTotal     float64 `json:"framebuffer_total"`
	FramebufferUsed      float64 `json:"framebuffer_used"`
	FramebufferFree      float64 `json:"framebuffer_free"`
	PCIeLinkGen          float64 `json:"pcie_link_gen"`
	PCIeLinkWidth        float64 `json:"pcie_link_width"`
	Temperature          float64 `json:"temperature"`
	MemoryTemperature    float64 `json:"memory_temperature"`
	PowerUsage           float64 `json:"power_usage"`
	GPUUtilization       float64 `json:"gpu_utilization"`
	IntPipeActive        float64 `json:"int_pipe_active"`
	FP16PipeActive       float64 `json:"fp16_pipe_active"`
	FP32PipeActive       float64 `json:"fp32_pipe_active"`
	FP64PipeActive       float64 `json:"fp64_pipe_active"`
	ClocksEventReasons   float64 `json:"clocks_event_reasons"`
	XIDErrors            float64 `json:"xid_errors"`
	PowerViolation       float64 `json:"power_violation"`
	ThermalViolation     float64 `json:"thermal_violation"`
	SMClock              float64 `json:"sm_clock"`
	MemClock             float64 `json:"mem_clock"`

	Timestamp time.Time `json:"timestamp"`
}

// gpuContainerKey uniquely identifies a container for GPU metric lookup.
type gpuContainerKey struct {
	Pod       string
	Container string
	Namespace string
}

const (
	// Well-known label used by the zxporter-nodemon DaemonSet helm chart.
	nodemonLabelSelector = "app.kubernetes.io/name=zxporter-nodemon"
	// Default HTTP port for the nodemon.
	nodemonDefaultPort = 6061
	// How long to cache the node→podIP mapping before re-discovering.
	nodemonCacheTTL = 30 * time.Second
)

// NodemonClient auto-discovers zxporter-nodemon DaemonSet pods and
// routes metrics requests to the correct pod based on node name.
// Discovery is cached with a TTL to handle new nodes and pod restarts.
type NodemonClient struct {
	k8sClient  kubernetes.Interface
	namespace  string
	port       int
	httpClient *http.Client
	log        logr.Logger

	// cached discovery
	mu            sync.RWMutex
	nodeToIP      map[string]string // nodeName → podIP
	lastRefreshed time.Time

	// state tracking
	lastKnownCount int  // last known number of nodemon pods
	stateLogged    bool // whether we've logged the initial state
}

// NewNodemonClient creates a client that auto-discovers nodemon pods
// in the given namespace using well-known labels.
func NewNodemonClient(
	k8sClient kubernetes.Interface,
	namespace string,
	log logr.Logger,
) *NodemonClient {
	return &NodemonClient{
		k8sClient: k8sClient,
		namespace: namespace,
		port:      nodemonDefaultPort,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		log: log.WithName("nodemon-client"),
	}
}

// refreshCache re-discovers nodemon pods if the cache has expired.
func (c *NodemonClient) refreshCache(ctx context.Context) (map[string]string, error) {
	c.mu.RLock()
	if time.Since(c.lastRefreshed) < nodemonCacheTTL && c.nodeToIP != nil {
		cached := c.nodeToIP
		c.mu.RUnlock()
		return cached, nil
	}
	c.mu.RUnlock()

	// Cache expired — re-discover
	pods, err := c.k8sClient.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: nodemonLabelSelector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("listing nodemon pods: %w", err)
	}

	nodeToIP := make(map[string]string)
	for _, pod := range pods.Items {
		if pod.Status.PodIP != "" && pod.Spec.NodeName != "" {
			nodeToIP[pod.Spec.NodeName] = pod.Status.PodIP
		}
	}

	currentCount := len(nodeToIP)

	c.mu.Lock()
	c.nodeToIP = nodeToIP
	c.lastRefreshed = time.Now()
	previousCount := c.lastKnownCount
	wasLogged := c.stateLogged
	c.lastKnownCount = currentCount
	c.stateLogged = true
	c.mu.Unlock()

	// Only log when state changes or on first discovery
	if !wasLogged {
		if currentCount > 0 {
			c.log.Info("nodemon pods discovered", "nodesWithGPU", currentCount)
		} else {
			c.log.Info("No nodemon pods found, will use Prometheus for GPU metrics")
		}
	} else if currentCount != previousCount {
		if previousCount == 0 && currentCount > 0 {
			c.log.Info("nodemon pods now available", "nodesWithGPU", currentCount)
		} else if previousCount > 0 && currentCount == 0 {
			c.log.Info("nodemon pods no longer available, falling back to Prometheus")
		} else {
			// Count changed but still > 0
			c.log.Info("nodemon pod count changed", "previous", previousCount, "current", currentCount)
		}
	}
	// If state hasn't changed, don't log anything (reduces noise)

	return nodeToIP, nil
}

// HasExporters returns true if any nodemon pods were discovered.
func (c *NodemonClient) HasExporters(ctx context.Context) bool {
	m, err := c.refreshCache(ctx)
	if err != nil {
		return false
	}
	return len(m) > 0
}

// FetchAllMetrics discovers all nodemon pods and fetches metrics from each,
// merging the results into a single slice. Used by the container collector.
func (c *NodemonClient) FetchAllMetrics(ctx context.Context) ([]NodemonMetric, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodeToIP) == 0 {
		return nil, nil
	}

	var allMetrics []NodemonMetric
	for nodeName, podIP := range nodeToIP {
		url := fmt.Sprintf("http://%s:%d/container/metrics", podIP, c.port)
		metrics, fetchErr := c.fetchMetrics(ctx, url)
		if fetchErr != nil {
			c.log.Error(
				fetchErr,
				"Failed to fetch GPU metrics from exporter pod",
				"node",
				nodeName,
				"podIP",
				podIP,
			)
			continue
		}
		allMetrics = append(allMetrics, metrics...)
	}

	return allMetrics, nil
}

// FetchMetricsByNode fetches GPU metrics from the exporter pod running on the given node.
// Returns nil if no exporter is running on that node (expected for non-GPU nodes).
func (c *NodemonClient) FetchMetricsByNode(
	ctx context.Context,
	nodeName string,
) ([]NodemonMetric, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return nil, err
	}

	podIP, ok := nodeToIP[nodeName]
	if !ok {
		return nil, nil
	}

	url := fmt.Sprintf("http://%s:%d/container/metrics", podIP, c.port)
	return c.fetchMetrics(ctx, url)
}

func (c *NodemonClient) fetchMetrics(
	ctx context.Context,
	url string,
) ([]NodemonMetric, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to nodemon failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodemon returned status %d", resp.StatusCode)
	}

	const maxResponseBytes = 16 << 20 // 16MiB safety cap
	var metrics []NodemonMetric
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&metrics); err != nil {
		return nil, fmt.Errorf("decoding nodemon response: %w", err)
	}

	return metrics, nil
}

// fetchContainerMetrics fetches from a single nodemon pod's /v2/container/metrics endpoint.
func (c *NodemonClient) fetchContainerMetrics(
	ctx context.Context,
	baseURL string,
) ([]UnifiedContainerMetric, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v2/container/metrics", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to nodemon failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodemon returned status %d", resp.StatusCode)
	}

	var metrics []UnifiedContainerMetric
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		return nil, fmt.Errorf("decoding nodemon response: %w", err)
	}

	return metrics, nil
}

// FetchAllContainerMetrics fetches container metrics from all discovered nodemon pods,
// merging the results into a single slice.
func (c *NodemonClient) FetchAllContainerMetrics(ctx context.Context) ([]UnifiedContainerMetric, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodeToIP) == 0 {
		return nil, nil
	}

	var allMetrics []UnifiedContainerMetric
	for nodeName, podIP := range nodeToIP {
		baseURL := fmt.Sprintf("http://%s:%d", podIP, c.port)
		metrics, fetchErr := c.fetchContainerMetrics(ctx, baseURL)
		if fetchErr != nil {
			c.log.Error(
				fetchErr,
				"Failed to fetch container metrics from nodemon pod",
				"node", nodeName,
				"podIP", podIP,
			)
			continue
		}
		allMetrics = append(allMetrics, metrics...)
	}

	return allMetrics, nil
}

// fetchNodeMetrics fetches from a single nodemon pod's /node/metrics endpoint.
func (c *NodemonClient) fetchNodeMetrics(
	ctx context.Context,
	baseURL string,
) (*UnifiedNodeMetric, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/node/metrics", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to nodemon failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodemon returned status %d", resp.StatusCode)
	}

	var metric UnifiedNodeMetric
	if err := json.NewDecoder(resp.Body).Decode(&metric); err != nil {
		return nil, fmt.Errorf("decoding nodemon response: %w", err)
	}

	return &metric, nil
}

// FetchNodeMetricsByNode fetches node metrics from the nodemon pod running on the given node.
// Returns nil if no nodemon pod is running on that node.
func (c *NodemonClient) FetchNodeMetricsByNode(
	ctx context.Context,
	nodeName string,
) (*UnifiedNodeMetric, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return nil, err
	}

	podIP, ok := nodeToIP[nodeName]
	if !ok {
		return nil, nil
	}

	baseURL := fmt.Sprintf("http://%s:%d", podIP, c.port)
	return c.fetchNodeMetrics(ctx, baseURL)
}

// fetchPVCMetrics fetches from a single nodemon pod's /pvc/metrics endpoint.
func (c *NodemonClient) fetchPVCMetrics(
	ctx context.Context,
	baseURL string,
) ([]UnifiedPVCMetric, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/pvc/metrics", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to nodemon failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodemon returned status %d", resp.StatusCode)
	}

	var metrics []UnifiedPVCMetric
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		return nil, fmt.Errorf("decoding nodemon response: %w", err)
	}

	return metrics, nil
}

// FetchAllPVCMetrics fetches PVC metrics from all discovered nodemon pods,
// merging the results into a single slice.
func (c *NodemonClient) FetchAllPVCMetrics(ctx context.Context) ([]UnifiedPVCMetric, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodeToIP) == 0 {
		return nil, nil
	}

	var allMetrics []UnifiedPVCMetric
	for nodeName, podIP := range nodeToIP {
		baseURL := fmt.Sprintf("http://%s:%d", podIP, c.port)
		metrics, fetchErr := c.fetchPVCMetrics(ctx, baseURL)
		if fetchErr != nil {
			c.log.Error(
				fetchErr,
				"Failed to fetch PVC metrics from nodemon pod",
				"node", nodeName,
				"podIP", podIP,
			)
			continue
		}
		allMetrics = append(allMetrics, metrics...)
	}

	return allMetrics, nil
}

// IndexByContainer indexes GPU metrics by (pod, container, namespace) for O(1) lookup.
// Multiple GPUs for the same container are grouped together.
func IndexByContainer(metrics []NodemonMetric) map[gpuContainerKey][]NodemonMetric {
	index := make(map[gpuContainerKey][]NodemonMetric)
	for _, m := range metrics {
		key := gpuContainerKey{
			Pod:       m.Pod,
			Container: m.Container,
			Namespace: m.Namespace,
		}
		index[key] = append(index[key], m)
	}
	return index
}

// ContainerGPUMetricsFromNodemon converts nodemon metrics for a container into
// the map[string]interface{} format expected by processContainerMetrics.
func ContainerGPUMetricsFromNodemon(
	gpuMetrics []NodemonMetric,
	gpuRequestCount, gpuLimitCount int64,
) map[string]interface{} {
	if len(gpuMetrics) == 0 {
		return make(map[string]interface{})
	}

	metrics := make(map[string]interface{})
	gpuCount := float64(len(gpuMetrics))
	metrics["GPUMetricsCount"] = gpuCount

	// Aggregate metrics
	var totalUtil, totalMemUsed, totalMemFree, totalPower float64
	var totalTemp, totalSMClock, totalMemClock float64

	gpuUUIDSet := make(map[string]bool)
	gpuModels := make(map[string]int)
	individualGPUs := make([]map[string]interface{}, 0, len(gpuMetrics))

	for _, gm := range gpuMetrics {
		totalUtil += gm.GPUUtilization
		totalMemUsed += gm.FramebufferUsed
		totalMemFree += gm.FramebufferFree
		totalPower += gm.PowerUsage
		totalTemp += gm.Temperature
		totalSMClock += gm.SMClock
		totalMemClock += gm.MemClock

		if gm.DeviceUUID != "" {
			gpuUUIDSet[gm.DeviceUUID] = true
		}
		if gm.ModelName != "" {
			gpuModels[gm.ModelName]++
		}

		totalMem := gm.FramebufferUsed + gm.FramebufferFree
		memUtilPct := 0.0
		if totalMem > 0 {
			memUtilPct = (gm.FramebufferUsed / totalMem) * 100
		}

		individualGPUs = append(individualGPUs, map[string]interface{}{
			"UUID":                        gm.DeviceUUID,
			"ModelName":                   gm.ModelName,
			"DeviceIndex":                 gm.Device,
			"Utilization":                 gm.GPUUtilization,
			"MemoryUsed":                  gm.FramebufferUsed,
			"MemoryFree":                  gm.FramebufferFree,
			"TotalMemory":                 totalMem,
			"MemoryUtilizationPercentage": memUtilPct,
			"PowerUsage":                  gm.PowerUsage,
			"Temperature":                 gm.Temperature,
			"SMClock":                     gm.SMClock,
			"MemClock":                    gm.MemClock,
		})
	}

	avgUtil := totalUtil / gpuCount
	avgTemp := totalTemp / gpuCount
	avgSMClock := totalSMClock / gpuCount
	avgMemClock := totalMemClock / gpuCount

	metrics["GPUUtilizationPercentage"] = avgUtil
	metrics["GPUMemoryUsedMb"] = totalMemUsed
	metrics["GPUMemoryFreeMb"] = totalMemFree
	metrics["GPUTotalMemoryMb"] = totalMemUsed + totalMemFree
	metrics["GPUPowerUsageWatts"] = totalPower
	metrics["GPUTemperatureCelsius"] = avgTemp
	metrics["GPUSMClockMHz"] = avgSMClock
	metrics["GPUMemClockMHz"] = avgMemClock
	metrics["GPUUsage"] = totalUtil / 100.0

	gpuUUIDs := make([]string, 0, len(gpuUUIDSet))
	for uuid := range gpuUUIDSet {
		gpuUUIDs = append(gpuUUIDs, uuid)
	}
	modelSummary := make([]string, 0, len(gpuModels))
	for model, count := range gpuModels {
		modelSummary = append(modelSummary, fmt.Sprintf("%dx %s", count, model))
	}

	metrics["GPUModels"] = modelSummary
	metrics["GPUUUIDs"] = gpuUUIDs
	metrics["IndividualGPUs"] = individualGPUs
	metrics["GPURequestCount"] = gpuRequestCount
	metrics["GPULimitCount"] = gpuLimitCount

	return metrics
}

// NodeGPUMetricsFromNodemon converts nodemon metrics for a node into
// the map[string]interface{} format expected by the node collector.
func NodeGPUMetricsFromNodemon(gpuMetrics []NodemonMetric) map[string]interface{} {
	if len(gpuMetrics) == 0 {
		return make(map[string]interface{})
	}

	metrics := make(map[string]interface{})
	gpuCount := float64(len(gpuMetrics))

	var totalUtil, maxUtil float64
	var totalMemUsed, totalMemFree, totalPower float64
	var totalTemp, maxTemp, totalMemTemp, maxMemTemp float64
	var totalTensor, totalDram float64
	var totalPCIeTx, totalPCIeRx float64
	var totalGraphics float64

	gpuUUIDSet := make(map[string]bool)
	gpuModels := make(map[string]int)

	for i, gm := range gpuMetrics {
		totalUtil += gm.GPUUtilization
		if i == 0 || gm.GPUUtilization > maxUtil {
			maxUtil = gm.GPUUtilization
		}

		totalMemUsed += gm.FramebufferUsed
		totalMemFree += gm.FramebufferFree
		totalPower += gm.PowerUsage

		totalTemp += gm.Temperature
		if i == 0 || gm.Temperature > maxTemp {
			maxTemp = gm.Temperature
		}

		totalMemTemp += gm.MemoryTemperature
		if i == 0 || gm.MemoryTemperature > maxMemTemp {
			maxMemTemp = gm.MemoryTemperature
		}

		totalTensor += gm.TensorActive
		totalDram += gm.DRAMActive
		totalPCIeTx += gm.PCIeTXBytes
		totalPCIeRx += gm.PCIeRXBytes
		totalGraphics += gm.GraphicsEngineActive

		if gm.DeviceUUID != "" {
			gpuUUIDSet[gm.DeviceUUID] = true
		}
		if gm.ModelName != "" {
			gpuModels[gm.ModelName]++
		}
	}

	metrics["GPUCount"] = gpuCount
	metrics["GPUUtilizationAvg"] = totalUtil / gpuCount
	metrics["GPUUtilizationMax"] = maxUtil
	metrics["GPUMemoryUsedTotal"] = totalMemUsed
	metrics["GPUMemoryFreeTotal"] = totalMemFree
	metrics["GPUMemoryTotalMb"] = totalMemUsed + totalMemFree
	metrics["GPUPowerUsageTotal"] = totalPower
	metrics["GPUTemperatureAvg"] = totalTemp / gpuCount
	metrics["GPUTemperatureMax"] = maxTemp
	metrics["GPUMemoryTemperatureAvg"] = totalMemTemp / gpuCount
	metrics["GPUMemoryTemperatureMax"] = maxMemTemp
	metrics["GPUTensorUtilizationAvg"] = totalTensor / gpuCount
	metrics["GPUDramUtilizationAvg"] = totalDram / gpuCount
	metrics["GPUPCIeTxBytesTotal"] = totalPCIeTx
	metrics["GPUPCIeRxBytesTotal"] = totalPCIeRx
	metrics["GPUGraphicsUtilizationAvg"] = totalGraphics / gpuCount
	metrics["GPUUsage"] = totalUtil / 100.0

	gpuUUIDs := make([]string, 0, len(gpuUUIDSet))
	for uuid := range gpuUUIDSet {
		gpuUUIDs = append(gpuUUIDs, uuid)
	}
	modelSummary := make([]string, 0, len(gpuModels))
	for model, count := range gpuModels {
		modelSummary = append(modelSummary, fmt.Sprintf("%dx %s", count, model))
	}

	metrics["GPUModels"] = modelSummary
	metrics["GPUUUIDs"] = gpuUUIDs

	return metrics
}

// compareKeysNumeric is the set of keys to compare numerically between exporter and Prometheus.
var compareKeysNumeric = []string{
	"GPUMetricsCount",
	"GPUUtilizationPercentage",
	"GPUMemoryUsedMb",
	"GPUMemoryFreeMb",
	"GPUTotalMemoryMb",
	"GPUPowerUsageWatts",
	"GPUTemperatureCelsius",
	"GPUSMClockMHz",
	"GPUMemClockMHz",
	"GPUUsage",
	// Node-level keys
	"GPUCount",
	"GPUUtilizationAvg",
	"GPUUtilizationMax",
	"GPUMemoryUsedTotal",
	"GPUMemoryFreeTotal",
	"GPUMemoryTotalMb",
	"GPUPowerUsageTotal",
	"GPUTemperatureAvg",
	"GPUTemperatureMax",
	"GPUMemoryTemperatureAvg",
	"GPUMemoryTemperatureMax",
	"GPUTensorUtilizationAvg",
	"GPUDramUtilizationAvg",
	"GPUPCIeTxBytesTotal",
	"GPUPCIeRxBytesTotal",
	"GPUGraphicsUtilizationAvg",
}

// CompareGPUMetrics compares GPU metrics from the exporter and Prometheus, logging any diffs.
// tolerance is the relative difference threshold (0.05 = 5%). Keys present in only one source
// are also logged. The exporter result is always used as the primary source; this is purely
// for observability.
func CompareGPUMetrics(
	log logr.Logger,
	label string,
	exporterMetrics, prometheusMetrics map[string]interface{},
) {
	if len(exporterMetrics) == 0 && len(prometheusMetrics) == 0 {
		return
	}

	const tolerance = 0.05 // 5% relative diff

	var matchedKeys []string
	var diffCount, onlyExpCount, onlyPromCount int

	for _, key := range compareKeysNumeric {
		expVal, expOk := toFloat64(exporterMetrics[key])
		promVal, promOk := toFloat64(prometheusMetrics[key])

		if !expOk && !promOk {
			continue // key absent from both
		}

		if expOk && !promOk {
			// Skip zero-valued exporter-only keys — these are DCGM profiling counters
			// that are not enabled, so they're noise.
			if expVal == 0 {
				continue
			}
			log.Info("[GPU-COMPARE] key only in exporter",
				"label", label, "key", key, "exporter", expVal)
			onlyExpCount++
			continue
		}
		if !expOk && promOk {
			if promVal == 0 {
				continue
			}
			log.Info("[GPU-COMPARE] key only in prometheus",
				"label", label, "key", key, "prometheus", promVal)
			onlyPromCount++
			continue
		}

		// Both present — compare with tolerance
		if !withinTolerance(expVal, promVal, tolerance) {
			log.Info("[GPU-COMPARE] metric diff",
				"label", label,
				"key", key,
				"exporter", expVal,
				"prometheus", promVal,
				"diffPercent", relativeDiffPct(expVal, promVal))
			diffCount++
		} else {
			matchedKeys = append(matchedKeys, key)
		}
	}

	// Always log a summary so user can see what matched
	log.Info("[GPU-COMPARE] summary",
		"label", label,
		"matched", len(matchedKeys),
		"diffs", diffCount,
		"onlyInExporter", onlyExpCount,
		"onlyInPrometheus", onlyPromCount,
		"matchedKeys", matchedKeys)
}

func toFloat64(v interface{}) (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}

func withinTolerance(a, b, tol float64) bool {
	if a == b {
		return true
	}
	denom := math.Max(math.Abs(a), math.Abs(b))
	if denom == 0 {
		return true
	}
	return math.Abs(a-b)/denom <= tol
}

func relativeDiffPct(a, b float64) float64 {
	denom := math.Max(math.Abs(a), math.Abs(b))
	if denom == 0 {
		return 0
	}
	return math.Abs(a-b) / denom * 100
}
