// internal/collector/customresource_collector.go
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// CustomResourceCollector watches for custom resource instances based on CRDs
type CustomResourceCollector struct {
	apiExtensionsClient    apiextensionsclientset.Interface
	discoveryClient        discovery.DiscoveryInterface
	dynamicClient          dynamic.Interface
	informerFactories      map[schema.GroupVersionResource]dynamicinformer.DynamicSharedInformerFactory
	informers              map[schema.GroupVersionResource]cache.SharedIndexInformer
	resourceChan           chan CollectedResource
	stopCh                 chan struct{}
	namespaces             []string
	excludedResourcesByGVR map[schema.GroupVersionResource]map[types.NamespacedName]bool
	excludedCRDGroups      map[string]bool // Exclude entire CRD groups
	watchedCRDs            map[string]bool // If specified, only watch these CRDs
	resourcesMutex         sync.RWMutex
	informersMutex         sync.RWMutex
	logger                 logr.Logger
}

// ExcludedCustomResource defines a custom resource to exclude from collection
type ExcludedCustomResource struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
}

// CustomResourceCollectorConfig holds configuration for CustomResourceCollector
type CustomResourceCollectorConfig struct {
	Namespaces        []string
	ExcludedResources []ExcludedCustomResource
	ExcludedCRDGroups []string // Exclude entire CRD groups
	WatchedCRDs       []string // If non-empty, only watch these CRDs
	ResyncPeriod      time.Duration
}

// NewCustomResourceCollector creates a collector for custom resources
func NewCustomResourceCollector(
	apiExtensionsClient apiextensionsclientset.Interface,
	discoveryClient discovery.DiscoveryInterface,
	dynamicClient dynamic.Interface,
	config CustomResourceCollectorConfig,
	logger logr.Logger,
) *CustomResourceCollector {
	// Convert excluded CRD groups to a map for quicker lookups
	excludedCRDGroupsMap := make(map[string]bool)
	for _, group := range config.ExcludedCRDGroups {
		excludedCRDGroupsMap[group] = true
	}

	// Convert watched CRDs to a map for quicker lookups
	watchedCRDsMap := make(map[string]bool)
	for _, crd := range config.WatchedCRDs {
		watchedCRDsMap[crd] = true
	}

	// Convert excluded resources to nested maps for quicker lookups
	excludedResourcesByGVR := make(map[schema.GroupVersionResource]map[types.NamespacedName]bool)
	for _, res := range config.ExcludedResources {
		if _, exists := excludedResourcesByGVR[res.GVR]; !exists {
			excludedResourcesByGVR[res.GVR] = make(map[types.NamespacedName]bool)
		}
		excludedResourcesByGVR[res.GVR][types.NamespacedName{
			Namespace: res.Namespace,
			Name:      res.Name,
		}] = true
	}

	// Default resync period if not set
	if config.ResyncPeriod <= 0 {
		config.ResyncPeriod = 10 * time.Minute
	}

	return &CustomResourceCollector{
		apiExtensionsClient:    apiExtensionsClient,
		discoveryClient:        discoveryClient,
		dynamicClient:          dynamicClient,
		informerFactories:      make(map[schema.GroupVersionResource]dynamicinformer.DynamicSharedInformerFactory),
		informers:              make(map[schema.GroupVersionResource]cache.SharedIndexInformer),
		resourceChan:           make(chan CollectedResource, 500), // Higher buffer for many CRs
		stopCh:                 make(chan struct{}),
		namespaces:             config.Namespaces,
		excludedResourcesByGVR: excludedResourcesByGVR,
		excludedCRDGroups:      excludedCRDGroupsMap,
		watchedCRDs:            watchedCRDsMap,
		logger:                 logger.WithName("customresource-collector"),
	}
}

