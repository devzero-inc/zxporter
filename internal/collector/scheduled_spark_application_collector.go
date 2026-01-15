// internal/collector/scheduled_spark_application_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
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

// ScheduledSparkApplicationCollector watches for Scheduled Spark Application resources
type ScheduledSparkApplicationCollector struct {
	dynamicClient        dynamic.Interface
	batchChan            chan CollectedResource
	resourceChan         chan []CollectedResource
	batcher              *ResourcesBatcher
	stopCh               chan struct{}
	informers            map[string]cache.SharedIndexInformer
	informerStopChs      map[string]chan struct{}
	namespaces           []string
	excludedApplications map[types.NamespacedName]bool
	logger               logr.Logger
	telemetryLogger      telemetry_logger.Logger
	mu                   sync.RWMutex
}

// NewScheduledSparkApplicationCollector creates a new collector for Scheduled Spark Application resources
func NewScheduledSparkApplicationCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedApplications []ExcludedScheduledSparkApplication,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *ScheduledSparkApplicationCollector {
	// map for quicker lookups
	excludedApplicationsMap := make(map[types.NamespacedName]bool)
	for _, app := range excludedApplications {
		excludedApplicationsMap[types.NamespacedName{
			Namespace: app.Namespace,
			Name:      app.Name,
		}] = true
	}

	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &ScheduledSparkApplicationCollector{
		dynamicClient:        dynamicClient,
		batchChan:            batchChan,
		resourceChan:         resourceChan,
		batcher:              batcher,
		stopCh:               make(chan struct{}),
		informers:            make(map[string]cache.SharedIndexInformer),
		informerStopChs:      make(map[string]chan struct{}),
		namespaces:           namespaces,
		excludedApplications: excludedApplicationsMap,
		logger:               logger.WithName("scheduled-spark-application-collector"),
		telemetryLogger:      telemetryLogger,
	}
}

