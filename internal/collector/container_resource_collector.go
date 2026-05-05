// internal/collector/container_resource_collector.go
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/nodemon"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// ContainerResourceCollectorConfig holds configuration for the resource collector
type ContainerResourceCollectorConfig struct {
	// UpdateInterval specifies how often to collect metrics
	UpdateInterval time.Duration

	// DisableGPUMetrics determines whether to disable GPU metrics collection
	// Default is false, so metrics are collected by default
	DisableGPUMetrics bool
}

// throttleTracker tracks last emission time for CPU throttle events per container to avoid duplicates.
type throttleTracker struct {
	lastEmitted map[string]time.Time // key: "ns/pod/container" → last emit time
	mu          sync.Mutex
}

// ContainerResourceCollector collects container resource usage metrics
type ContainerResourceCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	nodemonClient   *NodemonClient
	informerFactory informers.SharedInformerFactory
	podInformer     cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	ticker          *time.Ticker
	config          ContainerResourceCollectorConfig
	namespaces      []string
	excludedPods    map[types.NamespacedName]bool
	logger          logr.Logger
	metrics         *TelemetryMetrics
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
	throttle        throttleTracker
	// nodemonContainerMetricsCache holds pre-fetched container metrics from nodemon,
	// indexed by "namespace/podName", refreshed once per collection cycle.
	nodemonContainerMetricsCache map[string][]UnifiedContainerMetric
	// networkByteRates computes per-second rates from cumulative network byte counters
	// returned by kubelet stats/summary (which are totals, not rates).
	networkByteRates *nodemon.RateCalculator
	rsLister                     appslisters.ReplicaSetLister
	rsInformer                   cache.SharedIndexInformer
}

// NewContainerResourceCollector creates a new collector for container resource metrics
func NewContainerResourceCollector(
	k8sClient kubernetes.Interface,
	metricsClient *metricsv1.Clientset,
	config ContainerResourceCollectorConfig,
	namespaces []string,
	excludedPods []ExcludedPod,
	maxBatchSize int, // Added parameter
	maxBatchTime time.Duration, // Added parameter
	logger logr.Logger,
	metrics *TelemetryMetrics,
	telemetryLogger telemetry_logger.Logger,
) *ContainerResourceCollector {
	// Convert excluded pods to a map for quicker lookups
	excludedPodsMap := make(map[types.NamespacedName]bool)
	for _, pod := range excludedPods {
		excludedPodsMap[types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      pod.Name,
		}] = true
	}

	// Default update interval if not specified
	if config.UpdateInterval <= 0 {
		config.UpdateInterval = 10 * time.Second
	}

	// Create channels
	batchChan := make(
		chan CollectedResource,
		500,
	) // Keep original buffer size for individual items
	resourceChan := make(chan []CollectedResource, 200) // Buffer for batches

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	// Initialize nodemon client for auto-discovery in constructor
	// so IsAvailable() can check it before Start() is called.
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "devzero-system"
	}
	nodemonClient := NewNodemonClient(k8sClient, ns, logger)

	return &ContainerResourceCollector{
		k8sClient:          k8sClient,
		metricsClient:      metricsClient,
		nodemonClient:      nodemonClient,
		batchChan:          batchChan,
		resourceChan:       resourceChan,
		batcher:            batcher,
		stopCh:             make(chan struct{}),
		config:             config,
		namespaces:         namespaces,
		excludedPods:       excludedPodsMap,
		logger:             logger.WithName("container-resource-collector"),
		metrics:            metrics,
		telemetryLogger:    telemetryLogger,
		throttle:           throttleTracker{lastEmitted: make(map[string]time.Time)},
		networkByteRates:   nodemon.NewRateCalculator(),
	}
}

