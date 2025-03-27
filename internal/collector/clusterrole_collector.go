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
	resourceChan         chan CollectedResource
	stopCh               chan struct{}
	excludedClusterRoles map[string]bool
	logger               logr.Logger
	mu                   sync.RWMutex
}

// NewClusterRoleCollector creates a new collector for ClusterRole resources
func NewClusterRoleCollector(
	client kubernetes.Interface,
	excludedClusterRoles []string,
	logger logr.Logger,
) *ClusterRoleCollector {
	// Convert excluded ClusterRoles to a map for quicker lookups
	excludedClusterRolesMap := make(map[string]bool)
	for _, role := range excludedClusterRoles {
		excludedClusterRolesMap[role] = true
	}

	return &ClusterRoleCollector{
		client:               client,
		resourceChan:         make(chan CollectedResource, 100),
		stopCh:               make(chan struct{}),
		excludedClusterRoles: excludedClusterRolesMap,
		logger:               logger.WithName("clusterrole-collector"),
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
			c.handleClusterRoleEvent(role, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRole := oldObj.(*rbacv1.ClusterRole)
			newRole := newObj.(*rbacv1.ClusterRole)

			// Only handle meaningful updates
			if c.clusterRoleChanged(oldRole, newRole) {
				c.handleClusterRoleEvent(newRole, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			role := obj.(*rbacv1.ClusterRole)
			c.handleClusterRoleEvent(role, "delete")
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

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleClusterRoleEvent processes ClusterRole events
func (c *ClusterRoleCollector) handleClusterRoleEvent(role *rbacv1.ClusterRole, eventType string) {
	if c.isExcluded(role) {
		return
	}

	c.logger.V(4).Info("Processing ClusterRole event",
		"name", role.Name,
		"eventType", eventType)

	// Send the raw ClusterRole object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: ClusterRole,
		Object:       role, // Send the entire ClusterRole object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          role.Name, // ClusterRoles are cluster-scoped, so just the name is sufficient
	}
}

// clusterRoleChanged detects meaningful changes in a ClusterRole
func (c *ClusterRoleCollector) clusterRoleChanged(oldRole, newRole *rbacv1.ClusterRole) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldRole.ResourceVersion == newRole.ResourceVersion {
		return false
	}

	// Check for rules changes
	if !reflect.DeepEqual(oldRole.Rules, newRole.Rules) {
		return true
	}

	// Check for aggregation rule changes
	if !reflect.DeepEqual(oldRole.AggregationRule, newRole.AggregationRule) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldRole.Labels, newRole.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldRole.Annotations, newRole.Annotations) {
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ClusterRoleCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ClusterRoleCollector) GetType() string {
	return "clusterrole"
}

// IsAvailable checks if ClusterRole resources are available in the cluster
func (c *ClusterRoleCollector) IsAvailable(ctx context.Context) bool {
	return true
}
