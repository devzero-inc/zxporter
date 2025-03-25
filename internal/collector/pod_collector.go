// internal/collector/pod_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

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
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	namespaces      []string
	excludedPods    map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// NewPodCollector creates a new collector for pod resources
func NewPodCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedPods []ExcludedPod,
	logger logr.Logger,
) *PodCollector {
	// Convert excluded pods to a map for quicker lookups
	excludedPodsMap := make(map[types.NamespacedName]bool)
	for _, pod := range excludedPods {
		excludedPodsMap[types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      pod.Name,
		}] = true
	}

	return &PodCollector{
		client:       client,
		resourceChan: make(chan CollectedResource, 500),
		stopCh:       make(chan struct{}),
		namespaces:   namespaces,
		excludedPods: excludedPodsMap,
		logger:       logger.WithName("pod-collector"),
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
			c.handlePodEvent(pod, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			c.handlePodUpdate(oldPod, newPod)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodEvent(pod, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handlePodEvent processes pod add and delete events
func (c *PodCollector) handlePodEvent(pod *corev1.Pod, eventType string) {
	if c.isExcluded(pod) {
		return
	}

	c.logger.V(4).Info("Processing pod event",
		"namespace", pod.Namespace,
		"name", pod.Name,
		"eventType", eventType)

	// Send the raw pod object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: "pod",
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
	c.handlePodEvent(newPod, "update")

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
			c.sendContainerEvent(newPod, newStatus.Name, "containerStarted", &newStatus)
		}

		// Check for container stop
		if (exists && oldStatus.State.Running != nil) && newStatus.State.Running == nil {
			c.sendContainerEvent(newPod, newStatus.Name, "containerStopped", &newStatus)
		}

		// Check for container restart
		if exists && newStatus.RestartCount > oldStatus.RestartCount {
			reason := "containerRestarted"

			// Check if the restart was due to OOM
			if newStatus.LastTerminationState.Terminated != nil &&
				newStatus.LastTerminationState.Terminated.Reason == "OOMKilled" {
				reason = "containerOOMKilled"
				c.logger.Info("Container OOM killed",
					"namespace", newPod.Namespace,
					"pod", newPod.Name,
					"container", newStatus.Name)
			}

			c.sendContainerEvent(newPod, newStatus.Name, reason, &newStatus)
		}
	}
}

// sendContainerEvent sends a container-specific event
func (c *PodCollector) sendContainerEvent(pod *corev1.Pod, containerName, eventType string, status *corev1.ContainerStatus) {
	containerKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, containerName)

	// We're still sending the entire pod, but with additional context about which container and event
	c.resourceChan <- CollectedResource{
		ResourceType: "container",
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *PodCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *PodCollector) GetType() string {
	return "pod"
}
