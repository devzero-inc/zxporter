// internal/collector/pod_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PodCollector watches for pod events and collects pod data
type PodCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	podInformer     cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	excludedPods    map[types.NamespacedName]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
}

// NewPodCollector creates a new collector for pod resources
func NewPodCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedPods []ExcludedPod,
	maxBatchSize int, // Added parameter
	maxBatchTime time.Duration, // Added parameter
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *PodCollector {
	// Convert excluded pods to a map for quicker lookups
	excludedPodsMap := make(map[types.NamespacedName]bool)
	for _, pod := range excludedPods {
		excludedPodsMap[types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      pod.Name,
		}] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 500)      // Keep original buffer size for individual items
	resourceChan := make(chan []CollectedResource, 200) // Buffer for batches

	// Create the batcher, passing through the configurable parameters
	batcher := NewResourcesBatcher(
		maxBatchSize, // Use provided parameter
		maxBatchTime, // Use provided parameter
		batchChan,
		resourceChan,
		logger,
	)

	return &PodCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		excludedPods:    excludedPodsMap,
		logger:          logger.WithName("pod-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the pod collection process
func (c *PodCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting pod collector", "namespaces", c.namespaces)

	// Create informer factory based on namespace configuration
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
			c.client,
			0, // No resync period, rely on events
			informers.WithNamespace(c.namespaces[0]),
		)
	} else {
		// Watch all namespaces
		c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)
	}

	// Create pod informer
	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Add event handlers
	_, err := c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodEvent(pod, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			c.handlePodUpdate(oldPod, newPod)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodEvent(pod, EventTypeDelete)
		},
	})
	if err != nil {
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"PodCollector",
				"Failed to add event handler",
				err,
				map[string]string{"namespaces": fmt.Sprintf("%v", c.namespaces)},
			)
		}
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced) {
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"PodCollector",
				"Timed out waiting for caches to sync",
				fmt.Errorf("cache sync timeout"),
				map[string]string{"namespaces": fmt.Sprintf("%v", c.namespaces)},
			)
		}
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for pods")
	c.batcher.start()

	// Keep this goroutine alive until context cancellation or stop
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

// handlePodEvent processes pod add and delete events
func (c *PodCollector) handlePodEvent(pod *corev1.Pod, eventType EventType) {
	if c.isExcluded(pod) {
		return
	}

	// Send the raw pod object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Pod,
		Object:       pod, // Send the entire pod object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
	}
}

// handlePodUpdate processes pod update events with special handling for container status changes
func (c *PodCollector) handlePodUpdate(oldPod, newPod *corev1.Pod) {
	if c.isExcluded(newPod) {
		return
	}

	// Send the basic pod update
	c.handlePodEvent(newPod, EventTypeUpdate)

	// Check for container events like OOMKilled
	c.checkForContainerEvents(oldPod, newPod)
}

// checkForContainerEvents checks for container-specific events like OOMKilled
func (c *PodCollector) checkForContainerEvents(oldPod, newPod *corev1.Pod) {
	// Create maps of old container statuses for efficient lookup
	oldStatuses := make(map[string]corev1.ContainerStatus)
	for _, status := range oldPod.Status.ContainerStatuses {
		oldStatuses[status.Name] = status
	}

	// Check each container in the new pod
	for _, newStatus := range newPod.Status.ContainerStatuses {
		oldStatus, exists := oldStatuses[newStatus.Name]

		// Special events to look for:
		// 1. Container started (wasn't running before, is running now)
		// 2. Container stopped (was running before, isn't running now)
		// 3. Container restarted (restart count increased)
		// 4. Container OOMKilled

		// Check for container start
		if (!exists || oldStatus.State.Running == nil) && newStatus.State.Running != nil {
			c.sendContainerEvent(newPod, newStatus.Name, EventTypeContainerStarted, &newStatus)
		}

		// Check for container stop
		if (exists && oldStatus.State.Running != nil) && newStatus.State.Running == nil {
			c.sendContainerEvent(newPod, newStatus.Name, EventTypeContainerStopped, &newStatus)
		}

		// Check for container restart
		if exists && newStatus.RestartCount > oldStatus.RestartCount {
			// reason := "containerRestarted"

			// Check if the restart was due to OOM
			if newStatus.LastTerminationState.Terminated != nil &&
				newStatus.LastTerminationState.Terminated.Reason == "OOMKilled" {
				// reason = "containerOOMKilled"
				c.logger.Info("Container OOM killed",
					"namespace", newPod.Namespace,
					"pod", newPod.Name,
					"container", newStatus.Name)

				// Report OOM event to telemetry
				if c.telemetryLogger != nil {
					c.telemetryLogger.Report(
						gen.LogLevel_LOG_LEVEL_WARN,
						"PodCollector",
						"Container OOM killed",
						fmt.Errorf("container %s in pod %s/%s was OOM killed", newStatus.Name, newPod.Namespace, newPod.Name),
						map[string]string{
							"namespace":         newPod.Namespace,
							"pod":               newPod.Name,
							"container":         newStatus.Name,
							"restartCount":      fmt.Sprintf("%d", newStatus.RestartCount),
							"exitCode":          fmt.Sprintf("%d", newStatus.LastTerminationState.Terminated.ExitCode),
							"terminationReason": newStatus.LastTerminationState.Terminated.Reason,
						},
					)
				}
			}

			c.sendContainerEvent(newPod, newStatus.Name, EventTypeContainerRestarted, &newStatus)
		}
	}
}

// sendContainerEvent sends a container-specific event
func (c *PodCollector) sendContainerEvent(pod *corev1.Pod, containerName string, eventType EventType, status *corev1.ContainerStatus) {
	containerKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, containerName)

	// Send the container event to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Container, // Note: Still using Container type for these specific events
		Object: map[string]interface{}{
			"pod":           pod,           // The entire pod object
			"containerName": containerName, // The specific container name
			"status":        status,        // The container status
			"eventDetail":   eventType,     // The specific event type
		},
		Timestamp: time.Now(),
		EventType: eventType,
		Key:       containerKey,
	}
}

// isExcluded checks if a pod should be excluded from collection
func (c *PodCollector) isExcluded(pod *corev1.Pod) bool {
	// Check if monitoring specific namespaces and this pod isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == pod.Namespace {
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
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}
	return c.excludedPods[key]
}

// Stop gracefully shuts down the pod collector
func (c *PodCollector) Stop() error {
	c.logger.Info("Stopping pod collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Pod collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed pod collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed pod collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Pod collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *PodCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *PodCollector) GetType() string {
	return "pod"
}

// IsAvailable checks if Pod resources can be accessed in the cluster
func (c *PodCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a pod resource to be processed by the collector
func (c *PodCollector) AddResource(resource interface{}) error {
	pod, ok := resource.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected *corev1.Pod, got %T", resource)
	}

	c.handlePodEvent(pod, EventTypeAdd)
	return nil
}
