// internal/collector/statefulset_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// StatefulSetCollector watches for statefulset events and collects statefulset data
type StatefulSetCollector struct {
	client               kubernetes.Interface
	informerFactory      informers.SharedInformerFactory
	statefulSetInformer  cache.SharedIndexInformer
	batchChan            chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan         chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher              *ResourcesBatcher
	stopCh               chan struct{}
	namespaces           []string
	excludedStatefulSets map[types.NamespacedName]bool
	logger               logr.Logger
	mu                   sync.RWMutex
}

// NewStatefulSetCollector creates a new collector for statefulset resources
func NewStatefulSetCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedStatefulSets []ExcludedStatefulSet,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *StatefulSetCollector {
	// Convert excluded statefulsets to a map for quicker lookups
	excludedStatefulSetsMap := make(map[types.NamespacedName]bool)
	for _, statefulset := range excludedStatefulSets {
		excludedStatefulSetsMap[types.NamespacedName{
			Namespace: statefulset.Namespace,
			Name:      statefulset.Name,
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

	return &StatefulSetCollector{
		client:               client,
		batchChan:            batchChan,
		resourceChan:         resourceChan,
		batcher:              batcher,
		stopCh:               make(chan struct{}),
		namespaces:           namespaces,
		excludedStatefulSets: excludedStatefulSetsMap,
		logger:               logger.WithName("statefulset-collector"),
	}
}

// Start begins the statefulset collection process
func (c *StatefulSetCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting statefulset collector", "namespaces", c.namespaces)

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

	// Create statefulset informer
	c.statefulSetInformer = c.informerFactory.Apps().V1().StatefulSets().Informer()

	// Add event handlers
	_, err := c.statefulSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			statefulset := obj.(*appsv1.StatefulSet)
			c.handleStatefulSetEvent(statefulset, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldStatefulSet := oldObj.(*appsv1.StatefulSet)
			newStatefulSet := newObj.(*appsv1.StatefulSet)

			// Only handle meaningful updates
			if c.statefulSetChanged(oldStatefulSet, newStatefulSet) {
				c.handleStatefulSetEvent(newStatefulSet, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			statefulset := obj.(*appsv1.StatefulSet)
			c.handleStatefulSetEvent(statefulset, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.statefulSetInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for statefulsets")
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

// handleStatefulSetEvent processes statefulset events
func (c *StatefulSetCollector) handleStatefulSetEvent(statefulset *appsv1.StatefulSet, eventType EventType) {
	if c.isExcluded(statefulset) {
		return
	}

	c.logger.Info("Processing statefulset event",
		"namespace", statefulset.Namespace,
		"name", statefulset.Name,
		"eventType", eventType.String())

	// Send the raw statefulset object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: StatefulSet,
		Object:       statefulset, // Send the entire statefulset object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", statefulset.Namespace, statefulset.Name),
	}
}

// statefulSetChanged detects meaningful changes in a statefulset
func (c *StatefulSetCollector) statefulSetChanged(oldStatefulSet, newStatefulSet *appsv1.StatefulSet) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldStatefulSet.ResourceVersion == newStatefulSet.ResourceVersion {
		return false
	}

	// Check for spec changes
	if oldStatefulSet.Spec.Replicas == nil || newStatefulSet.Spec.Replicas == nil {
		return true
	}

	if *oldStatefulSet.Spec.Replicas != *newStatefulSet.Spec.Replicas {
		return true
	}

	// Check for status changes
	if oldStatefulSet.Status.Replicas != newStatefulSet.Status.Replicas ||
		oldStatefulSet.Status.AvailableReplicas != newStatefulSet.Status.AvailableReplicas ||
		oldStatefulSet.Status.ReadyReplicas != newStatefulSet.Status.ReadyReplicas ||
		oldStatefulSet.Status.UpdatedReplicas != newStatefulSet.Status.UpdatedReplicas ||
		oldStatefulSet.Status.CurrentReplicas != newStatefulSet.Status.CurrentReplicas {
		return true
	}

	// Check for generation changes
	if oldStatefulSet.Generation != newStatefulSet.Generation ||
		oldStatefulSet.Status.ObservedGeneration != newStatefulSet.Status.ObservedGeneration {
		return true
	}

	// Check for changes in update strategy
	if oldStatefulSet.Spec.UpdateStrategy.Type != newStatefulSet.Spec.UpdateStrategy.Type {
		return true
	}

	// Check for changes in conditions
	if len(oldStatefulSet.Status.Conditions) != len(newStatefulSet.Status.Conditions) {
		return true
	}

	// Deep check on conditions
	if len(newStatefulSet.Status.Conditions) > 0 {
		oldConditions := make(map[string]appsv1.StatefulSetCondition)
		for _, condition := range oldStatefulSet.Status.Conditions {
			oldConditions[string(condition.Type)] = condition
		}

		for _, newCondition := range newStatefulSet.Status.Conditions {
			oldCondition, exists := oldConditions[string(newCondition.Type)]
			if !exists || oldCondition.Status != newCondition.Status ||
				oldCondition.Reason != newCondition.Reason ||
				oldCondition.Message != newCondition.Message {
				return true
			}
		}
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a statefulset should be excluded from collection
func (c *StatefulSetCollector) isExcluded(statefulset *appsv1.StatefulSet) bool {
	// Check if monitoring specific namespaces and this statefulset isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == statefulset.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if statefulset is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: statefulset.Namespace,
		Name:      statefulset.Name,
	}
	return c.excludedStatefulSets[key]
}

// Stop gracefully shuts down the statefulset collector
func (c *StatefulSetCollector) Stop() error {
	c.logger.Info("Stopping statefulset collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("StatefulSet collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed statefulset collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed statefulset collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("StatefulSet collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *StatefulSetCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *StatefulSetCollector) GetType() string {
	return "statefulset"
}

// IsAvailable checks if StatefulSet resources can be accessed in the cluster
func (c *StatefulSetCollector) IsAvailable(ctx context.Context) bool {
	return true
}
