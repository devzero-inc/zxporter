// internal/collector/scaledobject_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	kedainformers "github.com/kedacore/keda/v2/pkg/generated/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

// ScaledObjectCollector watches for ScaledObject events and collects ScaledObject data
type ScaledObjectCollector struct {
	client                kedaclient.Interface
	informerFactory       kedainformers.SharedInformerFactory
	scaledObjectInformer  cache.SharedIndexInformer
	batchChan             chan CollectedResource
	resourceChan          chan []CollectedResource
	batcher               *ResourcesBatcher
	stopCh                chan struct{}
	namespaces            []string
	excludedScaledObjects map[types.NamespacedName]bool
	logger                logr.Logger
	mu                    sync.RWMutex
	cDHelper              ChangeDetectionHelper
}

// ExcludedScaledObject represents a ScaledObject to exclude from collection
type ExcludedScaledObject struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
}

// NewScaledObjectCollector creates a new collector for ScaledObject resources
func NewScaledObjectCollector(
	client kedaclient.Interface,
	namespaces []string,
	excludedScaledObjects []ExcludedScaledObject,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ScaledObjectCollector {
	// Convert excluded ScaledObjects to a map for quicker lookups
	excludedScaledObjectsMap := make(map[types.NamespacedName]bool)
	for _, scaledObject := range excludedScaledObjects {
		excludedScaledObjectsMap[types.NamespacedName{
			Namespace: scaledObject.Namespace,
			Name:      scaledObject.Name,
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

	newLogger := logger.WithName("keda-scaledobject-collector")
	return &ScaledObjectCollector{
		client:                client,
		batchChan:             batchChan,
		resourceChan:          resourceChan,
		batcher:               batcher,
		stopCh:                make(chan struct{}),
		namespaces:            namespaces,
		excludedScaledObjects: excludedScaledObjectsMap,
		logger:                newLogger,
		cDHelper:              ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the ScaledObject collection process
func (c *ScaledObjectCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting ScaledObject collector", "namespaces", c.namespaces)

	// Create informer factory based on namespace configuration
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = kedainformers.NewSharedInformerFactoryWithOptions(
			c.client,
			0, // No resync period, rely on events
			kedainformers.WithNamespace(c.namespaces[0]),
		)
	} else {
		// Watch all namespaces
		c.informerFactory = kedainformers.NewSharedInformerFactory(c.client, 0)
	}

	// Create ScaledObject informer
	c.scaledObjectInformer = c.informerFactory.Keda().V1alpha1().ScaledObjects().Informer()

	// Add event handlers
	_, err := c.scaledObjectInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			scaledObject := obj.(*kedav1alpha1.ScaledObject)
			c.handleScaledObjectEvent(scaledObject, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldScaledObject := oldObj.(*kedav1alpha1.ScaledObject)
			newScaledObject := newObj.(*kedav1alpha1.ScaledObject)

			// Only handle meaningful updates
			if c.scaledObjectChanged(oldScaledObject, newScaledObject) {
				c.handleScaledObjectEvent(newScaledObject, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			scaledObject := obj.(*kedav1alpha1.ScaledObject)
			c.handleScaledObjectEvent(scaledObject, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.scaledObjectInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for ScaledObjects")
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

// handleScaledObjectEvent processes ScaledObject events
func (c *ScaledObjectCollector) handleScaledObjectEvent(scaledObject *kedav1alpha1.ScaledObject, eventType EventType) {
	if c.isExcluded(scaledObject) {
		return
	}

	c.logger.Info("Processing ScaledObject event",
		"namespace", scaledObject.Namespace,
		"name", scaledObject.Name,
		"eventType", eventType.String(),
		"scaleTargetRef", scaledObject.Spec.ScaleTargetRef.Name)

	// Send the raw ScaledObject object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: KedaScaledObject,
		Object:       scaledObject,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", scaledObject.Namespace, scaledObject.Name),
	}
}

// scaledObjectChanged detects meaningful changes in a ScaledObject
func (c *ScaledObjectCollector) scaledObjectChanged(oldScaledObject, newScaledObject *kedav1alpha1.ScaledObject) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldScaledObject.Name,
		oldScaledObject.ObjectMeta,
		newScaledObject.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for spec changes (most important for KEDA)
	if !reflect.DeepEqual(oldScaledObject.Spec, newScaledObject.Spec) {
		c.logger.V(1).Info("ScaledObject spec changed",
			"namespace", newScaledObject.Namespace,
			"name", newScaledObject.Name)
		return true
	}

	// Check for status changes that matter for scaling decisions
	if !c.scaledObjectStatusEqual(&oldScaledObject.Status, &newScaledObject.Status) {
		c.logger.V(1).Info("ScaledObject status changed",
			"namespace", newScaledObject.Namespace,
			"name", newScaledObject.Name)
		return true
	}

	// Check for generation changes
	if oldScaledObject.Generation != newScaledObject.Generation {
		return true
	}

	// No significant changes detected
	return false
}

// scaledObjectStatusEqual compares ScaledObject status for meaningful changes
func (c *ScaledObjectCollector) scaledObjectStatusEqual(oldStatus, newStatus *kedav1alpha1.ScaledObjectStatus) bool {
	// Compare scale target name changes
	if oldStatus.ScaleTargetGVKR != newStatus.ScaleTargetGVKR {
		return false
	}

	// Compare current replica counts
	if (oldStatus.LastActiveTime == nil) != (newStatus.LastActiveTime == nil) {
		return false
	}
	if oldStatus.LastActiveTime != nil && newStatus.LastActiveTime != nil &&
		!oldStatus.LastActiveTime.Equal(newStatus.LastActiveTime) {
		return false
	}

	// Compare external metric names (significant for troubleshooting)
	if !reflect.DeepEqual(oldStatus.ExternalMetricNames, newStatus.ExternalMetricNames) {
		return false
	}

	// Compare conditions length first for quick check
	if len(oldStatus.Conditions) != len(newStatus.Conditions) {
		return false
	}

	// Deep check on conditions - important for KEDA health monitoring
	oldConditions := make(map[string]kedav1alpha1.Condition)
	for _, condition := range oldStatus.Conditions {
		oldConditions[string(condition.Type)] = condition
	}

	for _, newCondition := range newStatus.Conditions {
		oldCondition, exists := oldConditions[string(newCondition.Type)]
		if !exists ||
			oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return false
		}
	}

	return true
}

// isExcluded checks if a ScaledObject should be excluded from collection
func (c *ScaledObjectCollector) isExcluded(scaledObject *kedav1alpha1.ScaledObject) bool {
	// Check if monitoring specific namespaces and this ScaledObject isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == scaledObject.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if ScaledObject is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: scaledObject.Namespace,
		Name:      scaledObject.Name,
	}
	return c.excludedScaledObjects[key]
}

// Stop gracefully shuts down the ScaledObject collector
func (c *ScaledObjectCollector) Stop() error {
	c.logger.Info("Stopping ScaledObject collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("ScaledObject collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed ScaledObject collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed ScaledObject collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ScaledObject collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ScaledObjectCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ScaledObjectCollector) GetType() string {
	return "scaledobject"
}

// IsAvailable checks if ScaledObject resources can be accessed in the cluster
func (c *ScaledObjectCollector) IsAvailable(ctx context.Context) bool {
	// Check if KEDA CRDs are installed by attempting to list ScaledObjects
	_, err := c.client.KedaV1alpha1().ScaledObjects("").List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.V(1).Info("ScaledObjects not available in cluster", "error", err.Error())
		return false
	}
	return true
}

// AddResource manually adds a scaled object resource to be processed by the collector
func (c *ScaledObjectCollector) AddResource(resource interface{}) error {
	scaledObject, ok := resource.(*kedav1alpha1.ScaledObject)
	if !ok {
		return fmt.Errorf("expected *kedav1alpha1.ScaledObject, got %T", resource)
	}
	
	c.handleScaledObjectEvent(scaledObject, EventTypeAdd)
	return nil
}
