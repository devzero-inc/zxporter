// internal/collector/persistentvolume_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PersistentVolumeCollector watches for PV events and collects PV data
type PersistentVolumeCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	pvInformer      cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	excludedPVs     map[string]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
	cDHelper        ChangeDetectionHelper
}

// NewPersistentVolumeCollector creates a new collector for PV resources
func NewPersistentVolumeCollector(
	client kubernetes.Interface,
	excludedPVs []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *PersistentVolumeCollector {
	// Convert excluded PVs to a map for quicker lookups
	excludedPVsMap := make(map[string]bool)
	for _, pv := range excludedPVs {
		excludedPVsMap[pv] = true
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

	newLogger := logger.WithName("persistentvolume-collector")
	return &PersistentVolumeCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		excludedPVs:     excludedPVsMap,
		logger:          newLogger,
		telemetryLogger: telemetryLogger,
		cDHelper:        ChangeDetectionHelper{logger: newLogger}}
}

// Start begins the PV collection process
func (c *PersistentVolumeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting PersistentVolume collector")

	// Create informer factory - PVs are cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create PV informer
	c.pvInformer = c.informerFactory.Core().V1().PersistentVolumes().Informer()

	// Add event handlers
	_, err := c.pvInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pv := obj.(*corev1.PersistentVolume)
			c.handlePVEvent(pv, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPV := oldObj.(*corev1.PersistentVolume)
			newPV := newObj.(*corev1.PersistentVolume)

			// Only handle meaningful updates
			if c.pvChanged(oldPV, newPV) {
				c.handlePVEvent(newPV, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pv := obj.(*corev1.PersistentVolume)
			c.handlePVEvent(pv, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.pvInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for PersistentVolumes")
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

// handlePVEvent processes PV events
func (c *PersistentVolumeCollector) handlePVEvent(pv *corev1.PersistentVolume, eventType EventType) {
	if c.isExcluded(pv) {
		return
	}

	// Send the raw PV object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: PersistentVolume,
		Object:       pv, // Send the entire PV object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          pv.Name, // PVs are cluster-scoped, so name is sufficient
	}
}

// pvChanged detects meaningful changes in a PV
func (c *PersistentVolumeCollector) pvChanged(oldPV, newPV *corev1.PersistentVolume) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldPV.Name,
		oldPV.ObjectMeta,
		newPV.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for phase changes
	if oldPV.Status.Phase != newPV.Status.Phase {
		return true
	}

	// Check for claim reference changes
	if !claimReferenceEqual(oldPV.Spec.ClaimRef, newPV.Spec.ClaimRef) {
		return true
	}

	// Check for reclaim policy changes
	if oldPV.Spec.PersistentVolumeReclaimPolicy != newPV.Spec.PersistentVolumeReclaimPolicy {
		return true
	}

	// Check for storage class changes
	oldStorageClass := ""
	if oldPV.Spec.StorageClassName != "" {
		oldStorageClass = oldPV.Spec.StorageClassName
	}

	newStorageClass := ""
	if newPV.Spec.StorageClassName != "" {
		newStorageClass = newPV.Spec.StorageClassName
	}

	if oldStorageClass != newStorageClass {
		return true
	}

	// Check for capacity changes
	oldCapacity := oldPV.Spec.Capacity
	newCapacity := newPV.Spec.Capacity
	if oldCapacity != nil && newCapacity != nil {
		oldStorage, oldHasStorage := oldCapacity[corev1.ResourceStorage]
		newStorage, newHasStorage := newCapacity[corev1.ResourceStorage]

		if oldHasStorage != newHasStorage {
			return true
		}

		if oldHasStorage && newHasStorage && !oldStorage.Equal(newStorage) {
			return true
		}
	} else if oldCapacity != nil || newCapacity != nil {
		return true
	}

	// Check for access modes changes
	if !accessModesEqual(oldPV.Spec.AccessModes, newPV.Spec.AccessModes) {
		return true
	}

	// Check for node affinity changes
	if !nodeAffinityEqual(oldPV.Spec.NodeAffinity, newPV.Spec.NodeAffinity) {
		return true
	}

	// Check for mount options changes
	if !stringSlicesEqual(oldPV.Spec.MountOptions, newPV.Spec.MountOptions) {
		return true
	}

	if !reflect.DeepEqual(oldPV.UID, newPV.UID) {
		return true
	}

	// Check for volume mode changes
	if (oldPV.Spec.VolumeMode == nil && newPV.Spec.VolumeMode != nil) ||
		(oldPV.Spec.VolumeMode != nil && newPV.Spec.VolumeMode == nil) ||
		(oldPV.Spec.VolumeMode != nil && newPV.Spec.VolumeMode != nil && *oldPV.Spec.VolumeMode != *newPV.Spec.VolumeMode) {
		return true
	}

	// No significant changes detected
	return false
}

// claimReferenceEqual compares two claim references for equality
func claimReferenceEqual(ref1, ref2 *corev1.ObjectReference) bool {
	if ref1 == nil && ref2 == nil {
		return true
	}

	if ref1 == nil || ref2 == nil {
		return false
	}

	return ref1.Name == ref2.Name &&
		ref1.Namespace == ref2.Namespace &&
		ref1.UID == ref2.UID
}

// accessModesEqual compares two access mode slices for equality
func accessModesEqual(modes1, modes2 []corev1.PersistentVolumeAccessMode) bool {
	if len(modes1) != len(modes2) {
		return false
	}

	// Create maps for efficient lookup
	modesMap1 := make(map[corev1.PersistentVolumeAccessMode]bool)
	modesMap2 := make(map[corev1.PersistentVolumeAccessMode]bool)

	for _, mode := range modes1 {
		modesMap1[mode] = true
	}

	for _, mode := range modes2 {
		modesMap2[mode] = true
	}

	// Compare the maps
	for mode := range modesMap1 {
		if !modesMap2[mode] {
			return false
		}
	}

	for mode := range modesMap2 {
		if !modesMap1[mode] {
			return false
		}
	}

	return true
}

// isExcluded checks if a PV should be excluded from collection
func (c *PersistentVolumeCollector) isExcluded(pv *corev1.PersistentVolume) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedPVs[pv.Name]
}

// Stop gracefully shuts down the PV collector
func (c *PersistentVolumeCollector) Stop() error {
	c.logger.Info("Stopping PersistentVolume collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("PersistentVolume collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed PersistentVolume collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed PersistentVolume collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("PersistentVolume collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *PersistentVolumeCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *PersistentVolumeCollector) GetType() string {
	return "persistentvolume"
}

// IsAvailable checks if PersistentVolume resources can be accessed in the cluster
func (c *PersistentVolumeCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a persistent volume resource to be processed by the collector
func (c *PersistentVolumeCollector) AddResource(resource interface{}) error {
	pv, ok := resource.(*corev1.PersistentVolume)
	if !ok {
		return fmt.Errorf("expected *corev1.PersistentVolume, got %T", resource)
	}

	c.handlePVEvent(pv, EventTypeAdd)
	return nil
}
