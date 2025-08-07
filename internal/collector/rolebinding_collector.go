// internal/collector/rolebinding_collector.go
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

// RoleBindingCollector watches for RoleBinding events and collects RoleBinding data
type RoleBindingCollector struct {
	client               kubernetes.Interface
	informerFactory      informers.SharedInformerFactory
	roleBindingInformer  cache.SharedIndexInformer
	batchChan            chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan         chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher              *ResourcesBatcher
	stopCh               chan struct{}
	namespaces           []string
	excludedRoleBindings map[types.NamespacedName]bool
	logger               logr.Logger
	telemetryLogger      telemetry_logger.Logger
	mu                   sync.RWMutex
	cDHelper             ChangeDetectionHelper
}

// NewRoleBindingCollector creates a new collector for RoleBinding resources
func NewRoleBindingCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedRoleBindings []ExcludedRoleBinding,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *RoleBindingCollector {
	// Convert excluded RoleBindings to a map for quicker lookups
	excludedRoleBindingsMap := make(map[types.NamespacedName]bool)
	for _, rb := range excludedRoleBindings {
		excludedRoleBindingsMap[types.NamespacedName{
			Namespace: rb.Namespace,
			Name:      rb.Name,
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

	newLogger := logger.WithName("rolebinding-collector")
	return &RoleBindingCollector{
		client:               client,
		batchChan:            batchChan,
		resourceChan:         resourceChan,
		batcher:              batcher,
		stopCh:               make(chan struct{}),
		namespaces:           namespaces,
		excludedRoleBindings: excludedRoleBindingsMap,
		logger:               newLogger,
		telemetryLogger:      telemetryLogger,
		cDHelper:             ChangeDetectionHelper{logger: newLogger}}
}

// Start begins the RoleBinding collection process
func (c *RoleBindingCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting RoleBinding collector", "namespaces", c.namespaces)

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

	// Create RoleBinding informer
	c.roleBindingInformer = c.informerFactory.Rbac().V1().RoleBindings().Informer()

	// Add event handlers
	_, err := c.roleBindingInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rb := obj.(*rbacv1.RoleBinding)
			c.handleRoleBindingEvent(rb, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRB := oldObj.(*rbacv1.RoleBinding)
			newRB := newObj.(*rbacv1.RoleBinding)

			// Only handle meaningful updates
			if c.roleBindingChanged(oldRB, newRB) {
				c.handleRoleBindingEvent(newRB, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			rb := obj.(*rbacv1.RoleBinding)
			c.handleRoleBindingEvent(rb, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.roleBindingInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for RoleBindings")
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

// handleRoleBindingEvent processes RoleBinding events
func (c *RoleBindingCollector) handleRoleBindingEvent(rb *rbacv1.RoleBinding, eventType EventType) {
	if c.isExcluded(rb) {
		return
	}

	// Send the raw RoleBinding object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: RoleBinding,
		Object:       rb, // Send the entire RoleBinding object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", rb.Namespace, rb.Name),
	}
}

// roleBindingChanged detects meaningful changes in a RoleBinding
func (c *RoleBindingCollector) roleBindingChanged(oldRB, newRB *rbacv1.RoleBinding) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldRB.Name,
		oldRB.ObjectMeta,
		newRB.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for role reference changes
	if oldRB.RoleRef.Kind != newRB.RoleRef.Kind ||
		oldRB.RoleRef.Name != newRB.RoleRef.Name ||
		oldRB.RoleRef.APIGroup != newRB.RoleRef.APIGroup {
		return true
	}

	// Check for subjects changes
	if !reflect.DeepEqual(oldRB.Subjects, newRB.Subjects) {
		return true
	}

	if !reflect.DeepEqual(oldRB.UID, newRB.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a RoleBinding should be excluded from collection
func (c *RoleBindingCollector) isExcluded(rb *rbacv1.RoleBinding) bool {
	// Check if monitoring specific namespaces and this RoleBinding isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == rb.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if RoleBinding is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: rb.Namespace,
		Name:      rb.Name,
	}
	return c.excludedRoleBindings[key]
}

// Stop gracefully shuts down the RoleBinding collector
func (c *RoleBindingCollector) Stop() error {
	c.logger.Info("Stopping RoleBinding collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("RoleBinding collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed RoleBinding collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed RoleBinding collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("RoleBinding collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *RoleBindingCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *RoleBindingCollector) GetType() string {
	return "role_binding"
}

// IsAvailable checks if RoleBinding resources can be accessed in the cluster
func (c *RoleBindingCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a RoleBinding resource to be processed by the collector
func (c *RoleBindingCollector) AddResource(resource interface{}) error {
	roleBinding, ok := resource.(*rbacv1.RoleBinding)
	if !ok {
		return fmt.Errorf("expected *rbacv1.RoleBinding, got %T", resource)
	}

	c.handleRoleBindingEvent(roleBinding, EventTypeAdd)
	return nil
}