// Start begins the container resource collection process
func (c *ContainerResourceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting container resource collector",
		"namespaces", c.namespaces,
		"updateInterval", c.config.UpdateInterval,
		"disableGPUMetrics", c.config.DisableGPUMetrics)

	// Check if metrics client is available
	if c.metricsClient == nil {
		return fmt.Errorf("metrics client is not available, cannot collect container resources")
	}

	// Create informer factory based on namespace configuration
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
			c.k8sClient,
			0, // No resync period, rely on events
			informers.WithNamespace(c.namespaces[0]),
		)
	} else {
		// Watch all namespaces
		c.informerFactory = informers.NewSharedInformerFactory(c.k8sClient, 0)
	}

	// Create pod informer to maintain a cache of pod information
	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Create ReplicaSet informer for workload resolution
	c.rsInformer = c.informerFactory.Apps().V1().ReplicaSets().Informer()
	c.rsLister = c.informerFactory.Apps().V1().ReplicaSets().Lister()

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced, c.rsInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for pod cache to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for container resources")
	c.batcher.start()

	// Start a ticker to collect resource metrics at regular intervals
	c.ticker = time.NewTicker(c.config.UpdateInterval)

	// Start the collection loop
	go c.collectResourcesLoop(ctx)

	// Monitor for context cancellation
	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// collectResourcesLoop collects container resource metrics at regular intervals
func (c *ContainerResourceCollector) collectResourcesLoop(ctx context.Context) {
	// Collect immediately on start
	c.collectAllContainerResources(ctx)

	// Then collect based on ticker
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.collectAllContainerResources(ctx)
		}
	}
}

// collectAllContainerResources collects resource metrics for all containers
func (c *ContainerResourceCollector) collectAllContainerResources(ctx context.Context) {
	// Build pod metrics from nodemon data
	podMetricsList, err := c.buildPodMetricsFromNodemon(ctx)

	if err != nil {
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"ContainerResourceCollector",
				"Failed to get pod metrics",
				err,
				map[string]string{
					"namespaces":       fmt.Sprintf("%v", c.namespaces),
					"error_type":       "metrics_query_failed",
					"source":           "nodemon",
					"zxporter_version": version.Get().String(),
				},
			)
		}
		c.logger.Error(err, "Failed to get pod metrics", "source", "nodemon")
		return
	}

	if c.telemetryLogger != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"ContainerResourceCollector",
			"Successfully fetched pod metrics",
			nil,
			map[string]string{
				"pod_count":        fmt.Sprintf("%d", len(podMetricsList.Items)),
				"namespaces":       fmt.Sprintf("%v", c.namespaces),
				"source":           "nodemon",
				"event_type":       "metrics_query_success",
				"zxporter_version": version.Get().String(),
			},
		)
	}

	// Pre-fetch container metrics from nodemon for network/IO/throttle (one call per cycle)
	c.nodemonContainerMetricsCache = nil
	if c.nodemonClient != nil {
		allContainerMetrics, err := c.nodemonClient.FetchAllContainerMetrics(ctx)
		if err != nil {
			c.logger.Error(err, "Failed to fetch container metrics from nodemon")
		} else {
			c.nodemonContainerMetricsCache = indexContainerMetricsByPod(allContainerMetrics)
		}
	}

	// Process each pod's metrics
	for _, podMetrics := range podMetricsList.Items {
		// Skip excluded pods
		if c.isExcluded(podMetrics.Namespace, podMetrics.Name) {
			continue
		}

		// Get the pod object from the cache
		pod, err := c.getPodFromCache(podMetrics.Namespace, podMetrics.Name)
		if err != nil {
			c.logger.Error(err, "Failed to get pod from cache",
				"namespace", podMetrics.Namespace,
				"name", podMetrics.Name)
			continue
		}

		// Fetch network metrics from nodemon
		var networkMetrics map[string]float64
		if c.nodemonContainerMetricsCache != nil {
			networkMetrics = c.collectPodNetworkMetrics(ctx, pod)
		}

		// Process each container's metrics
		for _, containerMetrics := range podMetrics.Containers {

			var ioMetrics map[string]float64
			var gpuMetrics map[string]interface{}
			var throttleFraction float64

			// Collect CPU throttle metrics from nodemon
			if c.nodemonContainerMetricsCache != nil {
				throttleFraction = c.collectContainerCPUThrottleMetrics(ctx, pod, containerMetrics.Name)
			}

			// Emit CPU throttle event if fraction exceeds threshold
			if throttleFraction > 0.1 {
				c.emitCPUThrottleEvent(pod, containerMetrics, throttleFraction)
			}

			// Collect I/O metrics for this container from nodemon
			if c.nodemonContainerMetricsCache != nil {
				ioMetrics = c.collectContainerIOMetrics(ctx, pod, containerMetrics.Name)
			}

			// GPU metrics are embedded in the unified container metrics when available.
			// No separate GPU collection needed — data flows through nodemonContainerMetricsCache.
			gpuMetrics = make(map[string]interface{})

			// Process the container metrics with optional network/IO data
			c.processContainerMetrics(
				pod,
				containerMetrics,
				networkMetrics,
				ioMetrics,
				gpuMetrics,
				throttleFraction,
			)
		}
	}
}

