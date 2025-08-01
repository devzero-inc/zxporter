package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// CSIDriverCollector watches for CSIDriver events and collects CSIDriver data
type CSIDriverCollector struct {
	client             kubernetes.Interface
	informerFactory    informers.SharedInformerFactory
	csiDriverInformer  cache.SharedIndexInformer
	batchChan          chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan       chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher            *ResourcesBatcher
	stopCh             chan struct{}
	excludedCSIDrivers map[string]bool
	logger             logr.Logger
	telemetryLogger    telemetry_logger.Logger
	mu                 sync.RWMutex
	cDHelper           ChangeDetectionHelper
}

// NewCSIDriverCollector creates a new collector for CSIDriver resources
func NewCSIDriverCollector(
	client kubernetes.Interface,
	excludedCSIDrivers []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *CSIDriverCollector {
	// Convert excluded CSIDrivers to a map for quicker lookups
	excludedCSIDriversMap := make(map[string]bool)
	for _, driver := range excludedCSIDrivers {
		excludedCSIDriversMap[driver] = true
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

	newLogger := logger.WithName("csidriver-collector")
	return &CSIDriverCollector{
		client:             client,
		batchChan:          batchChan,
		resourceChan:       resourceChan,
		batcher:            batcher,
		stopCh:             make(chan struct{}),
		excludedCSIDrivers: excludedCSIDriversMap,
		logger:             newLogger,
		telemetryLogger:    telemetryLogger,
		cDHelper:           ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the CSIDriver collection process
func (c *CSIDriverCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CSIDriver collector")

	// Create informer factory - CSIDrivers are cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create CSIDriver informer
	c.csiDriverInformer = c.informerFactory.Storage().V1().CSIDrivers().Informer()

	// Add event handlers
	_, err := c.csiDriverInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			driver := obj.(*storagev1.CSIDriver)
			c.handleCSIDriverEvent(driver, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDriver := oldObj.(*storagev1.CSIDriver)
			newDriver := newObj.(*storagev1.CSIDriver)

			// Only handle meaningful updates
			if c.csiDriverChanged(oldDriver, newDriver) {
				c.handleCSIDriverEvent(newDriver, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			driver := obj.(*storagev1.CSIDriver)
			c.handleCSIDriverEvent(driver, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.csiDriverInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for CSIDrivers")
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

// handleCSIDriverEvent processes CSIDriver events
func (c *CSIDriverCollector) handleCSIDriverEvent(driver *storagev1.CSIDriver, eventType EventType) {
	if c.isExcluded(driver) {
		return
	}

	c.logger.Info("Processing CSIDriver event",
		"name", driver.Name,
		"eventType", eventType.String())

	// Send the raw CSIDriver object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: CSIDriver,
		Object:       driver, // Send the entire CSIDriver object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          driver.Name, // CSIDrivers are cluster-scoped, so just the name is sufficient
	}
}

// csiDriverChanged detects meaningful changes in a CSIDriver
func (c *CSIDriverCollector) csiDriverChanged(oldDriver, newDriver *storagev1.CSIDriver) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldDriver.Name,
		oldDriver.ObjectMeta,
		newDriver.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for spec changes
	if !reflect.DeepEqual(oldDriver.Spec, newDriver.Spec) {
		return true
	}

	if !reflect.DeepEqual(oldDriver.UID, newDriver.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a CSIDriver should be excluded from collection
func (c *CSIDriverCollector) isExcluded(driver *storagev1.CSIDriver) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedCSIDrivers[driver.Name]
}

// Stop gracefully shuts down the CSIDriver collector
func (c *CSIDriverCollector) Stop() error {
	c.logger.Info("Stopping CSIDriver collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("CSIDriver collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed CSIDriver collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed CSIDriver collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("CSIDriver collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *CSIDriverCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CSIDriverCollector) GetType() string {
	return "csi_driver"
}

// IsAvailable checks if CSIDriver resources are available in the cluster
func (c *CSIDriverCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a CSIDriver resource to be processed by the collector
func (c *CSIDriverCollector) AddResource(resource interface{}) error {
	csiDriver, ok := resource.(*storagev1.CSIDriver)
	if !ok {
		return fmt.Errorf("expected *storagev1.CSIDriver, got %T", resource)
	}

	c.handleCSIDriverEvent(csiDriver, EventTypeAdd)
	return nil
}
