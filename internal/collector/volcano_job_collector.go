// internal/collector/volcano_job_collector.go
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

// VolcanoJobCollector watches for Volcano Job resources
type VolcanoJobCollector struct {
	dynamicClient   dynamic.Interface
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	informers       map[string]cache.SharedIndexInformer
	informerStopChs map[string]chan struct{}
	namespaces      []string
	excludedJobs    map[types.NamespacedName]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
}

// ExcludedVolcanoJob represents a Volcano Job to exclude from collection
type ExcludedVolcanoJob struct {
	Namespace string
	Name      string
}

// NewVolcanoJobCollector creates a new collector for Volcano Job resources
func NewVolcanoJobCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedJobs []ExcludedVolcanoJob,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *VolcanoJobCollector {
	// map for quicker lookups
	excludedJobsMap := make(map[types.NamespacedName]bool)
	for _, j := range excludedJobs {
		excludedJobsMap[types.NamespacedName{
			Namespace: j.Namespace,
			Name:      j.Name,
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

	return &VolcanoJobCollector{
		dynamicClient:   dynamicClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		informers:       make(map[string]cache.SharedIndexInformer),
		informerStopChs: make(map[string]chan struct{}),
		namespaces:      namespaces,
		excludedJobs:    excludedJobsMap,
		logger:          logger.WithName("volcano-job-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the Volcano Job resources collection process
func (c *VolcanoJobCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Volcano Job collector", "namespaces", c.namespaces)

	gvr := schema.GroupVersionResource{
		Group:    "batch.volcano.sh",
		Version:  "v1alpha1",
		Resource: "jobs",
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
					"VolcanoJobCollector_AddFunc",
					"Failed to convert object to unstructured",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", obj),
					},
				)
				return
			}
			c.handleJobEvent(u, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			_, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert old object to unstructured")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"VolcanoJobCollector_UpdateFunc",
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
					"VolcanoJobCollector_UpdateFunc",
					"Failed to convert new object to unstructured",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", newObj),
					},
				)
				return
			}

			c.handleJobEvent(newU, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleJobEvent(u, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"VolcanoJobCollector_DeleteFunc",
					"Failed to convert deleted object",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", obj),
					},
				)
				return
			}
			c.handleJobEvent(u, EventTypeDelete)
		},
	})
	if err != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"VolcanoJobCollector_Start",
			"Failed to add event handler to informer",
			err,
			map[string]string{
				"resource": "jobs",
			},
		)
		return fmt.Errorf("failed to add event handler to informer for Volcano Jobs: %w", err)
	}

	jobKey := "volcano-jobs"
	c.informers[jobKey] = informer
	c.informerStopChs[jobKey] = make(chan struct{})

	// Start the informer
	go informer.Run(c.informerStopChs[jobKey])

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"VolcanoJobCollector_Start",
			"Timeout waiting for cache to sync",
			fmt.Errorf("cache sync timeout"),
			map[string]string{
				"resource": "jobs",
				"timeout":  "30s",
			},
		)
		return fmt.Errorf("timeout waiting for Volcano Jobs cache to sync")
	}

	c.logger.Info("Successfully started informer for Volcano Jobs")

	c.logger.Info("Starting resources batcher for Volcano Jobs")
	c.batcher.start()

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

// handleJobEvent processes Volcano Job events
func (c *VolcanoJobCollector) handleJobEvent(obj *unstructured.Unstructured, eventType EventType) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// Check if this resource should be excluded
	if c.isExcluded(namespace, name) {
		return
	}

	processedObj := c.processJob(obj)

	key := fmt.Sprintf("%s/%s", namespace, name)

	// Send the processed resource to the batch channel
	c.logger.Info("Collected Volcano Job resource", "key", key, "eventType", eventType, "resource", processedObj)
	c.batchChan <- CollectedResource{
		ResourceType: VolcanoJob,
		Object:       processedObj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processJob extracts relevant fields from Volcano Job objects
func (c *VolcanoJobCollector) processJob(obj *unstructured.Unstructured) map[string]interface{} {
	result := map[string]interface{}{
		"name":              obj.GetName(),
		"namespace":         obj.GetNamespace(),
		"resourceVersion":   obj.GetResourceVersion(),
		"creationTimestamp": obj.GetCreationTimestamp().Unix(),
		"raw":               obj,
	}

	return result
}

// isExcluded checks if a job should be excluded
func (c *VolcanoJobCollector) isExcluded(namespace, name string) bool {
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
	return c.excludedJobs[key]
}

// Stop gracefully shuts down the Volcano Job collector
func (c *VolcanoJobCollector) Stop() error {
	c.logger.Info("Stopping Volcano Job collector")

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
		c.logger.Info("Volcano Job collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Volcano Job collector stop channel")
	}

	// Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Volcano Job collector batch input channel")
	}

	// Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Volcano Job collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *VolcanoJobCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *VolcanoJobCollector) GetType() string {
	return "volcano_job"
}

// IsAvailable checks if Volcano Job resources can be accessed in the cluster
func (c *VolcanoJobCollector) IsAvailable(ctx context.Context) bool {
	gvr := schema.GroupVersionResource{
		Group:    "batch.volcano.sh",
		Version:  "v1alpha1",
		Resource: "jobs",
	}

	_, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.Info("Volcano Job resources not available in the cluster", "error", err.Error())
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_WARN,
			"VolcanoJobCollector_IsAvailable",
			"Volcano Job resources not available in the cluster",
			err,
			map[string]string{
				"resource": "jobs",
			},
		)
		return false
	}
	return true
}

// AddResource manually adds a Volcano Job resource to be processed by the collector
func (c *VolcanoJobCollector) AddResource(resource interface{}) error {
	job, ok := resource.(*unstructured.Unstructured)
	if !ok {
		err := fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"VolcanoJobCollector_AddResource",
			"Invalid resource type",
			err,
			map[string]string{
				"expected_type": "*unstructured.Unstructured",
				"actual_type":   fmt.Sprintf("%T", resource),
			},
		)
		return err
	}

	c.handleJobEvent(job, EventTypeAdd)
	return nil
}
