// internal/collector/pod_collector.go
package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// startupLifecycleKey uniquely identifies a container startup attempt.
type startupLifecycleKey struct {
	podUID        types.UID
	containerName string
	restartCount  int32
}

// startupLifecycleEntry tracks phase transition timestamps for a single container startup.
type startupLifecycleEntry struct {
	namespace     string
	workloadName  string
	workloadKind  string
	podName       string
	containerName string
	restartCount  int32
	isRestart     bool
	pendingAt     *time.Time
	creatingAt    *time.Time
	runningAt     *time.Time
	lastSeen      time.Time
}

const startupTrackerTTL = 30 * time.Minute

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

	// Startup lifecycle tracking
	startupTracker   map[startupLifecycleKey]*startupLifecycleEntry
	startupTrackerMu sync.Mutex
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
		startupTracker:  make(map[startupLifecycleKey]*startupLifecycleEntry),
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
				map[string]string{
					"namespaces":       fmt.Sprintf("%v", c.namespaces),
					"zxporter_version": version.Get().String(),
				},
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
				map[string]string{
					"namespaces":       fmt.Sprintf("%v", c.namespaces),
					"zxporter_version": version.Get().String(),
				},
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

	// On ADD (initial sync), emit startup lifecycle snapshots for running containers.
	// This ensures lifecycle data is captured even if zxporter restarts.
	if eventType == EventTypeAdd {
		c.snapshotStartupLifecycles(pod)
	}
}

// handlePodUpdate processes pod update events with special handling for container status changes
func (c *PodCollector) handlePodUpdate(oldPod, newPod *corev1.Pod) {
	if c.isExcluded(newPod) {
		return
	}

	// Send the basic pod update
	c.handlePodEvent(newPod, EventTypeUpdate)

	// Check for container events like OOMKilled and CrashLoopBackOff
	c.checkForContainerEvents(oldPod, newPod)

	// Track startup lifecycle phase transitions
	c.trackStartupLifecycle(oldPod, newPod)
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
			terminated := newStatus.LastTerminationState.Terminated
			isOOM := terminated != nil && (terminated.Reason == "OOMKilled" ||
				(terminated.Reason == "StartError" && strings.Contains(strings.ToLower(terminated.Message), "oom")))
			if isOOM {
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
							"zxporter_version":  version.Get().String(),
						},
					)
				}

				// Emit structured OOM event for direct path
				c.emitContainerOOMEvent(newPod, newStatus)
			}

			c.sendContainerEvent(newPod, newStatus.Name, EventTypeContainerRestarted, &newStatus)
		}

		// Check for CrashLoopBackOff
		if newStatus.State.Waiting != nil && newStatus.State.Waiting.Reason == "CrashLoopBackOff" {
			// Only emit if this is a new CrashLoop state (wasn't in CrashLoop before)
			if !exists || oldStatus.State.Waiting == nil || oldStatus.State.Waiting.Reason != "CrashLoopBackOff" {
				c.emitContainerCrashLoopEvent(newPod, newStatus)
			}
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

// getWorkloadInfo extracts the top-level workload name and kind from a pod's owner references.
// For pods owned by ReplicaSets (which are owned by Deployments), it returns the Deployment info.
func getWorkloadInfo(pod *corev1.Pod) (name, kind string) {
	if len(pod.OwnerReferences) == 0 {
		return pod.Name, "Pod"
	}

	owner := pod.OwnerReferences[0]
	switch owner.Kind {
	case "ReplicaSet":
		// ReplicaSets created by Deployments have names like "<deployment>-<hash>"
		// Strip the hash suffix to get the Deployment name
		rsName := owner.Name
		if idx := strings.LastIndex(rsName, "-"); idx > 0 {
			return rsName[:idx], "Deployment"
		}
		return rsName, "ReplicaSet"
	case "StatefulSet":
		return owner.Name, "StatefulSet"
	case "DaemonSet":
		return owner.Name, "DaemonSet"
	case "Job":
		return owner.Name, "Job"
	default:
		return owner.Name, owner.Kind
	}
}

// getContainerResources returns the memory request, limit, and usage for a container.
func getContainerResources(pod *corev1.Pod, containerName string) (requestBytes, limitBytes int64) {
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			if req, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				requestBytes = req.Value()
			}
			if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				limitBytes = lim.Value()
			}
			return
		}
	}
	return 0, 0
}

