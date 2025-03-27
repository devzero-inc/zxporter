// internal/collector/verticalpodautoscaler_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
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
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	namespaces      []string
	excludedVPAs    map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// ExcludedVPA defines a VPA to exclude from collection
type ExcludedVPA struct {
	Namespace string
	Name      string
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
	logger logr.Logger,
) *VerticalPodAutoscalerCollector {
	// Convert excluded VPAs to a map for quicker lookups
	excludedVPAsMap := make(map[types.NamespacedName]bool)
	for _, vpa := range excludedVPAs {
		excludedVPAsMap[types.NamespacedName{
			Namespace: vpa.Namespace,
			Name:      vpa.Name,
		}] = true
	}

	return &VerticalPodAutoscalerCollector{
		client:       client,
		resourceChan: make(chan CollectedResource, 100),
		stopCh:       make(chan struct{}),
		namespaces:   namespaces,
		excludedVPAs: excludedVPAsMap,
		logger:       logger.WithName("vpa-collector"),
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
			c.handleVPAEvent(vpa, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldVPA := oldObj.(*unstructured.Unstructured)
			newVPA := newObj.(*unstructured.Unstructured)

			// Only handle meaningful updates
			if c.vpaChanged(oldVPA, newVPA) {
				c.handleVPAEvent(newVPA, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			vpa := obj.(*unstructured.Unstructured)
			c.handleVPAEvent(vpa, "delete")
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

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleVPAEvent processes VPA events
func (c *VerticalPodAutoscalerCollector) handleVPAEvent(vpa *unstructured.Unstructured, eventType string) {
	if c.isExcluded(vpa) {
		return
	}

	namespace := vpa.GetNamespace()
	name := vpa.GetName()

	c.logger.V(4).Info("Processing VerticalPodAutoscaler event",
		"namespace", namespace,
		"name", name,
		"eventType", eventType)

	// Send the raw VPA object directly to the resource channel
	c.resourceChan <- CollectedResource{
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *VerticalPodAutoscalerCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *VerticalPodAutoscalerCollector) GetType() string {
	return "verticalpodautoscaler"
}
