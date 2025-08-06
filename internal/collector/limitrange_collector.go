// internal/collector/limitrange_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// LimitRangeCollector watches for limitrange events and collects limitrange data
type LimitRangeCollector struct {
	client              kubernetes.Interface
	informerFactory     informers.SharedInformerFactory
	limitRangeInformer  cache.SharedIndexInformer
	batchChan           chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan        chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher             *ResourcesBatcher
	stopCh              chan struct{}
	namespaces          []string
	excludedLimitRanges map[types.NamespacedName]bool
	logger              logr.Logger
	mu                  sync.RWMutex
	cDHelper            ChangeDetectionHelper
}

// NewLimitRangeCollector creates a new collector for limitrange resources
func NewLimitRangeCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedLimitRanges []ExcludedLimitRange,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *LimitRangeCollector {
	// Convert excluded limitranges to a map for quicker lookups
	excludedLimitRangesMap := make(map[types.NamespacedName]bool)
	for _, lr := range excludedLimitRanges {
		excludedLimitRangesMap[types.NamespacedName{
			Namespace: lr.Namespace,
			Name:      lr.Name,
		}] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 50) // Keep lower buffer for infrequent LimitRanges
	resourceChan := make(chan []CollectedResource, 50)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	newLogger := logger.WithName("limitrange-collector")
	return &LimitRangeCollector{
		client:              client,
		batchChan:           batchChan,
		resourceChan:        resourceChan,
		batcher:             batcher,
		stopCh:              make(chan struct{}),
		namespaces:          namespaces,
		excludedLimitRanges: excludedLimitRangesMap,
		logger:              newLogger,
		cDHelper:            ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the limitrange collection process
func (c *LimitRangeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting limitrange collector", "namespaces", c.namespaces)

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

	// Create limitrange informer
	c.limitRangeInformer = c.informerFactory.Core().V1().LimitRanges().Informer()

	// Add event handlers
	_, err := c.limitRangeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			lr := obj.(*corev1.LimitRange)
			c.handleLimitRangeEvent(lr, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldLR := oldObj.(*corev1.LimitRange)
			newLR := newObj.(*corev1.LimitRange)

			// Only handle meaningful updates
			if c.limitRangeChanged(oldLR, newLR) {
				c.handleLimitRangeEvent(newLR, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			lr := obj.(*corev1.LimitRange)
			c.handleLimitRangeEvent(lr, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.limitRangeInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for LimitRanges")
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

// handleLimitRangeEvent processes limitrange events
func (c *LimitRangeCollector) handleLimitRangeEvent(lr *corev1.LimitRange, eventType EventType) {
	if c.isExcluded(lr) {
		return
	}

	c.logger.Info("Processing limitrange event",
		"namespace", lr.Namespace,
		"name", lr.Name,
		"eventType", eventType.String())

	// Send the raw limitrange object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: LimitRange,
		Object:       lr, // Send the entire limitrange object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", lr.Namespace, lr.Name),
	}
}

// limitRangeChanged detects meaningful changes in a limitrange
func (c *LimitRangeCollector) limitRangeChanged(oldLR, newLR *corev1.LimitRange) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldLR.Name,
		oldLR.ObjectMeta,
		newLR.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for limit changes
	if !reflect.DeepEqual(oldLR.Spec.Limits, newLR.Spec.Limits) {
		return true
	}

	if !reflect.DeepEqual(oldLR.UID, newLR.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a limitrange should be excluded from collection
func (c *LimitRangeCollector) isExcluded(lr *corev1.LimitRange) bool {
	// Check if monitoring specific namespaces and this limitrange isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == lr.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if limitrange is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: lr.Namespace,
		Name:      lr.Name,
	}
	return c.excludedLimitRanges[key]
}

// Stop gracefully shuts down the limitrange collector
func (c *LimitRangeCollector) Stop() error {
	c.logger.Info("Stopping limitrange collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("LimitRange collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed limitrange collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed limitrange collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("LimitRange collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *LimitRangeCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *LimitRangeCollector) GetType() string {
	return "limit_range"
}

// IsAvailable checks if LimitRange resources can be accessed in the cluster
func (c *LimitRangeCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a limitrange resource to be processed by the collector
func (c *LimitRangeCollector) AddResource(resource interface{}) error {
	lr, ok := resource.(*corev1.LimitRange)
	if !ok {
		return fmt.Errorf("expected *corev1.LimitRange, got %T", resource)
	}

	c.handleLimitRangeEvent(lr, EventTypeAdd)
	return nil
}
