// internal/collector/endpoints_collector.go
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

// EndpointCollector watches for endpoints events and collects endpoints data
type EndpointCollector struct {
	client            kubernetes.Interface
	informerFactory   informers.SharedInformerFactory
	endpointsInformer cache.SharedIndexInformer
	batchChan         chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan      chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher           *ResourcesBatcher
	stopCh            chan struct{}
	namespaces        []string
	excludedEndpoints map[types.NamespacedName]bool
	logger            logr.Logger
	mu                sync.RWMutex
}

// NewEndpointCollector creates a new collector for endpoints resources
func NewEndpointCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedEndpoints []ExcludedEndpoint,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *EndpointCollector {
	// Convert excluded endpoints to a map for quicker lookups
	excludedEndpointsMap := make(map[types.NamespacedName]bool)
	for _, endpoints := range excludedEndpoints {
		excludedEndpointsMap[types.NamespacedName{
			Namespace: endpoints.Namespace,
			Name:      endpoints.Name,
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

	return &EndpointCollector{
		client:            client,
		batchChan:         batchChan,
		resourceChan:      resourceChan,
		batcher:           batcher,
		stopCh:            make(chan struct{}),
		namespaces:        namespaces,
		excludedEndpoints: excludedEndpointsMap,
		logger:            logger.WithName("endpoints-collector"),
	}
}

// Start begins the endpoints collection process
func (c *EndpointCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting endpoints collector", "namespaces", c.namespaces)

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

	// Create endpoints informer
	c.endpointsInformer = c.informerFactory.Core().V1().Endpoints().Informer()

	// Add event handlers
	_, err := c.endpointsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			endpoints := obj.(*corev1.Endpoints)
			c.handleEndpointsEvent(endpoints, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldEndpoints := oldObj.(*corev1.Endpoints)
			newEndpoints := newObj.(*corev1.Endpoints)

			// Only handle meaningful updates
			if c.endpointsChanged(oldEndpoints, newEndpoints) {
				c.handleEndpointsEvent(newEndpoints, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			endpoints := obj.(*corev1.Endpoints)
			c.handleEndpointsEvent(endpoints, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.endpointsInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for Endpoints")
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

// handleEndpointsEvent processes endpoints events
func (c *EndpointCollector) handleEndpointsEvent(endpoints *corev1.Endpoints, eventType EventType) {
	if c.isExcluded(endpoints) {
		return
	}

	c.logger.Info("Processing endpoints event",
		"namespace", endpoints.Namespace,
		"name", endpoints.Name,
		"eventType", eventType.String())

	// Send the raw endpoints object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Endpoints,
		Object:       endpoints, // Send the entire endpoints object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", endpoints.Namespace, endpoints.Name),
	}
}

// endpointsChanged detects meaningful changes in an endpoints object
func (c *EndpointCollector) endpointsChanged(oldEndpoints, newEndpoints *corev1.Endpoints) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldEndpoints.ResourceVersion == newEndpoints.ResourceVersion {
		return false
	}

	// Check for changes in subsets
	if !reflect.DeepEqual(oldEndpoints.Subsets, newEndpoints.Subsets) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldEndpoints.Labels, newEndpoints.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldEndpoints.Annotations, newEndpoints.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if an endpoints object should be excluded from collection
func (c *EndpointCollector) isExcluded(endpoints *corev1.Endpoints) bool {
	// Check if monitoring specific namespaces and this endpoints isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == endpoints.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if endpoints is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: endpoints.Namespace,
		Name:      endpoints.Name,
	}
	return c.excludedEndpoints[key]
}

// Stop gracefully shuts down the endpoints collector
func (c *EndpointCollector) Stop() error {
	c.logger.Info("Stopping endpoints collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Endpoints collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed endpoints collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed endpoints collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Endpoints collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *EndpointCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *EndpointCollector) GetType() string {
	return "endpoints"
}

// IsAvailable checks if Endpoints resources can be accessed in the cluster
func (c *EndpointCollector) IsAvailable(ctx context.Context) bool {
	return true
}
