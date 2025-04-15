// internal/collector/container_collector.go
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

// ContainerCollector watches for container events within pods
type ContainerCollector struct {
	client             kubernetes.Interface
	informerFactory    informers.SharedInformerFactory
	podInformer        cache.SharedIndexInformer
	resourceChan       chan CollectedResource
	stopCh             chan struct{}
	namespaces         []string
	excludedContainers map[types.NamespacedName]bool
	logger             logr.Logger
	mu                 sync.RWMutex
}

// ExcludedContainer identifies a container to exclude from collection
type ExcludedContainer struct {
	Namespace     string
	PodName       string
	ContainerName string
}

// NewContainerCollector creates a new collector for container resources
func NewContainerCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedContainers []ExcludedContainer,
	logger logr.Logger,
) *ContainerCollector {
	// Convert excluded containers to a map for quicker lookups
	excludedContainersMap := make(map[types.NamespacedName]bool)
	for _, container := range excludedContainers {
		excludedContainersMap[types.NamespacedName{
			Namespace: container.Namespace,
			Name:      fmt.Sprintf("%s/%s", container.PodName, container.ContainerName),
		}] = true
	}

	return &ContainerCollector{
		client:             client,
		resourceChan:       make(chan CollectedResource, 500),
		stopCh:             make(chan struct{}),
		namespaces:         namespaces,
		excludedContainers: excludedContainersMap,
		logger:             logger.WithName("container-collector"),
	}
}

