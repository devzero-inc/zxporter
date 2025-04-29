// internal/collector/daemonset_collector.go
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

// DaemonSetCollector watches for daemonset events and collects daemonset data
type DaemonSetCollector struct {
	client             kubernetes.Interface
	informerFactory    informers.SharedInformerFactory
	daemonSetInformer  cache.SharedIndexInformer
	batchChan          chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan       chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher            *ResourcesBatcher
	stopCh             chan struct{}
	namespaces         []string
	excludedDaemonSets map[types.NamespacedName]bool
	logger             logr.Logger
	mu                 sync.RWMutex
}

// ExcludedDaemonSet defines a daemonset to exclude from collection
type ExcludedDaemonSet struct {
	Namespace string
	Name      string
}

// NewDaemonSetCollector creates a new collector for daemonset resources
func NewDaemonSetCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedDaemonSets []ExcludedDaemonSet,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *DaemonSetCollector {
	// Convert excluded daemonsets to a map for quicker lookups
	excludedDaemonSetsMap := make(map[types.NamespacedName]bool)
	for _, daemonset := range excludedDaemonSets {
		excludedDaemonSetsMap[types.NamespacedName{
			Namespace: daemonset.Namespace,
			Name:      daemonset.Name,
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

	return &DaemonSetCollector{
		client:             client,
		batchChan:          batchChan,
		resourceChan:       resourceChan,
		batcher:            batcher,
		stopCh:             make(chan struct{}),
		namespaces:         namespaces,
		excludedDaemonSets: excludedDaemonSetsMap,
		logger:             logger.WithName("daemonset-collector"),
	}
}

// Start begins the daemonset collection process
func (c *DaemonSetCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting daemonset collector", "namespaces", c.namespaces)

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

	// Create daemonset informer
	c.daemonSetInformer = c.informerFactory.Apps().V1().DaemonSets().Informer()

	// Add event handlers
	_, err := c.daemonSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			daemonset := obj.(*appsv1.DaemonSet)
			c.handleDaemonSetEvent(daemonset, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDaemonSet := oldObj.(*appsv1.DaemonSet)
			newDaemonSet := newObj.(*appsv1.DaemonSet)

			// Only handle meaningful updates
			if c.daemonSetChanged(oldDaemonSet, newDaemonSet) {
				c.handleDaemonSetEvent(newDaemonSet, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			daemonset := obj.(*appsv1.DaemonSet)
			c.handleDaemonSetEvent(daemonset, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.daemonSetInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for daemonsets")
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

// handleDaemonSetEvent processes daemonset events
func (c *DaemonSetCollector) handleDaemonSetEvent(daemonset *appsv1.DaemonSet, eventType EventType) {
	if c.isExcluded(daemonset) {
		return
	}

	c.logger.Info("Processing daemonset event",
		"namespace", daemonset.Namespace,
		"name", daemonset.Name,
		"eventType", eventType.String())

	// Send the raw daemonset object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: DaemonSet,
		Object:       daemonset, // Send the entire daemonset object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", daemonset.Namespace, daemonset.Name),
	}
}

// daemonSetChanged detects meaningful changes in a daemonset
func (c *DaemonSetCollector) daemonSetChanged(oldDaemonSet, newDaemonSet *appsv1.DaemonSet) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldDaemonSet.ResourceVersion == newDaemonSet.ResourceVersion {
		return false
	}

	// Check for status changes
	if oldDaemonSet.Status.CurrentNumberScheduled != newDaemonSet.Status.CurrentNumberScheduled ||
		oldDaemonSet.Status.DesiredNumberScheduled != newDaemonSet.Status.DesiredNumberScheduled ||
		oldDaemonSet.Status.NumberAvailable != newDaemonSet.Status.NumberAvailable ||
		oldDaemonSet.Status.NumberMisscheduled != newDaemonSet.Status.NumberMisscheduled ||
		oldDaemonSet.Status.NumberReady != newDaemonSet.Status.NumberReady ||
		oldDaemonSet.Status.NumberUnavailable != newDaemonSet.Status.NumberUnavailable ||
		oldDaemonSet.Status.UpdatedNumberScheduled != newDaemonSet.Status.UpdatedNumberScheduled {
		return true
	}

	// Check for generation changes
	if oldDaemonSet.Generation != newDaemonSet.Generation ||
		oldDaemonSet.Status.ObservedGeneration != newDaemonSet.Status.ObservedGeneration {
		return true
	}

	// Check for update strategy changes
	if oldDaemonSet.Spec.UpdateStrategy.Type != newDaemonSet.Spec.UpdateStrategy.Type {
		return true
	}

	// Check for changes in conditions
	if len(oldDaemonSet.Status.Conditions) != len(newDaemonSet.Status.Conditions) {
		return true
	}

	// Deep check on conditions
	if len(newDaemonSet.Status.Conditions) > 0 {
		oldConditions := make(map[string]appsv1.DaemonSetCondition)
		for _, condition := range oldDaemonSet.Status.Conditions {
			oldConditions[string(condition.Type)] = condition
		}

		for _, newCondition := range newDaemonSet.Status.Conditions {
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

// isExcluded checks if a daemonset should be excluded from collection
func (c *DaemonSetCollector) isExcluded(daemonset *appsv1.DaemonSet) bool {
	// Check if monitoring specific namespaces and this daemonset isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == daemonset.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if daemonset is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: daemonset.Namespace,
		Name:      daemonset.Name,
	}
	return c.excludedDaemonSets[key]
}

// Stop gracefully shuts down the daemonset collector
func (c *DaemonSetCollector) Stop() error {
	c.logger.Info("Stopping daemonset collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("DaemonSet collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed daemonset collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed daemonset collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("DaemonSet collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *DaemonSetCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *DaemonSetCollector) GetType() string {
	return "daemonset"
}

// IsAvailable checks if DaemonSet resources can be accessed in the cluster
func (c *DaemonSetCollector) IsAvailable(ctx context.Context) bool {
	return true
}
