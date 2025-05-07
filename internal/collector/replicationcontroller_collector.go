// internal/collector/replicationcontroller_collector.go
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

// ReplicationControllerCollector watches for replicationcontroller events and collects replicationcontroller data
type ReplicationControllerCollector struct {
	client                         kubernetes.Interface
	informerFactory                informers.SharedInformerFactory
	replicationControllerInformer  cache.SharedIndexInformer
	batchChan                      chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan                   chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                        *ResourcesBatcher
	stopCh                         chan struct{}
	namespaces                     []string
	excludedReplicationControllers map[types.NamespacedName]bool
	logger                         logr.Logger
	mu                             sync.RWMutex
}

// NewReplicationControllerCollector creates a new collector for replicationcontroller resources
func NewReplicationControllerCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedReplicationControllers []ExcludedReplicationController,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ReplicationControllerCollector {
	// Convert excluded replicationcontrollers to a map for quicker lookups
	excludedReplicationControllersMap := make(map[types.NamespacedName]bool)
	for _, rc := range excludedReplicationControllers {
		excludedReplicationControllersMap[types.NamespacedName{
			Namespace: rc.Namespace,
			Name:      rc.Name,
		}] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &ReplicationControllerCollector{
		client:                         client,
		batchChan:                      batchChan,
		resourceChan:                   resourceChan,
		batcher:                        batcher,
		stopCh:                         make(chan struct{}),
		namespaces:                     namespaces,
		excludedReplicationControllers: excludedReplicationControllersMap,
		logger:                         logger.WithName("replicationcontroller-collector"),
	}
}

// Start begins the replicationcontroller collection process
func (c *ReplicationControllerCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting replicationcontroller collector", "namespaces", c.namespaces)

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

	// Create replicationcontroller informer
	c.replicationControllerInformer = c.informerFactory.Core().V1().ReplicationControllers().Informer()

	// Add event handlers
	_, err := c.replicationControllerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rc := obj.(*corev1.ReplicationController)
			c.handleReplicationControllerEvent(rc, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRC := oldObj.(*corev1.ReplicationController)
			newRC := newObj.(*corev1.ReplicationController)

			// Only handle meaningful updates
			if c.replicationControllerChanged(oldRC, newRC) {
				c.handleReplicationControllerEvent(newRC, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			rc := obj.(*corev1.ReplicationController)
			c.handleReplicationControllerEvent(rc, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.replicationControllerInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for ReplicationControllers")
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

// handleReplicationControllerEvent processes replicationcontroller events
func (c *ReplicationControllerCollector) handleReplicationControllerEvent(rc *corev1.ReplicationController, eventType EventType) {
	if c.isExcluded(rc) {
		return
	}

	c.logger.Info("Processing replicationcontroller event",
		"namespace", rc.Namespace,
		"name", rc.Name,
		"eventType", eventType.String())

	// Send the raw replicationcontroller object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ReplicationController,
		Object:       rc, // Send the entire replicationcontroller object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", rc.Namespace, rc.Name),
	}
}

// replicationControllerChanged detects meaningful changes in a replicationcontroller
func (c *ReplicationControllerCollector) replicationControllerChanged(oldRC, newRC *corev1.ReplicationController) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldRC.ResourceVersion == newRC.ResourceVersion {
		return false
	}

	// Check if replicas changed
	if oldRC.Spec.Replicas == nil || newRC.Spec.Replicas == nil {
		return true
	}

	if *oldRC.Spec.Replicas != *newRC.Spec.Replicas {
		return true
	}

	// Check for status changes
	if oldRC.Status.Replicas != newRC.Status.Replicas ||
		oldRC.Status.AvailableReplicas != newRC.Status.AvailableReplicas ||
		oldRC.Status.ReadyReplicas != newRC.Status.ReadyReplicas ||
		oldRC.Status.FullyLabeledReplicas != newRC.Status.FullyLabeledReplicas {
		return true
	}

	// Check for selector changes
	if !mapsEqual(oldRC.Spec.Selector, newRC.Spec.Selector) {
		return true
	}

	// Check for generation changes
	if oldRC.Generation != newRC.Generation {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldRC.Labels, newRC.Labels) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a replicationcontroller should be excluded from collection
func (c *ReplicationControllerCollector) isExcluded(rc *corev1.ReplicationController) bool {
	// Check if monitoring specific namespaces and this replicationcontroller isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == rc.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if replicationcontroller is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: rc.Namespace,
		Name:      rc.Name,
	}
	return c.excludedReplicationControllers[key]
}

// Stop gracefully shuts down the replicationcontroller collector
func (c *ReplicationControllerCollector) Stop() error {
	c.logger.Info("Stopping replicationcontroller collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("ReplicationController collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed replicationcontroller collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed replicationcontroller collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ReplicationController collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ReplicationControllerCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ReplicationControllerCollector) GetType() string {
	return "replication_controller"
}

// IsAvailable checks if ReplicationController resources can be accessed in the cluster
func (c *ReplicationControllerCollector) IsAvailable(ctx context.Context) bool {
	return true
}