// processContainerMetrics processes metrics for a single container
func (c *ContainerResourceCollector) processContainerMetrics(
	pod *corev1.Pod,
	containerMetrics metricsv1beta1.ContainerMetrics,
	networkMetrics map[string]float64,
	ioMetrics map[string]float64,
	gpuMetrics map[string]interface{},
	throttleFraction float64,
) {
	// Find the container spec in the pod
	var containerSpec *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == containerMetrics.Name {
			containerSpec = &pod.Spec.Containers[i]
			break
		}
	}

	if containerSpec == nil {
		c.logger.Error(nil, "Container spec not found",
			"namespace", pod.Namespace,
			"pod", pod.Name,
			"container", containerMetrics.Name)
		return
	}

	// Get container status
	var containerStatus *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == containerMetrics.Name {
			containerStatus = &pod.Status.ContainerStatuses[i]
			break
		}
	}

	// Extract CPU usage in millicores
	cpuQuantity := containerMetrics.Usage.Cpu()
	cpuUsage := cpuQuantity.MilliValue()

	// Extract memory usage in bytes
	memoryQuantity := containerMetrics.Usage.Memory()
	memoryUsage := memoryQuantity.Value()

	// Get resource requests and limits from the container spec
	cpuRequestMillis := int64(0)
	cpuLimitMillis := int64(0)
	memoryRequestBytes := int64(0)
	memoryLimitBytes := int64(0)

	if containerSpec.Resources.Requests != nil {
		if cpu := containerSpec.Resources.Requests.Cpu(); cpu != nil {
			cpuRequestMillis = cpu.MilliValue()
		}
		if memory := containerSpec.Resources.Requests.Memory(); memory != nil {
			memoryRequestBytes = memory.Value()
		}
	}

	if containerSpec.Resources.Limits != nil {
		if cpu := containerSpec.Resources.Limits.Cpu(); cpu != nil {
			cpuLimitMillis = cpu.MilliValue()
		}
		if memory := containerSpec.Resources.Limits.Memory(); memory != nil {
			memoryLimitBytes = memory.Value()
		}
	}

	// Extract restart count and last termination reason
	restartCount := int32(0)
	lastTerminationReason := ""
	if containerStatus != nil {
		restartCount = containerStatus.RestartCount
		if containerStatus.LastTerminationState.Terminated != nil {
			lastTerminationReason = containerStatus.LastTerminationState.Terminated.Reason
			// Detect OOM during container init: Kubernetes reports as "StartError"
			// with message containing "OOM-killed" when memory limit is too low
			if lastTerminationReason == ReasonStartError {
				msg := containerStatus.LastTerminationState.Terminated.Message
				if strings.Contains(strings.ToLower(msg), "oom") {
					lastTerminationReason = ReasonOOMKilled
				}
			}
		}
	}

	// Create resource data with both metrics and pod info
	containerKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, containerMetrics.Name)

	metricsSnapshot := &ContainerMetricsSnapshot{
		// Container identification
		ContainerName: containerMetrics.Name,
		PodName:       pod.Name,
		Namespace:     pod.Namespace,
		NodeName:      pod.Spec.NodeName,

		// CPU/Memory resource usage
		CpuUsageMillis:   cpuUsage,
		MemoryUsageBytes: memoryUsage,

		// Resource requests and limits
		CpuRequestMillis:   cpuRequestMillis,
		CpuLimitMillis:     cpuLimitMillis,
		MemoryRequestBytes: memoryRequestBytes,
		MemoryLimitBytes:   memoryLimitBytes,

		// Labels from the pod for correlation
		PodLabels: pod.Labels,

		// Container metadata for reference
		ContainerImage: containerSpec.Image,

		// Status info
		ContainerRunning:      containerStatus != nil && containerStatus.State.Running != nil,
		ContainerRestarts:     containerStatus != nil && containerStatus.RestartCount != 0,
		RestartCount:          int64(restartCount),
		LastTerminationReason: lastTerminationReason,
	}

	// Resolve Top-Level Workload
	wKind, wName := c.resolveWorkload(pod)
	if wKind != "" && wName != "" {
		metricsSnapshot.WorkloadKind = wKind
		metricsSnapshot.WorkloadName = wName
	}

	// Add network metrics if available
	if len(networkMetrics) > 0 {
		metricsSnapshot.NetworkMetricsArePodLevel = true
		metricsSnapshot.PodContainerCount = len(pod.Spec.Containers)
		metricsSnapshot.NetworkReceiveBytes = networkMetrics["NetworkReceiveBytes"]
		metricsSnapshot.NetworkTransmitBytes = networkMetrics["NetworkTransmitBytes"]
		metricsSnapshot.NetworkReceivePackets = networkMetrics["NetworkReceivePackets"]
		metricsSnapshot.NetworkTransmitPackets = networkMetrics["NetworkTransmitPackets"]
		metricsSnapshot.NetworkReceiveErrors = networkMetrics["NetworkReceiveErrors"]
		metricsSnapshot.NetworkTransmitErrors = networkMetrics["NetworkTransmitErrors"]
		metricsSnapshot.NetworkReceiveDropped = networkMetrics["NetworkReceiveDropped"]
		metricsSnapshot.NetworkTransmitDropped = networkMetrics["NetworkTransmitDropped"]
	}

	// Add I/O metrics if available
	if len(ioMetrics) > 0 {
		metricsSnapshot.FsReadBytes = ioMetrics["FSReadBytes"]
		metricsSnapshot.FsWriteBytes = ioMetrics["FSWriteBytes"]
		metricsSnapshot.FsReads = ioMetrics["FSReads"]
		metricsSnapshot.FsWrites = ioMetrics["FSWrites"]
	}

	// Add CPU throttle fraction if available
	metricsSnapshot.CpuThrottledFraction = throttleFraction

	if len(gpuMetrics) > 0 {
		metricsSnapshot.GpuUsage = gpuMetrics["GPUUsage"]
		metricsSnapshot.GpuMetricsCount = gpuMetrics["GPUMetricsCount"]
		metricsSnapshot.GpuUtilizationPercentage = gpuMetrics["GPUUtilizationPercentage"]
		metricsSnapshot.GpuMemoryUsedMb = gpuMetrics["GPUMemoryUsedMb"]
		metricsSnapshot.GpuMemoryFreeMb = gpuMetrics["GPUMemoryFreeMb"]
		metricsSnapshot.GpuPowerUsageWatts = gpuMetrics["GPUPowerUsageWatts"]
		metricsSnapshot.GpuTemperatureCelsius = gpuMetrics["GPUTemperatureCelsius"]
		metricsSnapshot.GpuSMClockMHz = gpuMetrics["GPUSMClockMHz"]
		metricsSnapshot.GpuMemClockMHz = gpuMetrics["GPUMemClockMHz"]
		metricsSnapshot.GpuModels = gpuMetrics["GPUModels"]
		metricsSnapshot.GpuUUIDs = gpuMetrics["GPUUUIDs"]
		metricsSnapshot.GpuRequestCount = gpuMetrics["GPURequestCount"]
		metricsSnapshot.GpuLimitCount = gpuMetrics["GPULimitCount"]
		metricsSnapshot.GpuTotalMemoryMb = gpuMetrics["GPUTotalMemoryMb"]
		if individualGPUs, ok := gpuMetrics["IndividualGPUs"]; ok {
			individualJSON, err := json.Marshal(individualGPUs)
			if err != nil {
				c.logger.Error(err, "Failed to marshal individual GPU metrics",
					"error", err)
			} else {
				metricsSnapshot.IndividualGPUMetrics = string(individualJSON)
			}
		}
	}

	// Send the resource usage data to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ContainerResource,
		Object:       metricsSnapshot,
		Timestamp:    time.Now(),
		EventType:    EventTypeMetrics,
		Key:          containerKey,
	}
}


