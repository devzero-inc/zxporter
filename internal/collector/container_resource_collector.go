// internal/collector/container_resource_collector.go
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	gpuconst "github.com/NVIDIA/KAI-scheduler/pkg/common/constants"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// ContainerResourceCollectorConfig holds configuration for the resource collector
type ContainerResourceCollectorConfig struct {
	// UpdateInterval specifies how often to collect metrics
	UpdateInterval time.Duration

	// PrometheusURL specifies the URL of the Prometheus instance to query
	// If empty, defaults to in-cluster Prometheus at http://prometheus.monitoring:9090
	PrometheusURL string

	// QueryTimeout specifies the timeout for Prometheus queries
	QueryTimeout time.Duration

	// DisableNetworkIOMetrics determines whether to disable network and I/O metrics collection
	// Default is false, so metrics are collected by default
	DisableNetworkIOMetrics bool

	// DisableGPUMetrics determines whether to disable GPU metrics collection
	// Default is false, so metrics are collected by default
	DisableGPUMetrics bool
}

type gpuQueryState struct {
	lastFailed bool
}

// ContainerResourceCollector collects container resource usage metrics
type ContainerResourceCollector struct {
	k8sClient          kubernetes.Interface
	metricsClient      *metricsv1.Clientset
	prometheusAPI      v1.API
	informerFactory    informers.SharedInformerFactory
	podInformer        cache.SharedIndexInformer
	batchChan          chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan       chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher            *ResourcesBatcher
	stopCh             chan struct{}
	ticker             *time.Ticker
	config             ContainerResourceCollectorConfig
	namespaces         []string
	excludedPods       map[types.NamespacedName]bool
	logger             logr.Logger
	metrics            *TelemetryMetrics
	telemetryLogger    telemetry_logger.Logger
	mu                 sync.RWMutex
	gpuQueryErrorState map[string]*gpuQueryState // most of the case we are not deploying zxporter to GPU nodes which cause GPU query to fail infinitely, and we dont want to get that GPU query fails error every minute for every container
	gpuQueryMu         sync.Mutex

	// Metrics server availability tracking
	metricsServerAvailable          bool
	lastMetricsServerCheck          time.Time
	metricsServerCheckInterval      time.Duration
	metricsServerUnavailableLogged  bool
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

	// Default Prometheus URL if not specified
	if config.PrometheusURL == "" {
		config.PrometheusURL = "http://prometheus-service.monitoring.svc.cluster.local:8080"
	}

	// Default query timeout if not specified
	if config.QueryTimeout <= 0 {
		config.QueryTimeout = 10 * time.Second
	}

	// Create channels
	batchChan := make(chan CollectedResource, 500)      // Keep original buffer size for individual items
	resourceChan := make(chan []CollectedResource, 200) // Buffer for batches

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &ContainerResourceCollector{
		k8sClient:          k8sClient,
		metricsClient:      metricsClient,
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
		gpuQueryErrorState: make(map[string]*gpuQueryState),
		metricsServerCheckInterval: 60 * time.Second, // Check every minute
		metricsServerAvailable:     false,
	}
}

