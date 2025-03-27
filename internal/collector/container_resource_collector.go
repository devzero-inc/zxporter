// internal/collector/container_resource_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// ContainerResourceCollector collects container resource usage metrics
type ContainerResourceCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	informerFactory informers.SharedInformerFactory
	podInformer     cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	ticker          *time.Ticker
	updateInterval  time.Duration
	namespaces      []string
	excludedPods    map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// NewContainerResourceCollector creates a new collector for container resource metrics
func NewContainerResourceCollector(
	k8sClient kubernetes.Interface,
	metricsClient *metricsv1.Clientset,
	namespaces []string,
	excludedPods []ExcludedPod,
	updateInterval time.Duration,
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
	if updateInterval <= 0 {
		updateInterval = 10 * time.Second
	}

	return &ContainerResourceCollector{
		k8sClient:      k8sClient,
		metricsClient:  metricsClient,
		resourceChan:   make(chan CollectedResource, 500),
		stopCh:         make(chan struct{}),
		updateInterval: updateInterval,
		namespaces:     namespaces,
		excludedPods:   excludedPodsMap,
		logger:         logger.WithName("container-resource-collector"),
	}
}

// Start begins the container resource collection process
func (c *ContainerResourceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting container resource collector",
		"namespaces", c.namespaces,
		"updateInterval", c.updateInterval)

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
	// c.podLister = c.informerFactory.Core().V1().Pods().Lister()

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for pod cache to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start a ticker to collect resource metrics at regular intervals
	c.ticker = time.NewTicker(c.updateInterval)

	// Start the collection loop
	go c.collectResourcesLoop(ctx)

	// Monitor for context cancellation
	go func() {
		<-ctx.Done()
		c.logger.Info("Context cancelled, stopping container resource collector")
		c.Stop()
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
	c.logger.V(4).Info("Collecting container resource metrics")

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

		// Process each container's metrics
		for _, containerMetrics := range podMetrics.Containers {
			c.processContainerMetrics(pod, containerMetrics)
		}
	}
}

// processContainerMetrics processes metrics for a single container
func (c *ContainerResourceCollector) processContainerMetrics(pod *corev1.Pod, containerMetrics metricsv1beta1.ContainerMetrics) {
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

		// Resource usage
		"cpuUsageMillis":   cpuUsage,
		"memoryUsageBytes": memoryUsage,

		// All this can be retrieved from the pod object so we can exclude this fields but
		// keeping for now to understand what we can get and why we sending this data.
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

	// Send the resource usage data to the channel
	c.resourceChan <- CollectedResource{
		ResourceType: ContainerResource,
		Object:       resourceData,
		Timestamp:    time.Now(),
		EventType:    "metrics",
		Key:          containerKey,
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

	if c.ticker != nil {
		c.ticker.Stop()
	}

	close(c.stopCh)
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
