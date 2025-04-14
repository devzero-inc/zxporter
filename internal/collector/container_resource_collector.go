// internal/collector/container_resource_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

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
}

// ContainerResourceCollector collects container resource usage metrics
type ContainerResourceCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	prometheusAPI   v1.API
	informerFactory informers.SharedInformerFactory
	podInformer     cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	ticker          *time.Ticker
	config          ContainerResourceCollectorConfig
	namespaces      []string
	excludedPods    map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// NewContainerResourceCollector creates a new collector for container resource metrics
func NewContainerResourceCollector(
	k8sClient kubernetes.Interface,
	metricsClient *metricsv1.Clientset,
	config ContainerResourceCollectorConfig,
	namespaces []string,
	excludedPods []ExcludedPod,
	logger logr.Logger,
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

	return &ContainerResourceCollector{
		k8sClient:     k8sClient,
		metricsClient: metricsClient,
		resourceChan:  make(chan CollectedResource, 500),
		stopCh:        make(chan struct{}),
		config:        config,
		namespaces:    namespaces,
		excludedPods:  excludedPodsMap,
		logger:        logger.WithName("container-resource-collector"),
	}
}

// Start begins the container resource collection process
func (c *ContainerResourceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting container resource collector",
		"namespaces", c.namespaces,
		"updateInterval", c.config.UpdateInterval)

	// Check if metrics client is available
	if c.metricsClient == nil {
		return fmt.Errorf("metrics client is not available, cannot collect container resources")
	}

	// Initialize Prometheus client

	c.logger.Info("Initializing Prometheus client for network and I/O metrics",
		"prometheusURL", c.config.PrometheusURL)
	client, err := api.NewClient(api.Config{
		Address: c.config.PrometheusURL,
	})
	if err != nil {
		c.logger.Error(err, "Failed to create Prometheus client, network and I/O metrics will be disabled")
	} else {
		c.prometheusAPI = v1.NewAPI(client)
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
	c.logger.Info("Collecting container resource metrics")

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
		c.logger.Error(err, "Failed to get pod metrics from metrics server")
		return
	}

	// Create a context with timeout for Prometheus queries if needed
	var queryCtx context.Context
	var cancel context.CancelFunc
	if c.prometheusAPI != nil {
		queryCtx, cancel = context.WithTimeout(ctx, c.config.QueryTimeout)
		defer cancel()
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

		// Fetch network metrics
		var networkMetrics map[string]float64
		if c.prometheusAPI != nil && queryCtx != nil {
			networkMetrics, err = c.collectPodNetworkMetrics(queryCtx, pod)
			if err != nil {
				c.logger.Error(err, "Failed to collect network metrics",
					"namespace", podMetrics.Namespace,
					"name", podMetrics.Name)
				// Continue with CPU/memory metrics
				networkMetrics = make(map[string]float64)
			}
		}

		// Process each container's metrics
		for _, containerMetrics := range podMetrics.Containers {
			// Fetch I/O metrics for this container
			var ioMetrics map[string]float64
			if c.prometheusAPI != nil && queryCtx != nil {
				ioMetrics, err = c.collectContainerIOMetrics(queryCtx, pod, containerMetrics.Name)
				if err != nil {
					c.logger.Error(err, "Failed to collect I/O metrics",
						"namespace", podMetrics.Namespace,
						"pod", podMetrics.Name,
						"container", containerMetrics.Name)
					// Continue with CPU/memory metrics
					ioMetrics = make(map[string]float64)
				}
			}

			// Process the container metrics with optional network/IO data
			c.processContainerMetrics(pod, containerMetrics, networkMetrics, ioMetrics)
		}
	}
}