// Start begins the container resource collection process
func (c *ContainerResourceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting container resource collector",
		"namespaces", c.namespaces,
		"updateInterval", c.config.UpdateInterval,
		"disableNetworkIOMetrics", c.config.DisableNetworkIOMetrics,
		"disableGPUMetrics", c.config.DisableGPUMetrics)

	// Check if metrics client is available
	if c.metricsClient == nil {
		return fmt.Errorf("metrics client is not available, cannot collect container resources")
	}

	// Initialize Prometheus client if network/IO metrics are not disabled
	if !c.config.DisableNetworkIOMetrics && !c.config.DisableGPUMetrics {
		c.logger.Info("Initializing Prometheus client for network/IO or GPU metrics",
			"prometheusURL", c.config.PrometheusURL)

		// Create a custom HTTP client with metrics
		httpClient := NewPrometheusClient(c.metrics)

		client, err := api.NewClient(api.Config{
			Address: c.config.PrometheusURL,
			Client:  httpClient,
		})
		if err != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"ContainerResourceCollector",
				"Failed to create Prometheus client",
				err,
				map[string]string{
					"prometheus_url":   c.config.PrometheusURL,
					"zxporter_version": version.Get().String(),
				},
			)

			c.logger.Error(err, "Failed to create Prometheus client, network/IO and GPU metrics will be disabled")
		} else {
			c.prometheusAPI = v1.NewAPI(client)
		}
	} else {
		c.logger.Info("Network, I/O and GPU metrics collection is disabled")
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

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced) {
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
	// Fetch pod metrics from the metrics server
	var podMetricsList *metricsv1beta1.PodMetricsList
	var err error

	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Fetch metrics for a specific namespace
		podMetricsList, err = c.metricsClient.MetricsV1beta1().PodMetricses(c.namespaces[0]).List(ctx, metav1.ListOptions{})
	} else {
		// Fetch metrics for all namespaces
		podMetricsList, err = c.metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
	}

	if err != nil {
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"ContainerResourceCollector",
				"Failed to get pod metrics from metrics server",
				err,
				map[string]string{
					"namespaces":       fmt.Sprintf("%v", c.namespaces),
					"error_type":       "metrics_server_query_failed",
					"zxporter_version": version.Get().String(),
				},
			)
		}
		c.logger.Error(err, "Failed to get pod metrics from metrics server")
		return
	}

	if c.telemetryLogger != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"ContainerResourceCollector",
			"Successfully fetched pod metrics from metrics server",
			nil,
			map[string]string{
				"pod_count":        fmt.Sprintf("%d", len(podMetricsList.Items)),
				"namespaces":       fmt.Sprintf("%v", c.namespaces),
				"event_type":       "metrics_server_query_success",
				"zxporter_version": version.Get().String(),
			},
		)
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

		// Create a context with timeout for Prometheus queries if needed
		var queryCtx context.Context
		var cancel context.CancelFunc
		if c.prometheusAPI != nil {
			queryCtx, cancel = context.WithTimeout(ctx, c.config.QueryTimeout)
			defer cancel()
		}

		// Fetch network metrics
		var networkMetrics map[string]float64
		if !c.config.DisableNetworkIOMetrics && c.prometheusAPI != nil && queryCtx != nil {
			networkMetrics, err = c.collectPodNetworkMetrics(queryCtx, pod)
			if err != nil {
				if c.telemetryLogger != nil {
					c.telemetryLogger.Report(
						gen.LogLevel_LOG_LEVEL_WARN,
						"ContainerResourceCollector",
						"Failed to collect network metrics from Prometheus",
						err,
						map[string]string{
							"namespace":        podMetrics.Namespace,
							"pod":              podMetrics.Name,
							"error_type":       "prometheus_network_query_failed",
							"prometheus_url":   c.config.PrometheusURL,
							"zxporter_version": version.Get().String(),
						},
					)
				}
				c.logger.Error(err, "Failed to collect network metrics",
					"namespace", podMetrics.Namespace,
					"name", podMetrics.Name)
				// Continue with CPU/memory metrics
				networkMetrics = make(map[string]float64)
			}
		}

		// Process each container's metrics
		for _, containerMetrics := range podMetrics.Containers {

			var ioMetrics map[string]float64
			var gpuMetrics map[string]interface{}
			if c.prometheusAPI != nil && queryCtx != nil {

				// Fetch I/O metrics for this container
				if !c.config.DisableNetworkIOMetrics {
					ioMetrics, err = c.collectContainerIOMetrics(queryCtx, pod, containerMetrics.Name)
					if err != nil {
						if c.telemetryLogger != nil {
							c.telemetryLogger.Report(
								gen.LogLevel_LOG_LEVEL_WARN,
								"ContainerResourceCollector",
								"Failed to collect I/O metrics from Prometheus",
								err,
								map[string]string{
									"namespace":        podMetrics.Namespace,
									"pod":              podMetrics.Name,
									"container":        containerMetrics.Name,
									"error_type":       "prometheus_io_query_failed",
									"prometheus_url":   c.config.PrometheusURL,
									"zxporter_version": version.Get().String(),
								},
							)
						}
						c.logger.Error(err, "Failed to collect I/O metrics",
							"namespace", podMetrics.Namespace,
							"pod", podMetrics.Name,
							"container", containerMetrics.Name)
						// Continue with CPU/memory metrics
						ioMetrics = make(map[string]float64)
					}
				}

				// Add GPU metrics collection if enabled
				if !c.config.DisableGPUMetrics {
					gpuMetrics, err = c.collectContainerGPUMetrics(queryCtx, pod, containerMetrics.Name)
					if err != nil {
						c.logger.Error(err, "Failed to collect container GPU metrics. If you are not using GPU, this is expected. To disable GPU metrics, set DISABLE_GPU_METRICS environment variable to true",
							"namespace", podMetrics.Namespace,
							"pod", podMetrics.Name,
							"container", containerMetrics.Name)
						// Continue with other metrics
						gpuMetrics = make(map[string]interface{})
					}
				}
			}
			// Process the container metrics with optional network/IO data
			c.processContainerMetrics(pod, containerMetrics, networkMetrics, ioMetrics, gpuMetrics)
		}
	}
}

