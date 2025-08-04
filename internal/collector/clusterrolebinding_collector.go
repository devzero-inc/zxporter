// internal/collector/clusterrolebinding_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ClusterRoleBindingCollector watches for ClusterRoleBinding events and collects ClusterRoleBinding data
type ClusterRoleBindingCollector struct {
	client                      kubernetes.Interface
	informerFactory             informers.SharedInformerFactory
	clusterRoleBindingInformer  cache.SharedIndexInformer
	batchChan                   chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan                chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher                     *ResourcesBatcher
	stopCh                      chan struct{}
	excludedClusterRoleBindings map[string]bool
	logger                      logr.Logger
	mu                          sync.RWMutex
	cDHelper                    ChangeDetectionHelper
}

// NewClusterRoleBindingCollector creates a new collector for ClusterRoleBinding resources
func NewClusterRoleBindingCollector(
	client kubernetes.Interface,
	excludedClusterRoleBindings []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ClusterRoleBindingCollector {
	// Convert excluded ClusterRoleBindings to a map for quicker lookups
	excludedClusterRoleBindingsMap := make(map[string]bool)
	for _, crb := range excludedClusterRoleBindings {
		excludedClusterRoleBindingsMap[crb] = true
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

	newLogger := logger.WithName("clusterrolebinding-collector")
	return &ClusterRoleBindingCollector{
		client:                      client,
		batchChan:                   batchChan,
		resourceChan:                resourceChan,
		batcher:                     batcher,
		stopCh:                      make(chan struct{}),
		excludedClusterRoleBindings: excludedClusterRoleBindingsMap,
		logger:                      newLogger,
		cDHelper:                    ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the ClusterRoleBinding collection process
func (c *ClusterRoleBindingCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting ClusterRoleBinding collector")

	// Create informer factory - ClusterRoleBindings are cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create ClusterRoleBinding informer
	c.clusterRoleBindingInformer = c.informerFactory.Rbac().V1().ClusterRoleBindings().Informer()

	// Add event handlers
	_, err := c.clusterRoleBindingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			crb := obj.(*rbacv1.ClusterRoleBinding)
			c.handleClusterRoleBindingEvent(crb, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCRB := oldObj.(*rbacv1.ClusterRoleBinding)
			newCRB := newObj.(*rbacv1.ClusterRoleBinding)

			// Only handle meaningful updates
			if c.clusterRoleBindingChanged(oldCRB, newCRB) {
				c.handleClusterRoleBindingEvent(newCRB, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			crb := obj.(*rbacv1.ClusterRoleBinding)
			c.handleClusterRoleBindingEvent(crb, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.clusterRoleBindingInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for ClusterRoleBindings")
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

// handleClusterRoleBindingEvent processes ClusterRoleBinding events
func (c *ClusterRoleBindingCollector) handleClusterRoleBindingEvent(crb *rbacv1.ClusterRoleBinding, eventType EventType) {
	if c.isExcluded(crb) {
		return
	}

	c.logger.Info("Processing ClusterRoleBinding event",
		"name", crb.Name,
		"eventType", eventType.String())

	// Send the raw ClusterRoleBinding object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ClusterRoleBinding,
		Object:       crb, // Send the entire ClusterRoleBinding object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          crb.Name, // ClusterRoleBindings are cluster-scoped, so just the name is sufficient
	}
}

// clusterRoleBindingChanged detects meaningful changes in a ClusterRoleBinding
func (c *ClusterRoleBindingCollector) clusterRoleBindingChanged(oldCRB, newCRB *rbacv1.ClusterRoleBinding) bool {

	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldCRB.Name,
		oldCRB.ObjectMeta,
		newCRB.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}
	// Check for role reference changes
	if oldCRB.RoleRef.Kind != newCRB.RoleRef.Kind ||
		oldCRB.RoleRef.Name != newCRB.RoleRef.Name ||
		oldCRB.RoleRef.APIGroup != newCRB.RoleRef.APIGroup {
		return true
	}

	// Check for subjects changes
	if !reflect.DeepEqual(oldCRB.Subjects, newCRB.Subjects) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a ClusterRoleBinding should be excluded from collection
func (c *ClusterRoleBindingCollector) isExcluded(crb *rbacv1.ClusterRoleBinding) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedClusterRoleBindings[crb.Name]
}

// Stop gracefully shuts down the ClusterRoleBinding collector
func (c *ClusterRoleBindingCollector) Stop() error {
	c.logger.Info("Stopping ClusterRoleBinding collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("ClusterRoleBinding collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed ClusterRoleBinding collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed ClusterRoleBinding collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ClusterRoleBinding collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ClusterRoleBindingCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ClusterRoleBindingCollector) GetType() string {
	return "cluster_role_binding"
}

// IsAvailable checks if ClusterRoleBinding resources are available in the cluster
func (c *ClusterRoleBindingCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a ClusterRoleBinding resource to be processed by the collector
func (c *ClusterRoleBindingCollector) AddResource(resource interface{}) error {
	clusterRoleBinding, ok := resource.(*rbacv1.ClusterRoleBinding)
	if !ok {
		return fmt.Errorf("expected *rbacv1.ClusterRoleBinding, got %T", resource)
	}

	c.handleClusterRoleBindingEvent(clusterRoleBinding, EventTypeAdd)
	return nil
}
