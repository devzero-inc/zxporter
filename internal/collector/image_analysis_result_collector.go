package collector

import (
	"context"
	"fmt"
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

// ImageAnalysisResultCollector watches for ImageAnalysisResult CRD events
// and sends completed analysis results to the control plane via gRPC.
type ImageAnalysisResultCollector struct {
	client          dynamic.Interface
	informerFactory dynamicinformer.DynamicSharedInformerFactory
	iarInformer     cache.SharedIndexInformer
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	stopOnce        sync.Once
	mu              sync.RWMutex
	stopped         bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
}

// ImageAnalysisResult CRD resource identifier (cluster-scoped)
var imageAnalysisResultGVR = schema.GroupVersionResource{
	Group:    "devzero.io",
	Version:  "v1",
	Resource: "imageanalysisresults",
}

// NewImageAnalysisResultCollector creates a new collector for ImageAnalysisResult CRDs.
func NewImageAnalysisResultCollector(
	client dynamic.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *ImageAnalysisResultCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &ImageAnalysisResultCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("image-analysis-result-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the ImageAnalysisResult collection process.
func (c *ImageAnalysisResultCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting ImageAnalysisResult collector")

	// ImageAnalysisResult is cluster-scoped, no namespace filtering needed.
	// Use 60s resync to catch any missed status transitions.
	resyncPeriod := 60 * time.Second
	c.informerFactory = dynamicinformer.NewDynamicSharedInformerFactory(c.client, resyncPeriod)

	c.iarInformer = c.informerFactory.ForResource(imageAnalysisResultGVR).Informer()

	_, err := c.iarInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			iar := obj.(*unstructured.Unstructured)
			c.handleEvent(iar, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldIAR := oldObj.(*unstructured.Unstructured)
			newIAR := newObj.(*unstructured.Unstructured)

			// Resync: same ResourceVersion means informer resync, not a real change.
			// Re-send completed results on resync to ensure the control plane is up-to-date.
			if oldIAR.GetResourceVersion() == newIAR.GetResourceVersion() {
				phase, _, _ := unstructured.NestedString(newIAR.Object, "status", "phase")
				if phase == "Completed" {
					c.handleEvent(newIAR, EventTypeUpdate)
				}
				return
			}

			// Only re-send if analyzedAt changed (new scan result)
			if c.analysisChanged(oldIAR, newIAR) {
				c.handleEvent(newIAR, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			iar := obj.(*unstructured.Unstructured)
			c.handleEvent(iar, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	c.informerFactory.Start(c.stopCh)

	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.iarInformer.HasSynced) {
		return fmt.Errorf("failed to sync caches")
	}
	c.logger.Info("Informer caches synced successfully")

	c.logger.Info("Starting resources batcher for ImageAnalysisResults")
	c.batcher.start()

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Stop()
		case <-stopCh:
		}
	}()

	return nil
}

// handleEvent processes an ImageAnalysisResult event.
func (c *ImageAnalysisResultCollector) handleEvent(iar *unstructured.Unstructured, eventType EventType) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stopped {
		return
	}

	name := iar.GetName()

	// Only send completed analysis results (skip Pending, Analyzing, Failed).
	// Delete events are always sent so the control plane can clean up.
	if eventType != EventTypeDelete {
		phase, _, _ := unstructured.NestedString(iar.Object, "status", "phase")
		if phase != "Completed" {
			c.logger.V(1).Info("Skipping ImageAnalysisResult with non-completed phase",
				"name", name, "phase", phase)
			return
		}
	}

	// Convert to trimmed wire format to minimize payload size
	var obj interface{}
	if eventType == EventTypeDelete {
		// For deletes, send minimal identifying info
		obj = map[string]interface{}{
			"imageDigest": getNestedString(iar.Object, "spec", "imageDigest"),
			"imageRef":    getNestedString(iar.Object, "spec", "imageRef"),
		}
	} else {
		wireFormat, err := ToImageAnalysisWireFormat(iar)
		if err != nil {
			c.logger.Error(err, "Failed to convert ImageAnalysisResult to wire format", "name", name)
			return
		}
		obj = wireFormat
	}

	c.logger.Info("Sending ImageAnalysisResult to control plane",
		"event_type", eventType.String(),
		"name", name,
	)

	c.batchChan <- CollectedResource{
		ResourceType: ImageAnalysisResult,
		Object:       obj,
		Timestamp:    time.Now().UTC(),
		EventType:    eventType,
		Key:          name,
	}
}

// analysisChanged detects meaningful changes in an ImageAnalysisResult.
// Only triggers on new scan results (analyzedAt changed) or phase changes.
func (c *ImageAnalysisResultCollector) analysisChanged(oldIAR, newIAR *unstructured.Unstructured) bool {
	if oldIAR.GetResourceVersion() == newIAR.GetResourceVersion() {
		return false
	}

	// Check if analyzedAt changed (indicates a new scan)
	oldAnalyzedAt, _, _ := unstructured.NestedString(oldIAR.Object, "status", "analyzedAt")
	newAnalyzedAt, _, _ := unstructured.NestedString(newIAR.Object, "status", "analyzedAt")
	if oldAnalyzedAt != newAnalyzedAt {
		return true
	}

	// Check if phase changed
	oldPhase, _, _ := unstructured.NestedString(oldIAR.Object, "status", "phase")
	newPhase, _, _ := unstructured.NestedString(newIAR.Object, "status", "phase")
	return oldPhase != newPhase
}

// Stop gracefully shuts down the collector.
func (c *ImageAnalysisResultCollector) Stop() error {
	c.logger.Info("Stopping ImageAnalysisResult collector")

	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.logger.Info("Closed ImageAnalysisResult collector stop channel")

		c.mu.Lock()
		c.stopped = true
		close(c.batchChan)
		c.batchChan = nil
		c.mu.Unlock()
		c.logger.Info("Closed ImageAnalysisResult collector batch input channel")
	})

	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ImageAnalysisResult collector batcher stopped")
	}

	return nil
}

// GetResourceChannel returns the channel for collected resource batches.
func (c *ImageAnalysisResultCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles.
func (c *ImageAnalysisResultCollector) GetType() string {
	return "image_analysis_result"
}

// IsAvailable checks if the ImageAnalysisResult CRD is installed in the cluster.
func (c *ImageAnalysisResultCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.client.Resource(imageAnalysisResultGVR).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		return true
	}

	if isResourceTypeUnavailableError(err) {
		c.logger.Info("ImageAnalysisResult CRD not installed in cluster", "error", err.Error())
		return false
	}

	c.logger.Error(err, "Error checking ImageAnalysisResult resource availability")
	return false
}

// AddResource manually adds an ImageAnalysisResult resource to be processed.
func (c *ImageAnalysisResultCollector) AddResource(resource interface{}) error {
	iar, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}

	c.handleEvent(iar, EventTypeAdd)
	return nil
}

// getNestedString is a helper to extract a nested string from unstructured data.
func getNestedString(obj map[string]interface{}, fields ...string) string {
	val, _, _ := unstructured.NestedString(obj, fields...)
	return val
}
