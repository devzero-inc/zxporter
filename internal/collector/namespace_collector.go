// internal/collector/namespace_collector.go
package collector

import (
	"context"
	"fmt"
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
	resourceChan       chan CollectedResource
	stopCh             chan struct{}
	excludedNamespaces map[string]bool
	logger             logr.Logger
	mu                 sync.RWMutex
}

// NewNamespaceCollector creates a new collector for namespace resources
func NewNamespaceCollector(
	client kubernetes.Interface,
	excludedNamespaces []string,
	logger logr.Logger,
) *NamespaceCollector {
	// Convert excluded namespaces to a map for quicker lookups
	excludedNamespacesMap := make(map[string]bool)
	for _, namespace := range excludedNamespaces {
		excludedNamespacesMap[namespace] = true
	}

	return &NamespaceCollector{
		client:             client,
		resourceChan:       make(chan CollectedResource, 50), // Namespaces change less frequently
		stopCh:             make(chan struct{}),
		excludedNamespaces: excludedNamespacesMap,
		logger:             logger.WithName("namespace-collector"),
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
			c.handleNamespaceEvent(namespace, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNamespace := oldObj.(*corev1.Namespace)
			newNamespace := newObj.(*corev1.Namespace)

			// Only handle meaningful updates
			if c.namespaceChanged(oldNamespace, newNamespace) {
				c.handleNamespaceEvent(newNamespace, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			namespace := obj.(*corev1.Namespace)
			c.handleNamespaceEvent(namespace, "delete")
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

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleNamespaceEvent processes namespace events
func (c *NamespaceCollector) handleNamespaceEvent(namespace *corev1.Namespace, eventType string) {
	if c.isExcluded(namespace) {
		return
	}

	c.logger.Info("Processing namespace event",
		"name", namespace.Name,
		"eventType", eventType)

	// Send the raw namespace object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: Namespace,
		Object:       namespace, // Send the entire namespace object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          namespace.Name,
	}
}

// namespaceChanged detects meaningful changes in a namespace
func (c *NamespaceCollector) namespaceChanged(oldNamespace, newNamespace *corev1.Namespace) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldNamespace.ResourceVersion == newNamespace.ResourceVersion {
		return false
	}

	// Check for status phase changes
	if oldNamespace.Status.Phase != newNamespace.Status.Phase {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldNamespace.Labels, newNamespace.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldNamespace.Annotations, newNamespace.Annotations) {
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *NamespaceCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *NamespaceCollector) GetType() string {
	return "namespace"
}

// finalizerSlicesEqual compares two FinalizerName slices for equality
func finalizerSlicesEqual(s1, s2 []corev1.FinalizerName) bool {
	if len(s1) != len(s2) {
		return false
	}

	for i := range s1 {
		if s1[i] != s2[i] {
			return false
		}
	}

	return true
}

// IsAvailable checks if Namespace resources can be accessed in the cluster
func (c *NamespaceCollector) IsAvailable(ctx context.Context) bool {
	return true
}
