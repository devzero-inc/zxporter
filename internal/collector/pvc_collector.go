// internal/collector/pvc_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PersistentVolumeClaimCollector watches for PVC events and collects PVC data
type PersistentVolumeClaimCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	pvcInformer     cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	namespaces      []string
	excludedPVCs    map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// ExcludedPVC defines a PVC to exclude from collection
type ExcludedPVC struct {
	Namespace string
	Name      string
}

// NewPersistentVolumeClaimCollector creates a new collector for PVC resources
func NewPersistentVolumeClaimCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedPVCs []ExcludedPVC,
	logger logr.Logger,
) *PersistentVolumeClaimCollector {
	// Convert excluded PVCs to a map for quicker lookups
	excludedPVCsMap := make(map[types.NamespacedName]bool)
	for _, pvc := range excludedPVCs {
		excludedPVCsMap[types.NamespacedName{
			Namespace: pvc.Namespace,
			Name:      pvc.Name,
		}] = true
	}

	return &PersistentVolumeClaimCollector{
		client:       client,
		resourceChan: make(chan CollectedResource, 100),
		stopCh:       make(chan struct{}),
		namespaces:   namespaces,
		excludedPVCs: excludedPVCsMap,
		logger:       logger.WithName("pvc-collector"),
	}
}

// Start begins the PVC collection process
func (c *PersistentVolumeClaimCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting PVC collector", "namespaces", c.namespaces)

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

	// Create PVC informer
	c.pvcInformer = c.informerFactory.Core().V1().PersistentVolumeClaims().Informer()

	// Add event handlers
	_, err := c.pvcInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pvc := obj.(*corev1.PersistentVolumeClaim)
			c.handlePVCEvent(pvc, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPVC := oldObj.(*corev1.PersistentVolumeClaim)
			newPVC := newObj.(*corev1.PersistentVolumeClaim)

			// Only handle meaningful updates
			if c.pvcChanged(oldPVC, newPVC) {
				c.handlePVCEvent(newPVC, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			pvc := obj.(*corev1.PersistentVolumeClaim)
			c.handlePVCEvent(pvc, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.pvcInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handlePVCEvent processes PVC events
func (c *PersistentVolumeClaimCollector) handlePVCEvent(pvc *corev1.PersistentVolumeClaim, eventType string) {
	if c.isExcluded(pvc) {
		return
	}

	c.logger.Info("Processing PVC event",
		"namespace", pvc.Namespace,
		"name", pvc.Name,
		"eventType", eventType)

	// Send the raw PVC object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: PersistentVolumeClaim,
		Object:       pvc, // Send the entire PVC object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name),
	}
}

// pvcChanged detects meaningful changes in a PVC
func (c *PersistentVolumeClaimCollector) pvcChanged(oldPVC, newPVC *corev1.PersistentVolumeClaim) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldPVC.ResourceVersion == newPVC.ResourceVersion {
		return false
	}

	// Check for status phase changes
	if oldPVC.Status.Phase != newPVC.Status.Phase {
		return true
	}

	// Check for bound volume changes
	if oldPVC.Spec.VolumeName != newPVC.Spec.VolumeName {
		return true
	}

	// Check for capacity changes
	oldCapacity := oldPVC.Status.Capacity
	newCapacity := newPVC.Status.Capacity
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

	// Check for access mode changes
	if !accessModesEqual(oldPVC.Spec.AccessModes, newPVC.Spec.AccessModes) {
		return true
	}

	// Check for condition changes
	if len(oldPVC.Status.Conditions) != len(newPVC.Status.Conditions) {
		return true
	}

	// Deep check on conditions
	oldConditions := make(map[string]corev1.PersistentVolumeClaimCondition)
	for _, condition := range oldPVC.Status.Conditions {
		oldConditions[string(condition.Type)] = condition
	}

	for _, newCondition := range newPVC.Status.Conditions {
		oldCondition, exists := oldConditions[string(newCondition.Type)]
		if !exists || oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return true
		}
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a PVC should be excluded from collection
func (c *PersistentVolumeClaimCollector) isExcluded(pvc *corev1.PersistentVolumeClaim) bool {
	// Check if monitoring specific namespaces and this PVC isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == pvc.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if PVC is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: pvc.Namespace,
		Name:      pvc.Name,
	}
	return c.excludedPVCs[key]
}

// Stop gracefully shuts down the PVC collector
func (c *PersistentVolumeClaimCollector) Stop() error {
	c.logger.Info("Stopping PVC collector")
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *PersistentVolumeClaimCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *PersistentVolumeClaimCollector) GetType() string {
	return "persistentvolumeclaim"
}