// buildPodMetricsFromNodemon fetches container metrics from nodemon and converts them
// into a PodMetricsList compatible with the metrics-server format. This allows the rest
// of collectAllContainerResources to work unchanged — CPU/memory come from nodemon's
// stats/summary data (usageNanoCores, workingSetBytes) instead of the metrics-server API.
func (c *ContainerResourceCollector) buildPodMetricsFromNodemon(ctx context.Context) (*metricsv1beta1.PodMetricsList, error) {
	allMetrics, err := c.nodemonClient.FetchAllContainerMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch container metrics from nodemon: %w", err)
	}

	// Group by pod
	podMap := make(map[string]*metricsv1beta1.PodMetrics)
	for _, m := range allMetrics {
		key := m.Namespace + "/" + m.Pod
		pm, exists := podMap[key]
		if !exists {
			pm = &metricsv1beta1.PodMetrics{
				ObjectMeta: metav1.ObjectMeta{
					Name:      m.Pod,
					Namespace: m.Namespace,
				},
			}
			podMap[key] = pm
		}

		// Convert nanocores to resource.Quantity (millicores)
		cpuMillis := int64(m.CPUUsageNanoCores / 1_000_000)
		cpuQuantity := *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI)

		// Memory in bytes
		memQuantity := *resource.NewQuantity(int64(m.MemoryWorkingSet), resource.BinarySI)

		pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{
			Name: m.Container,
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    cpuQuantity,
				corev1.ResourceMemory: memQuantity,
			},
		})
	}

	// Build the list
	result := &metricsv1beta1.PodMetricsList{}
	for _, pm := range podMap {
		result.Items = append(result.Items, *pm)
	}

	c.logger.V(1).Info("Built pod metrics from nodemon", "pods", len(result.Items), "containers", len(allMetrics))
	return result, nil
}

