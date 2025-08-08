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

// VolumeAttachmentCollector watches for VolumeAttachment events and collects VolumeAttachment data
type VolumeAttachmentCollector struct {
	client                    kubernetes.Interface
	informerFactory           informers.SharedInformerFactory
	volumeAttachmentInformer  cache.SharedIndexInformer
	batchChan                 chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan              chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                   *ResourcesBatcher
	stopCh                    chan struct{}
	excludedVolumeAttachments map[string]bool
	logger                    logr.Logger
	telemetryLogger           telemetry_logger.Logger
	mu                        sync.RWMutex
	cDHelper                  ChangeDetectionHelper
}

// NewVolumeAttachmentCollector creates a new collector for VolumeAttachment resources
func NewVolumeAttachmentCollector(
	client kubernetes.Interface,
	excludedVolumeAttachments []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *VolumeAttachmentCollector {
	// Convert excluded VolumeAttachments to a map for quicker lookups
	excludedVolumeAttachmentsMap := make(map[string]bool)
	for _, va := range excludedVolumeAttachments {
		excludedVolumeAttachmentsMap[va] = true
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

	newLogger := logger.WithName("volumeattachment-collector")
	return &VolumeAttachmentCollector{
		client:                    client,
		batchChan:                 batchChan,
		resourceChan:              resourceChan,
		batcher:                   batcher,
		stopCh:                    make(chan struct{}),
		excludedVolumeAttachments: excludedVolumeAttachmentsMap,
		logger:                    newLogger,
		telemetryLogger:           telemetryLogger,
		cDHelper:                  ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the VolumeAttachment collection process
func (c *VolumeAttachmentCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting VolumeAttachment collector")

	// Create informer factory - VolumeAttachments are cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create VolumeAttachment informer
	c.volumeAttachmentInformer = c.informerFactory.Storage().V1().VolumeAttachments().Informer()

	// Add event handlers
	_, err := c.volumeAttachmentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			va := obj.(*storagev1.VolumeAttachment)
			c.handleVolumeAttachmentEvent(va, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldVA := oldObj.(*storagev1.VolumeAttachment)
			newVA := newObj.(*storagev1.VolumeAttachment)

			// Only handle meaningful updates
			if c.volumeAttachmentChanged(oldVA, newVA) {
				c.handleVolumeAttachmentEvent(newVA, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			va := obj.(*storagev1.VolumeAttachment)
			c.handleVolumeAttachmentEvent(va, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.volumeAttachmentInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for VolumeAttachments")
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

// handleVolumeAttachmentEvent processes VolumeAttachment events
func (c *VolumeAttachmentCollector) handleVolumeAttachmentEvent(va *storagev1.VolumeAttachment, eventType EventType) {
	if c.isExcluded(va) {
		return
	}

	// Send the raw VolumeAttachment object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: VolumeAttachment,
		Object:       va, // Send the entire VolumeAttachment object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          va.Name, // VolumeAttachments are cluster-scoped, so just the name is sufficient
	}
}

// volumeAttachmentChanged detects meaningful changes in a VolumeAttachment
func (c *VolumeAttachmentCollector) volumeAttachmentChanged(oldVA, newVA *storagev1.VolumeAttachment) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldVA.Name,
		oldVA.ObjectMeta,
		newVA.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for spec changes
	if !reflect.DeepEqual(oldVA.Spec, newVA.Spec) {
		return true
	}

	// Check for status changes
	if !reflect.DeepEqual(oldVA.Status, newVA.Status) {
		return true
	}

	if !reflect.DeepEqual(oldVA.UID, newVA.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a VolumeAttachment should be excluded from collection
func (c *VolumeAttachmentCollector) isExcluded(va *storagev1.VolumeAttachment) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedVolumeAttachments[va.Name]
}

// Stop gracefully shuts down the VolumeAttachment collector
func (c *VolumeAttachmentCollector) Stop() error {
	c.logger.Info("Stopping VolumeAttachment collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("VolumeAttachment collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed VolumeAttachment collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed VolumeAttachment collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("VolumeAttachment collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *VolumeAttachmentCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *VolumeAttachmentCollector) GetType() string {
	return "volume_attachment"
}

// IsAvailable checks if VolumeAttachment resources are available in the cluster
func (c *VolumeAttachmentCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a VolumeAttachment resource to be processed by the collector
func (c *VolumeAttachmentCollector) AddResource(resource interface{}) error {
	volumeAttachment, ok := resource.(*storagev1.VolumeAttachment)
	if !ok {
		return fmt.Errorf("expected *storagev1.VolumeAttachment, got %T", resource)
	}

	c.handleVolumeAttachmentEvent(volumeAttachment, EventTypeAdd)
	return nil
}
