// internal/collector/kubeflow_notebook_collector.go
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

// KubeflowNotebookCollector watches for Kubeflow Notebook resources
type KubeflowNotebookCollector struct {
	dynamicClient     dynamic.Interface
	batchChan         chan CollectedResource
	resourceChan      chan []CollectedResource
	batcher           *ResourcesBatcher
	stopCh            chan struct{}
	informers         map[string]cache.SharedIndexInformer
	informerStopChs   map[string]chan struct{}
	namespaces        []string
	excludedNotebooks map[types.NamespacedName]bool
	logger            logr.Logger
	telemetryLogger   telemetry_logger.Logger
	mu                sync.RWMutex
}

// ExcludedKubeflowNotebook represents a Kubeflow Notebook to exclude from collection
type ExcludedKubeflowNotebook struct {
	Namespace string
	Name      string
}

// NewKubeflowNotebookCollector creates a new collector for Kubeflow Notebook resources
func NewKubeflowNotebookCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedNotebooks []ExcludedKubeflowNotebook,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *KubeflowNotebookCollector {
	// map for quicker lookups
	excludedNotebooksMap := make(map[types.NamespacedName]bool)
	for _, n := range excludedNotebooks {
		excludedNotebooksMap[types.NamespacedName{
			Namespace: n.Namespace,
			Name:      n.Name,
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

	return &KubeflowNotebookCollector{
		dynamicClient:     dynamicClient,
		batchChan:         batchChan,
		resourceChan:      resourceChan,
		batcher:           batcher,
		stopCh:            make(chan struct{}),
		informers:         make(map[string]cache.SharedIndexInformer),
		informerStopChs:   make(map[string]chan struct{}),
		namespaces:        namespaces,
		excludedNotebooks: excludedNotebooksMap,
		logger:            logger.WithName("kubeflow-notebook-collector"),
		telemetryLogger:   telemetryLogger,
	}
}

// Start begins the Kubeflow Notebook resources collection process
func (c *KubeflowNotebookCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Kubeflow Notebook collector", "namespaces", c.namespaces)

	gvr := schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1",
		Resource: "notebooks",
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
					"KubeflowNotebookCollector_AddFunc",
					"Failed to convert object to unstructured",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", obj),
					},
				)
				return
			}
			c.handleNotebookEvent(u, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			_, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert old object to unstructured")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"KubeflowNotebookCollector_UpdateFunc",
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
					"KubeflowNotebookCollector_UpdateFunc",
					"Failed to convert new object to unstructured",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", newObj),
					},
				)
				return
			}

			c.handleNotebookEvent(newU, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleNotebookEvent(u, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"KubeflowNotebookCollector_DeleteFunc",
					"Failed to convert deleted object",
					fmt.Errorf("type assertion failed"),
					map[string]string{
						"object_type": fmt.Sprintf("%T", obj),
					},
				)
				return
			}
			c.handleNotebookEvent(u, EventTypeDelete)
		},
	})
	if err != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"KubeflowNotebookCollector_Start",
			"Failed to add event handler to informer",
			err,
			map[string]string{
				"resource": "notebooks",
			},
		)
		return fmt.Errorf("failed to add event handler to informer for Kubeflow Notebooks: %w", err)
	}

	notebookKey := "notebooks"
	c.informers[notebookKey] = informer
	c.informerStopChs[notebookKey] = make(chan struct{})

	// Start the informer
	go informer.Run(c.informerStopChs[notebookKey])

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"KubeflowNotebookCollector_Start",
			"Timeout waiting for cache to sync",
			fmt.Errorf("cache sync timeout"),
			map[string]string{
				"resource": "notebooks",
				"timeout":  "30s",
			},
		)
		// Prevent leaked informer on startup failure.
		if stopCh, ok := c.informerStopChs[notebookKey]; ok {
			close(stopCh)
			delete(c.informerStopChs, notebookKey)
			delete(c.informers, notebookKey)
		}
		return fmt.Errorf("timeout waiting for Kubeflow Notebooks cache to sync")
	}

	c.logger.Info("Successfully started informer for Kubeflow Notebooks")

	c.logger.Info("Starting resources batcher for Kubeflow Notebooks")
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

// handleNotebookEvent processes Kubeflow Notebook events
func (c *KubeflowNotebookCollector) handleNotebookEvent(obj *unstructured.Unstructured, eventType EventType) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// Check if this resource should be excluded
	if c.isExcluded(namespace, name) {
		return
	}

	processedObj := c.processNotebook(obj)

	key := fmt.Sprintf("%s/%s", namespace, name)

	// Send the processed resource to the batch channel
	c.logger.Info("Collected Kubeflow Notebook resource", "key", key, "eventType", eventType, "resource", processedObj)
	c.batchChan <- CollectedResource{
		ResourceType: KubeflowNotebook,
		Object:       processedObj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processNotebook extracts relevant fields from Kubeflow Notebook objects
func (c *KubeflowNotebookCollector) processNotebook(obj *unstructured.Unstructured) map[string]interface{} {
	result := map[string]interface{}{
		"name":              obj.GetName(),
		"namespace":         obj.GetNamespace(),
		"resourceVersion":   obj.GetResourceVersion(),
		"creationTimestamp": obj.GetCreationTimestamp().Unix(),
		"raw":               obj,
	}

	return result
}

// isExcluded checks if a notebook should be excluded
func (c *KubeflowNotebookCollector) isExcluded(namespace, name string) bool {
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
	return c.excludedNotebooks[key]
}

// Stop gracefully shuts down the Kubeflow Notebook collector
func (c *KubeflowNotebookCollector) Stop() error {
	c.logger.Info("Stopping Kubeflow Notebook collector")

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
		c.logger.Info("Kubeflow Notebook collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Kubeflow Notebook collector stop channel")
	}

	// Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Kubeflow Notebook collector batch input channel")
	}

	// Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Kubeflow Notebook collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *KubeflowNotebookCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *KubeflowNotebookCollector) GetType() string {
	return "kubeflow_notebook"
}

// IsAvailable checks if Kubeflow Notebook resources can be accessed in the cluster
func (c *KubeflowNotebookCollector) IsAvailable(ctx context.Context) bool {
	gvr := schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1",
		Resource: "notebooks",
	}

	_, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.Info("Kubeflow Notebook resources not available in the cluster", "error", err.Error())
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_WARN,
			"KubeflowNotebookCollector_IsAvailable",
			"Kubeflow Notebook resources not available in the cluster",
			err,
			map[string]string{
				"resource": "notebooks",
			},
		)
		return false
	}
	return true
}

// AddResource manually adds a Kubeflow Notebook resource to be processed by the collector
func (c *KubeflowNotebookCollector) AddResource(resource interface{}) error {
	notebook, ok := resource.(*unstructured.Unstructured)
	if !ok {
		err := fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"KubeflowNotebookCollector_AddResource",
			"Invalid resource type",
			err,
			map[string]string{
				"expected_type": "*unstructured.Unstructured",
				"actual_type":   fmt.Sprintf("%T", resource),
			},
		)
		return err
	}

	c.handleNotebookEvent(notebook, EventTypeAdd)
	return nil
}
