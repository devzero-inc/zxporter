// internal/collector/clusterrole_collector.go
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

// ClusterRoleCollector watches for ClusterRole events and collects ClusterRole data
type ClusterRoleCollector struct {
	client               kubernetes.Interface
	informerFactory      informers.SharedInformerFactory
	clusterRoleInformer  cache.SharedIndexInformer
	batchChan            chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan         chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher              *ResourcesBatcher
	stopCh               chan struct{}
	excludedClusterRoles map[string]bool
	logger               logr.Logger
	mu                   sync.RWMutex
	cDHelper             ChangeDetectionHelper
}

// NewClusterRoleCollector creates a new collector for ClusterRole resources
func NewClusterRoleCollector(
	client kubernetes.Interface,
	excludedClusterRoles []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ClusterRoleCollector {
	// Convert excluded ClusterRoles to a map for quicker lookups
	excludedClusterRolesMap := make(map[string]bool)
	for _, role := range excludedClusterRoles {
		excludedClusterRolesMap[role] = true
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

	newLogger := logger.WithName("clusterrole-collector")
	return &ClusterRoleCollector{
		client:               client,
		batchChan:            batchChan,
		resourceChan:         resourceChan,
		batcher:              batcher,
		stopCh:               make(chan struct{}),
		excludedClusterRoles: excludedClusterRolesMap,
		logger:               newLogger,
		cDHelper:             ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the ClusterRole collection process
func (c *ClusterRoleCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting ClusterRole collector")

	// Create informer factory - ClusterRoles are cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create ClusterRole informer
	c.clusterRoleInformer = c.informerFactory.Rbac().V1().ClusterRoles().Informer()

	// Add event handlers
	_, err := c.clusterRoleInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			role := obj.(*rbacv1.ClusterRole)
			c.handleClusterRoleEvent(role, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRole := oldObj.(*rbacv1.ClusterRole)
			newRole := newObj.(*rbacv1.ClusterRole)

			// Only handle meaningful updates
			if c.clusterRoleChanged(oldRole, newRole) {
				c.handleClusterRoleEvent(newRole, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			role := obj.(*rbacv1.ClusterRole)
			c.handleClusterRoleEvent(role, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.clusterRoleInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for ClusterRoles")
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

// handleClusterRoleEvent processes ClusterRole events
func (c *ClusterRoleCollector) handleClusterRoleEvent(role *rbacv1.ClusterRole, eventType EventType) {
	if c.isExcluded(role) {
		return
	}

	c.logger.Info("Processing ClusterRole event",
		"name", role.Name,
		"eventType", eventType.String())

	// Send the raw ClusterRole object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ClusterRole,
		Object:       role, // Send the entire ClusterRole object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          role.Name, // ClusterRoles are cluster-scoped, so just the name is sufficient
	}
}

// clusterRoleChanged detects meaningful changes in a ClusterRole
func (c *ClusterRoleCollector) clusterRoleChanged(oldRole, newRole *rbacv1.ClusterRole) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldRole.Name,
		oldRole.ObjectMeta,
		newRole.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for rules changes
	if !reflect.DeepEqual(oldRole.Rules, newRole.Rules) {
		return true
	}

	// Check for aggregation rule changes
	if !reflect.DeepEqual(oldRole.AggregationRule, newRole.AggregationRule) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a ClusterRole should be excluded from collection
func (c *ClusterRoleCollector) isExcluded(role *rbacv1.ClusterRole) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedClusterRoles[role.Name]
}

// Stop gracefully shuts down the ClusterRole collector
func (c *ClusterRoleCollector) Stop() error {
	c.logger.Info("Stopping ClusterRole collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("ClusterRole collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed ClusterRole collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed ClusterRole collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ClusterRole collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ClusterRoleCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ClusterRoleCollector) GetType() string {
	return "cluster_role"
}

// IsAvailable checks if ClusterRole resources are available in the cluster
func (c *ClusterRoleCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a cluster role resource to be processed by the collector
func (c *ClusterRoleCollector) AddResource(resource interface{}) error {
	clusterRole, ok := resource.(*rbacv1.ClusterRole)
	if !ok {
		return fmt.Errorf("expected *rbacv1.ClusterRole, got %T", resource)
	}

	c.handleClusterRoleEvent(clusterRole, EventTypeAdd)
	return nil
}