// emitContainerOOMEvent sends a structured OOM event through the batch channel.
func (c *PodCollector) emitContainerOOMEvent(pod *corev1.Pod, status corev1.ContainerStatus) {
	workloadName, workloadKind := getWorkloadInfo(pod)
	requestBytes, limitBytes := getContainerResources(pod, status.Name)

	var exitCode int32
	var usageBytes int64
	if status.LastTerminationState.Terminated != nil {
		exitCode = status.LastTerminationState.Terminated.ExitCode
	}
	// Memory usage at OOM time is not directly available from status;
	// use the limit as an approximation (OOM means usage >= limit)
	usageBytes = limitBytes

	c.batchChan <- CollectedResource{
		ResourceType: ContainerOOMEvent,
		Object: map[string]interface{}{
			"namespace":            pod.Namespace,
			"workload_name":        workloadName,
			"workload_kind":        workloadKind,
			"pod_name":             pod.Name,
			"container_name":       status.Name,
			"memory_usage_bytes":   usageBytes,
			"memory_request_bytes": requestBytes,
			"memory_limit_bytes":   limitBytes,
			"restart_count":        status.RestartCount,
			"exit_code":            exitCode,
			"timestamp":            time.Now().Format(time.RFC3339Nano),
		},
		Timestamp: time.Now(),
		EventType: EventTypeAdd,
		Key:       fmt.Sprintf("oom/%s/%s/%s", pod.Namespace, pod.Name, status.Name),
	}
}

// emitContainerCrashLoopEvent sends a structured CrashLoopBackOff event through the batch channel.
func (c *PodCollector) emitContainerCrashLoopEvent(pod *corev1.Pod, status corev1.ContainerStatus) {
	workloadName, workloadKind := getWorkloadInfo(pod)

	var lastTerminationReason string
	var exitCode int32
	var isOOMRelated bool
	if status.LastTerminationState.Terminated != nil {
		lastTerminationReason = status.LastTerminationState.Terminated.Reason
		exitCode = status.LastTerminationState.Terminated.ExitCode
		isOOMRelated = lastTerminationReason == "OOMKilled"
	}

	c.logger.Info("Container CrashLoopBackOff detected",
		"namespace", pod.Namespace,
		"pod", pod.Name,
		"container", status.Name,
		"restartCount", status.RestartCount,
		"isOOMRelated", isOOMRelated)

	c.batchChan <- CollectedResource{
		ResourceType: ContainerCrashLoopEvent,
		Object: map[string]interface{}{
			"namespace":               pod.Namespace,
			"workload_name":           workloadName,
			"workload_kind":           workloadKind,
			"pod_name":                pod.Name,
			"container_name":          status.Name,
			"restart_count":           status.RestartCount,
			"last_termination_reason": lastTerminationReason,
			"exit_code":               exitCode,
			"is_oom_related":          isOOMRelated,
			"timestamp":               time.Now().Format(time.RFC3339Nano),
		},
		Timestamp: time.Now(),
		EventType: EventTypeAdd,
		Key:       fmt.Sprintf("crashloop/%s/%s/%s", pod.Namespace, pod.Name, status.Name),
	}
}

// trackStartupLifecycle tracks container startup phase transitions and emits lifecycle events.
func (c *PodCollector) trackStartupLifecycle(_, newPod *corev1.Pod) {
	now := time.Now()

	// Periodically clean up stale entries
	c.cleanupStartupTracker()

	for _, newStatus := range newPod.Status.ContainerStatuses {
		key := startupLifecycleKey{
			podUID:        newPod.UID,
			containerName: newStatus.Name,
			restartCount:  newStatus.RestartCount,
		}

		c.startupTrackerMu.Lock()
		entry, exists := c.startupTracker[key]

		if !exists {
			// New lifecycle entry — pod is in Pending or later phase
			workloadName, workloadKind := getWorkloadInfo(newPod)
			entry = &startupLifecycleEntry{
				namespace:     newPod.Namespace,
				workloadName:  workloadName,
				workloadKind:  workloadKind,
				podName:       newPod.Name,
				containerName: newStatus.Name,
				restartCount:  newStatus.RestartCount,
				isRestart:     newStatus.RestartCount > 0,
				lastSeen:      now,
			}

			// Record pending time from pod condition
			for _, cond := range newPod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue {
					t := cond.LastTransitionTime.Time
					entry.pendingAt = &t
					break
				}
			}

			c.startupTracker[key] = entry
		}

		entry.lastSeen = now

		// Track ContainerCreating phase
		if newStatus.State.Waiting != nil && newStatus.State.Waiting.Reason == "ContainerCreating" && entry.creatingAt == nil {
			entry.creatingAt = &now
		}

		// Track Running phase
		if newStatus.State.Running != nil && entry.runningAt == nil {
			t := newStatus.State.Running.StartedAt.Time
			if t.IsZero() {
				t = now
			}
			entry.runningAt = &t
		}

		// Track Ready phase — emit the lifecycle event
		if newStatus.Ready && entry.runningAt != nil {
			// Calculate durations
			var timeToRunningMs, timeToReadyMs *int64
			if entry.pendingAt != nil && entry.runningAt != nil {
				ms := entry.runningAt.Sub(*entry.pendingAt).Milliseconds()
				timeToRunningMs = &ms
			}
			if entry.pendingAt != nil {
				ms := now.Sub(*entry.pendingAt).Milliseconds()
				timeToReadyMs = &ms
			}

			c.emitStartupLifecycleEvent(entry, &now, timeToRunningMs, timeToReadyMs)

			// Clean up the entry
			delete(c.startupTracker, key)
			c.startupTrackerMu.Unlock()
			continue
		}

		c.startupTrackerMu.Unlock()
	}
}

