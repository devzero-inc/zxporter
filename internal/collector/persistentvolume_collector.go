// internal/collector/persistentvolume_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

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
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	excludedPVs     map[string]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// NewPersistentVolumeCollector creates a new collector for PV resources
func NewPersistentVolumeCollector(
	client kubernetes.Interface,
	excludedPVs []string,
	logger logr.Logger,
) *PersistentVolumeCollector {
	// Convert excluded PVs to a map for quicker lookups
	excludedPVsMap := make(map[string]bool)
	for _, pv := range excludedPVs {
		excludedPVsMap[pv] = true
	}

	return &PersistentVolumeCollector{
		client:       client,
		resourceChan: make(chan CollectedResource, 100),
		stopCh:       make(chan struct{}),
		excludedPVs:  excludedPVsMap,
		logger:       logger.WithName("persistentvolume-collector"),
	}
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
			c.handlePVEvent(pv, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPV := oldObj.(*corev1.PersistentVolume)
			newPV := newObj.(*corev1.PersistentVolume)

			// Only handle meaningful updates
			if c.pvChanged(oldPV, newPV) {
				c.handlePVEvent(newPV, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			pv := obj.(*corev1.PersistentVolume)
			c.handlePVEvent(pv, "delete")
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
func (c *PersistentVolumeCollector) handlePVEvent(pv *corev1.PersistentVolume, eventType string) {
	if c.isExcluded(pv) {
		return
	}

	c.logger.V(4).Info("Processing PersistentVolume event",
		"name", pv.Name,
		"eventType", eventType,
		"phase", pv.Status.Phase)

	// Send the raw PV object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: PersistentVolume,
		Object:       pv, // Send the entire PV object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          pv.Name, // PVs are cluster-scoped, so name is sufficient
	}
}

// pvChanged detects meaningful changes in a PV
func (c *PersistentVolumeCollector) pvChanged(oldPV, newPV *corev1.PersistentVolume) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldPV.ResourceVersion == newPV.ResourceVersion {
		return false
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

// nodeAffinityEqual compares two node affinities for equality
func nodeAffinityEqual(affinity1, affinity2 *corev1.VolumeNodeAffinity) bool {
	if affinity1 == nil && affinity2 == nil {
		return true
	}

	if affinity1 == nil || affinity2 == nil {
		return false
	}

	// If there's no required field, they're equal
	if affinity1.Required == nil && affinity2.Required == nil {
		return true
	}

	if affinity1.Required == nil || affinity2.Required == nil {
		return false
	}

	// Compare the node selector terms
	terms1 := affinity1.Required.NodeSelectorTerms
	terms2 := affinity2.Required.NodeSelectorTerms

	if len(terms1) != len(terms2) {
		return false
	}

	// This is a simplistic comparison that assumes the order of terms matters
	// A more robust implementation would handle reordering of equivalent terms
	for i, term1 := range terms1 {
		term2 := terms2[i]

		// Compare match expressions
		if len(term1.MatchExpressions) != len(term2.MatchExpressions) {
			return false
		}

		for j, expr1 := range term1.MatchExpressions {
			expr2 := term2.MatchExpressions[j]

			if expr1.Key != expr2.Key ||
				expr1.Operator != expr2.Operator ||
				!stringSlicesEqual(expr1.Values, expr2.Values) {
				return false
			}
		}

		// Compare match fields
		if len(term1.MatchFields) != len(term2.MatchFields) {
			return false
		}

		for j, field1 := range term1.MatchFields {
			field2 := term2.MatchFields[j]

			if field1.Key != field2.Key ||
				field1.Operator != field2.Operator ||
				!stringSlicesEqual(field1.Values, field2.Values) {
				return false
			}
		}
	}

	return true
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

// stringSlicesEqual compares two string slices for equality
func stringSlicesEqual(s1, s2 []string) bool {
	if len(s1) != len(s2) {
		return false
	}

	// Create maps for efficient lookup
	map1 := make(map[string]bool)
	map2 := make(map[string]bool)

	for _, s := range s1 {
		map1[s] = true
	}

	for _, s := range s2 {
		map2[s] = true
	}

	// Compare the maps
	for s := range map1 {
		if !map2[s] {
			return false
		}
	}

	for s := range map2 {
		if !map1[s] {
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
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *PersistentVolumeCollector) GetResourceChannel() <-chan CollectedResource {
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