// Start begins the Scheduled Spark Application resources collection process
func (c *ScheduledSparkApplicationCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Scheduled Spark Application collector", "namespaces", c.namespaces)

	gvr := schema.GroupVersionResource{
		Group:    "sparkoperator.k8s.io",
		Version:  "v1beta2",
		Resource: "scheduledsparkapplications",
	}

	// Set up informers based on namespace configuration
	var factory dynamicinformer.DynamicSharedInformerFactory
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.dynamicClient,
			0,
			c.namespaces[0],
			nil,
		)
	} else {
		factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.dynamicClient,
			0,
			"", // All namespaces
			nil,
		)
	}

	informer := factory.ForResource(gvr).Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to unstructured")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"ScheduledSparkApplicationCollector_AddFunc",
					"Failed to convert object to unstructured",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", obj),
					},
				)
				return
			}
			c.handleApplicationEvent(u, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			_, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert old object to unstructured")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"ScheduledSparkApplicationCollector_UpdateFunc",
					"Failed to convert old object to unstructured",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", oldObj),
					},
				)
				return
			}

			newU, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert new object to unstructured")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"ScheduledSparkApplicationCollector_UpdateFunc",
					"Failed to convert new object to unstructured",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", newObj),
					},
				)
				return
			}

			c.handleApplicationEvent(newU, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleApplicationEvent(u, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"ScheduledSparkApplicationCollector_DeleteFunc",
					"Failed to convert deleted object",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", obj),
					},
				)
				return
			}
			c.handleApplicationEvent(u, EventTypeDelete)
		},
	})
	if err != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"ScheduledSparkApplicationCollector_Start",
			"Failed to add event handler to informer",
			err,
			map[string]string{
				"resource": "scheduledsparkapplications",
			},
		)
		return fmt.Errorf("failed to add event handler to informer for Scheduled Spark Applications: %w", err)
	}

	appKey := "scheduled-spark-applications"
	c.informers[appKey] = informer
	c.informerStopChs[appKey] = make(chan struct{})

	// Start the informer
	go informer.Run(c.informerStopChs[appKey])

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"ScheduledSparkApplicationCollector_Start",
			"Timeout waiting for cache to sync",
			fmt.Errorf("cache sync timeout"),
			map[string]string{
				"resource": "scheduledsparkapplications",
				"timeout":  "30s",
			},
		)
		return fmt.Errorf("timeout waiting for Scheduled Spark Applications cache to sync")
	}

	c.logger.Info("Successfully started informer for Scheduled Spark Applications")

	c.logger.Info("Starting resources batcher for Scheduled Spark Applications")
	c.batcher.start()

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			if err := c.Stop(); err != nil {
				c.logger.Error(err, "Error stopping Scheduled Spark Application collector")
			}
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// handleApplicationEvent processes Scheduled Spark Application events
func (c *ScheduledSparkApplicationCollector) handleApplicationEvent(obj *unstructured.Unstructured, eventType EventType) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// Check if this resource should be excluded
	if c.isExcluded(namespace, name) {
		return
	}

	processedObj := c.processApplication(obj)

	key := fmt.Sprintf("%s/%s", namespace, name)

	// Send the processed resource to the batch channel
	c.logger.Info("Collected Scheduled Spark Application resource", "key", key, "eventType", eventType, "resource", processedObj)
	c.batchChan <- CollectedResource{
		ResourceType: ScheduledSparkApplication,
		Object:       processedObj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processApplication extracts relevant fields from Scheduled Spark Application objects
func (c *ScheduledSparkApplicationCollector) processApplication(obj *unstructured.Unstructured) map[string]interface{} {
	result := map[string]interface{}{
		"name":              obj.GetName(),
		"namespace":         obj.GetNamespace(),
		"resourceVersion":   obj.GetResourceVersion(),
		"creationTimestamp": obj.GetCreationTimestamp().Unix(),
		"raw":               obj,
	}

	return result
}

// isExcluded checks if an application should be excluded
func (c *ScheduledSparkApplicationCollector) isExcluded(namespace, name string) bool {
	// Check if monitoring specific namespaces and this resource isn't in them
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

	// Check if resource is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	return c.excludedApplications[key]
}

// Stop gracefully shuts down the Scheduled Spark Application collector
func (c *ScheduledSparkApplicationCollector) Stop() error {
	c.logger.Info("Stopping Scheduled Spark Application collector")

	// Stop all informers
	for key, stopCh := range c.informerStopChs {
		c.logger.Info("Stopping informer", "resource", key)
		close(stopCh)
	}

	c.informers = make(map[string]cache.SharedIndexInformer)
	c.informerStopChs = make(map[string]chan struct{})

	// Close the main stop channel (signals informers to stop)
	select {
	case <-c.stopCh:
		c.logger.Info("Scheduled Spark Application collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Scheduled Spark Application collector stop channel")
	}

	// Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Scheduled Spark Application collector batch input channel")
	}

	// Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Scheduled Spark Application collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ScheduledSparkApplicationCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ScheduledSparkApplicationCollector) GetType() string {
	return "scheduled_spark_application"
}

// IsAvailable checks if Scheduled Spark Application resources can be accessed in the cluster
func (c *ScheduledSparkApplicationCollector) IsAvailable(ctx context.Context) bool {
	gvr := schema.GroupVersionResource{
		Group:    "sparkoperator.k8s.io",
		Version:  "v1beta2",
		Resource: "scheduledsparkapplications",
	}

	_, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.Info("Scheduled Spark Application resources not available in the cluster", "error", err.Error())
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_WARN,
			"ScheduledSparkApplicationCollector_IsAvailable",
			"Scheduled Spark Application resources not available in the cluster",
			err,
			map[string]string{
				"resource": "scheduledsparkapplications",
			},
		)
		return false
	}
	return true
}

// AddResource manually adds a Scheduled Spark Application resource to be processed by the collector
func (c *ScheduledSparkApplicationCollector) AddResource(resource interface{}) error {
	app, ok := resource.(*unstructured.Unstructured)
	if !ok {
		err := fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"ScheduledSparkApplicationCollector_AddResource",
			"Invalid resource type",
			err,
			map[string]string{
				"expected_type": "*unstructured.Unstructured",
				"actual_type":   fmt.Sprintf("%T", resource),
			},
		)
		return err
	}

	c.handleApplicationEvent(app, EventTypeAdd)
	return nil
}
