// internal/collector/workload_recommendation_collector.go
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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// WorkloadRecommendationCollector watches for WorkloadRecommendation CRD events and collects data
// This collector sends in-cluster generated WorkloadRecommendations back to the control plane
type WorkloadRecommendationCollector struct {
	client          dynamic.Interface
	informerFactory dynamicinformer.DynamicSharedInformerFactory
	wrInformer      cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
}

// WorkloadRecommendation CRD resource identifier
var workloadRecommendationGVR = schema.GroupVersionResource{
	Group:    "dakr.devzero.io",
	Version:  "v1alpha1",
	Resource: "workloadrecommendations",
}

// NewWorkloadRecommendationCollector creates a new collector for WorkloadRecommendation CRDs
func NewWorkloadRecommendationCollector(
	client dynamic.Interface,
	namespaces []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *WorkloadRecommendationCollector {
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

	return &WorkloadRecommendationCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		logger:          logger.WithName("workload-recommendation-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the WorkloadRecommendation collection process
func (c *WorkloadRecommendationCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting WorkloadRecommendation collector", "namespaces", c.namespaces)

	// Create dynamic informer factory based on namespace configuration
	// Use a 60s resync period to catch any missed status updates (e.g. Pending -> Applied transitions)
	// Without resync, a dropped watch event means the control plane never learns about applied recommendations
	resyncPeriod := 60 * time.Second
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.client,
			resyncPeriod,
			c.namespaces[0],
			nil,
		)
	} else {
		// Watch all namespaces
		c.informerFactory = dynamicinformer.NewDynamicSharedInformerFactory(c.client, resyncPeriod)
	}

	// Create WorkloadRecommendation informer
	c.wrInformer = c.informerFactory.ForResource(workloadRecommendationGVR).Informer()

	// Add event handlers
	_, err := c.wrInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			wr := obj.(*unstructured.Unstructured)
			c.handleWorkloadRecommendationEvent(wr, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldWR := oldObj.(*unstructured.Unstructured)
			newWR := newObj.(*unstructured.Unstructured)

			// Handle resync events: same ResourceVersion means informer resync, not a real change.
			// Re-send terminal-state recommendations on resync to catch any previously missed updates.
			if oldWR.GetResourceVersion() == newWR.GetResourceVersion() {
				phase, _, _ := unstructured.NestedString(newWR.Object, "status", "phase")
				if phase == "Applied" || phase == "Failed" || phase == "Skipped" {
					c.handleWorkloadRecommendationEvent(newWR, EventTypeUpdate)
				}
				return
			}

			// Only handle meaningful updates
			if c.workloadRecommendationChanged(oldWR, newWR) {
				c.handleWorkloadRecommendationEvent(newWR, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			wr := obj.(*unstructured.Unstructured)
			c.handleWorkloadRecommendationEvent(wr, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.wrInformer.HasSynced) {
		return fmt.Errorf("failed to sync caches")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for WorkloadRecommendations")
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

// handleWorkloadRecommendationEvent processes WorkloadRecommendation events
func (c *WorkloadRecommendationCollector) handleWorkloadRecommendationEvent(wr *unstructured.Unstructured, eventType EventType) {
	namespace := wr.GetNamespace()
	name := wr.GetName()

	c.logger.Info("WorkloadRecommendation event",
		"event_type", eventType.String(),
		"namespace", namespace,
		"name", name,
	)

	// Only send recommendations that have reached a terminal state
	// Delete events are always sent so the control plane knows about removals
	if eventType != EventTypeDelete {
		phase, _, _ := unstructured.NestedString(wr.Object, "status", "phase")
		if phase != "Applied" && phase != "Failed" && phase != "Skipped" {
			c.logger.Info("Skipping WorkloadRecommendation with non-terminal phase",
				"namespace", namespace,
				"name", name,
				"phase", phase,
			)
			return
		}
	}

	// Log and send the raw WorkloadRecommendation object to the batch channel
	phase, _, _ := unstructured.NestedString(wr.Object, "status", "phase")
	c.logger.Info("Sending WorkloadRecommendation to control plane",
		"event_type", eventType.String(),
		"namespace", namespace,
		"name", name,
		"phase", phase,
	)
	c.batchChan <- CollectedResource{
		ResourceType: WorkloadRecommendation,
		Object:       wr, // Send the entire WorkloadRecommendation object as-is
		Timestamp:    time.Now().UTC(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", namespace, name),
	}
}

// workloadRecommendationChanged detects meaningful changes in a WorkloadRecommendation
func (c *WorkloadRecommendationCollector) workloadRecommendationChanged(oldWR, newWR *unstructured.Unstructured) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldWR.GetResourceVersion() == newWR.GetResourceVersion() {
		return false
	}

	// Check for spec changes
	oldSpec, _, err := unstructured.NestedMap(oldWR.Object, "spec")
	if err != nil {
		c.logger.Error(err, "Failed to get spec from old WorkloadRecommendation")
		return true // Assume changed on error
	}

	newSpec, _, err := unstructured.NestedMap(newWR.Object, "spec")
	if err != nil {
		c.logger.Error(err, "Failed to get spec from new WorkloadRecommendation")
		return true // Assume changed on error
	}

	if !reflect.DeepEqual(oldSpec, newSpec) {
		return true
	}

	// Check for status changes - this is important for tracking recommendation application state
	oldStatus, _, err := unstructured.NestedMap(oldWR.Object, "status")
	if err != nil {
		c.logger.Error(err, "Failed to get status from old WorkloadRecommendation")
		return true // Assume changed on error
	}

	newStatus, _, err := unstructured.NestedMap(newWR.Object, "status")
	if err != nil {
		c.logger.Error(err, "Failed to get status from new WorkloadRecommendation")
		return true // Assume changed on error
	}

	if !reflect.DeepEqual(oldStatus, newStatus) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldWR.GetLabels(), newWR.GetLabels()) {
		return true
	}

	if !reflect.DeepEqual(oldWR.GetUID(), newWR.GetUID()) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldWR.GetAnnotations(), newWR.GetAnnotations()) {
		return true
	}

	// No significant changes detected
	return false
}

// Stop gracefully shuts down the WorkloadRecommendation collector
func (c *WorkloadRecommendationCollector) Stop() error {
	c.logger.Info("Stopping WorkloadRecommendation collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("WorkloadRecommendation collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed WorkloadRecommendation collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed WorkloadRecommendation collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("WorkloadRecommendation collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *WorkloadRecommendationCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *WorkloadRecommendationCollector) GetType() string {
	return "workload_recommendation"
}

// IsAvailable checks if WorkloadRecommendation CRD is available in the cluster
func (c *WorkloadRecommendationCollector) IsAvailable(ctx context.Context) bool {
	// Try to list WorkloadRecommendation resources with limit=1 to see if the resource type exists
	_, err := c.client.Resource(workloadRecommendationGVR).List(ctx, metav1.ListOptions{Limit: 1})

	if err == nil {
		return true
	}

	// For "no matching resource" or other similar errors
	if isResourceTypeUnavailableError(err) {
		c.logger.Info("WorkloadRecommendation CRD not installed in cluster",
			"error", err.Error())
		return false
	}

	// For other errors (permissions, etc.), log but assume resource might exist
	c.logger.Error(err, "Error checking WorkloadRecommendation resource availability")
	return false
}

// AddResource manually adds a WorkloadRecommendation resource to be processed by the collector
func (c *WorkloadRecommendationCollector) AddResource(resource interface{}) error {
	wr, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}

	c.handleWorkloadRecommendationEvent(wr, EventTypeAdd)
	return nil
}
