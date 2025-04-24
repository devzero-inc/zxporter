// internal/collector/resourcequota_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ResourceQuotaCollector watches for resourcequota events and collects resourcequota data
type ResourceQuotaCollector struct {
	client                 kubernetes.Interface
	informerFactory        informers.SharedInformerFactory
	resourceQuotaInformer  cache.SharedIndexInformer
	batchChan              chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan           chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                *ResourcesBatcher
	stopCh                 chan struct{}
	namespaces             []string
	excludedResourceQuotas map[types.NamespacedName]bool
	logger                 logr.Logger
	mu                     sync.RWMutex
}

// ExcludedResourceQuota defines a resourcequota to exclude from collection
type ExcludedResourceQuota struct {
	Namespace string
	Name      string
}

// NewResourceQuotaCollector creates a new collector for resourcequota resources
func NewResourceQuotaCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedResourceQuotas []ExcludedResourceQuota,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ResourceQuotaCollector {
	// Convert excluded resourcequotas to a map for quicker lookups
	excludedResourceQuotasMap := make(map[types.NamespacedName]bool)
	for _, rq := range excludedResourceQuotas {
		excludedResourceQuotasMap[types.NamespacedName{
			Namespace: rq.Namespace,
			Name:      rq.Name,
		}] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 50) // Keep lower buffer for infrequent ResourceQuotas
	resourceChan := make(chan []CollectedResource, 50)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &ResourceQuotaCollector{
		client:                 client,
		batchChan:              batchChan,
		resourceChan:           resourceChan,
		batcher:                batcher,
		stopCh:                 make(chan struct{}),
		namespaces:             namespaces,
		excludedResourceQuotas: excludedResourceQuotasMap,
		logger:                 logger.WithName("resourcequota-collector"),
	}
}

// Start begins the resourcequota collection process
func (c *ResourceQuotaCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting resourcequota collector", "namespaces", c.namespaces)

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

	// Create resourcequota informer
	c.resourceQuotaInformer = c.informerFactory.Core().V1().ResourceQuotas().Informer()

	// Add event handlers
	_, err := c.resourceQuotaInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rq := obj.(*corev1.ResourceQuota)
			c.handleResourceQuotaEvent(rq, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRQ := oldObj.(*corev1.ResourceQuota)
			newRQ := newObj.(*corev1.ResourceQuota)

			// Only handle meaningful updates
			if c.resourceQuotaChanged(oldRQ, newRQ) {
				c.handleResourceQuotaEvent(newRQ, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			rq := obj.(*corev1.ResourceQuota)
			c.handleResourceQuotaEvent(rq, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.resourceQuotaInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for ResourceQuotas")
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

// handleResourceQuotaEvent processes resourcequota events
func (c *ResourceQuotaCollector) handleResourceQuotaEvent(rq *corev1.ResourceQuota, eventType string) {
	if c.isExcluded(rq) {
		return
	}

	c.logger.Info("Processing resourcequota event",
		"namespace", rq.Namespace,
		"name", rq.Name,
		"eventType", eventType)

	// Send the raw resourcequota object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ResourceQuota,
		Object:       rq, // Send the entire resourcequota object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", rq.Namespace, rq.Name),
	}
}

// resourceQuotaChanged detects meaningful changes in a resourcequota
func (c *ResourceQuotaCollector) resourceQuotaChanged(oldRQ, newRQ *corev1.ResourceQuota) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldRQ.ResourceVersion == newRQ.ResourceVersion {
		return false
	}

	// Check for spec resource changes
	if !reflect.DeepEqual(oldRQ.Spec.Hard, newRQ.Spec.Hard) {
		return true
	}

	// Check for scope selector changes
	if !reflect.DeepEqual(oldRQ.Spec.ScopeSelector, newRQ.Spec.ScopeSelector) {
		return true
	}

	// Check for scopes changes
	if !reflect.DeepEqual(oldRQ.Spec.Scopes, newRQ.Spec.Scopes) {
		return true
	}

	// Check for status changes
	if !reflect.DeepEqual(oldRQ.Status.Hard, newRQ.Status.Hard) ||
		!reflect.DeepEqual(oldRQ.Status.Used, newRQ.Status.Used) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldRQ.Labels, newRQ.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldRQ.Annotations, newRQ.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a resourcequota should be excluded from collection
func (c *ResourceQuotaCollector) isExcluded(rq *corev1.ResourceQuota) bool {
	// Check if monitoring specific namespaces and this resourcequota isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == rq.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if resourcequota is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: rq.Namespace,
		Name:      rq.Name,
	}
	return c.excludedResourceQuotas[key]
}

// Stop gracefully shuts down the resourcequota collector
func (c *ResourceQuotaCollector) Stop() error {
	c.logger.Info("Stopping resourcequota collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("ResourceQuota collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed resourcequota collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed resourcequota collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ResourceQuota collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ResourceQuotaCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ResourceQuotaCollector) GetType() string {
	return "resource_quota"
}

// IsAvailable checks if ResourceQuota resources can be accessed in the cluster
func (c *ResourceQuotaCollector) IsAvailable(ctx context.Context) bool {
	return true
}
