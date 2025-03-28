// internal/collector/crd_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// CRDCollector watches for CRD events and collects CRD data
type CRDCollector struct {
	client          apiextensionsclientset.Interface
	informerFactory apiextensionsinformers.SharedInformerFactory
	crdInformer     cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	excludedCRDs    map[string]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// NewCRDCollector creates a new collector for CRD resources
func NewCRDCollector(
	client apiextensionsclientset.Interface,
	excludedCRDs []string,
	logger logr.Logger,
) *CRDCollector {
	// Convert excluded CRDs to a map for quicker lookups
	excludedCRDsMap := make(map[string]bool)
	for _, crd := range excludedCRDs {
		excludedCRDsMap[crd] = true
	}

	return &CRDCollector{
		client:       client,
		resourceChan: make(chan CollectedResource, 100),
		stopCh:       make(chan struct{}),
		excludedCRDs: excludedCRDsMap,
		logger:       logger.WithName("crd-collector"),
	}
}

// Start begins the CRD collection process
func (c *CRDCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CustomResourceDefinition collector")

	// Create informer factory - CRDs are cluster-scoped, not namespaced
	c.informerFactory = apiextensionsinformers.NewSharedInformerFactory(c.client, 0)

	// Create CRD informer
	c.crdInformer = c.informerFactory.Apiextensions().V1().CustomResourceDefinitions().Informer()

	// Add event handlers
	_, err := c.crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			crd := obj.(*apiextensionsv1.CustomResourceDefinition)
			c.handleCRDEvent(crd, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCRD := oldObj.(*apiextensionsv1.CustomResourceDefinition)
			newCRD := newObj.(*apiextensionsv1.CustomResourceDefinition)

			// Only handle meaningful updates
			if c.crdChanged(oldCRD, newCRD) {
				c.handleCRDEvent(newCRD, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			crd := obj.(*apiextensionsv1.CustomResourceDefinition)
			c.handleCRDEvent(crd, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.crdInformer.HasSynced) {
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

// handleCRDEvent processes CRD events
func (c *CRDCollector) handleCRDEvent(crd *apiextensionsv1.CustomResourceDefinition, eventType string) {
	if c.isExcluded(crd) {
		return
	}

	c.logger.V(4).Info("Processing CustomResourceDefinition event",
		"name", crd.Name,
		"group", crd.Spec.Group,
		"names", crd.Spec.Names.Kind,
		"eventType", eventType)

	// Send the raw CRD object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: CustomResourceDefinition,
		Object:       crd, // Send the entire CRD object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          crd.Name, // CRDs are cluster-scoped, so just the name is sufficient
	}
}

// crdChanged detects meaningful changes in a CRD
func (c *CRDCollector) crdChanged(oldCRD, newCRD *apiextensionsv1.CustomResourceDefinition) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldCRD.ResourceVersion == newCRD.ResourceVersion {
		return false
	}

	// Check for spec changes
	if !reflect.DeepEqual(oldCRD.Spec, newCRD.Spec) {
		return true
	}

	// Check for status changes - particularly conditions
	if len(oldCRD.Status.Conditions) != len(newCRD.Status.Conditions) {
		return true
	}

	for i, oldCondition := range oldCRD.Status.Conditions {
		if i >= len(newCRD.Status.Conditions) {
			return true
		}

		newCondition := newCRD.Status.Conditions[i]
		if oldCondition.Type != newCondition.Type ||
			oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return true
		}
	}

	// Check for accepted names changes
	if !reflect.DeepEqual(oldCRD.Status.AcceptedNames, newCRD.Status.AcceptedNames) {
		return true
	}

	// Check for stored versions changes
	if !stringSlicesEqual(oldCRD.Status.StoredVersions, newCRD.Status.StoredVersions) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldCRD.Labels, newCRD.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldCRD.Annotations, newCRD.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a CRD should be excluded from collection
func (c *CRDCollector) isExcluded(crd *apiextensionsv1.CustomResourceDefinition) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedCRDs[crd.Name]
}

// Stop gracefully shuts down the CRD collector
func (c *CRDCollector) Stop() error {
	c.logger.Info("Stopping CustomResourceDefinition collector")
	if c.stopCh != nil {
		if c.stopCh != nil {
			close(c.stopCh)
			c.stopCh = nil
		}
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *CRDCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CRDCollector) GetType() string {
	return "customresourcedefinition"
}

// IsAvailable checks if CRD resources can be accessed in the cluster
func (c *CRDCollector) IsAvailable(ctx context.Context) bool {
	// Try to list CRDs with limit=1 to check availability with minimal overhead
	_, err := c.client.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{
		Limit: 1,
	})

	if err != nil {
		c.logger.Info("CustomResourceDefinition API not available", "error", err.Error())
		return false
	}

	return true
}