// emitStartupLifecycleEvent sends a completed startup lifecycle event through the batch channel.
func (c *PodCollector) emitStartupLifecycleEvent(entry *startupLifecycleEntry, readyAt *time.Time, timeToRunningMs, timeToReadyMs *int64) {
	payload := map[string]interface{}{
		"namespace":      entry.namespace,
		"workload_name":  entry.workloadName,
		"workload_kind":  entry.workloadKind,
		"pod_name":       entry.podName,
		"container_name": entry.containerName,
		"restart_count":  entry.restartCount,
		"is_restart":     entry.isRestart,
		"timestamp":      time.Now().Format(time.RFC3339Nano),
	}

	if entry.pendingAt != nil {
		payload["pending_at"] = entry.pendingAt.Format(time.RFC3339Nano)
	}
	if entry.creatingAt != nil {
		payload["container_creating_at"] = entry.creatingAt.Format(time.RFC3339Nano)
	}
	if entry.runningAt != nil {
		payload["running_at"] = entry.runningAt.Format(time.RFC3339Nano)
	}
	if readyAt != nil {
		payload["ready_at"] = readyAt.Format(time.RFC3339Nano)
	}
	if timeToRunningMs != nil {
		payload["time_to_running_ms"] = *timeToRunningMs
	}
	if timeToReadyMs != nil {
		payload["time_to_ready_ms"] = *timeToReadyMs
	}

	c.batchChan <- CollectedResource{
		ResourceType: ContainerStartupLifecycle,
		Object:       payload,
		Timestamp:    time.Now(),
		EventType:    EventTypeAdd,
		Key:          fmt.Sprintf("startup/%s/%s/%s/%d", entry.namespace, entry.podName, entry.containerName, entry.restartCount),
	}
}

// cleanupStartupTracker removes stale entries that never reached Ready state.
func (c *PodCollector) cleanupStartupTracker() {
	c.startupTrackerMu.Lock()
	defer c.startupTrackerMu.Unlock()

	now := time.Now()
	for key, entry := range c.startupTracker {
		if now.Sub(entry.lastSeen) > startupTrackerTTL {
			delete(c.startupTracker, key)
		}
	}
}

// snapshotStartupLifecycles reconstructs and emits startup lifecycle events from a pod's
// current status. Called during initial cache sync (ADD events) so that lifecycle data
// is captured for containers that started before zxporter was running. The DB upsert
// (ON CONFLICT DO NOTHING) ensures no duplicates if the event-driven path already captured it.
func (c *PodCollector) snapshotStartupLifecycles(pod *corev1.Pod) {
	// Only emit for pods that have started
	if pod.Status.StartTime == nil {
		return
	}

	// Find the Ready condition for ready_at timestamp
	var readyAt *time.Time
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.ContainersReady && cond.Status == corev1.ConditionTrue {
			t := cond.LastTransitionTime.Time
			if !t.IsZero() {
				readyAt = &t
			}
			break
		}
	}

	workloadName, workloadKind := getWorkloadInfo(pod)
	pendingAt := pod.Status.StartTime.Time

	for _, cs := range pod.Status.ContainerStatuses {
		// Only snapshot containers that are running AND ready.
		// Non-ready containers are left for the event-driven path to capture,
		// avoiding partial records that would block the full record via DoNothing upsert.
		if cs.State.Running == nil || !cs.Ready || readyAt == nil {
			continue
		}

		runningAt := cs.State.Running.StartedAt.Time
		if runningAt.IsZero() {
			continue
		}

		entry := &startupLifecycleEntry{
			namespace:     pod.Namespace,
			workloadName:  workloadName,
			workloadKind:  workloadKind,
			podName:       pod.Name,
			containerName: cs.Name,
			restartCount:  cs.RestartCount,
			isRestart:     cs.RestartCount > 0,
			pendingAt:     &pendingAt,
			runningAt:     &runningAt,
		}

		var timeToRunningMs, timeToReadyMs *int64

		ms := runningAt.Sub(pendingAt).Milliseconds()
		if ms > 0 {
			timeToRunningMs = &ms
		}

		ms = readyAt.Sub(pendingAt).Milliseconds()
		if ms > 0 {
			timeToReadyMs = &ms
		}

		c.emitStartupLifecycleEvent(entry, readyAt, timeToRunningMs, timeToReadyMs)
	}
}
