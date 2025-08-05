package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// CSIStorageCapacityCollector watches for CSIStorageCapacity events and collects CSIStorageCapacity data
type CSIStorageCapacityCollector struct {
	client                       kubernetes.Interface
	informerFactory              informers.SharedInformerFactory
	csiStorageCapacityInformer   cache.SharedIndexInformer
	batchChan                    chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan                 chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                      *ResourcesBatcher
	stopCh                       chan struct{}
	namespaces                   []string
	excludedCSIStorageCapacities map[types.NamespacedName]bool
	logger                       logr.Logger
	mu                           sync.RWMutex
	cDHelper                     ChangeDetectionHelper
}

// NewCSIStorageCapacityCollector creates a new collector for CSIStorageCapacity resources
func NewCSIStorageCapacityCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedCSIStorageCapacities []ExcludedCSIStorageCapacity,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *CSIStorageCapacityCollector {
	// Convert excluded CSIStorageCapacities to a map for quicker lookups
	excludedCSIStorageCapacitiesMap := make(map[types.NamespacedName]bool)
	for _, csc := range excludedCSIStorageCapacities {
		excludedCSIStorageCapacitiesMap[types.NamespacedName{
			Namespace: csc.Namespace,
			Name:      csc.Name,
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

	newLogger := logger.WithName("csistoragecapacity-collector")
	return &CSIStorageCapacityCollector{
		client:                       client,
		batchChan:                    batchChan,
		resourceChan:                 resourceChan,
		batcher:                      batcher,
		stopCh:                       make(chan struct{}),
		namespaces:                   namespaces,
		excludedCSIStorageCapacities: excludedCSIStorageCapacitiesMap,
		logger:                       newLogger,
		cDHelper:                     ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the CSIStorageCapacity collection process
func (c *CSIStorageCapacityCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CSIStorageCapacity collector", "namespaces", c.namespaces)

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

	// Create CSIStorageCapacity informer
	c.csiStorageCapacityInformer = c.informerFactory.Storage().V1().CSIStorageCapacities().Informer()

	// Add event handlers
	_, err := c.csiStorageCapacityInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			csc := obj.(*storagev1.CSIStorageCapacity)
			c.handleCSIStorageCapacityEvent(csc, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCSC := oldObj.(*storagev1.CSIStorageCapacity)
			newCSC := newObj.(*storagev1.CSIStorageCapacity)

			// Only handle meaningful updates
			if c.csiStorageCapacityChanged(oldCSC, newCSC) {
				c.handleCSIStorageCapacityEvent(newCSC, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			csc := obj.(*storagev1.CSIStorageCapacity)
			c.handleCSIStorageCapacityEvent(csc, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.csiStorageCapacityInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for CSIStorageCapacities")
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

// handleCSIStorageCapacityEvent processes CSIStorageCapacity events
func (c *CSIStorageCapacityCollector) handleCSIStorageCapacityEvent(csc *storagev1.CSIStorageCapacity, eventType EventType) {
	if c.isExcluded(csc) {
		return
	}

	c.logger.Info("Processing CSIStorageCapacity event",
		"namespace", csc.Namespace,
		"name", csc.Name,
		"eventType", eventType.String())

	// Send the raw CSIStorageCapacity object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: CSIStorageCapacity,
		Object:       csc, // Send the entire CSIStorageCapacity object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", csc.Namespace, csc.Name),
	}
}

// csiStorageCapacityChanged detects meaningful changes in a CSIStorageCapacity
func (c *CSIStorageCapacityCollector) csiStorageCapacityChanged(oldCSC, newCSC *storagev1.CSIStorageCapacity) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldCSC.Name,
		oldCSC.ObjectMeta,
		newCSC.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for capacity changes
	if oldCSC.Capacity != nil && newCSC.Capacity != nil {
		if oldCSC.Capacity.Cmp(*newCSC.Capacity) != 0 {
			return true
		}
	} else if oldCSC.Capacity != newCSC.Capacity {
		return true
	}

	// Check for maximum volume size changes
	if oldCSC.MaximumVolumeSize != nil && newCSC.MaximumVolumeSize != nil {
		if oldCSC.MaximumVolumeSize.Cmp(*newCSC.MaximumVolumeSize) != 0 {
			return true
		}
	} else if oldCSC.MaximumVolumeSize != newCSC.MaximumVolumeSize {
		return true
	}

	// Check for storage class parameters changes
	if !reflect.DeepEqual(oldCSC.NodeTopology, newCSC.NodeTopology) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a CSIStorageCapacity should be excluded from collection
func (c *CSIStorageCapacityCollector) isExcluded(csc *storagev1.CSIStorageCapacity) bool {
	// Check if monitoring specific namespaces and this csc isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == csc.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if csc is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: csc.Namespace,
		Name:      csc.Name,
	}
	return c.excludedCSIStorageCapacities[key]
}

// Stop gracefully shuts down the CSIStorageCapacity collector
func (c *CSIStorageCapacityCollector) Stop() error {
	c.logger.Info("Stopping CSIStorageCapacity collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("CSIStorageCapacity collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed CSIStorageCapacity collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed CSIStorageCapacity collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("CSIStorageCapacity collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *CSIStorageCapacityCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CSIStorageCapacityCollector) GetType() string {
	return "csi_storage_capacity"
}

// IsAvailable checks if CSIStorageCapacity resources can be accessed in the cluster
func (c *CSIStorageCapacityCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a CSI storage capacity resource to be processed by the collector
func (c *CSIStorageCapacityCollector) AddResource(resource interface{}) error {
	csc, ok := resource.(*storagev1.CSIStorageCapacity)
	if !ok {
		return fmt.Errorf("expected *storagev1.CSIStorageCapacity, got %T", resource)
	}

	c.handleCSIStorageCapacityEvent(csc, EventTypeAdd)
	return nil
}

// ExcludedCSIStorageCapacity defines a CSIStorageCapacity to be excluded from collection
type ExcludedCSIStorageCapacity struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
}