// indexContainerMetricsByPod builds a lookup map keyed by "namespace/podName" from a
// flat slice of UnifiedContainerMetric returned by nodemon.
func indexContainerMetricsByPod(metrics []UnifiedContainerMetric) map[string][]UnifiedContainerMetric {
	idx := make(map[string][]UnifiedContainerMetric, len(metrics))
	for _, m := range metrics {
		key := m.Namespace + "/" + m.Pod
		idx[key] = append(idx[key], m)
	}
	return idx
}

// collectPodNetworkMetrics returns pod-level network metrics from the
// pre-fetched nodemon container metrics cache.
func (c *ContainerResourceCollector) collectPodNetworkMetrics(
	_ context.Context,
	pod *corev1.Pod,
) map[string]float64 {
	metrics := map[string]float64{
		"NetworkReceiveBytes":    0,
		"NetworkTransmitBytes":   0,
		"NetworkReceivePackets":  0,
		"NetworkTransmitPackets": 0,
		"NetworkReceiveErrors":   0,
		"NetworkTransmitErrors":  0,
		"NetworkReceiveDropped":  0,
		"NetworkTransmitDropped": 0,
	}

	key := pod.Namespace + "/" + pod.Name
	containerMetrics, ok := c.nodemonContainerMetricsCache[key]
	if !ok || len(containerMetrics) == 0 {
		return metrics
	}

	// Network metrics are pod-level (shared network namespace). Pick the first
	// container's values since nodemon reports identical pod-level counters for each.
	//
	// NetworkRxBytes/TxBytes from stats/summary are CUMULATIVE totals.
	// The MPA proto expects bytes/sec (NetworkReceiveBytesPerSec), so we compute
	// per-second rates from successive samples using RateCalculator.
	// Packet/error/drop rates are already per-second from the cAdvisor scraper.
	m := containerMetrics[0]
	now := time.Now()
	podKey := pod.Namespace + "/" + pod.Name
	metrics["NetworkReceiveBytes"] = c.networkByteRates.Rate(podKey, "rx_bytes", float64(m.NetworkRxBytes), now)
	metrics["NetworkTransmitBytes"] = c.networkByteRates.Rate(podKey, "tx_bytes", float64(m.NetworkTxBytes), now)
	metrics["NetworkReceivePackets"] = m.NetworkRxPacketsPerSec
	metrics["NetworkTransmitPackets"] = m.NetworkTxPacketsPerSec
	metrics["NetworkReceiveErrors"] = m.NetworkRxErrorsPerSec
	metrics["NetworkTransmitErrors"] = m.NetworkTxErrorsPerSec
	metrics["NetworkReceiveDropped"] = m.NetworkRxDropsPerSec
	metrics["NetworkTransmitDropped"] = m.NetworkTxDropsPerSec

	return metrics
}