func sanitize(podCloned *corev1.Pod) {
	for _, cont := range podCloned.Spec.Containers {
		cont.Env = []corev1.EnvVar{}
		cont.EnvFrom = []corev1.EnvFromSource{}
	}
	for _, cont := range podCloned.Spec.EphemeralContainers {
		cont.Env = []corev1.EnvVar{}
		cont.EnvFrom = []corev1.EnvFromSource{}
	}
	for _, cont := range podCloned.Spec.InitContainers {
		cont.Env = []corev1.EnvVar{}
		cont.EnvFrom = []corev1.EnvFromSource{}
	}
}

// processContainerMetrics processes metrics for a single container
func (c *ContainerResourceCollector) processContainerMetrics(
	pod *corev1.Pod,
	containerMetrics metricsv1beta1.ContainerMetrics,
	networkMetrics map[string]float64,
	ioMetrics map[string]float64,
	gpuMetrics map[string]interface{},
) {
	podCloned := pod.DeepCopy()

	// clean out potentially sensitive info
	sanitize(podCloned)

	// Find the container spec in the pod
	var containerSpec *corev1.Container
	for i := range podCloned.Spec.Containers {
		if podCloned.Spec.Containers[i].Name == containerMetrics.Name {
			containerSpec = &podCloned.Spec.Containers[i]
			break
		}
	}

	if containerSpec == nil {
		c.logger.Error(nil, "Container spec not found",
			"namespace", podCloned.Namespace,
			"pod", podCloned.Name,
			"container", containerMetrics.Name)
		return
	}

	// Get container status
	var containerStatus *corev1.ContainerStatus
	for i := range podCloned.Status.ContainerStatuses {
		if podCloned.Status.ContainerStatuses[i].Name == containerMetrics.Name {
			containerStatus = &podCloned.Status.ContainerStatuses[i]
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

	// Create resource data with both metrics and pod info
	containerKey := fmt.Sprintf("%s/%s/%s", podCloned.Namespace, podCloned.Name, containerMetrics.Name)
	resourceData := map[string]interface{}{
		// Container identification
		"containerName": containerMetrics.Name,
		"podName":       podCloned.Name,
		"namespace":     podCloned.Namespace,
		"nodeName":      podCloned.Spec.NodeName,

		// CPU/Memory resource usage
		"cpuUsageMillis":   cpuUsage,
		"memoryUsageBytes": memoryUsage,

		// Resource requests and limits
		"cpuRequestMillis":   cpuRequestMillis,
		"cpuLimitMillis":     cpuLimitMillis,
		"memoryRequestBytes": memoryRequestBytes,
		"memoryLimitBytes":   memoryLimitBytes,

		// Labels from the pod for correlation
		"podLabels": podCloned.Labels,

		// Container metadata for reference
		"containerImage": containerSpec.Image,

		// Status info
		"containerRunning":  containerStatus != nil && containerStatus.State.Running != nil,
		"containerRestarts": containerStatus != nil && containerStatus.RestartCount != 0,

		// Include the full pod object for any other needed details
		"pod": podCloned,
	}

	// Add network metrics if available
	if len(networkMetrics) > 0 {
		resourceData["networkReceiveBytes"] = networkMetrics["NetworkReceiveBytes"]
		resourceData["networkTransmitBytes"] = networkMetrics["NetworkTransmitBytes"]
		resourceData["networkReceivePackets"] = networkMetrics["NetworkReceivePackets"]
		resourceData["networkTransmitPackets"] = networkMetrics["NetworkTransmitPackets"]
		resourceData["networkReceiveErrors"] = networkMetrics["NetworkReceiveErrors"]
		resourceData["networkTransmitErrors"] = networkMetrics["NetworkTransmitErrors"]
		resourceData["networkReceiveDropped"] = networkMetrics["NetworkReceiveDropped"]
		resourceData["networkTransmitDropped"] = networkMetrics["NetworkTransmitDropped"]
	}

	// Add I/O metrics if available
	if len(ioMetrics) > 0 {
		resourceData["fsReadBytes"] = ioMetrics["FSReadBytes"]
		resourceData["fsWriteBytes"] = ioMetrics["FSWriteBytes"]
		resourceData["fsReads"] = ioMetrics["FSReads"]
		resourceData["fsWrites"] = ioMetrics["FSWrites"]
	}

	if len(gpuMetrics) > 0 {
		resourceData["gpuUsage"] = gpuMetrics["GPUUsage"]
		resourceData["gpuMetricsCount"] = gpuMetrics["GPUMetricsCount"]
		resourceData["gpuUtilizationPercentage"] = gpuMetrics["GPUUtilizationPercentage"]
		resourceData["gpuMemoryUsedMb"] = gpuMetrics["GPUMemoryUsedMb"]
		resourceData["gpuMemoryFreeMb"] = gpuMetrics["GPUMemoryFreeMb"]
		resourceData["gpuPowerUsageWatts"] = gpuMetrics["GPUPowerUsageWatts"]
		resourceData["gpuTemperatureCelsius"] = gpuMetrics["GPUTemperatureCelsius"]
		resourceData["gpuSMClockMHz"] = gpuMetrics["GPUSMClockMHz"]
		resourceData["gpuMemClockMHz"] = gpuMetrics["GPUMemClockMHz"]
		resourceData["gpuModels"] = gpuMetrics["GPUModels"]
		resourceData["gpuUUIDs"] = gpuMetrics["GPUUUIDs"]
		resourceData["gpuRequestCount"] = gpuMetrics["GPURequestCount"]
		resourceData["gpuLimitCount"] = gpuMetrics["GPULimitCount"]
		resourceData["gpuTotalMemoryMb"] = gpuMetrics["GPUTotalMemoryMb"]
		if individualGPUs, ok := gpuMetrics["IndividualGPUs"]; ok {
			individualJSON, err := json.Marshal(individualGPUs)
			if err != nil {
				c.logger.Error(err, "Failed to marshal individual GPU metrics",
					"error", err)
			} else {
				resourceData["individualGPUMetrics"] = string(individualJSON)
			}
		}
	}

	// Send the resource usage data to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ContainerResource,
		Object:       resourceData,
		Timestamp:    time.Now(),
		EventType:    EventTypeMetrics,
		Key:          containerKey,
	}
}

// collectPodNetworkMetrics collects network metrics for a pod using Prometheus queries
func (c *ContainerResourceCollector) collectPodNetworkMetrics(ctx context.Context, pod *corev1.Pod) (map[string]float64, error) {
	metrics := make(map[string]float64)

	// Define queries for network metrics
	queries := map[string]string{
		"NetworkReceiveBytes":    fmt.Sprintf(`sum(rate(container_network_receive_bytes_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
		"NetworkTransmitBytes":   fmt.Sprintf(`sum(rate(container_network_transmit_bytes_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
		"NetworkReceivePackets":  fmt.Sprintf(`sum(rate(container_network_receive_packets_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
		"NetworkTransmitPackets": fmt.Sprintf(`sum(rate(container_network_transmit_packets_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
		"NetworkReceiveErrors":   fmt.Sprintf(`sum(rate(container_network_receive_errors_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
		"NetworkTransmitErrors":  fmt.Sprintf(`sum(rate(container_network_transmit_errors_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
		"NetworkReceiveDropped":  fmt.Sprintf(`sum(rate(container_network_receive_packets_dropped_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
		"NetworkTransmitDropped": fmt.Sprintf(`sum(rate(container_network_transmit_packets_dropped_total{namespace="%s", pod="%s"}[5m]))`, pod.Namespace, pod.Name),
	}

	// Execute each query and store the result
	for metricName, query := range queries {
		metrics[metricName] = 0 // Default to 0 for all metrics

		result, _, err := c.prometheusAPI.Query(ctx, query, time.Now())
		if err != nil {
			c.logger.Error(err, "Error querying Prometheus",
				"metric", metricName,
				"query", query)
			continue
		}

		// Extract value from result (if any)
		if result.Type() == model.ValVector {
			vector := result.(model.Vector)
			if len(vector) > 0 {
				metrics[metricName] = float64(vector[0].Value)
			}
		}
	}

	return metrics, nil
}

// collectContainerIOMetrics collects I/O metrics for a container using Prometheus queries
func (c *ContainerResourceCollector) collectContainerIOMetrics(ctx context.Context, pod *corev1.Pod, containerName string) (map[string]float64, error) {
	metrics := make(map[string]float64)

	// Define queries for I/O metrics
	queries := map[string]string{
		"FSReadBytes":  fmt.Sprintf(`sum(rate(container_fs_reads_bytes_total{namespace="%s", pod="%s", container="%s"}[5m]))`, pod.Namespace, pod.Name, containerName),
		"FSWriteBytes": fmt.Sprintf(`sum(rate(container_fs_writes_bytes_total{namespace="%s", pod="%s", container="%s"}[5m]))`, pod.Namespace, pod.Name, containerName),
		"FSReads":      fmt.Sprintf(`sum(rate(container_fs_reads_total{namespace="%s", pod="%s", container="%s"}[5m]))`, pod.Namespace, pod.Name, containerName),
		"FSWrites":     fmt.Sprintf(`sum(rate(container_fs_writes_total{namespace="%s", pod="%s", container="%s"}[5m]))`, pod.Namespace, pod.Name, containerName),
	}

	// Execute each query and store the result
	for metricName, query := range queries {
		metrics[metricName] = 0 // Default to 0 for all metrics

		result, _, err := c.prometheusAPI.Query(ctx, query, time.Now())
		if err != nil {
			c.logger.Error(err, "Error querying Prometheus",
				"metric", metricName,
				"query", query,
				"container", containerName)
			continue
		}

		// Extract value from result (if any)
		if result.Type() == model.ValVector {
			vector := result.(model.Vector)
			if len(vector) > 0 {
				metrics[metricName] = float64(vector[0].Value)
			}
		}
	}

	return metrics, nil
}

// collectContainerGPUMetrics collects GPU metrics for a container using Prometheus queries
func (c *ContainerResourceCollector) collectContainerGPUMetrics(ctx context.Context, pod *corev1.Pod, containerName string) (map[string]interface{}, error) {
	metrics := make(map[string]interface{})

	namespace := pod.Namespace
	podName := pod.Name
	baseLabels := fmt.Sprintf(`namespace="%s", pod="%s", container="%s"`, namespace, podName, containerName)
	queryTime := time.Now()

	stateKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, containerName)
	gpuCountQuery := fmt.Sprintf(`count(DCGM_FI_DEV_GPU_UTIL{%s})`, baseLabels)

	c.gpuQueryMu.Lock()
	state, exists := c.gpuQueryErrorState[stateKey]
	if !exists {
		state = &gpuQueryState{lastFailed: false}
		c.gpuQueryErrorState[stateKey] = state
	}
	c.gpuQueryMu.Unlock()

	result, _, err := c.prometheusAPI.Query(ctx, gpuCountQuery, queryTime)
	if err != nil {
		// BEGIN: latching
		c.gpuQueryMu.Lock()
		defer c.gpuQueryMu.Unlock()
		if !state.lastFailed {
			// Report/log the FIRST failure
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"ContainerResourceCollector",
				fmt.Sprintf("Failed querying GPU metric for %s", stateKey),
				err,
				map[string]string{
					"namespace":        pod.Namespace,
					"pod":              pod.Name,
					"container":        containerName,
					"error_type":       "prometheus_gpu_query_failed",
					"prometheus_url":   c.config.PrometheusURL,
					"query":            gpuCountQuery,
					"zxporter_version": version.Get().String(),
				},
			)
			state.lastFailed = true
		}
		return nil, fmt.Errorf("error querying GPU availability: %w", err)
	}

	// On success, if previously had error then only log a success transition
	c.gpuQueryMu.Lock()
	if state.lastFailed {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"ContainerResourceCollector",
			fmt.Sprintf("GPU metric query succeeded again for %s", stateKey),
			nil,
			map[string]string{
				"namespace":        pod.Namespace,
				"pod":              pod.Name,
				"container":        containerName,
				"event_type":       "prometheus_gpu_query_succeed",
				"prometheus_url":   c.config.PrometheusURL,
				"query":            gpuCountQuery,
				"zxporter_version": version.Get().String(),
			},
		)
		state.lastFailed = false
	}
	c.gpuQueryMu.Unlock()

	gpuCount := 0.0
	if result.Type() == model.ValVector {
		vector := result.(model.Vector)
		if len(vector) > 0 {
			gpuCount = float64(vector[0].Value)
		}
	}

	if gpuCount == 0 {
		return metrics, nil
	}

	metrics["GPUMetricsCount"] = gpuCount

	metricDefinitions := []struct {
		name        string // Human-readable name for logs
		promMetric  string // Prometheus metric name
		resultKey   string // Field name in individual GPUs
		aggregateOp string // Aggregation operation (sum, avg)
		metricKey   string // Field name in aggregate metrics
	}{
		{"GPU Utilization", "DCGM_FI_DEV_GPU_UTIL", "Utilization", "avg", "GPUUtilizationPercentage"},
		{"Memory Used", "DCGM_FI_DEV_FB_USED", "MemoryUsed", "sum", "GPUMemoryUsedMb"},
		{"Memory Free", "DCGM_FI_DEV_FB_FREE", "MemoryFree", "sum", "GPUMemoryFreeMb"},
		{"Power Usage", "DCGM_FI_DEV_POWER_USAGE", "PowerUsage", "sum", "GPUPowerUsageWatts"},
		{"Temperature", "DCGM_FI_DEV_GPU_TEMP", "Temperature", "avg", "GPUTemperatureCelsius"},
		{"SM Clock", "DCGM_FI_DEV_SM_CLOCK", "SMClock", "avg", "GPUSMClockMHz"},
		{"Memory Clock", "DCGM_FI_DEV_MEM_CLOCK", "MemClock", "avg", "GPUMemClockMHz"},
	}

	individualGPUs := make(map[string]map[string]interface{})
	gpuUUIDSet := make(map[string]bool)
	gpuModels := make(map[string]int)

	// Process each metric with a single query
	for _, def := range metricDefinitions {
		query := fmt.Sprintf(`%s{%s}`, def.promMetric, baseLabels)

		result, _, err := c.prometheusAPI.Query(ctx, query, queryTime)
		if err != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"ContainerResourceCollector",
				"Error querying GPU metric",
				err,
				map[string]string{
					"namespace":        pod.Namespace,
					"pod":              pod.Name,
					"container":        containerName,
					"error_type":       "prometheus_gpu_query_failed",
					"query":            query,
					"prometheus_url":   c.config.PrometheusURL,
					"zxporter_version": version.Get().String(),
				},
			)
			c.logger.Error(err, "Error querying GPU metric",
				"metric", def.name, "query", query)
			continue
		}

		// Calculate aggregate metrics
		var aggValue float64
		var validSamples float64

		if result.Type() == model.ValVector {
			vector := result.(model.Vector)

			for _, sample := range vector {
				uuid := string(sample.Metric["UUID"])
				if uuid == "" {
					continue
				}

				gpuUUIDSet[uuid] = true

				model := string(sample.Metric["modelName"])
				if model != "" {
					gpuModels[model]++
				}

				if _, exists := individualGPUs[uuid]; !exists {
					individualGPUs[uuid] = map[string]interface{}{
						"UUID":        uuid,
						"ModelName":   model,
						"DeviceIndex": string(sample.Metric["device"]),
					}
				}

				value := float64(sample.Value)
				individualGPUs[uuid][def.resultKey] = value

				if def.aggregateOp == "sum" {
					aggValue += value
				} else { // "avg"
					aggValue += value
					validSamples++
				}
			}

			if def.aggregateOp == "avg" && validSamples > 0 {
				aggValue /= validSamples
			}

			metrics[def.metricKey] = aggValue
		}
	}

	for _, gpu := range individualGPUs {
		memUsed, hasUsed := gpu["MemoryUsed"].(float64)
		memFree, hasFree := gpu["MemoryFree"].(float64)

		if hasUsed && hasFree {
			totalMem := memUsed + memFree
			gpu["TotalMemory"] = totalMem
			gpu["MemoryUtilizationPercentage"] = (memUsed / totalMem) * 100
		}
	}

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

	// Convert individual GPU map to array for JSON serialization
	gpuArray := make([]map[string]interface{}, 0, len(individualGPUs))
	for _, gpu := range individualGPUs {
		gpuArray = append(gpuArray, gpu)
	}
	metrics["IndividualGPUs"] = gpuArray

	// GPUUsage = (utilization percentage * GPU count) / 100
	if gpuCount > 0 {
		if gpuUtil, ok := metrics["GPUUtilizationPercentage"].(float64); ok {
			metrics["GPUUsage"] = (gpuUtil * gpuCount) / 100.0
		}
	}

	// Calculate total memory
	if memUsed, ok := metrics["GPUMemoryUsedMb"].(float64); ok {
		if memFree, ok := metrics["GPUMemoryFreeMb"].(float64); ok {
			metrics["GPUTotalMemoryMb"] = memUsed + memFree
		}
	}

	// Get resource requests and limits from container spec
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == containerName {
			requests := pod.Spec.Containers[i].Resources.Requests
			limits := pod.Spec.Containers[i].Resources.Limits

			if gpuReq, ok := requests[gpuconst.GpuResource]; ok {
				metrics["GPURequestCount"] = gpuReq.Value()
			}

			if gpuLim, ok := limits[gpuconst.GpuResource]; ok {
				metrics["GPULimitCount"] = gpuLim.Value()
			}
			break
		}
	}

	return metrics, nil
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

// IsAvailable checks if container resource metrics are available in the cluster
func (c *ContainerResourceCollector) IsAvailable(ctx context.Context) bool {
	// First verify the metrics client is initialized
	if c.metricsClient == nil {
		c.logger.Info("Metrics client is not available, cannot collect container resources")
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"ContainerResourceCollector",
			"Metrics client is not available or set properly, cannot collect container resources",
			fmt.Errorf("metrics server client is not available or set"),
			map[string]string{
				"collector_type":   c.GetType(),
				"zxporter_version": version.Get().String(),
			},
		)
		return false
	}

	// Try to list pod metrics to check metrics API availability
	_, err := c.metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{
		Limit: 1, // Only request a single item to minimize load
	})

	if err != nil {
		c.logger.Info("Metrics server API not available", "error", err.Error())
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"ContainerResourceCollector",
			"Metrics server API not available",
			err,
			map[string]string{
				"collector_type":   c.GetType(),
				"zxporter_version": version.Get().String(),
			},
		)
		return false
	}

	// If network/IO and GPU metrics are not disabled, also check Prometheus availability
	if !c.config.DisableNetworkIOMetrics && !c.config.DisableGPUMetrics {
		if c.prometheusAPI == nil {
			c.logger.Info("Prometheus client is not available for network/IO or GPU metrics")
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"ContainerResourceCollector",
				"Prometheus client is not available for network/IO or GPU metrics",
				fmt.Errorf("prometehus client not available or set properly"),
				map[string]string{
					"collector_type":   c.GetType(),
					"zxporter_version": version.Get().String(),
				},
			)
			// Still return true since the main metrics are available
			return true
		}

		// Try a simple query to check if Prometheus is available
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		_, _, err = c.prometheusAPI.Query(queryCtx, "up", time.Now())
		if err != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"ContainerResourceCollector",
				"Prometheus API not available for network and I/O metrics",
				err,
				map[string]string{
					"collector_type":   c.GetType(),
					"zxporter_version": version.Get().String(),
				},
			)
			c.logger.Info("Prometheus API not available for network and I/O metrics", "error", err.Error())
			// Still return true since the main metrics are available
		}
	}

	return true
}

// AddResource is a no-op for container resource collector - we never sync individual containers
func (c *ContainerResourceCollector) AddResource(resource interface{}) error {
	// Container resources are collected automatically via metrics scraping, not via individual resource refresh
	return nil
}
