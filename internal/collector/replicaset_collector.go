// internal/collector/replicaset_collector.go
package collector

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ReplicaSetCollector watches for replicaset events and collects replicaset data
type ReplicaSetCollector struct {
	client              kubernetes.Interface
	informerFactory     informers.SharedInformerFactory
	replicaSetInformer  cache.SharedIndexInformer
	resourceChan        chan CollectedResource
	stopCh              chan struct{}
	namespaces          []string
	excludedReplicaSets map[types.NamespacedName]bool
	logger              logr.Logger
	mu                  sync.RWMutex
}

// ExcludedReplicaSet defines a replicaset to exclude from collection
type ExcludedReplicaSet struct {
	Namespace string
	Name      string
}

// NewReplicaSetCollector creates a new collector for replicaset resources
func NewReplicaSetCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedReplicaSets []ExcludedReplicaSet,
	logger logr.Logger,
) *ReplicaSetCollector {
	// Convert excluded replicasets to a map for quicker lookups
	excludedReplicaSetsMap := make(map[types.NamespacedName]bool)
	for _, replicaset := range excludedReplicaSets {
		excludedReplicaSetsMap[types.NamespacedName{
			Namespace: replicaset.Namespace,
			Name:      replicaset.Name,
		}] = true
	}

	return &ReplicaSetCollector{
		client:              client,
		resourceChan:        make(chan CollectedResource, 100),
		stopCh:              make(chan struct{}),
		namespaces:          namespaces,
		excludedReplicaSets: excludedReplicaSetsMap,
		logger:              logger.WithName("replicaset-collector"),
	}
}

// Start begins the replicaset collection process
func (c *ReplicaSetCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting replicaset collector", "namespaces", c.namespaces)

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

	// Create replicaset informer
	c.replicaSetInformer = c.informerFactory.Apps().V1().ReplicaSets().Informer()

	// Add event handlers
	_, err := c.replicaSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			replicaset := obj.(*appsv1.ReplicaSet)
			c.handleReplicaSetEvent(replicaset, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldReplicaSet := oldObj.(*appsv1.ReplicaSet)
			newReplicaSet := newObj.(*appsv1.ReplicaSet)

			// Only handle meaningful updates
			if c.replicaSetChanged(oldReplicaSet, newReplicaSet) {
				c.handleReplicaSetEvent(newReplicaSet, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			replicaset := obj.(*appsv1.ReplicaSet)
			c.handleReplicaSetEvent(replicaset, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.replicaSetInformer.HasSynced) {
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

// handleReplicaSetEvent processes replicaset events
func (c *ReplicaSetCollector) handleReplicaSetEvent(replicaset *appsv1.ReplicaSet, eventType string) {
	if c.isExcluded(replicaset) {
		return
	}

	c.logger.Info("Processing replicaset event",
		"namespace", replicaset.Namespace,
		"name", replicaset.Name,
		"eventType", eventType)

	// Send the raw replicaset object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: ReplicaSet,
		Object:       replicaset, // Send the entire replicaset object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", replicaset.Namespace, replicaset.Name),
	}
}

// replicaSetChanged detects meaningful changes in a replicaset
func (c *ReplicaSetCollector) replicaSetChanged(oldReplicaSet, newReplicaSet *appsv1.ReplicaSet) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldReplicaSet.ResourceVersion == newReplicaSet.ResourceVersion {
		return false
	}

	// Check if replicas changed
	if oldReplicaSet.Spec.Replicas == nil || newReplicaSet.Spec.Replicas == nil {
		return true
	}

	if *oldReplicaSet.Spec.Replicas != *newReplicaSet.Spec.Replicas {
		return true
	}

	// Check for status changes
	if oldReplicaSet.Status.Replicas != newReplicaSet.Status.Replicas ||
		oldReplicaSet.Status.AvailableReplicas != newReplicaSet.Status.AvailableReplicas ||
		oldReplicaSet.Status.ReadyReplicas != newReplicaSet.Status.ReadyReplicas ||
		oldReplicaSet.Status.FullyLabeledReplicas != newReplicaSet.Status.FullyLabeledReplicas {
		return true
	}

	// Check for generation changes
	if oldReplicaSet.Generation != newReplicaSet.Generation ||
		oldReplicaSet.Status.ObservedGeneration != newReplicaSet.Status.ObservedGeneration {
		return true
	}

	// Check for owner reference changes (could indicate adoption or orphaning)
	if len(oldReplicaSet.OwnerReferences) != len(newReplicaSet.OwnerReferences) {
		return true
	}

	// Deep check on owner references
	for i, oldRef := range oldReplicaSet.OwnerReferences {
		if i >= len(newReplicaSet.OwnerReferences) {
			return true
		}

		newRef := newReplicaSet.OwnerReferences[i]
		if oldRef.Kind != newRef.Kind ||
			oldRef.Name != newRef.Name ||
			oldRef.UID != newRef.UID ||
			oldRef.Controller != newRef.Controller {
			return true
		}
	}

	// Check for label changes which could affect pod selection
	if !mapsEqual(oldReplicaSet.Labels, newReplicaSet.Labels) {
		return true
	}

	// Check for selector changes
	if !metaLabelsEqual(oldReplicaSet.Spec.Selector, newReplicaSet.Spec.Selector) {
		return true
	}

	// No significant changes detected
	return false
}

// metaLabelsEqual compares two label selectors for equality
func metaLabelsEqual(s1, s2 *metav1.LabelSelector) bool {
	if s1 == nil && s2 == nil {
		return true
	}

	if s1 == nil || s2 == nil {
		return false
	}

	// Check match labels
	if !mapsEqual(s1.MatchLabels, s2.MatchLabels) {
		return false
	}

	// Check match expressions
	if len(s1.MatchExpressions) != len(s2.MatchExpressions) {
		return false
	}

	// Create a string representation of each expression for comparison
	// This is simpler than comparing each field individually
	expr1 := make([]string, len(s1.MatchExpressions))
	expr2 := make([]string, len(s2.MatchExpressions))

	for i, exp := range s1.MatchExpressions {
		expr1[i] = fmt.Sprintf("%s-%s-%v", exp.Key, exp.Operator, exp.Values)
	}

	for i, exp := range s2.MatchExpressions {
		expr2[i] = fmt.Sprintf("%s-%s-%v", exp.Key, exp.Operator, exp.Values)
	}

	// Sort both slices to ensure consistent comparison
	sort.Strings(expr1)
	sort.Strings(expr2)

	// Compare the sorted expressions
	for i := range expr1 {
		if expr1[i] != expr2[i] {
			return false
		}
	}

	return true
}

// isExcluded checks if a replicaset should be excluded from collection
func (c *ReplicaSetCollector) isExcluded(replicaset *appsv1.ReplicaSet) bool {
	// Check if monitoring specific namespaces and this replicaset isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == replicaset.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if replicaset is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: replicaset.Namespace,
		Name:      replicaset.Name,
	}
	return c.excludedReplicaSets[key]
}

// Stop gracefully shuts down the replicaset collector
func (c *ReplicaSetCollector) Stop() error {
	c.logger.Info("Stopping replicaset collector")
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ReplicaSetCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ReplicaSetCollector) GetType() string {
	return "replica_set"
}

// IsAvailable checks if ReplicaSet resources can be accessed in the cluster
func (c *ReplicaSetCollector) IsAvailable(ctx context.Context) bool {
	return true
}