// collectContainerIOMetrics returns container-level disk I/O metrics from
// the pre-fetched nodemon cache.
func (c *ContainerResourceCollector) collectContainerIOMetrics(
	_ context.Context,
	pod *corev1.Pod,
	containerName string,
) map[string]float64 {
	metrics := map[string]float64{
		"FSReadBytes":  0,
		"FSWriteBytes": 0,
		"FSReads":      0,
		"FSWrites":     0,
	}

	key := pod.Namespace + "/" + pod.Name
	containerMetrics, ok := c.nodemonContainerMetricsCache[key]
	if !ok {
		return metrics
	}

	for _, m := range containerMetrics {
		if m.Container == containerName {
			metrics["FSReadBytes"] = m.DiskReadBytesPerSec
			metrics["FSWriteBytes"] = m.DiskWriteBytesPerSec
			metrics["FSReads"] = m.DiskReadOpsPerSec
			metrics["FSWrites"] = m.DiskWriteOpsPerSec
			return metrics
		}
	}

	return metrics
}

// collectContainerCPUThrottleMetrics returns the CPU throttle fraction from
// the pre-fetched nodemon cache for a specific container.
func (c *ContainerResourceCollector) collectContainerCPUThrottleMetrics(
	_ context.Context,
	pod *corev1.Pod,
	containerName string,
) float64 {
	key := pod.Namespace + "/" + pod.Name
	containerMetrics, ok := c.nodemonContainerMetricsCache[key]
	if !ok {
		return 0
	}

	for _, m := range containerMetrics {
		if m.Container == containerName {
			fraction := m.CPUThrottleFraction
			if fraction < 0 || math.IsNaN(fraction) {
				return 0
			}
			if fraction > 1 {
				return 1
			}
			return fraction
		}
	}

	return 0
}

// emitCPUThrottleEvent sends a CPU throttle event through the batch channel with 5-minute deduplication.
func (c *ContainerResourceCollector) emitCPUThrottleEvent(
	pod *corev1.Pod,
	containerMetrics metricsv1beta1.ContainerMetrics,
	throttleFraction float64,
) {
	dedupKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, containerMetrics.Name)

	c.throttle.mu.Lock()
	if lastEmit, ok := c.throttle.lastEmitted[dedupKey]; ok && time.Since(lastEmit) < 5*time.Minute {
		c.throttle.mu.Unlock()
		return
	}
	c.throttle.lastEmitted[dedupKey] = time.Now()
	c.throttle.mu.Unlock()

	// Resolve workload info
	workloadKind, workloadName := c.resolveWorkload(pod)
	if workloadKind == "" {
		workloadKind = "Pod"
		workloadName = pod.Name
	}

	// Get CPU resources from container spec
	var cpuUsageMillis, cpuRequestMillis, cpuLimitMillis int64
	cpuUsageMillis = containerMetrics.Usage.Cpu().MilliValue()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == containerMetrics.Name {
			if pod.Spec.Containers[i].Resources.Requests != nil {
				if cpu := pod.Spec.Containers[i].Resources.Requests.Cpu(); cpu != nil {
					cpuRequestMillis = cpu.MilliValue()
				}
			}
			if pod.Spec.Containers[i].Resources.Limits != nil {
				if cpu := pod.Spec.Containers[i].Resources.Limits.Cpu(); cpu != nil {
					cpuLimitMillis = cpu.MilliValue()
				}
			}
			break
		}
	}

	// Round timestamp to nearest minute for DB dedup
	now := time.Now()
	roundedTS := now.Truncate(time.Minute)

	c.batchChan <- CollectedResource{
		ResourceType: ContainerCPUThrottleEvent,
		Object: map[string]interface{}{
			"namespace":              pod.Namespace,
			"workload_name":          workloadName,
			"workload_kind":          workloadKind,
			"pod_name":               pod.Name,
			"container_name":         containerMetrics.Name,
			"cpu_usage_millicores":   cpuUsageMillis,
			"cpu_request_millicores": cpuRequestMillis,
			"cpu_limit_millicores":   cpuLimitMillis,
			"throttled_fraction":     throttleFraction,
			"timestamp":              roundedTS.Format(time.RFC3339Nano),
		},
		Timestamp: now,
		EventType: EventTypeAdd,
		Key:       fmt.Sprintf("cpu-throttle/%s/%s/%s", pod.Namespace, pod.Name, containerMetrics.Name),
	}
}


