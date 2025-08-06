// internal/collector/replicaset_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ReplicaSetCollector watches for replicaset events and collects replicaset data
type ReplicaSetCollector struct {
	client              kubernetes.Interface
	informerFactory     informers.SharedInformerFactory
	replicaSetInformer  cache.SharedIndexInformer
	batchChan           chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan        chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher             *ResourcesBatcher
	stopCh              chan struct{}
	namespaces          []string
	excludedReplicaSets map[types.NamespacedName]bool
	logger              logr.Logger
	mu                  sync.RWMutex
	cDHelper            ChangeDetectionHelper
}

// NewReplicaSetCollector creates a new collector for replicaset resources
func NewReplicaSetCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedReplicaSets []ExcludedReplicaSet,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ReplicaSetCollector {
	// Convert excluded replicasets to a map for quicker lookups
	excludedReplicaSetsMap := make(map[types.NamespacedName]bool)
	for _, replicaset := range excludedReplicaSets {
		excludedReplicaSetsMap[types.NamespacedName{
			Namespace: replicaset.Namespace,
			Name:      replicaset.Name,
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

	newLogger := logger.WithName("replicaset-collector")
	return &ReplicaSetCollector{
		client:              client,
		batchChan:           batchChan,
		resourceChan:        resourceChan,
		batcher:             batcher,
		stopCh:              make(chan struct{}),
		namespaces:          namespaces,
		excludedReplicaSets: excludedReplicaSetsMap,
		logger:              newLogger,
		cDHelper:            ChangeDetectionHelper{logger: newLogger}}
}

// Start begins the replicaset collection process
func (c *ReplicaSetCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting replicaset collector", "namespaces", c.namespaces)

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

	// Create replicaset informer
	c.replicaSetInformer = c.informerFactory.Apps().V1().ReplicaSets().Informer()

	// Add event handlers
	_, err := c.replicaSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			replicaset := obj.(*appsv1.ReplicaSet)
			c.handleReplicaSetEvent(replicaset, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldReplicaSet := oldObj.(*appsv1.ReplicaSet)
			newReplicaSet := newObj.(*appsv1.ReplicaSet)

			// Only handle meaningful updates
			if c.replicaSetChanged(oldReplicaSet, newReplicaSet) {
				c.handleReplicaSetEvent(newReplicaSet, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			replicaset := obj.(*appsv1.ReplicaSet)
			c.handleReplicaSetEvent(replicaset, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.replicaSetInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for replicasets")
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

// handleReplicaSetEvent processes replicaset events
func (c *ReplicaSetCollector) handleReplicaSetEvent(replicaset *appsv1.ReplicaSet, eventType EventType) {
	if c.isExcluded(replicaset) {
		return
	}

	c.logger.Info("Processing replicaset event",
		"namespace", replicaset.Namespace,
		"name", replicaset.Name,
		"eventType", eventType.String())

	// Send the raw replicaset object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ReplicaSet,
		Object:       replicaset, // Send the entire replicaset object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", replicaset.Namespace, replicaset.Name),
	}
}

// replicaSetChanged detects meaningful changes in a replicaset
func (c *ReplicaSetCollector) replicaSetChanged(oldReplicaSet, newReplicaSet *appsv1.ReplicaSet) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldReplicaSet.Name,
		oldReplicaSet.ObjectMeta,
		newReplicaSet.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check if replicas changed
	if oldReplicaSet.Spec.Replicas == nil || newReplicaSet.Spec.Replicas == nil {
		return true
	}

	if *oldReplicaSet.Spec.Replicas != *newReplicaSet.Spec.Replicas {
		return true
	}

	// Check for status changes
	if oldReplicaSet.Status.Replicas != newReplicaSet.Status.Replicas ||
		oldReplicaSet.Status.AvailableReplicas != newReplicaSet.Status.AvailableReplicas ||
		oldReplicaSet.Status.ReadyReplicas != newReplicaSet.Status.ReadyReplicas ||
		oldReplicaSet.Status.FullyLabeledReplicas != newReplicaSet.Status.FullyLabeledReplicas {
		return true
	}

	// Check for generation changes
	if oldReplicaSet.Generation != newReplicaSet.Generation ||
		oldReplicaSet.Status.ObservedGeneration != newReplicaSet.Status.ObservedGeneration {
		return true
	}

	// Check for owner reference changes (could indicate adoption or orphaning)
	if len(oldReplicaSet.OwnerReferences) != len(newReplicaSet.OwnerReferences) {
		return true
	}

	// Deep check on owner references
	for i, oldRef := range oldReplicaSet.OwnerReferences {
		if i >= len(newReplicaSet.OwnerReferences) {
			return true
		}

		newRef := newReplicaSet.OwnerReferences[i]
		if oldRef.Kind != newRef.Kind ||
			oldRef.Name != newRef.Name ||
			oldRef.UID != newRef.UID ||
			oldRef.Controller != newRef.Controller {
			return true
		}
	}

	// Check for selector changes
	if !metaLabelsEqual(oldReplicaSet.Spec.Selector, newReplicaSet.Spec.Selector) {
		return true
	}

	if !reflect.DeepEqual(oldReplicaSet.UID, newReplicaSet.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a replicaset should be excluded from collection
func (c *ReplicaSetCollector) isExcluded(replicaset *appsv1.ReplicaSet) bool {
	// Check if monitoring specific namespaces and this replicaset isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == replicaset.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if replicaset is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: replicaset.Namespace,
		Name:      replicaset.Name,
	}
	return c.excludedReplicaSets[key]
}

// Stop gracefully shuts down the replicaset collector
func (c *ReplicaSetCollector) Stop() error {
	c.logger.Info("Stopping replicaset collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("ReplicaSet collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed replicaset collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed replicaset collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ReplicaSet collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ReplicaSetCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ReplicaSetCollector) GetType() string {
	return "replica_set"
}

// IsAvailable checks if ReplicaSet resources can be accessed in the cluster
func (c *ReplicaSetCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a ReplicaSet resource to be processed by the collector
func (c *ReplicaSetCollector) AddResource(resource interface{}) error {
	replicaSet, ok := resource.(*appsv1.ReplicaSet)
	if !ok {
		return fmt.Errorf("expected *appsv1.ReplicaSet, got %T", resource)
	}

	c.handleReplicaSetEvent(replicaSet, EventTypeAdd)
	return nil
}
