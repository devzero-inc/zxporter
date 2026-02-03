// internal/collector/ingressclass_collector.go
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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// IngressClassCollector watches for IngressClass events and collects IngressClass data
type IngressClassCollector struct {
	client                 kubernetes.Interface
	informerFactory        informers.SharedInformerFactory
	ingressClassInformer   cache.SharedIndexInformer
	batchChan              chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan           chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                *ResourcesBatcher
	stopCh                 chan struct{}
	excludedIngressClasses map[string]bool
	logger                 logr.Logger
	telemetryLogger        telemetry_logger.Logger
	mu                     sync.RWMutex
	cDHelper               ChangeDetectionHelper
}

// NewIngressClassCollector creates a new collector for IngressClass resources
func NewIngressClassCollector(
	client kubernetes.Interface,
	excludedIngressClasses []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *IngressClassCollector {
	// Convert excluded IngressClasses to a map for quicker lookups
	excludedIngressClassesMap := make(map[string]bool)
	for _, class := range excludedIngressClasses {
		excludedIngressClassesMap[class] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 50) // Keep lower buffer for infrequent IngressClasses
	resourceChan := make(chan []CollectedResource, 50)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	newLogger := logger.WithName("ingressclass-collector")
	return &IngressClassCollector{
		client:                 client,
		batchChan:              batchChan,
		resourceChan:           resourceChan,
		batcher:                batcher,
		stopCh:                 make(chan struct{}),
		excludedIngressClasses: excludedIngressClassesMap,
		logger:                 newLogger,
		telemetryLogger:        telemetryLogger,
		cDHelper:               ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the IngressClass collection process
func (c *IngressClassCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting IngressClass collector")

	// Create informer factory - IngressClass is cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create IngressClass informer
	c.ingressClassInformer = c.informerFactory.Networking().V1().IngressClasses().Informer()

	// Add event handlers
	_, err := c.ingressClassInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ingressClass := obj.(*networkingv1.IngressClass)
			c.handleIngressClassEvent(ingressClass, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldIngressClass := oldObj.(*networkingv1.IngressClass)
			newIngressClass := newObj.(*networkingv1.IngressClass)

			// Only handle meaningful updates
			if c.ingressClassChanged(oldIngressClass, newIngressClass) {
				c.handleIngressClassEvent(newIngressClass, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ingressClass := obj.(*networkingv1.IngressClass)
			c.handleIngressClassEvent(ingressClass, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.ingressClassInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for IngressClasses")
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

// handleIngressClassEvent processes IngressClass events
func (c *IngressClassCollector) handleIngressClassEvent(ingressClass *networkingv1.IngressClass, eventType EventType) {
	if c.isExcluded(ingressClass) {
		return
	}

	// Send the raw IngressClass object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: IngressClass,
		Object:       ingressClass, // Send the entire IngressClass object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          ingressClass.Name, // IngressClass is cluster-scoped, so just the name is sufficient
	}
}

// ingressClassChanged detects meaningful changes in an IngressClass
func (c *IngressClassCollector) ingressClassChanged(oldIngressClass, newIngressClass *networkingv1.IngressClass) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldIngressClass.Name,
		oldIngressClass.ObjectMeta,
		newIngressClass.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for controller changes
	if oldIngressClass.Spec.Controller != newIngressClass.Spec.Controller {
		return true
	}

	// Check for parameters changes
	if (oldIngressClass.Spec.Parameters == nil) != (newIngressClass.Spec.Parameters == nil) {
		return true
	}

	if oldIngressClass.Spec.Parameters != nil && newIngressClass.Spec.Parameters != nil {
		if oldIngressClass.Spec.Parameters.APIGroup != newIngressClass.Spec.Parameters.APIGroup ||
			oldIngressClass.Spec.Parameters.Kind != newIngressClass.Spec.Parameters.Kind ||
			oldIngressClass.Spec.Parameters.Name != newIngressClass.Spec.Parameters.Name ||
			oldIngressClass.Spec.Parameters.Namespace != newIngressClass.Spec.Parameters.Namespace {
			return true
		}
	}

	if !reflect.DeepEqual(oldIngressClass.UID, newIngressClass.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if an IngressClass should be excluded from collection
func (c *IngressClassCollector) isExcluded(ingressClass *networkingv1.IngressClass) bool {
	// Check if IngressClass is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedIngressClasses[ingressClass.Name]
}

// Stop gracefully shuts down the IngressClass collector
func (c *IngressClassCollector) Stop() error {
	c.logger.Info("Stopping IngressClass collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("IngressClass collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed IngressClass collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed IngressClass collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("IngressClass collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *IngressClassCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *IngressClassCollector) GetType() string {
	return "ingress_class"
}

// IsAvailable checks if IngressClass resources can be accessed in the cluster
func (c *IngressClassCollector) IsAvailable(ctx context.Context) bool {
	// Try to list IngressClasses with a limit of 1 to check availability with minimal overhead
	_, err := c.client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{
		Limit: 1,
	})
	if err != nil {
		// Check if this is a "resource not found" type error
		if strings.Contains(err.Error(),
			"the server could not find the requested resource") {
			c.logger.Info("IngressClass API not available in cluster (requires Kubernetes 1.19+)")
			return false
		}

		// For other errors (permissions, etc), log the error
		c.logger.Error(err, "Error checking IngressClass availability")
		return false
	}

	return true
}

// AddResource manually adds a IngressClass resource to be processed by the collector
func (c *IngressClassCollector) AddResource(resource interface{}) error {
	ingressClass, ok := resource.(*networkingv1.IngressClass)
	if !ok {
		return fmt.Errorf("expected *networkingv1.IngressClass, got %T", resource)
	}

	c.handleIngressClassEvent(ingressClass, EventTypeAdd)
	return nil
}
