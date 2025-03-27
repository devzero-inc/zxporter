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
	resourceChan                chan CollectedResource
	stopCh                      chan struct{}
	excludedClusterRoleBindings map[string]bool
	logger                      logr.Logger
	mu                          sync.RWMutex
}

// NewClusterRoleBindingCollector creates a new collector for ClusterRoleBinding resources
func NewClusterRoleBindingCollector(
	client kubernetes.Interface,
	excludedClusterRoleBindings []string,
	logger logr.Logger,
) *ClusterRoleBindingCollector {
	// Convert excluded ClusterRoleBindings to a map for quicker lookups
	excludedClusterRoleBindingsMap := make(map[string]bool)
	for _, crb := range excludedClusterRoleBindings {
		excludedClusterRoleBindingsMap[crb] = true
	}

	return &ClusterRoleBindingCollector{
		client:                      client,
		resourceChan:                make(chan CollectedResource, 100),
		stopCh:                      make(chan struct{}),
		excludedClusterRoleBindings: excludedClusterRoleBindingsMap,
		logger:                      logger.WithName("clusterrolebinding-collector"),
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
			c.handleClusterRoleBindingEvent(crb, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCRB := oldObj.(*rbacv1.ClusterRoleBinding)
			newCRB := newObj.(*rbacv1.ClusterRoleBinding)

			// Only handle meaningful updates
			if c.clusterRoleBindingChanged(oldCRB, newCRB) {
				c.handleClusterRoleBindingEvent(newCRB, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			crb := obj.(*rbacv1.ClusterRoleBinding)
			c.handleClusterRoleBindingEvent(crb, "delete")
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

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleClusterRoleBindingEvent processes ClusterRoleBinding events
func (c *ClusterRoleBindingCollector) handleClusterRoleBindingEvent(crb *rbacv1.ClusterRoleBinding, eventType string) {
	if c.isExcluded(crb) {
		return
	}

	c.logger.V(4).Info("Processing ClusterRoleBinding event",
		"name", crb.Name,
		"eventType", eventType)

	// Send the raw ClusterRoleBinding object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: ClusterRoleBinding,
		Object:       crb, // Send the entire ClusterRoleBinding object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          crb.Name, // ClusterRoleBindings are cluster-scoped, so just the name is sufficient
	}
}

// clusterRoleBindingChanged detects meaningful changes in a ClusterRoleBinding
func (c *ClusterRoleBindingCollector) clusterRoleBindingChanged(oldCRB, newCRB *rbacv1.ClusterRoleBinding) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldCRB.ResourceVersion == newCRB.ResourceVersion {
		return false
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

	// Check for label changes
	if !mapsEqual(oldCRB.Labels, newCRB.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldCRB.Annotations, newCRB.Annotations) {
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ClusterRoleBindingCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ClusterRoleBindingCollector) GetType() string {
	return "clusterrolebinding"
}

// IsAvailable checks if ClusterRoleBinding resources are available in the cluster
func (c *ClusterRoleBindingCollector) IsAvailable(ctx context.Context) bool {
	return true
}
