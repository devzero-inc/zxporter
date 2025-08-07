// internal/collector/horizontalpodautoscaler_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// HorizontalPodAutoscalerCollector watches for HPA events and collects HPA data
type HorizontalPodAutoscalerCollector struct {
	client                          kubernetes.Interface
	informerFactory                 informers.SharedInformerFactory
	horizontalPodAutoscalerInformer cache.SharedIndexInformer
	batchChan                       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan                    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                         *ResourcesBatcher
	stopCh                          chan struct{}
	namespaces                      []string
	excludedHPAs                    map[types.NamespacedName]bool
	logger                          logr.Logger
	telemetryLogger                 telemetry_logger.Logger
	mu                              sync.RWMutex
	cDHelper                        ChangeDetectionHelper
}

// NewHorizontalPodAutoscalerCollector creates a new collector for HPA resources
func NewHorizontalPodAutoscalerCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedHPAs []ExcludedHPA,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *HorizontalPodAutoscalerCollector {
	// Convert excluded HPAs to a map for quicker lookups
	excludedHPAsMap := make(map[types.NamespacedName]bool)
	for _, hpa := range excludedHPAs {
		excludedHPAsMap[types.NamespacedName{
			Namespace: hpa.Namespace,
			Name:      hpa.Name,
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

	newLogger := logger.WithName("hpa-collector")
	return &HorizontalPodAutoscalerCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		excludedHPAs:    excludedHPAsMap,
		logger:          newLogger,
		telemetryLogger: telemetryLogger,
		cDHelper:        ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the HPA collection process
func (c *HorizontalPodAutoscalerCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting HorizontalPodAutoscaler collector", "namespaces", c.namespaces)

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

	// Create HPA informer - use v2 version which is more feature-rich
	c.horizontalPodAutoscalerInformer = c.informerFactory.Autoscaling().V2().HorizontalPodAutoscalers().Informer()

	// Add event handlers
	_, err := c.horizontalPodAutoscalerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
			c.handleHPAEvent(hpa, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldHPA := oldObj.(*autoscalingv2.HorizontalPodAutoscaler)
			newHPA := newObj.(*autoscalingv2.HorizontalPodAutoscaler)

			// Only handle meaningful updates
			if c.hpaChanged(oldHPA, newHPA) {
				c.handleHPAEvent(newHPA, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
			c.handleHPAEvent(hpa, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.horizontalPodAutoscalerInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for HPAs")
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

// handleHPAEvent processes HPA events
func (c *HorizontalPodAutoscalerCollector) handleHPAEvent(hpa *autoscalingv2.HorizontalPodAutoscaler, eventType EventType) {
	if c.isExcluded(hpa) {
		return
	}

	// Send the raw HPA object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: HorizontalPodAutoscaler,
		Object:       hpa, // Send the entire HPA object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", hpa.Namespace, hpa.Name),
	}
}

// hpaChanged detects meaningful changes in a HPA
func (c *HorizontalPodAutoscalerCollector) hpaChanged(oldHPA, newHPA *autoscalingv2.HorizontalPodAutoscaler) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldHPA.Name,
		oldHPA.ObjectMeta,
		newHPA.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for min/max replicas changes
	if oldHPA.Spec.MinReplicas != newHPA.Spec.MinReplicas ||
		oldHPA.Spec.MaxReplicas != newHPA.Spec.MaxReplicas {
		return true
	}

	// Check for target reference changes
	if oldHPA.Spec.ScaleTargetRef.Kind != newHPA.Spec.ScaleTargetRef.Kind ||
		oldHPA.Spec.ScaleTargetRef.Name != newHPA.Spec.ScaleTargetRef.Name ||
		oldHPA.Spec.ScaleTargetRef.APIVersion != newHPA.Spec.ScaleTargetRef.APIVersion {
		return true
	}

	// Check for metric specification changes
	if !reflect.DeepEqual(oldHPA.Spec.Metrics, newHPA.Spec.Metrics) {
		return true
	}

	// Check for behavior changes
	if !reflect.DeepEqual(oldHPA.Spec.Behavior, newHPA.Spec.Behavior) {
		return true
	}

	// Check for status changes
	if oldHPA.Status.CurrentReplicas != newHPA.Status.CurrentReplicas ||
		oldHPA.Status.DesiredReplicas != newHPA.Status.DesiredReplicas {
		return true
	}

	// Check for current metric changes
	if !reflect.DeepEqual(oldHPA.Status.CurrentMetrics, newHPA.Status.CurrentMetrics) {
		return true
	}

	// Check for condition changes
	if !reflect.DeepEqual(oldHPA.Status.Conditions, newHPA.Status.Conditions) {
		return true
	}

	if !reflect.DeepEqual(oldHPA.UID, newHPA.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a HPA should be excluded from collection
func (c *HorizontalPodAutoscalerCollector) isExcluded(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	// Check if monitoring specific namespaces and this HPA isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == hpa.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if HPA is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: hpa.Namespace,
		Name:      hpa.Name,
	}
	return c.excludedHPAs[key]
}

// Stop gracefully shuts down the HPA collector
func (c *HorizontalPodAutoscalerCollector) Stop() error {
	c.logger.Info("Stopping HorizontalPodAutoscaler collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("HPA collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed HPA collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed HPA collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("HPA collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *HorizontalPodAutoscalerCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *HorizontalPodAutoscalerCollector) GetType() string {
	return "horizontal_pod_autoscaler"
}

// IsAvailable checks if HPA v2 resources can be accessed in the cluster
func (c *HorizontalPodAutoscalerCollector) IsAvailable(ctx context.Context) bool {
	// Try to list HPAs with a limit of 1 to check availability with minimal overhead
	_, err := c.client.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{
		Limit: 1,
	})

	if err != nil {
		// Check if this is a "resource not found" type error
		if strings.Contains(err.Error(),
			"the server could not find the requested resource") {
			c.logger.Info("HorizontalPodAutoscaler v2 API not available in cluster")
			return false
		}

		// For other errors (permissions, etc), log the error
		c.logger.Error(err, "Error checking HorizontalPodAutoscaler availability")
		return false
	}

	return true
}

// AddResource manually adds a HorizontalPodAutoscaler resource to be processed by the collector
func (c *HorizontalPodAutoscalerCollector) AddResource(resource interface{}) error {
	hpa, ok := resource.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok {
		return fmt.Errorf("expected *autoscalingv2.HorizontalPodAutoscaler, got %T", resource)
	}

	c.handleHPAEvent(hpa, EventTypeAdd)
	return nil
}
