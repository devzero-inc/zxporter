// internal/collector/rolebinding_collector.go
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

// RoleBindingCollector watches for RoleBinding events and collects RoleBinding data
type RoleBindingCollector struct {
	client               kubernetes.Interface
	informerFactory      informers.SharedInformerFactory
	roleBindingInformer  cache.SharedIndexInformer
	resourceChan         chan CollectedResource
	stopCh               chan struct{}
	namespaces           []string
	excludedRoleBindings map[types.NamespacedName]bool
	logger               logr.Logger
	mu                   sync.RWMutex
}

// ExcludedRoleBinding defines a RoleBinding to exclude from collection
type ExcludedRoleBinding struct {
	Namespace string
	Name      string
}

// NewRoleBindingCollector creates a new collector for RoleBinding resources
func NewRoleBindingCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedRoleBindings []ExcludedRoleBinding,
	logger logr.Logger,
) *RoleBindingCollector {
	// Convert excluded RoleBindings to a map for quicker lookups
	excludedRoleBindingsMap := make(map[types.NamespacedName]bool)
	for _, rb := range excludedRoleBindings {
		excludedRoleBindingsMap[types.NamespacedName{
			Namespace: rb.Namespace,
			Name:      rb.Name,
		}] = true
	}

	return &RoleBindingCollector{
		client:               client,
		resourceChan:         make(chan CollectedResource, 100),
		stopCh:               make(chan struct{}),
		namespaces:           namespaces,
		excludedRoleBindings: excludedRoleBindingsMap,
		logger:               logger.WithName("rolebinding-collector"),
	}
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
			c.handleRoleBindingEvent(rb, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRB := oldObj.(*rbacv1.RoleBinding)
			newRB := newObj.(*rbacv1.RoleBinding)

			// Only handle meaningful updates
			if c.roleBindingChanged(oldRB, newRB) {
				c.handleRoleBindingEvent(newRB, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			rb := obj.(*rbacv1.RoleBinding)
			c.handleRoleBindingEvent(rb, "delete")
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

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleRoleBindingEvent processes RoleBinding events
func (c *RoleBindingCollector) handleRoleBindingEvent(rb *rbacv1.RoleBinding, eventType string) {
	if c.isExcluded(rb) {
		return
	}

	c.logger.V(4).Info("Processing RoleBinding event",
		"namespace", rb.Namespace,
		"name", rb.Name,
		"eventType", eventType)

	// Send the raw RoleBinding object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: RoleBinding,
		Object:       rb, // Send the entire RoleBinding object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", rb.Namespace, rb.Name),
	}
}

// roleBindingChanged detects meaningful changes in a RoleBinding
func (c *RoleBindingCollector) roleBindingChanged(oldRB, newRB *rbacv1.RoleBinding) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldRB.ResourceVersion == newRB.ResourceVersion {
		return false
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

	// Check for label changes
	if !mapsEqual(oldRB.Labels, newRB.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldRB.Annotations, newRB.Annotations) {
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *RoleBindingCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *RoleBindingCollector) GetType() string {
	return "rolebinding"
}