// getPodFromCache retrieves a pod from the informer cache
func (c *ContainerResourceCollector) getPodFromCache(namespace, name string) (*corev1.Pod, error) {
	return c.informerFactory.Core().V1().Pods().Lister().Pods(namespace).Get(name)
}

// isExcluded checks if a pod should be excluded from collection
func (c *ContainerResourceCollector) isExcluded(namespace, name string) bool {
	// Check if monitoring specific namespaces and this pod isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if pod is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	return c.excludedPods[key]
}

// Stop gracefully shuts down the container resource collector
func (c *ContainerResourceCollector) Stop() error {
	c.logger.Info("Stopping container resource collector")

	// 1. Stop the ticker
	if c.ticker != nil {
		c.ticker.Stop()
		c.logger.Info("Stopped container resource collector ticker")
	}

	// 2. Signal the informer factory and collection loop to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Container resource collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed container resource collector stop channel")
	}

	// 3. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed container resource collector batch input channel")
	}

	// 4. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Container resource collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ContainerResourceCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ContainerResourceCollector) GetType() string {
	return "container_resource"
}

// IsAvailable checks whether the collector can operate.
// Always returns true — nodemon pods are discovered dynamically and may not be
// ready at the instant IsAvailable is called during startup. The collection loop
// gracefully handles empty nodemon responses.
func (c *ContainerResourceCollector) IsAvailable(ctx context.Context) bool {
	return c.nodemonClient != nil
}

// AddResource is a no-op for container resource collector - we never sync individual containers
func (c *ContainerResourceCollector) AddResource(resource interface{}) error {
	// Container resources are collected automatically via metrics scraping, not via individual resource refresh
	return nil
}

func (c *ContainerResourceCollector) resolveWorkload(pod *corev1.Pod) (string, string) {
	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		return "", ""
	}

	// Supported Top-Level Kinds
	preferredKinds := map[string]bool{
		"Rollout":     true, // Argo Rollouts
		"Deployment":  true,
		"StatefulSet": true,
		"DaemonSet":   true,
		"CronJob":     true,
	}

	// 1. Direct Owner Match (e.g. StatefulSet, DaemonSet, Job, CronJob)
	if preferredKinds[controllerRef.Kind] {
		return controllerRef.Kind, controllerRef.Name
	}

	// 2. ReplicaSet Owner (Deployment or Rollout)
	if controllerRef.Kind == "ReplicaSet" {
		rs, err := c.rsLister.ReplicaSets(pod.Namespace).Get(controllerRef.Name)
		if err == nil {
			rsController := metav1.GetControllerOf(rs)
			if rsController != nil && preferredKinds[rsController.Kind] {
				return rsController.Kind, rsController.Name
			}
		}
		// Fallback to ReplicaSet if standalone
		return "ReplicaSet", controllerRef.Name
	}

	return "", ""
}
