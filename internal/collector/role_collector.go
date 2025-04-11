// internal/collector/role_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// RoleCollector watches for Role events and collects Role data
type RoleCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	roleInformer    cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	namespaces      []string
	excludedRoles   map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// ExcludedRole defines a Role to exclude from collection
type ExcludedRole struct {
	Namespace string
	Name      string
}

// NewRoleCollector creates a new collector for Role resources
func NewRoleCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedRoles []ExcludedRole,
	logger logr.Logger,
) *RoleCollector {
	// Convert excluded Roles to a map for quicker lookups
	excludedRolesMap := make(map[types.NamespacedName]bool)
	for _, role := range excludedRoles {
		excludedRolesMap[types.NamespacedName{
			Namespace: role.Namespace,
			Name:      role.Name,
		}] = true
	}

	return &RoleCollector{
		client:        client,
		resourceChan:  make(chan CollectedResource, 100),
		stopCh:        make(chan struct{}),
		namespaces:    namespaces,
		excludedRoles: excludedRolesMap,
		logger:        logger.WithName("role-collector"),
	}
}

// Start begins the Role collection process
func (c *RoleCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Role collector", "namespaces", c.namespaces)

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

	// Create Role informer
	c.roleInformer = c.informerFactory.Rbac().V1().Roles().Informer()

	// Add event handlers
	_, err := c.roleInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			role := obj.(*rbacv1.Role)
			c.handleRoleEvent(role, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRole := oldObj.(*rbacv1.Role)
			newRole := newObj.(*rbacv1.Role)

			// Only handle meaningful updates
			if c.roleChanged(oldRole, newRole) {
				c.handleRoleEvent(newRole, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			role := obj.(*rbacv1.Role)
			c.handleRoleEvent(role, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.roleInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

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

// handleRoleEvent processes Role events
func (c *RoleCollector) handleRoleEvent(role *rbacv1.Role, eventType string) {
	if c.isExcluded(role) {
		return
	}

	c.logger.V(4).Info("Processing Role event",
		"namespace", role.Namespace,
		"name", role.Name,
		"eventType", eventType)

	// Send the raw Role object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: Role,
		Object:       role, // Send the entire Role object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", role.Namespace, role.Name),
	}
}

// roleChanged detects meaningful changes in a Role
func (c *RoleCollector) roleChanged(oldRole, newRole *rbacv1.Role) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldRole.ResourceVersion == newRole.ResourceVersion {
		return false
	}

	// Check for rules changes
	if !reflect.DeepEqual(oldRole.Rules, newRole.Rules) {
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

// isExcluded checks if a Role should be excluded from collection
func (c *RoleCollector) isExcluded(role *rbacv1.Role) bool {
	// Check if monitoring specific namespaces and this Role isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == role.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if Role is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: role.Namespace,
		Name:      role.Name,
	}
	return c.excludedRoles[key]
}

// Stop gracefully shuts down the Role collector
func (c *RoleCollector) Stop() error {
	c.logger.Info("Stopping Role collector")
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *RoleCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *RoleCollector) GetType() string {
	return "role"
}

// IsAvailable checks if Role resources can be accessed in the cluster
func (c *RoleCollector) IsAvailable(ctx context.Context) bool {
	return true
}
