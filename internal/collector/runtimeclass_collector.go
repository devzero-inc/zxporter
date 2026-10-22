// internal/collector/runtimeclass_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// RuntimeClassCollector watches RuntimeClass events and collects RuntimeClass
// data. RuntimeClasses are cluster-scoped (node.k8s.io/v1) and served by every
// cluster, so no discovery check is needed.
type RuntimeClassCollector struct {
	client                 kubernetes.Interface
	informerFactory        informers.SharedInformerFactory
	runtimeClassInformer   cache.SharedIndexInformer
	batchChan              chan CollectedResource   // individual resources -> input to batcher
	resourceChan           chan []CollectedResource // batched resources -> output from batcher
	batcher                *ResourcesBatcher
	stopCh                 chan struct{}
	excludedRuntimeClasses map[string]bool
	logger                 logr.Logger
	telemetryLogger        telemetry_logger.Logger
	mu                     sync.RWMutex
	cDHelper               ChangeDetectionHelper
	// chanMu guards batchChan against the send-after-close shutdown race:
	// informer callbacks RLock around sends, Stop write-locks before closing.
	chanMu  sync.RWMutex
	stopped bool
}

// NewRuntimeClassCollector creates a new collector for RuntimeClass resources
func NewRuntimeClassCollector(
	client kubernetes.Interface,
	excludedRuntimeClasses []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *RuntimeClassCollector {
	// Convert excluded RuntimeClasses to a map for quicker lookups
	excludedRuntimeClassesMap := make(map[string]bool)
	for _, runtimeClass := range excludedRuntimeClasses {
		excludedRuntimeClassesMap[runtimeClass] = true
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

	newLogger := logger.WithName("runtimeclass-collector")
	return &RuntimeClassCollector{
		client:                 client,
		batchChan:              batchChan,
		resourceChan:           resourceChan,
		batcher:                batcher,
		stopCh:                 make(chan struct{}),
		excludedRuntimeClasses: excludedRuntimeClassesMap,
		logger:                 newLogger,
		telemetryLogger:        telemetryLogger,
		cDHelper:               ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the RuntimeClass collection process
func (c *RuntimeClassCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting RuntimeClass collector")

	// Create informer factory - RuntimeClasses are cluster-scoped, not namespaced
	c.informerFactory = newInformerFactory(c.client, nil)

	// Create RuntimeClass informer
	c.runtimeClassInformer = c.informerFactory.Node().V1().RuntimeClasses().Informer()

	// Add event handlers
	_, err := c.runtimeClassInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			runtimeClass := obj.(*nodev1.RuntimeClass)
			c.handleRuntimeClassEvent(runtimeClass, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRuntimeClass := oldObj.(*nodev1.RuntimeClass)
			newRuntimeClass := newObj.(*nodev1.RuntimeClass)

			// Only handle meaningful updates
			if c.runtimeClassChanged(oldRuntimeClass, newRuntimeClass) {
				c.handleRuntimeClassEvent(newRuntimeClass, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			runtimeClass, ok := obj.(*nodev1.RuntimeClass)
			if !ok {
				// A watcher may deliver the final state wrapped in a tombstone;
				// recover the object so its uid still reaches the delete path.
				if tombstone, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
					runtimeClass, ok = tombstone.Obj.(*nodev1.RuntimeClass)
				}
			}
			if ok {
				c.handleRuntimeClassEvent(runtimeClass, EventTypeDelete)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.runtimeClassInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for RuntimeClasses")
	c.batcher.start()

	// Keep this goroutine alive until context cancellation or stop
	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			c.Stop() //nolint:errcheck
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// handleRuntimeClassEvent processes RuntimeClass events
func (c *RuntimeClassCollector) handleRuntimeClassEvent(
	runtimeClass *nodev1.RuntimeClass,
	eventType EventType,
) {
	if c.isExcluded(runtimeClass) {
		return
	}

	// Send under the read lock with an early return once stopped, so a parked
	// informer callback can never send on the closed batch channel.
	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}

	// Send the raw RuntimeClass object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: RuntimeClass,
		Object:       runtimeClass, // Send the entire RuntimeClass object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          runtimeClass.Name, // RuntimeClasses are cluster-scoped, so just the name is sufficient
	}
}

// runtimeClassChanged detects meaningful changes in a RuntimeClass
func (c *RuntimeClassCollector) runtimeClassChanged(oldRuntimeClass, newRuntimeClass *nodev1.RuntimeClass) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldRuntimeClass.Name,
		oldRuntimeClass.ObjectMeta,
		newRuntimeClass.ObjectMeta,
	)
	if changed == PushChanges {
		return true
	}

	// Check for spec changes: handler, overhead and scheduling are top-level
	// fields on node.k8s.io/v1, so the whole object's payload besides
	// ObjectMeta is these three fields.
	if !reflect.DeepEqual(oldRuntimeClass.Handler, newRuntimeClass.Handler) {
		return true
	}
	if !reflect.DeepEqual(oldRuntimeClass.Overhead, newRuntimeClass.Overhead) {
		return true
	}
	if !reflect.DeepEqual(oldRuntimeClass.Scheduling, newRuntimeClass.Scheduling) {
		return true
	}

	if !reflect.DeepEqual(oldRuntimeClass.UID, newRuntimeClass.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a RuntimeClass should be excluded from collection
func (c *RuntimeClassCollector) isExcluded(runtimeClass *nodev1.RuntimeClass) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedRuntimeClasses[runtimeClass.Name]
}

// Stop gracefully shuts down the RuntimeClass collector. The whole body holds
// c.mu's write lock: two callers can reach Stop concurrently (the manager's
// stopCollectorInternal calls Stop after cancelling the context, and Start's
// context watcher calls Stop on that same cancellation), and the select below
// is only idempotent if the check and the close happen under one lock --
// otherwise both callers pass through `default` and the second close panics.
// c.mu is the same lock isExcluded takes for reading; a sender that already
// released it proceeds, and nothing below re-takes it, so no deadlock.
func (c *RuntimeClassCollector) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Info("Stopping RuntimeClass collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("RuntimeClass collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed RuntimeClass collector stop channel")
	}

	// 2. Close the batch input channel under the write lock, so a callback
	// parked on the read lock either completes its send first or observes
	// stopped and returns without sending.
	c.chanMu.Lock()
	if !c.stopped {
		c.stopped = true
		close(c.batchChan)
	}
	c.chanMu.Unlock()

	// 3. Stop the batcher (waits for completion). resourceChan is closed by
	// the batcher's defer func.
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("RuntimeClass collector batcher stopped")
	}

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *RuntimeClassCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *RuntimeClassCollector) GetType() string {
	return RuntimeClass.String()
}

// IsAvailable checks if RuntimeClass resources are available in the cluster.
// RuntimeClass is a built-in node.k8s.io/v1 kind served by every cluster.
func (c *RuntimeClassCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a RuntimeClass resource to be processed by the collector
func (c *RuntimeClassCollector) AddResource(resource interface{}) error {
	runtimeClass, ok := resource.(*nodev1.RuntimeClass)
	if !ok {
		return fmt.Errorf("expected *nodev1.RuntimeClass, got %T", resource)
	}

	c.handleRuntimeClassEvent(runtimeClass, EventTypeAdd)
	return nil
}
