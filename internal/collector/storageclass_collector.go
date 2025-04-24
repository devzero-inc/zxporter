// internal/collector/storageclass_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// StorageClassCollector watches for StorageClass events and collects StorageClass data
type StorageClassCollector struct {
	client                 kubernetes.Interface
	informerFactory        informers.SharedInformerFactory
	storageClassInformer   cache.SharedIndexInformer
	batchChan              chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan           chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                *ResourcesBatcher
	stopCh                 chan struct{}
	excludedStorageClasses map[string]bool
	logger                 logr.Logger
	mu                     sync.RWMutex
}

// NewStorageClassCollector creates a new collector for StorageClass resources
func NewStorageClassCollector(
	client kubernetes.Interface,
	excludedStorageClasses []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *StorageClassCollector {
	// Convert excluded StorageClasses to a map for quicker lookups
	excludedStorageClassesMap := make(map[string]bool)
	for _, sc := range excludedStorageClasses {
		excludedStorageClassesMap[sc] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 50) // Keep lower buffer for infrequent StorageClasses
	resourceChan := make(chan []CollectedResource, 50)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &StorageClassCollector{
		client:                 client,
		batchChan:              batchChan,
		resourceChan:           resourceChan,
		batcher:                batcher,
		stopCh:                 make(chan struct{}),
		excludedStorageClasses: excludedStorageClassesMap,
		logger:                 logger.WithName("storageclass-collector"),
	}
}

// Start begins the StorageClass collection process
func (c *StorageClassCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting StorageClass collector")

	// Create informer factory - StorageClasses are cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create StorageClass informer
	c.storageClassInformer = c.informerFactory.Storage().V1().StorageClasses().Informer()

	// Add event handlers
	_, err := c.storageClassInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			sc := obj.(*storagev1.StorageClass)
			c.handleStorageClassEvent(sc, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldSC := oldObj.(*storagev1.StorageClass)
			newSC := newObj.(*storagev1.StorageClass)

			// Only handle meaningful updates
			if c.storageClassChanged(oldSC, newSC) {
				c.handleStorageClassEvent(newSC, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			sc := obj.(*storagev1.StorageClass)
			c.handleStorageClassEvent(sc, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.storageClassInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for StorageClasses")
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

// handleStorageClassEvent processes StorageClass events
func (c *StorageClassCollector) handleStorageClassEvent(sc *storagev1.StorageClass, eventType string) {
	if c.isExcluded(sc) {
		return
	}

	c.logger.Info("Processing StorageClass event",
		"name", sc.Name,
		"eventType", eventType,
		"provisioner", sc.Provisioner)

	// Send the raw StorageClass object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: StorageClass,
		Object:       sc, // Send the entire StorageClass object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          sc.Name, // StorageClasses are cluster-scoped, so name is sufficient
	}
}

// storageClassChanged detects meaningful changes in a StorageClass
func (c *StorageClassCollector) storageClassChanged(oldSC, newSC *storagev1.StorageClass) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldSC.ResourceVersion == newSC.ResourceVersion {
		return false
	}

	// Check for provisioner changes
	if oldSC.Provisioner != newSC.Provisioner {
		return true
	}

	// Check for reclaim policy changes
	if !reclaimPolicyEqual(oldSC.ReclaimPolicy, newSC.ReclaimPolicy) {
		return true
	}

	// Check for volume binding mode changes
	if !volumeBindingModeEqual(oldSC.VolumeBindingMode, newSC.VolumeBindingMode) {
		return true
	}

	// Check for allow volume expansion changes
	if !boolPointerEqual(oldSC.AllowVolumeExpansion, newSC.AllowVolumeExpansion) {
		return true
	}

	// Check for mount options changes
	if !stringSlicesEqual(oldSC.MountOptions, newSC.MountOptions) {
		return true
	}

	// Check for parameter changes (key-value pairs)
	if !mapsEqual(oldSC.Parameters, newSC.Parameters) {
		return true
	}

	// Check for allowed topologies changes
	if !allowedTopologiesEqual(oldSC.AllowedTopologies, newSC.AllowedTopologies) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldSC.Annotations, newSC.Annotations) {
		return true
	}

	// Check for default class annotation specifically
	oldDefault := oldSC.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
	newDefault := newSC.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
	if oldDefault != newDefault {
		return true
	}

	// No significant changes detected
	return false
}

// reclaimPolicyEqual compares two reclaim policies for equality
func reclaimPolicyEqual(policy1, policy2 *corev1.PersistentVolumeReclaimPolicy) bool {
	if policy1 == nil && policy2 == nil {
		return true
	}

	if policy1 == nil || policy2 == nil {
		return false
	}

	return *policy1 == *policy2
}

// volumeBindingModeEqual compares two volume binding modes for equality
func volumeBindingModeEqual(mode1, mode2 *storagev1.VolumeBindingMode) bool {
	if mode1 == nil && mode2 == nil {
		return true
	}

	if mode1 == nil || mode2 == nil {
		return false
	}

	return *mode1 == *mode2
}

// boolPointerEqual compares two bool pointers for equality
func boolPointerEqual(b1, b2 *bool) bool {
	if b1 == nil && b2 == nil {
		return true
	}

	if b1 == nil || b2 == nil {
		return false
	}

	return *b1 == *b2
}

// allowedTopologiesEqual compares two allowed topology slices for equality
func allowedTopologiesEqual(topologies1, topologies2 []corev1.TopologySelectorTerm) bool {
	if len(topologies1) != len(topologies2) {
		return false
	}

	// This is a simplified comparison that assumes the order is significant
	// A more robust implementation would handle reordering of equivalent terms
	for i, term1 := range topologies1 {
		term2 := topologies2[i]

		if len(term1.MatchLabelExpressions) != len(term2.MatchLabelExpressions) {
			return false
		}

		// Check match label expressions
		for j, expr1 := range term1.MatchLabelExpressions {
			expr2 := term2.MatchLabelExpressions[j]

			if expr1.Key != expr2.Key || !stringSlicesEqual(expr1.Values, expr2.Values) {
				return false
			}
		}
	}

	return true
}

// isExcluded checks if a StorageClass should be excluded from collection
func (c *StorageClassCollector) isExcluded(sc *storagev1.StorageClass) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedStorageClasses[sc.Name]
}

// Stop gracefully shuts down the StorageClass collector
func (c *StorageClassCollector) Stop() error {
	c.logger.Info("Stopping StorageClass collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("StorageClass collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed StorageClass collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed StorageClass collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("StorageClass collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *StorageClassCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *StorageClassCollector) GetType() string {
	return "storage_class"
}

// IsAvailable checks if StorageClass resources can be accessed in the cluster
func (c *StorageClassCollector) IsAvailable(ctx context.Context) bool {
	return true
}