// processContainerMetrics processes metrics for a single container
func (c *ContainerResourceCollector) processContainerMetrics(
	pod *corev1.Pod,
	containerMetrics metricsv1beta1.ContainerMetrics,
	networkMetrics map[string]float64,
	ioMetrics map[string]float64,
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

	// Create resource data with both metrics and pod info
	containerKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, containerMetrics.Name)
	resourceData := map[string]interface{}{
		// Container identification
		"containerName": containerMetrics.Name,
		"podName":       pod.Name,
		"namespace":     pod.Namespace,
		"nodeName":      pod.Spec.NodeName,

		// CPU/Memory resource usage
		"cpuUsageMillis":   cpuUsage,
		"memoryUsageBytes": memoryUsage,

		// Resource requests and limits
		"cpuRequestMillis":   cpuRequestMillis,
		"cpuLimitMillis":     cpuLimitMillis,
		"memoryRequestBytes": memoryRequestBytes,
		"memoryLimitBytes":   memoryLimitBytes,

		// Labels from the pod for correlation
		"podLabels": pod.Labels,

		// Container metadata for reference
		"containerImage": containerSpec.Image,

		// Status info
		"containerRunning":  containerStatus != nil && containerStatus.State.Running != nil,
		"containerRestarts": containerStatus != nil && containerStatus.RestartCount != 0,

		// Include the full pod object for any other needed details
		"pod": pod,
	}

	// Add network metrics if available
	if networkMetrics != nil {
		resourceData["networkReceiveBytes"] = networkMetrics["NetworkReceiveBytes"]
		resourceData["networkTransmitBytes"] = networkMetrics["NetworkTransmitBytes"]
		resourceData["networkReceivePackets"] = networkMetrics["NetworkReceivePackets"]
		resourceData["networkTransmitPackets"] = networkMetrics["NetworkTransmitPackets"]
		resourceData["networkReceiveErrors"] = networkMetrics["NetworkReceiveErrors"]
		resourceData["networkTransmitErrors"] = networkMetrics["NetworkTransmitErrors"]
		resourceData["networkReceiveDropped"] = networkMetrics["NetworkReceiveDropped"]
		resourceData["networkTransmitDropped"] = networkMetrics["NetworkTransmitDropped"]

		// Add human-readable network throughput
		resourceData["networkReceiveBytesHR"] = networkMetrics["NetworkReceiveBytes"]
		resourceData["networkTransmitBytesHR"] = networkMetrics["NetworkTransmitBytes"]
	}

	// Add I/O metrics if available
	if ioMetrics != nil {
		resourceData["fsReadBytes"] = ioMetrics["FSReadBytes"]
		resourceData["fsWriteBytes"] = ioMetrics["FSWriteBytes"]
		resourceData["fsReads"] = ioMetrics["FSReads"]
		resourceData["fsWrites"] = ioMetrics["FSWrites"]

		// Add human-readable I/O throughput
		resourceData["fsReadBytesHR"] = ioMetrics["FSReadBytes"]
		resourceData["fsWriteBytesHR"] = ioMetrics["FSWriteBytes"]
	}

	// Send the resource usage data to the channel
	c.resourceChan <- CollectedResource{
		ResourceType: ContainerResource,
		Object:       resourceData,
		Timestamp:    time.Now(),
		EventType:    "metrics",
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

// humanizeBytes converts bytes to a human-readable format (KB, MB, GB, etc.)
func humanizeBytes(bytes float64) string {
	const unit = 1024.0
	if bytes < unit {
		return fmt.Sprintf("%.2f B", bytes)
	}
	div, exp := unit, 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", bytes/div, "KMGTPE"[exp])
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

	if c.ticker != nil {
		c.ticker.Stop()
	}

	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ContainerResourceCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ContainerResourceCollector) GetType() string {
	return "container_resources"
}

// IsAvailable checks if container resource metrics are available in the cluster
func (c *ContainerResourceCollector) IsAvailable(ctx context.Context) bool {
	// First verify the metrics client is initialized
	if c.metricsClient == nil {
		c.logger.Info("Metrics client is not available, cannot collect container resources")
		return false
	}

	// Try to list pod metrics to check metrics API availability
	_, err := c.metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{
		Limit: 1, // Only request a single item to minimize load
	})

	if err != nil {
		c.logger.Info("Metrics server API not available", "error", err.Error())
		return false
	}

	// check Prometheus availability
	if c.prometheusAPI == nil {
		c.logger.Info("Prometheus client is not available")
		// Still return true since the main metrics are available
		return true
	}

	// Try a simple query to check if Prometheus is available
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, _, err = c.prometheusAPI.Query(queryCtx, "up", time.Now())
	if err != nil {
		c.logger.Info("Prometheus API not available for network and I/O metrics", "error", err.Error())
		// Still return true since the main metrics are available
	}

	return true
}
