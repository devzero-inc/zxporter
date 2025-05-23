// internal/collector/karpenter_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// KarpenterResource defines a Karpenter resource to be watched
type KarpenterResource struct {
	GroupVersion schema.GroupVersion
	Resource     string
	Kind         string
}

// KarpenterCollector watches for Karpenter resources
type KarpenterCollector struct {
	dynamicClient     dynamic.Interface
	batchChan         chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan      chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher           *ResourcesBatcher
	stopCh            chan struct{}
	logger            logr.Logger
	informers         map[string]cache.SharedIndexInformer
	informerStopChs   map[string]chan struct{}
	excludedResources map[string]map[string]bool // resourceType -> resourceName -> excluded
	mu                sync.RWMutex
}

// NewKarpenterCollector creates a new collector for Karpenter resources
func NewKarpenterCollector(
	dynamicClient dynamic.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *KarpenterCollector {
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

	return &KarpenterCollector{
		dynamicClient:     dynamicClient,
		batchChan:         batchChan,
		resourceChan:      resourceChan,
		batcher:           batcher,
		stopCh:            make(chan struct{}),
		logger:            logger.WithName("karpenter-collector"),
		informers:         make(map[string]cache.SharedIndexInformer),
		informerStopChs:   make(map[string]chan struct{}),
		excludedResources: make(map[string]map[string]bool),
	}
}

// Start begins the Karpenter resources collection process
func (c *KarpenterCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Karpenter collector")

	// Define all Karpenter resources to watch
	resources := []KarpenterResource{
		// v1alpha5 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha5"},
			Resource:     "provisioners",
			Kind:         "Provisioner",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha5"},
			Resource:     "machines",
			Kind:         "Machine",
		},
		// v1alpha1 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1alpha1"},
			Resource:     "awsnodetemplates",
			Kind:         "AWSNodeTemplate",
		},
		// v1beta1 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1beta1"},
			Resource:     "nodepools",
			Kind:         "NodePool",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1beta1"},
			Resource:     "nodeclaims",
			Kind:         "NodeClaim",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1beta1"},
			Resource:     "ec2nodeclasses",
			Kind:         "EC2NodeClass",
		},
		// v1 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1"},
			Resource:     "nodepools",
			Kind:         "NodePool",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1"},
			Resource:     "nodeclaims",
			Kind:         "NodeClaim",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1"},
			Resource:     "ec2nodeclasses",
			Kind:         "EC2NodeClass",
		},
	}

	// Create informers for each resource type
	for _, res := range resources {
		if err := c.startResourceInformer(ctx, res); err != nil {
			c.logger.Error(err, "Failed to start informer",
				"group", res.GroupVersion.Group,
				"version", res.GroupVersion.Version,
				"resource", res.Resource)
			// Continue with other resources even if one fails
		}
	}

	// Start the batcher after all informers are synced
	c.logger.Info("Starting resources batcher for Karpenter resources")
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

// startResourceInformer creates and starts an informer for a specific Karpenter resource
func (c *KarpenterCollector) startResourceInformer(ctx context.Context, res KarpenterResource) error {
	// Create a resource-specific GVR
	gvr := schema.GroupVersionResource{
		Group:    res.GroupVersion.Group,
		Version:  res.GroupVersion.Version,
		Resource: res.Resource,
	}

	// Create a unique key for this resource
	resKey := fmt.Sprintf("%s.%s.%s", res.GroupVersion.Group, res.GroupVersion.Version, res.Resource)

	// Create a dynamic informer factory
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient,
		0,  // No resync period
		"", // All namespaces
		nil,
	)

	// Create an informer for this resource
	informer := factory.ForResource(gvr).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to unstructured", "resource", resKey)
				return
			}
			c.handleKarpenterResourceEvent(u, res, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			_, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert old object to unstructured", "resource", resKey)
				return
			}

			newU, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert new object to unstructured", "resource", resKey)
				return
			}

			c.handleKarpenterResourceEvent(newU, res, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleKarpenterResourceEvent(u, res, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object", "resource", resKey)
				return
			}
			c.handleKarpenterResourceEvent(u, res, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler to informer for %s: %w", resKey, err)
	}

	// Create a stop channel for this informer
	stopCh := make(chan struct{})
	c.informerStopChs[resKey] = stopCh

	// Store the informer
	c.informers[resKey] = informer

	// Start the informer
	go informer.Run(stopCh)

	// Wait for cache sync with timeout
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for %s cache to sync", resKey)
	}

	c.logger.Info("Successfully started informer",
		"group", res.GroupVersion.Group,
		"version", res.GroupVersion.Version,
		"resource", res.Resource)

	return nil
}

