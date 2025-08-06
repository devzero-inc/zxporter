// internal/collector/verticalpodautoscaler_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// VerticalPodAutoscalerCollector watches for VPA events and collects VPA data
type VerticalPodAutoscalerCollector struct {
	client          dynamic.Interface
	informerFactory dynamicinformer.DynamicSharedInformerFactory
	vpaInformer     cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	excludedVPAs    map[types.NamespacedName]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
}

// VPA resource identifier
var vpaGVR = schema.GroupVersionResource{
	Group:    "autoscaling.k8s.io",
	Version:  "v1",
	Resource: "verticalpodautoscalers",
}

// NewVerticalPodAutoscalerCollector creates a new collector for VPA resources
func NewVerticalPodAutoscalerCollector(
	client dynamic.Interface,
	namespaces []string,
	excludedVPAs []ExcludedVPA,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *VerticalPodAutoscalerCollector {
	// Convert excluded VPAs to a map for quicker lookups
	excludedVPAsMap := make(map[types.NamespacedName]bool)
	for _, vpa := range excludedVPAs {
		excludedVPAsMap[types.NamespacedName{
			Namespace: vpa.Namespace,
			Name:      vpa.Name,
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

	return &VerticalPodAutoscalerCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		excludedVPAs:    excludedVPAsMap,
		logger:          logger.WithName("vpa-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the VPA collection process
func (c *VerticalPodAutoscalerCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting VerticalPodAutoscaler collector", "namespaces", c.namespaces)

	// Create dynamic informer factory based on namespace configuration
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.client,
			0, // No resync period, rely on events
			c.namespaces[0],
			nil,
		)
	} else {
		// Watch all namespaces
		c.informerFactory = dynamicinformer.NewDynamicSharedInformerFactory(c.client, 0)
	}

	// Create VPA informer
	c.vpaInformer = c.informerFactory.ForResource(vpaGVR).Informer()

	// Add event handlers
	_, err := c.vpaInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			vpa := obj.(*unstructured.Unstructured)
			c.handleVPAEvent(vpa, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldVPA := oldObj.(*unstructured.Unstructured)
			newVPA := newObj.(*unstructured.Unstructured)

			// Only handle meaningful updates
			if c.vpaChanged(oldVPA, newVPA) {
				c.handleVPAEvent(newVPA, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			vpa := obj.(*unstructured.Unstructured)
			c.handleVPAEvent(vpa, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.vpaInformer.HasSynced) {
		return fmt.Errorf("failed to sync caches")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for VPAs")
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

// handleVPAEvent processes VPA events
func (c *VerticalPodAutoscalerCollector) handleVPAEvent(vpa *unstructured.Unstructured, eventType EventType) {
	if c.isExcluded(vpa) {
		return
	}

	namespace := vpa.GetNamespace()
	name := vpa.GetName()

	c.logger.Info("Processing VerticalPodAutoscaler event",
		"namespace", namespace,
		"name", name,
		"eventType", eventType.String())

	// Send the raw VPA object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: VerticalPodAutoscaler,
		Object:       vpa, // Send the entire VPA object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", namespace, name),
	}
}

// vpaChanged detects meaningful changes in a VPA
func (c *VerticalPodAutoscalerCollector) vpaChanged(oldVPA, newVPA *unstructured.Unstructured) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldVPA.GetResourceVersion() == newVPA.GetResourceVersion() {
		return false
	}

	// Check for spec changes
	oldSpec, _, err := unstructured.NestedMap(oldVPA.Object, "spec")
	if err != nil {
		c.logger.Error(err, "Failed to get spec from old VPA")
		return true // Assume changed on error
	}

	newSpec, _, err := unstructured.NestedMap(newVPA.Object, "spec")
	if err != nil {
		c.logger.Error(err, "Failed to get spec from new VPA")
		return true // Assume changed on error
	}

	if !reflect.DeepEqual(oldSpec, newSpec) {
		return true
	}

	// Check for status changes
	oldStatus, _, err := unstructured.NestedMap(oldVPA.Object, "status")
	if err != nil {
		c.logger.Error(err, "Failed to get status from old VPA")
		return true // Assume changed on error
	}

	newStatus, _, err := unstructured.NestedMap(newVPA.Object, "status")
	if err != nil {
		c.logger.Error(err, "Failed to get status from new VPA")
		return true // Assume changed on error
	}

	if !reflect.DeepEqual(oldStatus, newStatus) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldVPA.GetLabels(), newVPA.GetLabels()) {
		return true
	}

	if !reflect.DeepEqual(oldVPA.GetUID(), newVPA.GetUID()) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldVPA.GetAnnotations(), newVPA.GetAnnotations()) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a VPA should be excluded from collection
func (c *VerticalPodAutoscalerCollector) isExcluded(vpa *unstructured.Unstructured) bool {
	namespace := vpa.GetNamespace()
	name := vpa.GetName()

	// Check if monitoring specific namespaces and this VPA isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if VPA is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	return c.excludedVPAs[key]
}

// Stop gracefully shuts down the VPA collector
func (c *VerticalPodAutoscalerCollector) Stop() error {
	c.logger.Info("Stopping VerticalPodAutoscaler collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("VPA collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed VPA collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed VPA collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("VPA collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *VerticalPodAutoscalerCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *VerticalPodAutoscalerCollector) GetType() string {
	return "vertical_pod_autoscaler"
}

// IsAvailable checks if VPA resources are available in the cluster
func (c *VerticalPodAutoscalerCollector) IsAvailable(ctx context.Context) bool {
	// Try to list VPA resources with limit=1 to see if the resource type exists
	_, err := c.client.Resource(vpaGVR).List(ctx, metav1.ListOptions{Limit: 1})

	if err == nil {
		return true
	}

	// For "no matching resource" or other similar errors
	if isResourceTypeUnavailableError(err) {
		c.logger.Info("VerticalPodAutoscaler CRD not installed in cluster",
			"error", err.Error())
		return false
	}

	// For other errors (permissions, etc.), log but assume resource might exist
	c.logger.Error(err, "Error checking VPA resource availability")
	return false
}

// AddResource manually adds a VPA resource to be processed by the collector
func (c *VerticalPodAutoscalerCollector) AddResource(resource interface{}) error {
	vpa, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}

	c.handleVPAEvent(vpa, EventTypeAdd)
	return nil
}