// Start begins the custom resource collection process
func (c *CustomResourceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CustomResource collector", "namespaces", c.namespaces)

	// Discover all CRDs in the cluster
	crds, err := c.discoverCRDs(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover CRDs: %w", err)
	}

	// Setup informers for each CRD
	for _, crd := range crds {
		if err := c.setupInformersForCRD(ctx, crd); err != nil {
			c.logger.Error(err, "Failed to set up informer for CRD",
				"name", crd.Name,
				"group", crd.Spec.Group)
		}
	}

	// Start all informers
	c.informersMutex.RLock()
	c.logger.Info("Starting all custom resource informers", "count", len(c.informerFactories))
	for gvr, factory := range c.informerFactories {
		c.logger.V(4).Info("Starting informer factory", "gvr", gvr.String())
		factory.Start(c.stopCh)
	}
	c.informersMutex.RUnlock()

	// Wait for all informers to sync
	c.logger.Info("Waiting for custom resource informer caches to sync")
	c.informersMutex.RLock()
	for gvr, informer := range c.informers {
		if !cache.WaitForCacheSync(c.stopCh, informer.HasSynced) {
			c.logger.Error(nil, "Timed out waiting for cache to sync", "gvr", gvr.String())
		}
	}
	c.informersMutex.RUnlock()
	c.logger.Info("Custom resource informer caches synced")

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

// discoverCRDs finds all CRDs in the cluster
func (c *CustomResourceCollector) discoverCRDs(ctx context.Context) ([]apiextensionsv1.CustomResourceDefinition, error) {
	crdList, err := c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list CRDs: %w", err)
	}

	// Apply filtering based on configuration
	var filteredCRDs []apiextensionsv1.CustomResourceDefinition

	for _, crd := range crdList.Items {
		// Skip if the CRD's group is in the excluded groups
		if c.excludedCRDGroups[crd.Spec.Group] {
			c.logger.V(4).Info("Skipping excluded CRD group",
				"name", crd.Name,
				"group", crd.Spec.Group)
			continue
		}

		// If we have a specific list of CRDs to watch, check if this one is included
		if len(c.watchedCRDs) > 0 {
			if !c.watchedCRDs[crd.Name] {
				c.logger.V(4).Info("Skipping non-watched CRD",
					"name", crd.Name,
					"group", crd.Spec.Group)
				continue
			}
		}

		filteredCRDs = append(filteredCRDs, crd)
	}

	c.logger.Info("Discovered CRDs after filtering",
		"total", len(crdList.Items),
		"filtered", len(filteredCRDs))

	return filteredCRDs, nil
}

// setupInformersForCRD creates informers for a specific CRD
func (c *CustomResourceCollector) setupInformersForCRD(ctx context.Context, crd apiextensionsv1.CustomResourceDefinition) error {
	for _, version := range crd.Spec.Versions {
		// Skip versions that aren't served
		if !version.Served {
			continue
		}

		// Create GVR for this custom resource
		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  version.Name,
			Resource: crd.Spec.Names.Plural,
		}

		// Skip if we already have an informer for this GVR
		c.informersMutex.RLock()
		_, exists := c.informers[gvr]
		c.informersMutex.RUnlock()
		if exists {
			continue
		}

		c.logger.V(4).Info("Setting up informer for custom resource",
			"name", crd.Name,
			"group", gvr.Group,
			"version", gvr.Version,
			"resource", gvr.Resource)

		// Create dynamic informer factory for this GVR
		var factory dynamicinformer.DynamicSharedInformerFactory

		if len(c.namespaces) == 1 && c.namespaces[0] != "" {
			// Watch a specific namespace
			factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
				c.dynamicClient,
				0, // No resync period, rely on events
				c.namespaces[0],
				nil,
			)
		} else {
			// Watch all namespaces
			factory = dynamicinformer.NewDynamicSharedInformerFactory(
				c.dynamicClient,
				0,
			)
		}

		// Create informer for this resource
		informer := factory.ForResource(gvr).Informer()

		// Add event handlers
		_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				u := obj.(*unstructured.Unstructured)
				c.handleCustomResourceEvent(gvr, u, "add")
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldU := oldObj.(*unstructured.Unstructured)
				newU := newObj.(*unstructured.Unstructured)

				if c.customResourceChanged(oldU, newU) {
					c.handleCustomResourceEvent(gvr, newU, "update")
				}
			},
			DeleteFunc: func(obj interface{}) {
				u := obj.(*unstructured.Unstructured)
				c.handleCustomResourceEvent(gvr, u, "delete")
			},
		})
		if err != nil {
			return fmt.Errorf("failed to add event handler for %s: %w", gvr.String(), err)
		}

		// Store the factory and informer
		c.informersMutex.Lock()
		c.informerFactories[gvr] = factory
		c.informers[gvr] = informer
		c.informersMutex.Unlock()
	}

	return nil
}