// handleKarpenterResourceEvent processes Karpenter resource events
func (c *KarpenterCollector) handleKarpenterResourceEvent(
	obj *unstructured.Unstructured,
	resource KarpenterResource,
	eventType EventType,
) {
	name := obj.GetName()
	namespace := obj.GetNamespace()
	kind := obj.GetKind()

	// Check if this resource should be excluded
	if c.isExcluded(resource.Resource, namespace, name) {
		return
	}

	// Create a resource-specific key
	var key string
	if namespace != "" {
		key = fmt.Sprintf("%s/%s/%s", resource.Resource, namespace, name)
	} else {
		key = fmt.Sprintf("%s/%s", resource.Resource, name)
	}

	c.logger.Info("Processing Karpenter resource event",
		"kind", kind,
		"apiVersion", obj.GetAPIVersion(),
		"name", name,
		"namespace", namespace,
		"eventType", eventType.String())

	// Process resource based on its kind
	var processedObj map[string]interface{}

	switch resource.Kind {
	case "Provisioner":
		processedObj = c.processProvisioner(obj)
	case "Machine":
		processedObj = c.processMachine(obj)
	case "NodePool":
		processedObj = c.processNodePool(obj)
	case "NodeClaim":
		processedObj = c.processNodeClaim(obj)
	case "AWSNodeTemplate":
		processedObj = c.processAWSNodeTemplate(obj)
	case "EC2NodeClass":
		processedObj = c.processEC2NodeClass(obj)
	default:
		// Generic processing for unknown types
		processedObj = c.processGenericResource(obj)
	}

	// Send the Karpenter resource to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Karpenter,
		Object:       processedObj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processProvisioner extracts relevant fields from Provisioner objects
func (c *KarpenterCollector) processProvisioner(obj *unstructured.Unstructured) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract provisioner-specific fields
	limits, found, _ := unstructured.NestedMap(obj.Object, "spec", "limits")
	if found {
		result["limits"] = limits
	}

	requirements, found, _ := unstructured.NestedSlice(obj.Object, "spec", "requirements")
	if found {
		result["requirements"] = requirements
	}

	// Add any status information
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	return result
}

// processMachine extracts relevant fields from Machine objects
func (c *KarpenterCollector) processMachine(obj *unstructured.Unstructured) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract machine-specific fields
	machineClass, found, _ := unstructured.NestedString(obj.Object, "spec", "machineClass")
	if found {
		result["machineClass"] = machineClass
	}

	// Get node name if assigned
	nodeName, found, _ := unstructured.NestedString(obj.Object, "status", "nodeName")
	if found {
		result["nodeName"] = nodeName
	}

	// Get phase
	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if found {
		result["phase"] = phase
	}

	// Get conditions
	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if found {
		result["conditions"] = conditions
	}

	return result
}

// processNodePool extracts relevant fields from NodePool objects
func (c *KarpenterCollector) processNodePool(obj *unstructured.Unstructured) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract nodepool-specific fields
	limits, found, _ := unstructured.NestedMap(obj.Object, "spec", "limits")
	if found {
		result["limits"] = limits
	}

	disruption, found, _ := unstructured.NestedMap(obj.Object, "spec", "disruption")
	if found {
		result["disruption"] = disruption
	}

	template, found, _ := unstructured.NestedMap(obj.Object, "spec", "template")
	if found {
		result["template"] = template
	}

	// Status information
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	return result
}

// processNodeClaim extracts relevant fields from NodeClaim objects
func (c *KarpenterCollector) processNodeClaim(obj *unstructured.Unstructured) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract nodeclaim-specific fields
	requirements, found, _ := unstructured.NestedSlice(obj.Object, "spec", "requirements")
	if found {
		result["requirements"] = requirements
	}

	resources, found, _ := unstructured.NestedMap(obj.Object, "spec", "resources")
	if found {
		result["resources"] = resources
	}

	// Status information
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	// Get node name if assigned
	nodeName, found, _ := unstructured.NestedString(obj.Object, "status", "nodeName")
	if found {
		result["nodeName"] = nodeName
	}

	// Get phase
	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if found {
		result["phase"] = phase
	}

	return result
}

// processAWSNodeTemplate extracts relevant fields from AWSNodeTemplate objects
func (c *KarpenterCollector) processAWSNodeTemplate(obj *unstructured.Unstructured) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract AWS-specific fields
	instanceTypes, found, _ := unstructured.NestedSlice(obj.Object, "spec", "instanceTypes")
	if found {
		result["instanceTypes"] = instanceTypes
	}

	subnetSelector, found, _ := unstructured.NestedMap(obj.Object, "spec", "subnetSelector")
	if found {
		result["subnetSelector"] = subnetSelector
	}

	securityGroupSelector, found, _ := unstructured.NestedMap(obj.Object, "spec", "securityGroupSelector")
	if found {
		result["securityGroupSelector"] = securityGroupSelector
	}

	amiFamilies, found, _ := unstructured.NestedSlice(obj.Object, "spec", "amiFamilies")
	if found {
		result["amiFamilies"] = amiFamilies
	}

	return result
}

