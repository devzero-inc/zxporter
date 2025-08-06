// internal/collector/namespace_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NamespaceCollector watches for namespace events and collects namespace data
type NamespaceCollector struct {
	client             kubernetes.Interface
	informerFactory    informers.SharedInformerFactory
	namespaceInformer  cache.SharedIndexInformer
	batchChan          chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan       chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher            *ResourcesBatcher
	stopCh             chan struct{}
	excludedNamespaces map[string]bool
	logger             logr.Logger
	mu                 sync.RWMutex
	cDHelper           ChangeDetectionHelper
}

// NewNamespaceCollector creates a new collector for namespace resources
func NewNamespaceCollector(
	client kubernetes.Interface,
	excludedNamespaces []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *NamespaceCollector {
	// Convert excluded namespaces to a map for quicker lookups
	excludedNamespacesMap := make(map[string]bool)
	for _, namespace := range excludedNamespaces {
		excludedNamespacesMap[namespace] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 50) // Keep lower buffer for infrequent Namespaces
	resourceChan := make(chan []CollectedResource, 50)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	newLogger := logger.WithName("namespace-collector")
	return &NamespaceCollector{
		client:             client,
		batchChan:          batchChan,
		resourceChan:       resourceChan,
		batcher:            batcher,
		stopCh:             make(chan struct{}),
		excludedNamespaces: excludedNamespacesMap,
		logger:             newLogger,
		cDHelper:           ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the namespace collection process
func (c *NamespaceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting namespace collector")

	// Create informer factory - namespace collector always watches all namespaces
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create namespace informer
	c.namespaceInformer = c.informerFactory.Core().V1().Namespaces().Informer()

	// Add event handlers
	_, err := c.namespaceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			namespace := obj.(*corev1.Namespace)
			c.handleNamespaceEvent(namespace, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNamespace := oldObj.(*corev1.Namespace)
			newNamespace := newObj.(*corev1.Namespace)

			// Only handle meaningful updates
			if c.namespaceChanged(oldNamespace, newNamespace) {
				c.handleNamespaceEvent(newNamespace, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			namespace := obj.(*corev1.Namespace)
			c.handleNamespaceEvent(namespace, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.namespaceInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for Namespaces")
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

// handleNamespaceEvent processes namespace events
func (c *NamespaceCollector) handleNamespaceEvent(namespace *corev1.Namespace, eventType EventType) {
	if c.isExcluded(namespace) {
		return
	}

	c.logger.Info("Processing namespace event",
		"name", namespace.Name,
		"eventType", eventType.String())

	// Send the raw namespace object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Namespace,
		Object:       namespace, // Send the entire namespace object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          namespace.Name,
	}
}

// namespaceChanged detects meaningful changes in a namespace
func (c *NamespaceCollector) namespaceChanged(oldNamespace, newNamespace *corev1.Namespace) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldNamespace.Name,
		oldNamespace.ObjectMeta,
		newNamespace.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for status phase changes
	if oldNamespace.Status.Phase != newNamespace.Status.Phase {
		return true
	}

	// Check for changes in conditions
	if len(oldNamespace.Status.Conditions) != len(newNamespace.Status.Conditions) {
		return true
	}

	// Deep check on conditions
	oldConditions := make(map[string]corev1.NamespaceCondition)
	for _, condition := range oldNamespace.Status.Conditions {
		oldConditions[string(condition.Type)] = condition
	}

	for _, newCondition := range newNamespace.Status.Conditions {
		oldCondition, exists := oldConditions[string(newCondition.Type)]
		if !exists || oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return true
		}
	}

	if !reflect.DeepEqual(oldNamespace.UID, newNamespace.UID) {
		return true
	}

	// Check for finalizerName changes
	if !finalizerSlicesEqual(oldNamespace.Spec.Finalizers, newNamespace.Spec.Finalizers) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a namespace should be excluded from collection
func (c *NamespaceCollector) isExcluded(namespace *corev1.Namespace) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedNamespaces[namespace.Name]
}

// Stop gracefully shuts down the namespace collector
func (c *NamespaceCollector) Stop() error {
	c.logger.Info("Stopping namespace collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Namespace collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed namespace collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed namespace collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Namespace collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *NamespaceCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *NamespaceCollector) GetType() string {
	return "namespace"
}

// IsAvailable checks if Namespace resources can be accessed in the cluster
func (c *NamespaceCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a namespace resource to be processed by the collector
func (c *NamespaceCollector) AddResource(resource interface{}) error {
	namespace, ok := resource.(*corev1.Namespace)
	if !ok {
		return fmt.Errorf("expected *corev1.Namespace, got %T", resource)
	}

	c.handleNamespaceEvent(namespace, EventTypeAdd)
	return nil
}
