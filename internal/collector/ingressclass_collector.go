// internal/collector/ingressclass_collector.go
package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// IngressClassCollector watches for IngressClass events and collects IngressClass data
type IngressClassCollector struct {
	client                 kubernetes.Interface
	informerFactory        informers.SharedInformerFactory
	ingressClassInformer   cache.SharedIndexInformer
	resourceChan           chan CollectedResource
	stopCh                 chan struct{}
	excludedIngressClasses map[string]bool
	logger                 logr.Logger
	mu                     sync.RWMutex
}

// NewIngressClassCollector creates a new collector for IngressClass resources
func NewIngressClassCollector(
	client kubernetes.Interface,
	excludedIngressClasses []string,
	logger logr.Logger,
) *IngressClassCollector {
	// Convert excluded IngressClasses to a map for quicker lookups
	excludedIngressClassesMap := make(map[string]bool)
	for _, class := range excludedIngressClasses {
		excludedIngressClassesMap[class] = true
	}

	return &IngressClassCollector{
		client:                 client,
		resourceChan:           make(chan CollectedResource, 50), // IngressClasses change infrequently
		stopCh:                 make(chan struct{}),
		excludedIngressClasses: excludedIngressClassesMap,
		logger:                 logger.WithName("ingressclass-collector"),
	}
}

// Start begins the IngressClass collection process
func (c *IngressClassCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting IngressClass collector")

	// Create informer factory - IngressClass is cluster-scoped, not namespaced
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create IngressClass informer
	c.ingressClassInformer = c.informerFactory.Networking().V1().IngressClasses().Informer()

	// Add event handlers
	_, err := c.ingressClassInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ingressClass := obj.(*networkingv1.IngressClass)
			c.handleIngressClassEvent(ingressClass, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldIngressClass := oldObj.(*networkingv1.IngressClass)
			newIngressClass := newObj.(*networkingv1.IngressClass)

			// Only handle meaningful updates
			if c.ingressClassChanged(oldIngressClass, newIngressClass) {
				c.handleIngressClassEvent(newIngressClass, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			ingressClass := obj.(*networkingv1.IngressClass)
			c.handleIngressClassEvent(ingressClass, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.ingressClassInformer.HasSynced) {
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

// handleIngressClassEvent processes IngressClass events
func (c *IngressClassCollector) handleIngressClassEvent(ingressClass *networkingv1.IngressClass, eventType string) {
	if c.isExcluded(ingressClass) {
		return
	}

	c.logger.V(4).Info("Processing IngressClass event",
		"name", ingressClass.Name,
		"eventType", eventType,
		"controller", ingressClass.Spec.Controller)

	// Send the raw IngressClass object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: IngressClass,
		Object:       ingressClass, // Send the entire IngressClass object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          ingressClass.Name, // IngressClass is cluster-scoped, so just the name is sufficient
	}
}

// ingressClassChanged detects meaningful changes in an IngressClass
func (c *IngressClassCollector) ingressClassChanged(oldIngressClass, newIngressClass *networkingv1.IngressClass) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldIngressClass.ResourceVersion == newIngressClass.ResourceVersion {
		return false
	}

	// Check for controller changes
	if oldIngressClass.Spec.Controller != newIngressClass.Spec.Controller {
		return true
	}

	// Check for parameters changes
	if (oldIngressClass.Spec.Parameters == nil) != (newIngressClass.Spec.Parameters == nil) {
		return true
	}

	if oldIngressClass.Spec.Parameters != nil && newIngressClass.Spec.Parameters != nil {
		if oldIngressClass.Spec.Parameters.APIGroup != newIngressClass.Spec.Parameters.APIGroup ||
			oldIngressClass.Spec.Parameters.Kind != newIngressClass.Spec.Parameters.Kind ||
			oldIngressClass.Spec.Parameters.Name != newIngressClass.Spec.Parameters.Name ||
			oldIngressClass.Spec.Parameters.Namespace != newIngressClass.Spec.Parameters.Namespace {
			return true
		}
	}

	// Check for annotation changes
	if !mapsEqual(oldIngressClass.Annotations, newIngressClass.Annotations) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldIngressClass.Labels, newIngressClass.Labels) {
		return true
	}

	// Check default class annotation
	oldDefault := oldIngressClass.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true"
	newDefault := newIngressClass.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true"
	if oldDefault != newDefault {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if an IngressClass should be excluded from collection
func (c *IngressClassCollector) isExcluded(ingressClass *networkingv1.IngressClass) bool {
	// Check if IngressClass is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedIngressClasses[ingressClass.Name]
}

// Stop gracefully shuts down the IngressClass collector
func (c *IngressClassCollector) Stop() error {
	c.logger.Info("Stopping IngressClass collector")
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *IngressClassCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *IngressClassCollector) GetType() string {
	return "ingress_class"
}

// IsAvailable checks if IngressClass resources can be accessed in the cluster
func (c *IngressClassCollector) IsAvailable(ctx context.Context) bool {
	// Try to list IngressClasses with a limit of 1 to check availability with minimal overhead
	_, err := c.client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{
		Limit: 1,
	})

	if err != nil {
		// Check if this is a "resource not found" type error
		if strings.Contains(err.Error(),
			"the server could not find the requested resource") {
			c.logger.Info("IngressClass API not available in cluster (requires Kubernetes 1.19+)")
			return false
		}

		// For other errors (permissions, etc), log the error
		c.logger.Error(err, "Error checking IngressClass availability")
		return false
	}

	return true
}
