// internal/collector/role_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
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
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	excludedRoles   map[types.NamespacedName]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
	cDHelper        ChangeDetectionHelper
}

// NewRoleCollector creates a new collector for Role resources
func NewRoleCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedRoles []ExcludedRole,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *RoleCollector {
	// Convert excluded Roles to a map for quicker lookups
	excludedRolesMap := make(map[types.NamespacedName]bool)
	for _, role := range excludedRoles {
		excludedRolesMap[types.NamespacedName{
			Namespace: role.Namespace,
			Name:      role.Name,
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

	newLogger := logger.WithName("role-collector")
	return &RoleCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		excludedRoles:   excludedRolesMap,
		logger:          newLogger,
		telemetryLogger: telemetryLogger,
		cDHelper:        ChangeDetectionHelper{logger: newLogger}}
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
			c.handleRoleEvent(role, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRole := oldObj.(*rbacv1.Role)
			newRole := newObj.(*rbacv1.Role)

			// Only handle meaningful updates
			if c.roleChanged(oldRole, newRole) {
				c.handleRoleEvent(newRole, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			role := obj.(*rbacv1.Role)
			c.handleRoleEvent(role, EventTypeDelete)
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

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for Roles")
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

// handleRoleEvent processes Role events
func (c *RoleCollector) handleRoleEvent(role *rbacv1.Role, eventType EventType) {
	if c.isExcluded(role) {
		return
	}

	c.logger.Info("Processing Role event",
		"namespace", role.Namespace,
		"name", role.Name,
		"eventType", eventType.String())

	// Send the raw Role object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Role,
		Object:       role, // Send the entire Role object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", role.Namespace, role.Name),
	}
}

// roleChanged detects meaningful changes in a Role
func (c *RoleCollector) roleChanged(oldRole, newRole *rbacv1.Role) bool {
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

	if !reflect.DeepEqual(oldRole.UID, newRole.UID) {
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

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Role collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Role collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Role collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Role collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *RoleCollector) GetResourceChannel() <-chan []CollectedResource {
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

// AddResource manually adds a Role resource to be processed by the collector
func (c *RoleCollector) AddResource(resource interface{}) error {
	role, ok := resource.(*rbacv1.Role)
	if !ok {
		return fmt.Errorf("expected *rbacv1.Role, got %T", resource)
	}

	c.handleRoleEvent(role, EventTypeAdd)
	return nil
}