// handleCustomResourceEvent processes custom resource events
func (c *CustomResourceCollector) handleCustomResourceEvent(
	gvr schema.GroupVersionResource,
	obj *unstructured.Unstructured,
	eventType string,
) {
	// Skip if this resource is excluded
	if c.isExcluded(gvr, obj) {
		return
	}

	name := obj.GetName()
	namespace := obj.GetNamespace()

	c.logger.V(4).Info("Processing custom resource event",
		"gvr", gvr.String(),
		"namespace", namespace,
		"name", name,
		"eventType", eventType)

	// Create a key based on whether the resource is namespaced
	var key string
	if namespace != "" {
		key = fmt.Sprintf("%s/%s", namespace, name)
	} else {
		key = name
	}

	// Send the custom resource to the channel
	c.resourceChan <- CollectedResource{
		ResourceType: CustomResource,
		Object:       obj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// customResourceChanged detects meaningful changes in a custom resource
func (c *CustomResourceCollector) customResourceChanged(oldObj, newObj *unstructured.Unstructured) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldObj.GetResourceVersion() == newObj.GetResourceVersion() {
		return false
	}

	// Check for spec changes
	oldSpec, _, _ := unstructured.NestedMap(oldObj.Object, "spec")
	newSpec, _, _ := unstructured.NestedMap(newObj.Object, "spec")
	if !reflect.DeepEqual(oldSpec, newSpec) {
		return true
	}

	// Check for status changes
	oldStatus, _, _ := unstructured.NestedMap(oldObj.Object, "status")
	newStatus, _, _ := unstructured.NestedMap(newObj.Object, "status")
	if !reflect.DeepEqual(oldStatus, newStatus) {
		return true
	}

	// Check for metadata changes (excluding resourceVersion)
	if !reflect.DeepEqual(oldObj.GetLabels(), newObj.GetLabels()) ||
		!reflect.DeepEqual(oldObj.GetAnnotations(), newObj.GetAnnotations()) ||
		!reflect.DeepEqual(oldObj.GetFinalizers(), newObj.GetFinalizers()) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a custom resource should be excluded from collection
func (c *CustomResourceCollector) isExcluded(
	gvr schema.GroupVersionResource,
	obj *unstructured.Unstructured,
) bool {
	namespace := obj.GetNamespace()
	name := obj.GetName()

	// Check if monitoring specific namespaces and this resource isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" && namespace != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if resource is specifically excluded
	c.resourcesMutex.RLock()
	defer c.resourcesMutex.RUnlock()

	if excludedResources, gvrExists := c.excludedResourcesByGVR[gvr]; gvrExists {
		key := types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}
		return excludedResources[key]
	}

	return false
}

// Stop gracefully shuts down the collector
func (c *CustomResourceCollector) Stop() error {
	c.logger.Info("Stopping CustomResource collector")
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
func (c *CustomResourceCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CustomResourceCollector) GetType() string {
	return "customresource"
}

// IsAvailable checks if CustomResource collection is available in the cluster
func (c *CustomResourceCollector) IsAvailable(ctx context.Context) bool {
	// First check if the apiextensions API is available
	_, err := c.apiExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{
		Limit: 1, // Only request a single item to minimize load
	})

	if err != nil {
		c.logger.Info("CustomResourceDefinition API not available", "error", err.Error())
		return false
	}

	// The dynamic client should always be available if the apiextensions API is,
	// but we need both for this collector to function properly
	if c.dynamicClient == nil {
		c.logger.Info("Dynamic client not available")
		return false
	}

	return true
}
