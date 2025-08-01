// internal/collector/serviceaccount_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ServiceAccountCollector watches for serviceaccount events and collects serviceaccount data
type ServiceAccountCollector struct {
	client                  kubernetes.Interface
	informerFactory         informers.SharedInformerFactory
	serviceAccountInformer  cache.SharedIndexInformer
	batchChan               chan CollectedResource
	resourceChan            chan []CollectedResource
	batcher                 *ResourcesBatcher
	stopCh                  chan struct{}
	namespaces              []string
	excludedServiceAccounts map[types.NamespacedName]bool
	logger                  logr.Logger
	telemetryLogger         telemetry_logger.Logger
	mu                      sync.RWMutex
	cDHelper                ChangeDetectionHelper
}

// NewServiceAccountCollector creates a new collector for serviceaccount resources
func NewServiceAccountCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedServiceAccounts []ExcludedServiceAccount,
	maxBatchSize int, // Added parameter
	maxBatchTime time.Duration, // Added parameter
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *ServiceAccountCollector {
	// Convert excluded serviceaccounts to a map for quicker lookups
	excludedServiceAccountsMap := make(map[types.NamespacedName]bool)
	for _, sa := range excludedServiceAccounts {
		excludedServiceAccountsMap[types.NamespacedName{
			Namespace: sa.Namespace,
			Name:      sa.Name,
		}] = true
	}

	// Create channels
	// batchChan receives individual events from the informer
	batchChan := make(chan CollectedResource, 100)
	// resourceChan receives batches from the ResourcesBatcher
	resourceChan := make(chan []CollectedResource, 100)

	// Create the batcher, passing through the configurable parameters
	batcher := NewResourcesBatcher(
		maxBatchSize, // Use provided parameter
		maxBatchTime, // Use provided parameter
		batchChan,
		resourceChan,
		logger,
	)

	newLogger := logger.WithName("serviceaccount-collector")
	return &ServiceAccountCollector{
		client:                  client,
		batchChan:               batchChan,
		resourceChan:            resourceChan,
		batcher:                 batcher,
		stopCh:                  make(chan struct{}),
		namespaces:              namespaces,
		excludedServiceAccounts: excludedServiceAccountsMap,
		logger:                  newLogger,
		telemetryLogger:         telemetryLogger,
		cDHelper:                ChangeDetectionHelper{logger: newLogger}}
}

// Start begins the serviceaccount collection process
func (c *ServiceAccountCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting serviceaccount collector", "namespaces", c.namespaces)

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

	// Create serviceaccount informer
	c.serviceAccountInformer = c.informerFactory.Core().V1().ServiceAccounts().Informer()

	// Add event handlers
	_, err := c.serviceAccountInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			sa := obj.(*corev1.ServiceAccount)
			c.handleServiceAccountEvent(sa, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldSA := oldObj.(*corev1.ServiceAccount)
			newSA := newObj.(*corev1.ServiceAccount)

			// Only handle meaningful updates
			if c.serviceAccountChanged(oldSA, newSA) {
				c.handleServiceAccountEvent(newSA, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			sa := obj.(*corev1.ServiceAccount)
			c.handleServiceAccountEvent(sa, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.serviceAccountInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for service accounts")
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

// handleServiceAccountEvent processes serviceaccount events
func (c *ServiceAccountCollector) handleServiceAccountEvent(sa *corev1.ServiceAccount, eventType EventType) {
	if c.isExcluded(sa) {
		return
	}

	c.logger.Info("Processing serviceaccount event",
		"namespace", sa.Namespace,
		"name", sa.Name,
		"eventType", eventType.String())

	// Send the raw serviceaccount object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ServiceAccount,
		Object:       sa, // Send the entire serviceaccount object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", sa.Namespace, sa.Name),
	}
}

// serviceAccountChanged detects meaningful changes in a serviceaccount
func (c *ServiceAccountCollector) serviceAccountChanged(oldSA, newSA *corev1.ServiceAccount) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldSA.Name,
		oldSA.ObjectMeta,
		newSA.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for secret reference changes
	if !objectReferencesEqual(oldSA.Secrets, newSA.Secrets) {
		return true
	}

	// Check for automount service account token changes
	if !boolPointerEqual(oldSA.AutomountServiceAccountToken, newSA.AutomountServiceAccountToken) {
		return true
	}

	if !reflect.DeepEqual(oldSA.UID, newSA.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a serviceaccount should be excluded from collection
func (c *ServiceAccountCollector) isExcluded(sa *corev1.ServiceAccount) bool {
	// Check if monitoring specific namespaces and this serviceaccount isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == sa.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if serviceaccount is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: sa.Namespace,
		Name:      sa.Name,
	}
	return c.excludedServiceAccounts[key]
}

// Stop gracefully shuts down the serviceaccount collector
func (c *ServiceAccountCollector) Stop() error {
	c.logger.Info("Stopping serviceaccount collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	// Use a select to prevent double-closing stopCh.
	select {
	case <-c.stopCh:
		// Already closed
		c.logger.Info("ServiceAccount collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed serviceaccount collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher) to signal no more new items.
	// Check if it's nil before closing.
	if c.batchChan != nil {
		// Assuming batchChan is not closed by others. Add recovery if needed.
		close(c.batchChan)
		c.batchChan = nil // Set to nil to prevent further use
		c.logger.Info("Closed serviceaccount collector batch input channel")
	}

	// 3. Stop the batcher. This will wait for the batcher's goroutine
	//    to finish processing remaining items from batchChan. The batcher's
	//    defer function will close resourceChan upon exiting.
	if c.batcher != nil {
		c.batcher.stop() // This blocks until the batcher goroutine exits
		c.logger.Info("ServiceAccount collector batcher stopped")
	}
	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ServiceAccountCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ServiceAccountCollector) GetType() string {
	return "service_account"
}

// IsAvailable checks if ServiceAccount resources can be accessed in the cluster
func (c *ServiceAccountCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a ServiceAccount resource to be processed by the collector
func (c *ServiceAccountCollector) AddResource(resource interface{}) error {
	serviceAccount, ok := resource.(*corev1.ServiceAccount)
	if !ok {
		return fmt.Errorf("expected *corev1.ServiceAccount, got %T", resource)
	}

	c.handleServiceAccountEvent(serviceAccount, EventTypeAdd)
	return nil
}