// processEC2NodeClass extracts relevant fields from EC2NodeClass objects
func (c *KarpenterCollector) processEC2NodeClass(obj *unstructured.Unstructured) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract EC2NodeClass-specific fields
	instanceTypes, found, _ := unstructured.NestedSlice(obj.Object, "spec", "instanceTypes")
	if found {
		result["instanceTypes"] = instanceTypes
	}

	subnetSelectorTerms, found, _ := unstructured.NestedSlice(obj.Object, "spec", "subnetSelectorTerms")
	if found {
		result["subnetSelectorTerms"] = subnetSelectorTerms
	}

	securityGroupSelectorTerms, found, _ := unstructured.NestedSlice(obj.Object, "spec", "securityGroupSelectorTerms")
	if found {
		result["securityGroupSelectorTerms"] = securityGroupSelectorTerms
	}

	amiSelectorTerms, found, _ := unstructured.NestedSlice(obj.Object, "spec", "amiSelectorTerms")
	if found {
		result["amiSelectorTerms"] = amiSelectorTerms
	}

	userData, found, _ := unstructured.NestedString(obj.Object, "spec", "userData")
	if found && userData != "" {
		result["hasUserData"] = true
	}

	role, found, _ := unstructured.NestedString(obj.Object, "spec", "role")
	if found {
		result["role"] = role
	}

	// Check status
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	return result
}

// processGenericResource provides basic processing for unknown resource types
func (c *KarpenterCollector) processGenericResource(obj *unstructured.Unstructured) map[string]interface{} {
	return c.extractCommonFields(obj)
}

// extractCommonFields gets fields common to all Karpenter resources
func (c *KarpenterCollector) extractCommonFields(obj *unstructured.Unstructured) map[string]interface{} {
	// Basic metadata
	result := map[string]interface{}{
		"name":              obj.GetName(),
		"namespace":         obj.GetNamespace(),
		"uid":               string(obj.GetUID()),
		"kind":              obj.GetKind(),
		"apiVersion":        obj.GetAPIVersion(),
		"labels":            obj.GetLabels(),
		"annotations":       obj.GetAnnotations(),
		"creationTimestamp": obj.GetCreationTimestamp().Unix(),
		"resourceVersion":   obj.GetResourceVersion(),
		"raw":               obj,
	}

	// Get owner references if any
	if owners := obj.GetOwnerReferences(); len(owners) > 0 {
		ownerRefs := make([]map[string]interface{}, 0, len(owners))
		for _, owner := range owners {
			ownerRefs = append(ownerRefs, map[string]interface{}{
				"apiVersion": owner.APIVersion,
				"kind":       owner.Kind,
				"name":       owner.Name,
				"uid":        string(owner.UID),
			})
		}
		result["ownerReferences"] = ownerRefs
	}

	// Include finalizers if present
	if finalizers := obj.GetFinalizers(); len(finalizers) > 0 {
		result["finalizers"] = finalizers
	}

	return result
}

// isExcluded checks if a resource should be excluded
func (c *KarpenterCollector) isExcluded(resourceType, namespace, name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check if there are exclusions for this resource type
	excludedNames, exists := c.excludedResources[resourceType]
	if !exists {
		return false
	}

	// For namespaced resources, use namespace/name as the key
	key := name
	if namespace != "" {
		key = fmt.Sprintf("%s/%s", namespace, name)
	}

	return excludedNames[key]
}

// ExcludeResource adds a resource to the exclusion list
func (c *KarpenterCollector) ExcludeResource(resourceType, namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Initialize the exclusion map for this resource type if needed
	if _, exists := c.excludedResources[resourceType]; !exists {
		c.excludedResources[resourceType] = make(map[string]bool)
	}

	// For namespaced resources, use namespace/name as the key
	key := name
	if namespace != "" {
		key = fmt.Sprintf("%s/%s", namespace, name)
	}

	c.excludedResources[resourceType][key] = true
}

// Stop gracefully shuts down all informers
func (c *KarpenterCollector) Stop() error {
	c.logger.Info("Stopping Karpenter collector")

	// Stop all informers
	for key, stopCh := range c.informerStopChs {
		c.logger.Info("Stopping informer", "resource", key)
		close(stopCh)
	}

	// Clear maps
	c.informers = make(map[string]cache.SharedIndexInformer)
	c.informerStopChs = make(map[string]chan struct{})

	// Close the main stop channel (signals informers to stop)
	select {
	case <-c.stopCh:
		c.logger.Info("Karpenter collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Karpenter collector stop channel")
	}

	// Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Karpenter collector batch input channel")
	}

	// Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Karpenter collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *KarpenterCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *KarpenterCollector) GetType() string {
	return "karpenter"
}

// IsAvailable checks if Karpenter resources can be accessed in the cluster
func (c *KarpenterCollector) IsAvailable(ctx context.Context) bool {
	// Try to list NodePools to see if Karpenter is installed
	// Check both v1beta1 and v1 api versions
	gvrs := []schema.GroupVersionResource{
		{
			Group:    "karpenter.sh",
			Version:  "v1",
			Resource: "nodepools",
		},
		{
			Group:    "karpenter.sh",
			Version:  "v1beta1",
			Resource: "nodepools",
		},
		{
			Group:    "karpenter.sh",
			Version:  "v1alpha5",
			Resource: "provisioners",
		},
	}

	for _, gvr := range gvrs {
		_, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
		if err == nil {
			c.logger.Info("Karpenter resources available", "gvr", gvr.String())
			return true
		}
	}

	c.logger.Info("Karpenter resources not available in the cluster")
	return false
}