// Start begins the container collection process
func (c *ContainerCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting container collector", "namespaces", c.namespaces)

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

	// We still use pod informer since Kubernetes doesn't have a dedicated container informer
	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Add event handlers
	_, err := c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodAdd(pod)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			c.handlePodUpdate(oldPod, newPod)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodDelete(pod)
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

// handlePodAdd processes new pods and extracts container information
func (c *ContainerCollector) handlePodAdd(pod *corev1.Pod) {
	// When a pod is added, collect initial data for all its containers
	for _, container := range pod.Spec.Containers {
		if c.isExcluded(pod, container.Name) {
			continue
		}

		// Find the container status if available
		var containerStatus *corev1.ContainerStatus
		for i := range pod.Status.ContainerStatuses {
			if pod.Status.ContainerStatuses[i].Name == container.Name {
				containerStatus = &pod.Status.ContainerStatuses[i]
				break
			}
		}

		c.sendContainerEvent(pod, container.Name, "containerAdded", containerStatus, container)
	}
}

// handlePodUpdate processes pod updates and detects container-specific changes
func (c *ContainerCollector) handlePodUpdate(oldPod, newPod *corev1.Pod) {
	// Create maps of old container statuses for efficient lookup
	oldStatuses := make(map[string]corev1.ContainerStatus)
	for _, status := range oldPod.Status.ContainerStatuses {
		oldStatuses[status.Name] = status
	}

	// Check each container in the new pod
	for _, newStatus := range newPod.Status.ContainerStatuses {
		if c.isExcluded(newPod, newStatus.Name) {
			continue
		}

		// Find the corresponding container spec
		var containerSpec corev1.Container
		for _, container := range newPod.Spec.Containers {
			if container.Name == newStatus.Name {
				containerSpec = container
				break
			}
		}

		oldStatus, exists := oldStatuses[newStatus.Name]

		// Check for container state changes
		if !exists {
			// New container appeared
			c.sendContainerEvent(newPod, newStatus.Name, "containerAdded", &newStatus, containerSpec)
			continue
		}

		// Check for container start
		if oldStatus.State.Running == nil && newStatus.State.Running != nil {
			c.sendContainerEvent(newPod, newStatus.Name, "containerStarted", &newStatus, containerSpec)
		}

		// Check for container stop/termination
		if oldStatus.State.Running != nil && newStatus.State.Running == nil {
			var reason string

			if newStatus.State.Terminated != nil {
				reason = fmt.Sprintf("containerTerminated:%s", newStatus.State.Terminated.Reason)

				// Special handling for OOMKilled
				if newStatus.State.Terminated.Reason == "OOMKilled" {
					c.logger.Info("Container OOM killed",
						"namespace", newPod.Namespace,
						"pod", newPod.Name,
						"container", newStatus.Name)
					reason = "containerOOMKilled"
				}
			} else if newStatus.State.Waiting != nil {
				reason = fmt.Sprintf("containerWaiting:%s", newStatus.State.Waiting.Reason)
			} else {
				reason = "containerStopped"
			}

			c.sendContainerEvent(newPod, newStatus.Name, reason, &newStatus, containerSpec)
		}

		// Check for container restart
		if newStatus.RestartCount > oldStatus.RestartCount {
			reason := "containerRestarted"

			// Check if the restart was due to OOM
			if newStatus.LastTerminationState.Terminated != nil &&
				newStatus.LastTerminationState.Terminated.Reason == "OOMKilled" {
				reason = "containerOOMKilled"
			}

			c.sendContainerEvent(newPod, newStatus.Name, reason, &newStatus, containerSpec)
		}

		// Check for ready state changes
		if oldStatus.Ready != newStatus.Ready {
			if newStatus.Ready {
				c.sendContainerEvent(newPod, newStatus.Name, "containerReady", &newStatus, containerSpec)
			} else {
				c.sendContainerEvent(newPod, newStatus.Name, "containerNotReady", &newStatus, containerSpec)
			}
		}
	}

	// Check for containers that were removed
	for _, oldStatus := range oldPod.Status.ContainerStatuses {
		if c.isExcluded(oldPod, oldStatus.Name) {
			continue
		}

		// Check if this container still exists in the new pod
		found := false
		for _, newStatus := range newPod.Status.ContainerStatuses {
			if newStatus.Name == oldStatus.Name {
				found = true
				break
			}
		}

		if !found {
			// Find the corresponding container spec from old pod
			var containerSpec corev1.Container
			for _, container := range oldPod.Spec.Containers {
				if container.Name == oldStatus.Name {
					containerSpec = container
					break
				}
			}
			c.sendContainerEvent(oldPod, oldStatus.Name, "containerRemoved", &oldStatus, containerSpec)
		}
	}
}

// handlePodDelete processes pod deletion and marks all containers as removed
func (c *ContainerCollector) handlePodDelete(pod *corev1.Pod) {
	for _, container := range pod.Spec.Containers {
		if c.isExcluded(pod, container.Name) {
			continue
		}

		// Find the container status if available
		var containerStatus *corev1.ContainerStatus
		for i := range pod.Status.ContainerStatuses {
			if pod.Status.ContainerStatuses[i].Name == container.Name {
				containerStatus = &pod.Status.ContainerStatuses[i]
				break
			}
		}

		c.sendContainerEvent(pod, container.Name, "containerRemoved", containerStatus, container)
	}
}

// sendContainerEvent sends a container-specific event
func (c *ContainerCollector) sendContainerEvent(
	pod *corev1.Pod,
	containerName,
	eventType string,
	status *corev1.ContainerStatus,
	containerSpec corev1.Container,
) {
	containerKey := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, containerName)

	c.logger.Info("Container event",
		"namespace", pod.Namespace,
		"pod", pod.Name,
		"container", containerName,
		"eventType", eventType)

	// Prepare container metrics/data
	containerData := map[string]interface{}{
		"podName":        pod.Name,
		"podUID":         pod.UID,
		"namespace":      pod.Namespace,
		"containerName":  containerName,
		"eventType":      eventType,
		"podLabels":      pod.Labels,
		"podAnnotations": pod.Annotations,
		"containerSpec":  containerSpec,
	}

	// Add status information if available
	if status != nil {
		containerData["status"] = status
		containerData["restartCount"] = status.RestartCount
		containerData["ready"] = status.Ready

		if status.State.Running != nil {
			containerData["state"] = "running"
			containerData["startedAt"] = status.State.Running.StartedAt
		} else if status.State.Terminated != nil {
			containerData["state"] = "terminated"
			containerData["exitCode"] = status.State.Terminated.ExitCode
			containerData["reason"] = status.State.Terminated.Reason
			containerData["message"] = status.State.Terminated.Message
			containerData["startedAt"] = status.State.Terminated.StartedAt
			containerData["finishedAt"] = status.State.Terminated.FinishedAt
		} else if status.State.Waiting != nil {
			containerData["state"] = "waiting"
			containerData["reason"] = status.State.Waiting.Reason
			containerData["message"] = status.State.Waiting.Message
		}
	}

	// Resource requests and limits
	if containerSpec.Resources.Requests != nil {
		containerData["cpuRequest"] = containerSpec.Resources.Requests.Cpu().String()
		containerData["memoryRequest"] = containerSpec.Resources.Requests.Memory().String()
	}

	if containerSpec.Resources.Limits != nil {
		containerData["cpuLimit"] = containerSpec.Resources.Limits.Cpu().String()
		containerData["memoryLimit"] = containerSpec.Resources.Limits.Memory().String()
	}

	c.resourceChan <- CollectedResource{
		ResourceType: Container,
		Object:       containerData,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          containerKey,
	}
}

// isExcluded checks if a container should be excluded from collection
func (c *ContainerCollector) isExcluded(pod *corev1.Pod, containerName string) bool {
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

	// Check if container is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      fmt.Sprintf("%s/%s", pod.Name, containerName),
	}
	return c.excludedContainers[key]
}

// Stop gracefully shuts down the container collector
func (c *ContainerCollector) Stop() error {
	c.logger.Info("Stopping container collector")
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ContainerCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ContainerCollector) GetType() string {
	return "container"
}

// IsAvailable checks if Container resources can be accessed in the cluster
func (c *ContainerCollector) IsAvailable(ctx context.Context) bool {
	return true
}
